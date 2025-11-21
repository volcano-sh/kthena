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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
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

	kubeClient kubernetes.Interface

	workqueue   workqueue.TypedRateLimitingInterface[any]
	initialSync *atomic.Bool
	store       datastore.Store
}

func NewGatewayController(
	gatewayInformerFactory gatewayinformers.SharedInformerFactory,
	kubeClient kubernetes.Interface,
	store datastore.Store,
) *GatewayController {
	gatewayInformer := gatewayInformerFactory.Gateway().V1().Gateways()

	controller := &GatewayController{
		gatewayLister: gatewayInformer.Lister(),
		gatewaySynced: gatewayInformer.Informer().HasSynced,
		kubeClient:    kubeClient,
		workqueue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]()),
		initialSync:   &atomic.Bool{},
		store:         store,
	}

	controller.registration, _ = gatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueGateway,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueGateway(new)
		},
		DeleteFunc: controller.enqueueGateway,
	})

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
		// Gateway was deleted, clean up the service
		if err := c.deleteGatewayService(namespace, name); err != nil {
			klog.Errorf("Failed to delete service for gateway %s: %v", key, err)
		}
		_ = c.store.DeleteGateway(key)
		return nil
	}
	if err != nil {
		return err
	}

	// Only process Gateways that reference the kthena-router GatewayClass
	if string(gateway.Spec.GatewayClassName) != DefaultGatewayClassName {
		klog.V(4).Infof("Skipping Gateway %s/%s: does not reference %s GatewayClass", namespace, name, DefaultGatewayClassName)
		return nil
	}

	// Store the Gateway
	if err := c.store.AddOrUpdateGateway(gateway); err != nil {
		return err
	}

	// Create or update Service for the Gateway
	if err := c.ensureGatewayService(gateway); err != nil {
		return fmt.Errorf("failed to ensure service for gateway %s: %w", key, err)
	}

	return nil
}

// ensureGatewayService creates or updates a Service for the Gateway
func (c *GatewayController) ensureGatewayService(gateway *gatewayv1.Gateway) error {
	ctx := context.Background()
	serviceName := fmt.Sprintf("%s-service", gateway.Name)

	// Determine service ports from Gateway listeners
	var ports []corev1.ServicePort
	for _, listener := range gateway.Spec.Listeners {
		ports = append(ports, corev1.ServicePort{
			Name:     string(listener.Name),
			Port:     int32(listener.Port),
			Protocol: corev1.ProtocolTCP,
		})
	}

	// If no listeners, use default port
	if len(ports) == 0 {
		ports = []corev1.ServicePort{
			{
				Name:     "http",
				Port:     80,
				Protocol: corev1.ProtocolTCP,
			},
		}
	}

	// Set APIVersion and Kind explicitly for OwnerReference
	apiVersion := gateway.APIVersion
	if apiVersion == "" {
		apiVersion = "gateway.networking.k8s.io/v1"
	}
	kind := gateway.Kind
	if kind == "" {
		kind = "Gateway"
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: gateway.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: apiVersion,
					Kind:       kind,
					Name:       gateway.Name,
					UID:        gateway.UID,
					Controller: func() *bool { b := true; return &b }(),
				},
			},
			Labels: map[string]string{
				"app.kubernetes.io/component": "kthena-router",
				"gateway":                     gateway.Name,
				"gateway.networking.k8s.io":   "kthena-router",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: ports,
			Selector: map[string]string{
				"app.kubernetes.io/component": "kthena-router",
			},
		},
	}

	// Check if service already exists
	existing, err := c.kubeClient.CoreV1().Services(gateway.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Create the service
		_, err = c.kubeClient.CoreV1().Services(gateway.Namespace).Create(ctx, service, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create service: %w", err)
		}
		klog.Infof("Created Service %s/%s for Gateway %s/%s", gateway.Namespace, serviceName, gateway.Namespace, gateway.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	// Update the service if needed
	existing.Spec.Ports = service.Spec.Ports
	existing.Spec.Selector = service.Spec.Selector
	_, err = c.kubeClient.CoreV1().Services(gateway.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update service: %w", err)
	}
	klog.V(4).Infof("Updated Service %s/%s for Gateway %s/%s", gateway.Namespace, serviceName, gateway.Namespace, gateway.Name)
	return nil
}

// deleteGatewayService deletes the Service associated with a Gateway
func (c *GatewayController) deleteGatewayService(namespace, gatewayName string) error {
	ctx := context.Background()
	serviceName := fmt.Sprintf("%s-service", gatewayName)

	err := c.kubeClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		// Service doesn't exist, nothing to do
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}

	klog.Infof("Deleted Service %s/%s for Gateway %s/%s", namespace, serviceName, namespace, gatewayName)
	return nil
}

func (c *GatewayController) enqueueGateway(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
