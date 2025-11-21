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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type GatewayClassController struct {
	gatewayClient gatewayclientset.Interface

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewGatewayClassController(
	gatewayClient gatewayclientset.Interface,
	store datastore.Store,
) *GatewayClassController {
	controller := &GatewayClassController{
		gatewayClient: gatewayClient,
		workqueue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:   &atomic.Bool{},
		store:         store,
	}

	return controller
}

func (c *GatewayClassController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Create default GatewayClass if it doesn't exist
	if err := c.ensureDefaultGatewayClass(); err != nil {
		klog.Errorf("Failed to ensure default GatewayClass: %v", err)
		return err
	}

	c.workqueue.Add(initialSyncSignal)

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *GatewayClassController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *GatewayClassController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *GatewayClassController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj == initialSyncSignal {
		klog.V(2).Info("default GatewayClass has been ensured")
		c.workqueue.Forget(obj)
		c.initialSync.Store(true)
		return true
	}

	return true
}

// ensureDefaultGatewayClass creates the default GatewayClass if it doesn't exist
func (c *GatewayClassController) ensureDefaultGatewayClass() error {
	ctx := context.Background()

	// Check if GatewayClass already exists
	_, err := c.gatewayClient.GatewayV1().GatewayClasses().Get(ctx, DefaultGatewayClassName, metav1.GetOptions{})
	if err == nil {
		klog.V(2).Infof("Default GatewayClass %s already exists", DefaultGatewayClassName)
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check GatewayClass %s: %w", DefaultGatewayClassName, err)
	}

	// Create the default GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: DefaultGatewayClassName,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
		},
	}

	_, err = c.gatewayClient.GatewayV1().GatewayClasses().Create(ctx, gatewayClass, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(2).Infof("GatewayClass %s was created by another process", DefaultGatewayClassName)
			return nil
		}
		return fmt.Errorf("failed to create GatewayClass %s: %w", DefaultGatewayClassName, err)
	}

	klog.Infof("Created default GatewayClass %s", DefaultGatewayClassName)
	return nil
}
