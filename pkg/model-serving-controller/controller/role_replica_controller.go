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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

type RoleReplicaSyncController struct {
	kubeClientSet      kubernetes.Interface
	modelServingClient clientset.Interface

	modelServingLister    listerv1alpha1.ModelServingLister
	modelServingsInformer cache.SharedIndexInformer
	roleReplicaLister     listerv1alpha1.ModelServingRoleReplicaLister
	roleReplicasInformer  cache.SharedIndexInformer

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

func NewRoleReplicaSyncController(kubeClientSet kubernetes.Interface, modelServingClient clientset.Interface, modelServingInformerFactory informersv1alpha1.SharedInformerFactory) *RoleReplicaSyncController {
	modelServingInformer := modelServingInformerFactory.Workload().V1alpha1().ModelServings()
	roleReplicaInformer := modelServingInformerFactory.Workload().V1alpha1().ModelServingRoleReplicas()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: kubeClientSet.CoreV1().Events(""),
	})
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "rolereplica-sync-controller"},
	)

	c := &RoleReplicaSyncController{
		kubeClientSet:         kubeClientSet,
		modelServingClient:    modelServingClient,
		modelServingLister:    modelServingInformer.Lister(),
		modelServingsInformer: modelServingInformer.Informer(),
		roleReplicaLister:     roleReplicaInformer.Lister(),
		roleReplicasInformer:  roleReplicaInformer.Informer(),
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ModelServingRoleReplicas"),
		recorder:              recorder,
	}

	err := roleReplicaInformer.Informer().AddIndexers(cache.Indexers{
		"modelServingRef": func(obj interface{}) ([]string, error) {
			rr, ok := obj.(*workloadv1alpha1.ModelServingRoleReplica)
			if !ok {
				return []string{}, nil
			}
			return []string{rr.Spec.ModelServingRef.Name}, nil
		},
	})
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to add indexer for ModelServingRoleReplica: %v", err))
	}

	_, _ = roleReplicaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueueRoleReplica(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.enqueueRoleReplica(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			c.enqueueRoleReplica(obj)
		},
	})

	_, _ = modelServingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueueModelServing(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.enqueueModelServing(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			c.enqueueModelServing(obj)
		},
	})

	return c
}

func (c *RoleReplicaSyncController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("Starting RoleReplicaSyncController")

	if !cache.WaitForCacheSync(ctx.Done(), c.modelServingsInformer.HasSynced, c.roleReplicasInformer.HasSynced) {
		klog.Error("Timed out waiting for caches to sync for RoleReplicaSyncController")
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(func() { c.runWorker(ctx) }, time.Second, ctx.Done())
	}

	<-ctx.Done()
	klog.Info("Shutting down RoleReplicaSyncController")
}

func (c *RoleReplicaSyncController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *RoleReplicaSyncController) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.syncHandler(ctx, key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
	}

	return true
}

func (c *RoleReplicaSyncController) enqueueRoleReplica(obj interface{}) {
	var key string
	var err error
	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *RoleReplicaSyncController) enqueueModelServing(obj interface{}) {
	ms, ok := obj.(*workloadv1alpha1.ModelServing)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		ms, ok = tombstone.Obj.(*workloadv1alpha1.ModelServing)
		if !ok {
			return
		}
	}

	// Enqueue all related RoleReplicas
	objs, err := c.roleReplicasInformer.GetIndexer().ByIndex("modelServingRef", ms.Name)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to get RoleReplicas from indexer for ModelServing %s: %v", ms.Name, err))
		return
	}
	for _, obj := range objs {
		if rr, ok := obj.(*workloadv1alpha1.ModelServingRoleReplica); ok {
			c.enqueueRoleReplica(rr)
		}
	}
}

func (c *RoleReplicaSyncController) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	rr, err := c.roleReplicaLister.ModelServingRoleReplicas(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	ms, err := c.modelServingLister.ModelServings(namespace).Get(rr.Spec.ModelServingRef.Name)
	if apierrors.IsNotFound(err) {
		return nil // parent deleted
	}
	if err != nil {
		return err
	}

	var roleFound bool
	msRoleReplicas := int32(1)

	msCopy := ms.DeepCopy()
	for _, role := range msCopy.Spec.Template.Roles {
		if role.Name == rr.Spec.RoleName {
			roleFound = true
			if role.Replicas != nil {
				msRoleReplicas = *role.Replicas
			}

			// Sync ModelServing -> RoleReplica spec
			if rr.Spec.Replicas == nil {
				klog.Infof("Initializing RoleReplica %s/%s spec.replicas to %d", namespace, rr.Name, msRoleReplicas)
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					rrLatest, getErr := c.modelServingClient.WorkloadV1alpha1().ModelServingRoleReplicas(namespace).Get(ctx, rr.Name, metav1.GetOptions{})
					if getErr != nil {
						return getErr
					}
					rrLatest.Spec.Replicas = &msRoleReplicas
					_, updateErr := c.modelServingClient.WorkloadV1alpha1().ModelServingRoleReplicas(namespace).Update(ctx, rrLatest, metav1.UpdateOptions{})
					return updateErr
				})
				return err
			}

			if *rr.Spec.Replicas != msRoleReplicas {
				klog.Infof("Syncing role %s replicas from %d to %d based on HPA", role.Name, msRoleReplicas, *rr.Spec.Replicas)
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					msLatest, getErr := c.modelServingClient.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, ms.Name, metav1.GetOptions{})
					if getErr != nil {
						return getErr
					}
					for j, r := range msLatest.Spec.Template.Roles {
						if r.Name == role.Name {
							msLatest.Spec.Template.Roles[j].Replicas = rr.Spec.Replicas
							break
						}
					}
					_, updateErr := c.modelServingClient.WorkloadV1alpha1().ModelServings(namespace).Update(ctx, msLatest, metav1.UpdateOptions{})
					return updateErr
				})
				return err
			}
			break
		}
	}

	if !roleFound {
		klog.Warningf("Role %s not found in ModelServing %s/%s", rr.Spec.RoleName, namespace, ms.Name)
		return nil
	}

	// Sync ModelServing -> RoleReplica Status
	selector := labels.Set{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.RoleLabelKey:             rr.Spec.RoleName,
	}.AsSelector().String()
	
	if rr.Status.Replicas != msRoleReplicas || rr.Status.LabelSelector != selector {
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			rrLatest, getErr := c.modelServingClient.WorkloadV1alpha1().ModelServingRoleReplicas(namespace).Get(ctx, rr.Name, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			rrLatest.Status.Replicas = msRoleReplicas
			rrLatest.Status.LabelSelector = selector
			_, updateErr := c.modelServingClient.WorkloadV1alpha1().ModelServingRoleReplicas(namespace).UpdateStatus(ctx, rrLatest, metav1.UpdateOptions{})
			return updateErr
		})
		return err
	}

	return nil
}
