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
	gatewayClient gatewayclientset.Interface
	gatewayLister gatewaylisters.GatewayLister
	gatewaySynced cache.InformerSynced
	registration  cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewGatewayController(
	gatewayClient gatewayclientset.Interface,
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	store datastore.Store,
) *GatewayController {
	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()

	controller := &GatewayController{
		gatewayClient: gatewayClient,
		gatewayLister: gatewayInformer.Lister(),
		gatewaySynced: gatewayInformer.Informer().HasSynced,
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

	controller.registration, _ = gatewayInformer.Informer().AddEventHandler(filterHandler)

	return controller
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

	if err := c.syncHandler(key); err != nil {
		if c.workqueue.NumRequeues(key) < maxRetries {
			klog.Errorf("error syncing gateway %q: %s, requeuing", key, err.Error())
			c.workqueue.AddRateLimited(key)
			return true
		}
		klog.Errorf("giving up on syncing gateway %q after %d retries: %s", key, maxRetries, err)
		c.workqueue.Forget(obj)
	}
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

	// Store the Gateway
	if err := c.store.AddOrUpdateGateway(gateway); err != nil {
		return err
	}

	// Update Gateway status
	if err := c.updateGatewayStatus(gateway); err != nil {
		klog.Errorf("failed to update status for gateway %s: %v", key, err)
	}

	return nil
}

func (c *GatewayController) updateGatewayStatus(gateway *gatewayv1.Gateway) error {
	gw := gateway.DeepCopy()
	gatewayKey := fmt.Sprintf("%s/%s", gw.Namespace, gw.Name)

	// Gateway conditions
	accepted := string(gw.Spec.GatewayClassName) == DefaultGatewayClassName
	acceptedStatus := metav1.ConditionFalse
	acceptedReason := "NotAccepted"
	acceptedMessage := "Gateway is not accepted: unknown GatewayClass"
	if accepted {
		acceptedStatus = metav1.ConditionTrue
		acceptedReason = string(gatewayv1.GatewayReasonAccepted)
		acceptedMessage = "Gateway has been accepted by kthena-router"
	}
	setGatewayCondition(gw, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             acceptedStatus,
		ObservedGeneration: gw.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             acceptedReason,
		Message:            acceptedMessage,
	})

	allProgrammed := true
	programmedMessage := "Gateway has been programmed by kthena-router"
	for _, listener := range gw.Spec.Listeners {
		if !isSupportedProtocol(listener.Protocol) {
			allProgrammed = false

			programmedMessage = fmt.Sprintf("Listener %q uses unsupported protocol %s", listener.Name, listener.Protocol)
			break
		}
	}

	programmedStatus := metav1.ConditionFalse
	programmedReason := "Invalid"
	if allProgrammed {
		programmedStatus = metav1.ConditionTrue
		programmedReason = string(gatewayv1.GatewayReasonProgrammed)
	}
	setGatewayCondition(gw, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             programmedStatus,
		ObservedGeneration: gw.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             programmedReason,
		Message:            programmedMessage,
	})

	for _, listener := range gw.Spec.Listeners {
		acceptedCond, programmedCond := getGatewayListenerConditions(listener.Protocol, nil, gw.Generation)
		setGatewayListenerStatus(gw, listener.Name, listener.Protocol, []metav1.Condition{acceptedCond, programmedCond})
	}

	if gatewayStatusUnchanged(gateway, gw) {
		klog.V(4).Infof("Gateway %s status unchanged, skipping update", gatewayKey)
		return nil
	}

	klog.V(4).Infof("Updating status for gateway %s", gatewayKey)
	_, err := c.gatewayClient.GatewayV1().Gateways(gw.Namespace).UpdateStatus(
		context.Background(), gw, metav1.UpdateOptions{},
	)
	return err
}

func setGatewayCondition(gw *gatewayv1.Gateway, newCond metav1.Condition) {
	for i, existing := range gw.Status.Conditions {
		if existing.Type == newCond.Type {
			if existing.Status == newCond.Status &&
				existing.Reason == newCond.Reason &&
				existing.Message == newCond.Message {
				newCond.LastTransitionTime = existing.LastTransitionTime
			}
			gw.Status.Conditions[i] = newCond
			return
		}
	}
	gw.Status.Conditions = append(gw.Status.Conditions, newCond)
}

func setGatewayListenerStatus(gw *gatewayv1.Gateway, listenerName gatewayv1.SectionName, protocol gatewayv1.ProtocolType, conditions []metav1.Condition) {
	supportedKinds := []gatewayv1.RouteGroupKind{
		{
			Group: groupPtr(gatewayv1.GroupVersion.Group),
			Kind:  gatewayv1.Kind("HTTPRoute"),
		},
	}

	// Find an existing entry for this listener.
	for i, ls := range gw.Status.Listeners {
		if ls.Name == listenerName {
			// Preserve LastTransitionTime for unchanged conditions.
			mergedConditions := mergeListenerConditions(ls.Conditions, conditions)
			gw.Status.Listeners[i].Conditions = mergedConditions
			gw.Status.Listeners[i].SupportedKinds = supportedKinds
			return
		}
	}

	// No existing entry — create a new one.
	gw.Status.Listeners = append(gw.Status.Listeners, gatewayv1.ListenerStatus{
		Name:           listenerName,
		SupportedKinds: supportedKinds,
		Conditions:     conditions,
	})
}

func mergeListenerConditions(existing, incoming []metav1.Condition) []metav1.Condition {
	existingMap := make(map[string]metav1.Condition, len(existing))
	for _, c := range existing {
		existingMap[c.Type] = c
	}

	merged := make([]metav1.Condition, 0, len(incoming))
	for _, newCond := range incoming {
		if old, ok := existingMap[newCond.Type]; ok {
			if old.Status == newCond.Status &&
				old.Reason == newCond.Reason &&
				old.Message == newCond.Message {
				newCond.LastTransitionTime = old.LastTransitionTime
			}
		}
		merged = append(merged, newCond)
	}
	return merged
}

func getGatewayListenerConditions(protocol gatewayv1.ProtocolType, listenerErr error, generation int64) (accepted, programmed metav1.Condition) {
	now := metav1.Now()

	if isSupportedProtocol(protocol) {
		accepted = metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonAccepted),
			Message:            "Listener has been accepted",
		}
	} else {
		accepted = metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonUnsupportedProtocol),
			Message:            fmt.Sprintf("Protocol %s is not supported", protocol),
		}
	}

	if listenerErr == nil {
		programmed = metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionProgrammed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonProgrammed),
			Message:            "Listener has been programmed",
		}
	} else {
		programmed = metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionProgrammed),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             "Invalid",
			Message:            fmt.Sprintf("Listener failed to bind: %v", listenerErr),
		}
	}

	return accepted, programmed
}

func isSupportedProtocol(protocol gatewayv1.ProtocolType) bool {
	return protocol == gatewayv1.HTTPProtocolType || protocol == gatewayv1.HTTPSProtocolType
}

func gatewayStatusUnchanged(original, updated *gatewayv1.Gateway) bool {
	return conditionsEqual(original.Status.Conditions, updated.Status.Conditions) &&
		listenerStatusesEqual(original.Status.Listeners, updated.Status.Listeners)
}

func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Status != b[i].Status ||
			a[i].Reason != b[i].Reason || a[i].Message != b[i].Message {
			return false
		}
	}
	return true
}

func listenerStatusesEqual(a, b []gatewayv1.ListenerStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
		if !conditionsEqual(a[i].Conditions, b[i].Conditions) {
			return false
		}
	}
	return true
}

func groupPtr(group string) *gatewayv1.Group {
	g := gatewayv1.Group(group)
	return &g
}

func (c *GatewayController) enqueueGateway(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
