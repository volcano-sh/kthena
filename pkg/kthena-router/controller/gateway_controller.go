/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type GatewayController struct {
	gatewayLister gatewaylisters.GatewayLister
	gatewaySynced cache.InformerSynced
	registration  cache.ResourceEventHandlerRegistration
	gatewayClient gatewayclientset.Interface

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewGatewayController(
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	store datastore.Store,
	gatewayClient gatewayclientset.Interface,
) (*GatewayController, error) {
	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()

	controller := &GatewayController{
		gatewayLister: gatewayInformer.Lister(),
		gatewaySynced: gatewayInformer.Informer().HasSynced,
		gatewayClient: gatewayClient,
		workqueue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:   &atomic.Bool{},
		store:         store,
	}

	// Filter handler to only process Gateways that reference the kthena-router GatewayClass
	filterHandler := &cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			gateway, ok := obj.(*gatewayv1.Gateway)
			if !ok {
				return false
			}
			return string(gateway.Spec.GatewayClassName) == DefaultGatewayClassName
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: controller.enqueueGateway,
			UpdateFunc: func(old, new interface{}) {
				controller.enqueueGateway(new)
			},
			DeleteFunc: controller.enqueueGateway,
		},
	}
	var err error
	controller.registration, err = gatewayInformer.Informer().AddEventHandler(filterHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler for gateway controller: %w", err)
	}
	return controller, nil
}

func (c *GatewayController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.registration.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *GatewayController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *GatewayController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *GatewayController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial gateways have been synced")
		c.workqueue.Forget(obj)
		c.initialSync.Store(true)
		return true
	}

	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		c.workqueue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}

	err := c.syncHandler(key)
	retryOrForget(c.workqueue, "gateway", key, err, klog.Errorf)
	return true
}

func (c *GatewayController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	gateway, err := c.gatewayLister.Gateways(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		_ = c.store.DeleteGateway(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Check GatewayClassName after lister lookup: the informer filter only
	// fires OnDelete(old) when a Gateway is switched away from our class, so
	// the object retrieved from the lister may belong to another controller.
	// Without this check, we would write status onto a Gateway we no longer own.
	if string(gateway.Spec.GatewayClassName) != DefaultGatewayClassName {
		_ = c.store.DeleteGateway(key)
		return nil
	}

	// Store the Gateway
	if err := c.store.AddOrUpdateGateway(gateway); err != nil {
		return err
	}

	// Update Gateway status to Accepted=True and Programmed=True
	if err := c.updateGatewayStatus(gateway); err != nil {
		klog.Errorf("Failed to update Gateway %s/%s status: %v", namespace, name, err)
	}

	return nil
}

// updateGatewayStatus patches the Gateway status with Accepted=True and Programmed=True.
func (c *GatewayController) updateGatewayStatus(gateway *gatewayv1.Gateway) error {
	ctx := context.Background()

	current, err := c.gatewayClient.GatewayV1().Gateways(gateway.Namespace).Get(ctx, gateway.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get gateway for status update: %w", err)
	}

	now := metav1.Now()
	acceptedCondition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: current.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            "Gateway is accepted by kthena-router controller",
	}
	programmedCondition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: current.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayReasonProgrammed),
		Message:            "Gateway is programmed and listening",
	}

	existingAccepted := apimeta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	existingProgrammed := apimeta.FindStatusCondition(current.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	if existingAccepted != nil && existingAccepted.Status == metav1.ConditionTrue &&
		existingProgrammed != nil && existingProgrammed.Status == metav1.ConditionTrue {
		return nil
	}

	updated := current.DeepCopy()
	apimeta.SetStatusCondition(&updated.Status.Conditions, acceptedCondition)
	apimeta.SetStatusCondition(&updated.Status.Conditions, programmedCondition)

	// Populate per-listener status for each listener in the Gateway spec.
	updated.Status.Listeners = buildListenerStatuses(gateway)

	_, err = c.gatewayClient.GatewayV1().Gateways(gateway.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update gateway status: %w", err)
	}

	klog.V(2).Infof("Updated Gateway %s/%s status: Accepted=True, Programmed=True", gateway.Namespace, gateway.Name)
	return nil
}

// buildListenerStatuses constructs per-listener status for each listener in the Gateway spec.
// Each listener gets Accepted=True, Programmed=True, ResolvedRefs=True, Conflicted=False,
// supportedKinds based on protocol, and attachedRoutes count.
func buildListenerStatuses(gateway *gatewayv1.Gateway) []gatewayv1.ListenerStatus {
	var listenerStatuses []gatewayv1.ListenerStatus

	for _, listener := range gateway.Spec.Listeners {
		ls := gatewayv1.ListenerStatus{
			Name:           listener.Name,
			SupportedKinds: supportedKindsForListener(listener),
			AttachedRoutes: 0, // Updated by httproute controller if needed
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener is accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener is programmed",
				},
				{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "All references resolved",
				},
				{
					Type:               string(gatewayv1.ListenerConditionConflicted),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             string(gatewayv1.ListenerReasonNoConflicts),
					Message:            "No conflicts",
				},
			},
		}
		listenerStatuses = append(listenerStatuses, ls)
	}

	return listenerStatuses
}

// groupNamePtr returns a pointer to the given Group string.
func groupNamePtr(name string) *gatewayv1.Group {
	return (*gatewayv1.Group)(&name)
}

// supportedKindsForListener returns the Route kinds supported by a listener
// based on its protocol.
func supportedKindsForListener(listener gatewayv1.Listener) []gatewayv1.RouteGroupKind {
	switch listener.Protocol {
	case gatewayv1.HTTPProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: groupNamePtr(gatewayv1.GroupName), Kind: "HTTPRoute"},
		}
	case gatewayv1.HTTPSProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: groupNamePtr(gatewayv1.GroupName), Kind: "HTTPRoute"},
			{Group: groupNamePtr(gatewayv1.GroupName), Kind: "GRPCRoute"},
		}
	case gatewayv1.TLSProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: groupNamePtr(gatewayv1.GroupName), Kind: "TLSRoute"},
		}
	case gatewayv1.TCPProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: groupNamePtr(gatewayv1.GroupName), Kind: "TCPRoute"},
		}
	case gatewayv1.UDPProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: groupNamePtr(gatewayv1.GroupName), Kind: "UDPRoute"},
		}
	default:
		return nil
	}
}

func (c *GatewayController) enqueueGateway(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
