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
	"k8s.io/apimachinery/pkg/labels"
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

type HTTPRouteController struct {
	gatewayClient   gatewayclientset.Interface
	httpRouteLister gatewaylisters.HTTPRouteLister
	gatewayLister   gatewaylisters.GatewayLister
	httpRouteSynced cache.InformerSynced
	registration    cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewHTTPRouteController(
	gatewayClient gatewayclientset.Interface,
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	store datastore.Store,
) *HTTPRouteController {
	httpRouteInformer := gatewayInformerFactory.Gateway().V1().HTTPRoutes()
	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()

	controller := &HTTPRouteController{
		gatewayClient:   gatewayClient,
		httpRouteLister: httpRouteInformer.Lister(),
		gatewayLister:   gatewayInformer.Lister(),
		httpRouteSynced: httpRouteInformer.Informer().HasSynced,
		workqueue:       workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:     &atomic.Bool{},
		store:           store,
	}

	controller.registration, _ = httpRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueHTTPRoute,
		UpdateFunc: func(old, new interface{}) { controller.enqueueHTTPRoute(new) },
		DeleteFunc: controller.enqueueHTTPRoute,
	})

	gatewayFilter := &cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			gw, ok := obj.(*gatewayv1.Gateway)
			return ok && string(gw.Spec.GatewayClassName) == DefaultGatewayClassName
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueHTTPRoutesForGateway,
			UpdateFunc: func(_, new interface{}) { controller.enqueueHTTPRoutesForGateway(new) },
			DeleteFunc: func(obj interface{}) {
				if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					obj = d.Obj
				}
				controller.enqueueHTTPRoutesForGateway(obj)
			},
		},
	}
	_, _ = gatewayInformer.Informer().AddEventHandler(gatewayFilter)

	return controller
}

func (c *HTTPRouteController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.httpRouteSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *HTTPRouteController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *HTTPRouteController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *HTTPRouteController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial http routes have been synced")
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
			klog.Errorf("error syncing httproute %q: %s, requeuing", key, err.Error())
			c.workqueue.AddRateLimited(key)
			return true
		}
		klog.Errorf("giving up on syncing httproute %q after %d retries: %s", key, maxRetries, err)
		c.workqueue.Forget(obj)
	}
	return true
}

func (c *HTTPRouteController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	httpRoute, err := c.httpRouteLister.HTTPRoutes(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		_ = c.store.DeleteHTTPRoute(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Only process HTTPRoutes that reference kthena-router GatewayClass
	// Check all parentRefs - process immediately if any matches, retry only if no match and a Gateway is pending
	var gatewayPending bool
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Kind == nil || *parentRef.Kind != "Gateway" {
			continue
		}
		gatewayNamespace := httpRoute.Namespace
		if parentRef.Namespace != nil {
			gatewayNamespace = string(*parentRef.Namespace)
		}
		gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, string(parentRef.Name))
		gw := c.store.GetGateway(gatewayKey)
		if gw == nil {
			if _, err := c.gatewayLister.Gateways(gatewayNamespace).Get(string(parentRef.Name)); err == nil {
				gatewayPending = true
			}
			continue
		}
		if string(gw.Spec.GatewayClassName) == DefaultGatewayClassName {
			if err := c.store.AddOrUpdateHTTPRoute(httpRoute); err != nil {
				return err
			}
			if err := c.updateHTTPRouteStatus(httpRoute); err != nil {
				klog.Errorf("failed to update status for httproute %s/%s: %v", httpRoute.Namespace, httpRoute.Name, err)
			}
			return nil
		}
	}
	if gatewayPending {
		return fmt.Errorf("gateway not synced yet")
	}
	klog.V(4).Infof("Skipping HTTPRoute %s/%s: does not reference kthena-router Gateway", namespace, name)
	_ = c.store.DeleteHTTPRoute(key)
	return nil
}

func (c *HTTPRouteController) updateHTTPRouteStatus(httpRoute *gatewayv1.HTTPRoute) error {
	hr := httpRoute.DeepCopy()

	for _, parentRef := range hr.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
			continue
		}

		gatewayNamespace := hr.Namespace
		if parentRef.Namespace != nil {
			gatewayNamespace = string(*parentRef.Namespace)
		}
		gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, string(parentRef.Name))
		gw := c.store.GetGateway(gatewayKey)

		// Call c.store.GetGateway(gatewayKey). If nil or GatewayClassName != DefaultGatewayClassName, skip this parent (do NOT write status for it).
		if gw == nil || string(gw.Spec.GatewayClassName) != DefaultGatewayClassName {
			continue
		}

		acceptedCond := metav1.Condition{
			Type:               string(gatewayv1.RouteConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.RouteReasonAccepted),
			Message:            "HTTPRoute has been accepted by kthena-router",
			ObservedGeneration: hr.Generation,
		}

		resolvedStatus, reason, message := c.resolveBackendRefs(hr)
		resolvedCond := metav1.Condition{
			Type:               string(gatewayv1.RouteConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: hr.Generation,
		}
		if !resolvedStatus {
			resolvedCond.Status = metav1.ConditionFalse
		}

		newStatus := gatewayv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: gatewayv1.GatewayController(ControllerName),
			Conditions:     []metav1.Condition{},
		}

		// Find existing parent status to preserve LastTransitionTime
		var existingConditions []metav1.Condition
		for _, p := range httpRoute.Status.Parents {
			if isSameParentRef(p.ParentRef, parentRef, hr.Namespace) {
				existingConditions = p.Conditions
				break
			}
		}

		newStatus.Conditions = setRouteCondition(existingConditions, acceptedCond)
		newStatus.Conditions = setRouteCondition(newStatus.Conditions, resolvedCond)

		setHTTPRouteParentStatus(hr, newStatus, hr.Namespace)
	}

	if httpRouteStatusUnchanged(httpRoute, hr) {
		return nil
	}

	_, err := c.gatewayClient.GatewayV1().HTTPRoutes(hr.Namespace).UpdateStatus(
		context.Background(), hr, metav1.UpdateOptions{},
	)
	return err
}

func (c *HTTPRouteController) resolveBackendRefs(httpRoute *gatewayv1.HTTPRoute) (bool, string, string) {
	for _, rule := range httpRoute.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			ns := httpRoute.Namespace
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}

			kind := "Service"
			if ref.Kind != nil {
				kind = string(*ref.Kind)
			}

			if kind == "InferencePool" {
				key := fmt.Sprintf("%s/%s", ns, string(ref.Name))
				if pool := c.store.GetInferencePool(key); pool == nil {
					return false, "BackendNotFound", fmt.Sprintf("InferencePool %q not found in namespace %q", ref.Name, ns)
				}
			}
			// Kind is "Service": skip validation, treat as resolved.
		}
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All references resolved"
}

func isSameParentRef(a, b gatewayv1.ParentReference, routeNamespace string) bool {
	if a.Name != b.Name {
		return false
	}

	nsA := routeNamespace
	if a.Namespace != nil {
		nsA = string(*a.Namespace)
	}
	nsB := routeNamespace
	if b.Namespace != nil {
		nsB = string(*b.Namespace)
	}
	if nsA != nsB {
		return false
	}

	kindA := "Gateway"
	if a.Kind != nil {
		kindA = string(*a.Kind)
	}
	kindB := "Gateway"
	if b.Kind != nil {
		kindB = string(*b.Kind)
	}
	if kindA != kindB {
		return false
	}

	if (a.SectionName == nil) != (b.SectionName == nil) {
		return false
	}
	if a.SectionName != nil && *a.SectionName != *b.SectionName {
		return false
	}

	if (a.Port == nil) != (b.Port == nil) {
		return false
	}
	if a.Port != nil && *a.Port != *b.Port {
		return false
	}

	return true
}

func setHTTPRouteParentStatus(httpRoute *gatewayv1.HTTPRoute, newStatus gatewayv1.RouteParentStatus, routeNamespace string) {
	for i, existing := range httpRoute.Status.Parents {
		if isSameParentRef(existing.ParentRef, newStatus.ParentRef, routeNamespace) {
			httpRoute.Status.Parents[i] = newStatus
			return
		}
	}
	httpRoute.Status.Parents = append(httpRoute.Status.Parents, newStatus)
}

func httpRouteStatusUnchanged(original, updated *gatewayv1.HTTPRoute) bool {
	if len(original.Status.Parents) != len(updated.Status.Parents) {
		return false
	}

	for i := range updated.Status.Parents {
		found := false
		for j := range original.Status.Parents {
			if isSameParentRef(updated.Status.Parents[i].ParentRef, original.Status.Parents[j].ParentRef, updated.Namespace) {
				if updated.Status.Parents[i].ControllerName != original.Status.Parents[j].ControllerName {
					return false
				}
				if len(updated.Status.Parents[i].Conditions) != len(original.Status.Parents[j].Conditions) {
					return false
				}
				for _, newCond := range updated.Status.Parents[i].Conditions {
					condFound := false
					for _, oldCond := range original.Status.Parents[j].Conditions {
						if newCond.Type == oldCond.Type {
							if newCond.Status != oldCond.Status ||
								newCond.Reason != oldCond.Reason ||
								newCond.Message != oldCond.Message {
								return false
							}
							condFound = true
							break
						}
					}
					if !condFound {
						return false
					}
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func setRouteCondition(existing []metav1.Condition, newCond metav1.Condition) []metav1.Condition {
	newCond.LastTransitionTime = metav1.Now()
	for i, cond := range existing {
		if cond.Type == newCond.Type {
			if cond.Status == newCond.Status && cond.Reason == newCond.Reason && cond.Message == newCond.Message {
				newCond.LastTransitionTime = cond.LastTransitionTime
			}
			existing[i] = newCond
			return existing
		}
	}
	return append(existing, newCond)
}

func (c *HTTPRouteController) enqueueHTTPRoute(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *HTTPRouteController) enqueueHTTPRoutesForGateway(obj interface{}) {
	gw, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return
	}
	gatewayKey := fmt.Sprintf("%s/%s", gw.Namespace, gw.Name)
	routes, err := c.httpRouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list HTTPRoutes for gateway %s: %v", gatewayKey, err)
		return
	}
	for _, route := range routes {
		for _, parentRef := range route.Spec.ParentRefs {
			if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
				ns := route.Namespace
				if parentRef.Namespace != nil {
					ns = string(*parentRef.Namespace)
				}
				if fmt.Sprintf("%s/%s", ns, string(parentRef.Name)) == gatewayKey {
					c.workqueue.Add(fmt.Sprintf("%s/%s", route.Namespace, route.Name))
					break
				}
			}
		}
	}
}
