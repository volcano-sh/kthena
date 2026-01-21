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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
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

	return controller
}

func (c *InferencePoolController) Run(stopCh <-chan struct{}) error {
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
	if !exists {
		_ = c.store.DeleteInferencePool(key)
		return nil
	}

	unstructuredObj, ok := obj.(runtime.Unstructured)
	if !ok {
		return fmt.Errorf("invalid object type: %T", obj)
	}

	inferencePool := &inferencev1.InferencePool{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), inferencePool); err != nil {
		return fmt.Errorf("failed to convert unstructured to InferencePool: %w", err)
	}

	if err := c.store.AddOrUpdateInferencePool(inferencePool); err != nil {
		return err
	}

	return c.updateInferencePoolStatus(inferencePool)
}

func (s *InferencePoolController) updateInferencePoolStatus(inferencePool *inferencev1.InferencePool) error {
	inferencePool = inferencePool.DeepCopy()

	// TODO: Implement proper InferencePool status updates according to version 1.2.0 spec.
	// In version 1.2.0, InferencePool status is per-parent.
	// Finding and updating individual parent status requires traversing HTTPRoutes referencing this pool.

	// Convert back to unstructured to update status
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inferencePool)
	if err != nil {
		return fmt.Errorf("failed to convert InferencePool to unstructured: %w", err)
	}

	unstructuredObj := &unstructured.Unstructured{Object: content}
	gvr := inferencev1.SchemeGroupVersion.WithResource("inferencepools")
	_, err = s.dynamicClient.Resource(gvr).Namespace(inferencePool.Namespace).UpdateStatus(context.Background(), unstructuredObj, metav1.UpdateOptions{})
	return err
}

func (c *InferencePoolController) enqueueInferencePool(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
