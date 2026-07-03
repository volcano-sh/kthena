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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	kthenaclient "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type ModelRouteController struct {
	kthenaClient     kthenaclient.Interface
	modelRouteLister listerv1alpha1.ModelRouteLister
	modelRouteSynced cache.InformerSynced
	registration     cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewModelRouteController(
	kthenaClient kthenaclient.Interface,
	kthenaInformerFactory informersv1alpha1.SharedInformerFactory,
	store datastore.Store,
) *ModelRouteController {
	modelRouteInformer := kthenaInformerFactory.Networking().V1alpha1().ModelRoutes()

	controller := &ModelRouteController{
		kthenaClient:     kthenaClient,
		modelRouteLister: modelRouteInformer.Lister(),
		modelRouteSynced: modelRouteInformer.Informer().HasSynced,
		workqueue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:      &atomic.Bool{},
		store:            store,
	}

	controller.registration, _ = modelRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueModelRoute,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueModelRoute(new)
		},
		DeleteFunc: controller.enqueueModelRoute,
	})

	return controller
}

func (c *ModelRouteController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.registration.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	// add initialSync signal
	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *ModelRouteController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *ModelRouteController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ModelRouteController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial model routes have been synced")
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
			klog.V(2).Infof("error syncing modelRoute %q: %s, requeuing", key, err.Error())
			c.workqueue.AddRateLimited(key)
			return true
		}
		klog.V(2).Infof("giving up on syncing modelRoute %q after %d retries: %s", key, maxRetries, err)
		c.workqueue.Forget(obj)
	}
	return true
}

func (c *ModelRouteController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	mr, err := c.modelRouteLister.ModelRoutes(namespace).Get(name)
	if errors.IsNotFound(err) {
		_ = c.store.DeleteModelRoute(key)
		return nil
	}
	if err != nil {
		return err
	}

	if err := c.store.AddOrUpdateModelRoute(mr); err != nil {
		return err
	}

	return c.updateModelRouteStatus(mr)
}

func (c *ModelRouteController) updateModelRouteStatus(mr *aiv1alpha1.ModelRoute) error {
	// Check if status is already up-to-date
	if mr.Status.ObservedGeneration == mr.Generation {
		if cond := meta.FindStatusCondition(mr.Status.Conditions, "Ready"); cond != nil &&
			cond.Status == metav1.ConditionTrue &&
			cond.Reason == "RouteRegistered" {
			return nil
		}
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get latest version from API server to avoid conflicts
		ctx := context.Background()
		latest, err := c.kthenaClient.NetworkingV1alpha1().ModelRoutes(mr.Namespace).Get(ctx, mr.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		mrCopy := latest.DeepCopy()
		mrCopy.Status.ObservedGeneration = latest.Generation

		meta.SetStatusCondition(&mrCopy.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "RouteRegistered",
			Message: "ModelRoute registered in router store",
		})

		_, err = c.kthenaClient.NetworkingV1alpha1().ModelRoutes(mr.Namespace).UpdateStatus(ctx, mrCopy, metav1.UpdateOptions{})
		return err
	})
}

func (c *ModelRouteController) enqueueModelRoute(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
