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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type GatewayController struct {
	gatewayLister gatewaylisters.GatewayLister
	gatewaySynced cache.InformerSynced
	registration  cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewGatewayController(
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	store datastore.Store,
) *GatewayController {
	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()

	controller := &GatewayController{
		gatewayLister: gatewayInformer.Lister(),
		gatewaySynced: gatewayInformer.Informer().HasSynced,
		workqueue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:   &atomic.Bool{},
		store:         store,
	}

	// Filter handler to only process Gateways that reference the kthena-router GatewayClass
	filterHandler := &cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			gateway, ok := obj.(*gatewayv1.Gateway)
			if !ok {
				return false
			}
			return string(gateway.Spec.GatewayClassName) == DefaultGatewayClassName
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: controller.enqueueGateway,
			UpdateFunc: func(old, new interface{}) {
				controller.enqueueGateway(new)
			},
			DeleteFunc: controller.enqueueGateway,
		},
	}

	controller.registration, _ = gatewayInformer.Informer().AddEventHandler(filterHandler)

	return controller
}

func (c *GatewayController) Run(stopCh <-chan struct{}) error {
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

func (c *GatewayController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *GatewayController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *GatewayController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("initial gateways have been synced")
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
			klog.Errorf("error syncing gateway %q: %s, requeuing", key, err.Error())
			c.workqueue.AddRateLimited(key)
			return true
		}
		klog.Errorf("giving up on syncing gateway %q after %d retries: %s", key, maxRetries, err)
		c.workqueue.Forget(obj)
	}
	return true
}

func (c *GatewayController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	gateway, err := c.gatewayLister.Gateways(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		_ = c.store.DeleteGateway(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Store the Gateway
	if err := c.store.AddOrUpdateGateway(gateway); err != nil {
		return err
	}

	return nil
}

func (c *GatewayController) enqueueGateway(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
