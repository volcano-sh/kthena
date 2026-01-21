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

type HTTPRouteController struct {
	gatewayClient   gatewayclientset.Interface
	httpRouteLister gatewaylisters.HTTPRouteLister
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

	controller := &HTTPRouteController{
		gatewayClient:   gatewayClient,
		httpRouteLister: httpRouteInformer.Lister(),
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
	// Check if any parentRef references a Gateway with kthena-router GatewayClass
	shouldProcess := false
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
			gatewayNamespace := httpRoute.Namespace
			if parentRef.Namespace != nil {
				gatewayNamespace = string(*parentRef.Namespace)
			}
			gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, string(parentRef.Name))
			gw := c.store.GetGateway(gatewayKey)
			if gw != nil && gw.Spec.GatewayClassName != "" {
				if string(gw.Spec.GatewayClassName) == DefaultGatewayClassName {
					shouldProcess = true
					break
				}
			}
		}
	}

	if !shouldProcess {
		klog.V(4).Infof("Skipping HTTPRoute %s/%s: does not reference kthena-router Gateway", namespace, name)
		return nil
	}

	if err := c.store.AddOrUpdateHTTPRoute(httpRoute); err != nil {
		return err
	}

	return c.updateHTTPRouteStatus(httpRoute)
}

func (c *HTTPRouteController) updateHTTPRouteStatus(httpRoute *gatewayv1.HTTPRoute) error {
	httpRoute = httpRoute.DeepCopy()

	// For each parent reference, update its status
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
			continue
		}

		gatewayNamespace := httpRoute.Namespace
		if parentRef.Namespace != nil {
			gatewayNamespace = string(*parentRef.Namespace)
		}

		gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, string(parentRef.Name))
		gw := c.store.GetGateway(gatewayKey)
		if gw == nil || string(gw.Spec.GatewayClassName) != DefaultGatewayClassName {
			continue
		}

		// Found a gateway managed by kthena-router
		parentStatus := gatewayv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: gatewayv1.GatewayController(ControllerName),
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.RouteConditionAccepted),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.RouteReasonAccepted),
					Message:            "HTTPRoute has been accepted by kthena-router",
					LastTransitionTime: metav1.Now(),
					ObservedGeneration: httpRoute.Generation,
				},
				{
					Type:               string(gatewayv1.RouteConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.RouteReasonResolvedRefs),
					Message:            "All references in HTTPRoute are resolved",
					LastTransitionTime: metav1.Now(),
					ObservedGeneration: httpRoute.Generation,
				},
			},
		}

		c.setHTTPRouteParentStatus(httpRoute, parentStatus)
	}

	_, err := c.gatewayClient.GatewayV1().HTTPRoutes(httpRoute.Namespace).UpdateStatus(context.Background(), httpRoute, metav1.UpdateOptions{})
	return err
}

func (c *HTTPRouteController) setHTTPRouteParentStatus(httpRoute *gatewayv1.HTTPRoute, newStatus gatewayv1.RouteParentStatus) {
	for i, status := range httpRoute.Status.Parents {
		if c.isSameParentRef(status.ParentRef, newStatus.ParentRef) {
			httpRoute.Status.Parents[i] = newStatus
			return
		}
	}
	httpRoute.Status.Parents = append(httpRoute.Status.Parents, newStatus)
}

func (c *HTTPRouteController) isSameParentRef(a, b gatewayv1.ParentReference) bool {
	if a.Name != b.Name {
		return false
	}
	if (a.Namespace == nil) != (b.Namespace == nil) {
		return false
	}
	if a.Namespace != nil && *a.Namespace != *b.Namespace {
		return false
	}
	if (a.Kind == nil) != (b.Kind == nil) {
		return false
	}
	if a.Kind != nil && *a.Kind != *b.Kind {
		return false
	}
	return true
}

func (c *HTTPRouteController) enqueueHTTPRoute(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
