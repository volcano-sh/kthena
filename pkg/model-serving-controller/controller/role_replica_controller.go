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

	// nolint
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
		// nolint
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ModelServingRoleReplicas"),
		recorder:              recorder,
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
	rrs, err := c.roleReplicaLister.ModelServingRoleReplicas(ms.Namespace).List(labels.Everything())
	if err == nil {
		for _, rr := range rrs {
			if rr.Spec.ModelServingRef.Name == ms.Name {
				c.enqueueRoleReplica(rr)
			}
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
	for i, role := range msCopy.Spec.Template.Roles {
		if role.Name == rr.Spec.RoleName {
			roleFound = true
			if role.Replicas != nil {
				msRoleReplicas = *role.Replicas
			}

			// Sync HPA -> ModelServing
			if rr.Spec.Replicas == nil {
				klog.Infof("Initializing RoleReplica %s/%s spec.replicas to %d", namespace, rr.Name, msRoleReplicas)
				rrCopy := rr.DeepCopy()
				rrCopy.Spec.Replicas = &msRoleReplicas
				_, err = c.modelServingClient.WorkloadV1alpha1().ModelServingRoleReplicas(namespace).Update(ctx, rrCopy, metav1.UpdateOptions{})
				return err
			}

			if *rr.Spec.Replicas != msRoleReplicas {
				klog.Infof("Syncing role %s replicas from %d to %d based on HPA", role.Name, msRoleReplicas, *rr.Spec.Replicas)
				msCopy.Spec.Template.Roles[i].Replicas = rr.Spec.Replicas
				_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(namespace).Update(ctx, msCopy, metav1.UpdateOptions{})
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
	selector := fmt.Sprintf("%s=%s,%s=%s", workloadv1alpha1.ModelServingNameLabelKey, ms.Name, workloadv1alpha1.RoleLabelKey, rr.Spec.RoleName)
	if rr.Status.Replicas != msRoleReplicas || rr.Status.LabelSelector != selector {
		rrCopy := rr.DeepCopy()
		rrCopy.Status.Replicas = msRoleReplicas
		rrCopy.Status.LabelSelector = selector
		_, err = c.modelServingClient.WorkloadV1alpha1().ModelServingRoleReplicas(namespace).UpdateStatus(ctx, rrCopy, metav1.UpdateOptions{})
		return err
	}

	return nil
}
