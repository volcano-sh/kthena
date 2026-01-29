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

	"istio.io/istio/pkg/util/sets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

type InferencePoolController struct {
	inferencePoolInformer cache.SharedIndexInformer
	inferencePoolSynced   cache.InformerSynced
	registration          cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewInferencePoolController(
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	store datastore.Store,
) *InferencePoolController {
	gvr := inferencev1.SchemeGroupVersion.WithResource("inferencepools")
	inferencePoolInformer := dynamicInformerFactory.ForResource(gvr).Informer()

	controller := &InferencePoolController{
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
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	obj, exists, err := c.inferencePoolInformer.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		_ = c.store.DeleteModelServer(types.NamespacedName{Namespace: namespace, Name: name})
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

	// Translate InferencePool to ModelServer
	syntheticMS := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inferencePool.Name,
			Namespace: inferencePool.Namespace,
			Labels: map[string]string{
				"kthena.serving.volcano.sh/generated-from": "InferencePool",
			},
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM, // Default to vLLM
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: make(map[string]string),
			},
		},
	}

	for k, v := range inferencePool.Spec.Selector.MatchLabels {
		syntheticMS.Spec.WorkloadSelector.MatchLabels[string(k)] = string(v)
	}

	if len(inferencePool.Spec.TargetPorts) > 0 {
		syntheticMS.Spec.WorkloadPort = aiv1alpha1.WorkloadPort{
			Port: int32(inferencePool.Spec.TargetPorts[0].Number),
		}
	}

	// Find matching pods and bind them in the store
	selector, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: syntheticMS.Spec.WorkloadSelector.MatchLabels,
	})

	matchingPods := sets.New[types.NamespacedName]()
	allPods := c.store.GetAllPods()
	for podNamespacedName, podInfo := range allPods {
		if podNamespacedName.Namespace == syntheticMS.Namespace && selector.Matches(labels.Set(podInfo.Pod.Labels)) {
			matchingPods.Insert(podNamespacedName)
			// Ensure the pod knows it belongs to this synthetic ModelServer
			_ = c.store.AppendModelServerToPod(podInfo.Pod, []*aiv1alpha1.ModelServer{syntheticMS})
		}
	}

	// Store synthetic ModelServer
	if err := c.store.AddOrUpdateModelServer(syntheticMS, matchingPods); err != nil {
		return err
	}

	return c.store.AddOrUpdateInferencePool(inferencePool)
}

func (c *InferencePoolController) enqueueInferencePool(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
