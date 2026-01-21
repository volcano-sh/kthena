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

	return c.updateGatewayStatus(gateway)
}

func (c *GatewayController) updateGatewayStatus(gateway *gatewayv1.Gateway) error {
	gateway = gateway.DeepCopy()

	// Update conditions
	acceptedCond := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            "Gateway has been accepted by kthena-router",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: gateway.Generation,
	}

	programmedCond := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayReasonProgrammed),
		Message:            "Gateway has been programmed by kthena-router",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: gateway.Generation,
	}

	c.setGatewayCondition(gateway, acceptedCond)
	c.setGatewayCondition(gateway, programmedCond)

	// Update listener status
	gatewayKey := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)
	for _, listener := range gateway.Spec.Listeners {
		listenerErr := c.store.GetListenerStatus(gatewayKey, string(listener.Name))

		acceptedCond, programmedCond := c.getGatewayListenerConditions(listenerErr, gateway.Generation)
		c.setGatewayListenerStatus(gateway, listener.Name, []metav1.Condition{acceptedCond, programmedCond})
	}

	_, err := c.gatewayClient.GatewayV1().Gateways(gateway.Namespace).UpdateStatus(context.Background(), gateway, metav1.UpdateOptions{})
	return err
}

func (c *GatewayController) setGatewayCondition(gateway *gatewayv1.Gateway, newCond metav1.Condition) {
	for i, cond := range gateway.Status.Conditions {
		if cond.Type == newCond.Type {
			if cond.Status == newCond.Status && cond.Reason == newCond.Reason {
				newCond.LastTransitionTime = cond.LastTransitionTime
			}
			gateway.Status.Conditions[i] = newCond
			return
		}
	}
	gateway.Status.Conditions = append(gateway.Status.Conditions, newCond)
}

func (c *GatewayController) setGatewayListenerStatus(gateway *gatewayv1.Gateway, listenerName gatewayv1.SectionName, conditions []metav1.Condition) {
	var listenerStatus *gatewayv1.ListenerStatus
	for i := range gateway.Status.Listeners {
		if gateway.Status.Listeners[i].Name == listenerName {
			listenerStatus = &gateway.Status.Listeners[i]
			break
		}
	}

	if listenerStatus == nil {
		gateway.Status.Listeners = append(gateway.Status.Listeners, gatewayv1.ListenerStatus{
			Name: listenerName,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: (*gatewayv1.Group)(&gatewayv1.GroupVersion.Group),
					Kind:  gatewayv1.Kind("HTTPRoute"),
				},
			},
		})
		listenerStatus = &gateway.Status.Listeners[len(gateway.Status.Listeners)-1]
	}

	// Update conditions
	for _, newCond := range conditions {
		found := false
		for i, cond := range listenerStatus.Conditions {
			if cond.Type == newCond.Type {
				if cond.Status == newCond.Status && cond.Reason == newCond.Reason {
					newCond.LastTransitionTime = cond.LastTransitionTime
				}
				listenerStatus.Conditions[i] = newCond
				found = true
				break
			}
		}
		if !found {
			listenerStatus.Conditions = append(listenerStatus.Conditions, newCond)
		}
	}
}

func (c *GatewayController) getGatewayListenerConditions(listenerErr error, generation int64) (metav1.Condition, metav1.Condition) {
	acceptedStatus := metav1.ConditionTrue
	acceptedReason := string(gatewayv1.ListenerReasonAccepted)
	acceptedMessage := "Listener has been accepted"

	programmedStatus := metav1.ConditionTrue
	programmedReason := string(gatewayv1.ListenerReasonProgrammed)
	programmedMessage := "Listener has been programmed"

	if listenerErr != nil {
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = "PortUnavailable"
		acceptedMessage = fmt.Sprintf("Failed to start listener: %v", listenerErr)

		programmedStatus = metav1.ConditionFalse
		programmedReason = "Invalid"
		programmedMessage = "Listener could not be programmed due to error"
	}

	acceptedCond := metav1.Condition{
		Type:               string(gatewayv1.ListenerConditionAccepted),
		Status:             acceptedStatus,
		Reason:             acceptedReason,
		Message:            acceptedMessage,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: generation,
	}

	programmedCond := metav1.Condition{
		Type:               string(gatewayv1.ListenerConditionProgrammed),
		Status:             programmedStatus,
		Reason:             programmedReason,
		Message:            programmedMessage,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: generation,
	}

	return acceptedCond, programmedCond
}

func (c *GatewayController) enqueueGateway(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
