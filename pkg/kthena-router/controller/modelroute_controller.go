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

	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type ModelRouteController struct {
	modelRouteLister listerv1alpha1.ModelRouteLister
	modelRouteSynced cache.InformerSynced
	registration     cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewModelRouteController(
	kthenaInformerFactory informersv1alpha1.SharedInformerFactory,
	store datastore.Store,
) *ModelRouteController {
	modelRouteInformer := kthenaInformerFactory.Networking().V1alpha1().ModelRoutes()

	controller := &ModelRouteController{
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

	return nil
}

func (c *ModelRouteController) enqueueModelRoute(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
