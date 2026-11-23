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
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// SchedulerProfileController watches SchedulerProfile objects and keeps the
// router's store in sync. Per-profile plugin chains are built from the store
// by the scheduler via the SchedulerProfile store callbacks.
type SchedulerProfileController struct {
	schedulerProfileLister listerv1alpha1.SchedulerProfileLister
	schedulerProfileSynced cache.InformerSynced
	registration           cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewSchedulerProfileController(
	kthenaInformerFactory informersv1alpha1.SharedInformerFactory,
	store datastore.Store,
) (*SchedulerProfileController, error) {
	schedulerProfileInformer := kthenaInformerFactory.Networking().V1alpha1().SchedulerProfiles()

	controller := &SchedulerProfileController{
		schedulerProfileLister: schedulerProfileInformer.Lister(),
		schedulerProfileSynced: schedulerProfileInformer.Informer().HasSynced,
		workqueue:              workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:            &atomic.Bool{},
		store:                  store,
	}

	var err error
	controller.registration, err = schedulerProfileInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueSchedulerProfile,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueSchedulerProfile(new)
		},
		DeleteFunc: controller.enqueueSchedulerProfile,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler for schedulerprofile controller: %w", err)
	}

	return controller, nil
}

func (c *SchedulerProfileController) Run(stopCh <-chan struct{}) error {
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

func (c *SchedulerProfileController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *SchedulerProfileController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *SchedulerProfileController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial scheduler profiles have been synced")
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

	err := c.syncHandler(key)
	retryOrForget(c.workqueue, "schedulerProfile", key, err, klog.V(2).Infof)
	return true
}

func (c *SchedulerProfileController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	profile, err := c.schedulerProfileLister.SchedulerProfiles(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			c.store.DeleteSchedulerProfile(types.NamespacedName{Namespace: namespace, Name: name})
			return nil
		}
		return err
	}

	c.store.AddOrUpdateSchedulerProfile(profile)
	return nil
}

func (c *SchedulerProfileController) enqueueSchedulerProfile(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
