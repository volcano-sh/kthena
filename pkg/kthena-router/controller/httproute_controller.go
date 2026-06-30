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
	"fmt"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type HTTPRouteController struct {
	httpRouteLister gatewaylisters.HTTPRouteLister
	gatewayLister   gatewaylisters.GatewayLister
	namespaceLister corelisters.NamespaceLister
	httpRouteSynced cache.InformerSynced
	namespaceSynced cache.InformerSynced
	registration    cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewHTTPRouteController(
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	kubeInformerFactory informers.SharedInformerFactory,
	store datastore.Store,
) *HTTPRouteController {
	httpRouteInformer := gatewayInformerFactory.Gateway().V1().HTTPRoutes()
	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()
	namespaceInformer := kubeInformerFactory.Core().V1().Namespaces()

	controller := &HTTPRouteController{
		httpRouteLister: httpRouteInformer.Lister(),
		gatewayLister:   gatewayInformer.Lister(),
		namespaceLister: namespaceInformer.Lister(),
		httpRouteSynced: httpRouteInformer.Informer().HasSynced,
		namespaceSynced: namespaceInformer.Informer().HasSynced,
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

	_, _ = namespaceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueHTTPRoutesForNamespace,
		UpdateFunc: func(_, new interface{}) { controller.enqueueHTTPRoutesForNamespace(new) },
		DeleteFunc: controller.enqueueHTTPRoutesForNamespace,
	})

	return controller
}

func (c *HTTPRouteController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.httpRouteSynced, c.namespaceSynced); !ok {
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
		if !isGatewayParentRef(parentRef) {
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
			allowed, err := c.gatewayAllowsHTTPRoute(gw, httpRoute, parentRef)
			if err != nil {
				return err
			}
			if allowed {
				return c.store.AddOrUpdateHTTPRoute(httpRoute)
			}
		}
	}
	if gatewayPending {
		return fmt.Errorf("gateway not synced yet")
	}
	klog.V(4).Infof("Skipping HTTPRoute %s/%s: does not reference kthena-router Gateway", namespace, name)
	_ = c.store.DeleteHTTPRoute(key)
	return nil
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
			if isGatewayParentRef(parentRef) {
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

func isGatewayParentRef(parentRef gatewayv1.ParentReference) bool {
	if parentRef.Group != nil && string(*parentRef.Group) != gatewayv1.GroupName {
		return false
	}
	return parentRef.Kind == nil || *parentRef.Kind == "Gateway"
}

func (c *HTTPRouteController) gatewayAllowsHTTPRoute(gw *gatewayv1.Gateway, httpRoute *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference) (bool, error) {
	for _, listener := range gw.Spec.Listeners {
		if !parentRefSelectsListener(parentRef, listener) {
			continue
		}
		if !listenerAllowsHTTPRouteKind(listener) {
			continue
		}
		allowed, err := c.listenerAllowsRouteNamespace(listener, gw.Namespace, httpRoute.Namespace)
		if err != nil {
			return false, err
		}
		if allowed {
			return true, nil
		}
	}
	return false, nil
}

func parentRefSelectsListener(parentRef gatewayv1.ParentReference, listener gatewayv1.Listener) bool {
	if parentRef.SectionName != nil && *parentRef.SectionName != listener.Name {
		return false
	}
	if parentRef.Port != nil && *parentRef.Port != listener.Port {
		return false
	}
	return true
}

func listenerAllowsHTTPRouteKind(listener gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType
	}

	for _, kind := range listener.AllowedRoutes.Kinds {
		if kind.Group != nil && string(*kind.Group) != gatewayv1.GroupName {
			continue
		}
		if kind.Kind == "HTTPRoute" {
			return true
		}
	}
	return false
}

func (c *HTTPRouteController) listenerAllowsRouteNamespace(listener gatewayv1.Listener, gatewayNamespace, routeNamespace string) (bool, error) {
	if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil || listener.AllowedRoutes.Namespaces.From == nil {
		return gatewayNamespace == routeNamespace, nil
	}

	switch *listener.AllowedRoutes.Namespaces.From {
	case gatewayv1.NamespacesFromAll:
		return true, nil
	case gatewayv1.NamespacesFromSame:
		return gatewayNamespace == routeNamespace, nil
	case gatewayv1.NamespacesFromSelector:
		if listener.AllowedRoutes.Namespaces.Selector == nil {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(listener.AllowedRoutes.Namespaces.Selector)
		if err != nil {
			return false, err
		}
		namespace, err := c.namespaceLister.Get(routeNamespace)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return selector.Matches(labels.Set(namespace.Labels)), nil
	default:
		return false, nil
	}
}

func (c *HTTPRouteController) enqueueHTTPRoutesForNamespace(obj interface{}) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	ns, ok := obj.(metav1.Object)
	if !ok {
		return
	}
	routes, err := c.httpRouteLister.HTTPRoutes(ns.GetName()).List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list HTTPRoutes for namespace %s: %v", ns.GetName(), err)
		return
	}
	for _, route := range routes {
		c.workqueue.Add(fmt.Sprintf("%s/%s", route.Namespace, route.Name))
	}
}
