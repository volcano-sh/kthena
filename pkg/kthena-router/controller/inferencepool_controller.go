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
	"sort"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type InferencePoolController struct {
	dynamicClient         dynamic.Interface
	inferencePoolInformer cache.SharedIndexInformer
	inferencePoolSynced   cache.InformerSynced
	registration          cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewInferencePoolController(
	dynamicClient dynamic.Interface,
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	store datastore.Store,
) *InferencePoolController {
	gvr := inferencev1.SchemeGroupVersion.WithResource("inferencepools")
	inferencePoolInformer := dynamicInformerFactory.ForResource(gvr).Informer()

	controller := &InferencePoolController{
		dynamicClient:         dynamicClient,
		inferencePoolInformer: inferencePoolInformer,
		inferencePoolSynced:   inferencePoolInformer.HasSynced,
		workqueue:             workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:           &atomic.Bool{},
		store:                 store,
	}

	controller.registration, _ = inferencePoolInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueInferencePool,
		UpdateFunc: func(old, new interface{}) { controller.enqueueInferencePool(new) },
		DeleteFunc: controller.enqueueInferencePool,
	})
	httpRouteInformer := gatewayInformerFactory.Gateway().V1().HTTPRoutes()
	_, _ = httpRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueInferencePoolsForHTTPRoute,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueInferencePoolsForHTTPRoute(new)
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			controller.enqueueInferencePoolsForHTTPRoute(obj)
		},
	})

	return controller
}

func (c *InferencePoolController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.inferencePoolSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *InferencePoolController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *InferencePoolController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *InferencePoolController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial inference pools have been synced")
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
			klog.Errorf("error syncing inferencepool %q: %s, requeuing", key, err.Error())
			c.workqueue.AddRateLimited(key)
			return true
		}
		klog.Errorf("giving up on syncing inferencepool %q after %d retries: %s", key, maxRetries, err)
		c.workqueue.Forget(obj)
	}
	return true
}

func (c *InferencePoolController) syncHandler(key string) error {
	_, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	obj, exists, err := c.inferencePoolInformer.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists || obj == nil {
		_ = c.store.DeleteInferencePool(key)
		return nil
	}

	unstructuredObj := obj.(*unstructured.Unstructured)
	inferencePool := &inferencev1.InferencePool{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, inferencePool); err != nil {
		return fmt.Errorf("failed to convert unstructured to InferencePool: %w", err)
	}

	if err := c.store.AddOrUpdateInferencePool(inferencePool); err != nil {
		return err
	}

	if err := c.updateInferencePoolStatus(inferencePool); err != nil {
		return err
	}

	return nil
}

func (c *InferencePoolController) updateInferencePoolStatus(inferencePool *inferencev1.InferencePool) error {
	// Convert the typed object from informer cache — no redundant API Get needed
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inferencePool.DeepCopy())
	if err != nil {
		return fmt.Errorf("failed to convert InferencePool to unstructured: %w", err)
	}
	unstructuredObj := &unstructured.Unstructured{Object: content}

	observedGeneration := inferencePool.Generation

	// 1. Find all HTTPRoutes that reference this InferencePool
	uniqueGateways := make(map[types.NamespacedName]bool)

	allRoutes := c.store.GetAllHTTPRoutes()
	for _, route := range allRoutes {
		matches := false
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				if backend.Kind != nil && *backend.Kind == "InferencePool" &&
					string(backend.Name) == inferencePool.Name {
					backendNamespace := route.Namespace
					if backend.Namespace != nil {
						backendNamespace = string(*backend.Namespace)
					}
					if backendNamespace == inferencePool.Namespace {
						matches = true
						break
					}
				}
			}
			if matches {
				break
			}
		}

		if matches {
			for _, parentRef := range route.Spec.ParentRefs {
				if (parentRef.Kind == nil || *parentRef.Kind == "Gateway") &&
					(parentRef.Group == nil || *parentRef.Group == gatewayv1.GroupName) {
					gwNamespace := route.Namespace
					if parentRef.Namespace != nil {
						gwNamespace = string(*parentRef.Namespace)
					}
					gwName := string(parentRef.Name)

					gwKey := fmt.Sprintf("%s/%s", gwNamespace, gwName)
					gw := c.store.GetGateway(gwKey)
					if gw != nil && string(gw.Spec.GatewayClassName) == DefaultGatewayClassName {
						uniqueGateways[types.NamespacedName{Namespace: gwNamespace, Name: gwName}] = true
					}
				}
			}
		}
	}

	existingParents, _, _ := unstructured.NestedSlice(unstructuredObj.Object, "status", "parents")

	// Sort gateway keys for deterministic output (avoids unnecessary updates)
	var gatewayKeys []types.NamespacedName
	for k := range uniqueGateways {
		gatewayKeys = append(gatewayKeys, k)
	}
	sort.Slice(gatewayKeys, func(i, j int) bool {
		if gatewayKeys[i].Namespace != gatewayKeys[j].Namespace {
			return gatewayKeys[i].Namespace < gatewayKeys[j].Namespace
		}
		return gatewayKeys[i].Name < gatewayKeys[j].Name
	})

	var newParents []interface{}
	if len(gatewayKeys) > 0 {
		for _, pk := range gatewayKeys {
			pStatus := map[string]interface{}{
				"parentRef": map[string]interface{}{
					"group":     string(gatewayv1.GroupName),
					"kind":      "Gateway",
					"name":      pk.Name,
					"namespace": pk.Namespace,
				},
				"controllerName": ControllerName,
			}

			var existingConditions []interface{}
			for _, p := range existingParents {
				if ep, ok := p.(map[string]interface{}); ok {
					if ref, ok := ep["parentRef"].(map[string]interface{}); ok {
						if ref["name"] == pk.Name && ref["namespace"] == pk.Namespace {
							existingConditions, _ = ep["conditions"].([]interface{})
							break
						}
					}
				}
			}

			pStatus["conditions"] = []interface{}{
				buildCondition(existingConditions, "Accepted", "True", "Accepted", "InferencePool is referenced by HTTPRoute and accepted by kthena-router", observedGeneration),
				buildCondition(existingConditions, "ResolvedRefs", "True", "ResolvedRefs", "All references resolved", observedGeneration),
			}
			newParents = append(newParents, pStatus)
		}
	} else {
		fallbackParent := map[string]interface{}{
			"parentRef": map[string]interface{}{
				"name":      inferencePool.Name,
				"namespace": inferencePool.Namespace,
			},
			"controllerName": ControllerName,
		}

		var existingConditions []interface{}
		if len(existingParents) > 0 {
			if ep, ok := existingParents[0].(map[string]interface{}); ok {
				existingConditions, _ = ep["conditions"].([]interface{})
			}
		}

		fallbackParent["conditions"] = []interface{}{
			buildCondition(existingConditions, "Accepted", "False", "NoMatchingParent", "No HTTPRoute references this InferencePool", observedGeneration),
		}
		newParents = []interface{}{fallbackParent}
	}

	if !inferencePoolParentsChanged(existingParents, newParents) {
		return nil
	}

	if err := unstructured.SetNestedSlice(unstructuredObj.Object, newParents, "status", "parents"); err != nil {
		return err
	}

	gvr := inferencev1.SchemeGroupVersion.WithResource("inferencepools")
	_, err = c.dynamicClient.Resource(gvr).Namespace(inferencePool.Namespace).UpdateStatus(
		context.Background(), unstructuredObj, metav1.UpdateOptions{},
	)
	return err
}

func buildCondition(existing []interface{}, typeStr, status, reason, message string, generation int64) map[string]interface{} {
	for _, item := range existing {
		if cond, ok := item.(map[string]interface{}); ok {
			if cond["type"] == typeStr {
				if cond["status"] == status && cond["reason"] == reason && cond["message"] == message {
					return map[string]interface{}{
						"type":               typeStr,
						"status":             status,
						"reason":             reason,
						"message":            message,
						"lastTransitionTime": cond["lastTransitionTime"],
						"observedGeneration": generation,
					}
				}
			}
		}
	}

	return map[string]interface{}{
		"type":               typeStr,
		"status":             status,
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": metav1.Now().Format(time.RFC3339),
		"observedGeneration": generation,
	}
}

func inferencePoolParentsChanged(existing, updated []interface{}) bool {
	if len(existing) != len(updated) {
		return true
	}

	for i := range updated {
		found := false
		up, _ := updated[i].(map[string]interface{})
		upRef, _ := up["parentRef"].(map[string]interface{})

		for j := range existing {
			ep, _ := existing[j].(map[string]interface{})
			epRef, _ := ep["parentRef"].(map[string]interface{})

			if upRef["name"] == epRef["name"] && upRef["namespace"] == epRef["namespace"] && upRef["kind"] == epRef["kind"] && upRef["group"] == epRef["group"] {
				if up["controllerName"] != ep["controllerName"] {
					return true
				}
				uConds, _ := up["conditions"].([]interface{})
				eConds, _ := ep["conditions"].([]interface{})
				if !inferencePoolConditionsUnchanged(eConds, uConds) {
					return true
				}
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func inferencePoolConditionsUnchanged(existing, updated []interface{}) bool {
	if len(existing) != len(updated) {
		return false
	}
	for i := range existing {
		ec, ok1 := existing[i].(map[string]interface{})
		uc, ok2 := updated[i].(map[string]interface{})
		if !ok1 || !ok2 {
			return false
		}
		if ec["type"] != uc["type"] || ec["status"] != uc["status"] ||
			ec["reason"] != uc["reason"] || ec["message"] != uc["message"] {
			return false
		}
	}
	return true
}

func (c *InferencePoolController) enqueueInferencePool(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *InferencePoolController) enqueueInferencePoolsForHTTPRoute(obj interface{}) {
	httpRoute, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return
	}
	for _, rule := range httpRoute.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			kind := "Service"
			if ref.Kind != nil {
				kind = string(*ref.Kind)
			}
			if kind != "InferencePool" {
				continue
			}
			ns := httpRoute.Namespace
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}
			key := fmt.Sprintf("%s/%s", ns, string(ref.Name))
			c.workqueue.Add(key)
			klog.V(4).Infof("Enqueuing InferencePool %s due to HTTPRoute %s/%s change",
				key, httpRoute.Namespace, httpRoute.Name)
		}
	}
}
