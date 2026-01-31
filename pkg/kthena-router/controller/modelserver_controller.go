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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// ResourceType represents the type of Kubernetes resource
type ResourceType string

const (
	ResourceTypeModelServer ResourceType = "ModelServer"
	ResourceTypePod         ResourceType = "Pod"
)

// QueueItem represents an item in the work queue
type QueueItem struct {
	ResourceType ResourceType
	Key          string
}

type ModelServerController struct {
	modelServerLister listerv1alpha1.ModelServerLister
	podLister         corelisters.PodLister

	modelServerSynced cache.InformerSynced
	podSynced         cache.InformerSynced

	// Event handler registrations
	modelServerRegistration cache.ResourceEventHandlerRegistration
	podRegistration         cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[QueueItem]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewModelServerController(
	kthenaInformerFactory informersv1alpha1.SharedInformerFactory,
	kubeInformerFactory informers.SharedInformerFactory,
	store datastore.Store,
) *ModelServerController {
	modelServerInformer := kthenaInformerFactory.Networking().V1alpha1().ModelServers()
	podInformer := kubeInformerFactory.Core().V1().Pods()

	controller := &ModelServerController{
		modelServerLister: modelServerInformer.Lister(),
		podLister:         podInformer.Lister(),
		modelServerSynced: modelServerInformer.Informer().HasSynced,
		podSynced:         podInformer.Informer().HasSynced,
		workqueue:         workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[QueueItem]()),
		initialSync:       &atomic.Bool{},
		store:             store,
	}

	// Register ModelServer event handlers
	controller.modelServerRegistration, _ = modelServerInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueModelServer,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueModelServer(new)
		},
		DeleteFunc: controller.enqueueModelServer,
	})

	// Register Pod event handlers
	controller.podRegistration, _ = podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueuePod,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueuePod(new)
		},
		DeleteFunc: controller.enqueuePod,
	})

	return controller
}

func (c *ModelServerController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.modelServerSynced, c.podSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	// add initialSync signal
	c.workqueue.Add(QueueItem{})

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *ModelServerController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *ModelServerController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ModelServerController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	// Handle initial sync signal
	if obj.ResourceType == "" && obj.Key == "" {
		klog.V(2).Info("initial modelServer and pod resources have been synced")
		c.workqueue.Forget(obj)
		c.initialSync.Store(true)
		return true
	}

	var err error
	switch obj.ResourceType {
	case ResourceTypeModelServer:
		err = c.syncModelServerHandler(obj.Key)
	case ResourceTypePod:
		err = c.syncPodHandler(obj.Key)
	default:
		c.workqueue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("unexpected resource type in workqueue: %s", obj.ResourceType))
		return true
	}

	if err != nil {
		if c.workqueue.NumRequeues(obj) < maxRetries {
			klog.V(2).Infof("error syncing %s %q: %v, requeuing", obj.ResourceType, obj.Key, err)
			c.workqueue.AddRateLimited(obj)
			return true
		}
		klog.V(2).Infof("giving up on syncing %s %q after %d retries: %v", obj.ResourceType, obj.Key, maxRetries, err)
	}
	c.workqueue.Forget(obj)
	return true
}

func (c *ModelServerController) syncModelServerHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	ms, err := c.modelServerLister.ModelServers(namespace).Get(name)
	if errors.IsNotFound(err) {
		_ = c.store.DeleteModelServer(types.NamespacedName{Namespace: namespace, Name: name})
		return nil
	}
	if err != nil {
		return err
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: ms.Spec.WorkloadSelector.MatchLabels})
	if err != nil {
		return fmt.Errorf("invalid selector: %v", err)
	}

	podList, err := c.podLister.Pods(ms.Namespace).List(selector)
	if err != nil {
		return err
	}

	pods := sets.NewWithLength[types.NamespacedName](len(podList))
	for _, pod := range podList {
		if isPodReady(pod) {
			pods.Insert(utils.GetNamespaceName(pod))
		}
	}

	_ = c.store.AddOrUpdateModelServer(ms, pods)

	// Get already bound pods to avoid unnecessary updates
	existingPods, err := c.store.GetPodsByModelServer(utils.GetNamespaceName(ms))
	if err != nil {
		klog.V(4).Infof("failed to get existing pods for ModelServer %s/%s: %v", ms.Namespace, ms.Name, err)
	}

	// Build a set of existing pod names that are already bound to the model server
	existingPodNames := sets.New[types.NamespacedName]()
	for _, podInfo := range existingPods {
		if !podInfo.HasModelServer(utils.GetNamespaceName(ms)) {
			// If the pod is not bound to the model server, establish the binding
			if err := c.store.AppendModelServerToPod(podInfo.Pod, []*aiv1alpha1.ModelServer{ms}); err != nil {
				klog.Warningf("failed to append modelserver %s/%s to pod %s/%s: %v", ms.Namespace, ms.Name, podInfo.Pod.Namespace, podInfo.Pod.Name, err)
				continue
			}
		}
		existingPodNames.Insert(utils.GetNamespaceName(podInfo.Pod))
	}

	// Add new pods that are not yet bound to the store
	for _, pod := range podList {
		if !isPodReady(pod) {
			continue
		}

		podName := utils.GetNamespaceName(pod)
		// Skip pods that are already properly bound
		if existingPodNames.Contains(podName) {
			continue
		}

		if err := c.store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms}); err != nil {
			klog.Warningf("failed to add new pod %s/%s to data store: %v", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

func (c *ModelServerController) syncPodHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if errors.IsNotFound(err) {
		_ = c.store.DeletePod(types.NamespacedName{Namespace: namespace, Name: name})
		return nil
	}
	if err != nil {
		return err
	}

	if !isPodReady(pod) {
		_ = c.store.DeletePod(types.NamespacedName{Namespace: namespace, Name: name})
		return nil
	}

	return c.addOrUpdatePod(pod)
}

// addOrUpdatePod finds all ModelServers that match the given pod
// and adds or updates the pod-server binding in the data store
func (c *ModelServerController) addOrUpdatePod(pod *corev1.Pod) error {
	modelServers, err := c.modelServerLister.ModelServers(pod.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list ModelServers for pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}

	servers := []*aiv1alpha1.ModelServer{}
	for _, item := range modelServers {
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: item.Spec.WorkloadSelector.MatchLabels})
		if err != nil || !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		servers = append(servers, item)
	}

	if len(servers) > 0 {
		if err := c.store.AddOrUpdatePod(pod, servers); err != nil {
			return fmt.Errorf("failed to add or update pod %s/%s in data store: %v", pod.Namespace, pod.Name, err)
		}
	}

	return nil
}

func (c *ModelServerController) enqueueModelServer(obj interface{}) {
	var key string
	var err error
	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(QueueItem{
		ResourceType: ResourceTypeModelServer,
		Key:          key,
	})
}

func (c *ModelServerController) enqueuePod(obj interface{}) {
	var key string
	var err error
	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(QueueItem{
		ResourceType: ResourceTypePod,
		Key:          key,
	})
}

// isPodReady checks if the pod is in a running state and has a PodReady condition set to true.
func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
