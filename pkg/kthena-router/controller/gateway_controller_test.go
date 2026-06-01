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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func newTestGateway(name, namespace, gatewayClassName string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gatewayClassName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}
}

func TestGatewayController_Lifecycle(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	controller := NewGatewayController(gatewayInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	t.Run("GatewayCreate", func(t *testing.T) {
		// Verifies that creating a kthena-router Gateway appears in the
		// informer cache and gets synced into the datastore
		gw := newTestGateway("test-gateway", "default", DefaultGatewayClassName)

		_, err := gatewayClient.GatewayV1().Gateways("default").Create(
			context.Background(), gw, metav1.CreateOptions{})
		assert.NoError(t, err)

		if !waitForCacheSync(t, 5*time.Second, controller.gatewaySynced) {
			t.Fatal("Failed to sync caches within timeout")
		}

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.gatewayLister.Gateways("default").Get("test-gateway")
			return err == nil
		})
		assert.True(t, found, "Gateway should be in cache")

		err = controller.syncHandler("default/test-gateway")
		assert.NoError(t, err)

		stored := store.GetGateway("default/test-gateway")
		assert.NotNil(t, stored)
		assert.Equal(t, "test-gateway", stored.Name)
	})

	t.Run("GatewayUpdate", func(t *testing.T) {
		// Verifies that updating a Gateway spec is reflected in the
		// informer cache and datastore after syncHandler is called
		existing, err := gatewayClient.GatewayV1().Gateways("default").Get(
			context.Background(), "test-gateway", metav1.GetOptions{})
		assert.NoError(t, err)

		updated := existing.DeepCopy()
		updated.Spec.Listeners[0].Port = 8080

		_, err = gatewayClient.GatewayV1().Gateways("default").Update(
			context.Background(), updated, metav1.UpdateOptions{})
		assert.NoError(t, err)

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			gw, err := controller.gatewayLister.Gateways("default").Get("test-gateway")
			return err == nil && gw.Spec.Listeners[0].Port == 8080
		})
		assert.True(t, found, "Gateway update should be reflected in cache")

		err = controller.syncHandler("default/test-gateway")
		assert.NoError(t, err)

		stored := store.GetGateway("default/test-gateway")
		assert.NotNil(t, stored)
		assert.Equal(t, gatewayv1.PortNumber(8080), stored.Spec.Listeners[0].Port)
	})

	t.Run("GatewayDelete", func(t *testing.T) {
		// Verifies that deleting a Gateway removes it from the informer
		// cache and the datastore after syncHandler is called.
		err := gatewayClient.GatewayV1().Gateways("default").Delete(
			context.Background(), "test-gateway", metav1.DeleteOptions{})
		assert.NoError(t, err)

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.gatewayLister.Gateways("default").Get("test-gateway")
			return err != nil
		})
		assert.True(t, found, "Gateway should be removed from cache")

		err = controller.syncHandler("default/test-gateway")
		assert.NoError(t, err)

		stored := store.GetGateway("default/test-gateway")
		assert.Nil(t, stored)
	})
}

func TestGatewayController_GatewayClassFilter(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	controller := NewGatewayController(gatewayInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	if !waitForCacheSync(t, 5*time.Second, controller.gatewaySynced) {
		t.Fatal("Failed to sync caches within timeout")
	}

	t.Run("NonKthenaGatewayNotStored", func(t *testing.T) {
		// Verifies that the FilteringResourceEventHandler only enqueues Gateways
		// with DefaultGatewayClassName. A Gateway with a different class should
		// not appear in the workqueue after the informer processes it.
		for controller.workqueue.Len() > 0 {
			controller.processNextWorkItem()
		}

		gw := newTestGateway("other-gateway", "default", "other-gateway-class")
		_, err := gatewayClient.GatewayV1().Gateways("default").Create(
			context.Background(), gw, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for the Gateway to appear in the informer cache first,
		// then verify it is never enqueued.
		waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.gatewayLister.Gateways("default").Get("other-gateway")
			return err == nil
		})
		assert.Never(t, func() bool {
			return controller.workqueue.Len() > 0
		}, 500*time.Millisecond, 10*time.Millisecond,
			"Non-kthena gateway should not be enqueued by the filter")

		stored := store.GetGateway("default/other-gateway")
		assert.Nil(t, stored, "Non-kthena gateway should not be in store")
	})
}

func TestGatewayController_ErrorHandling(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	controller := NewGatewayController(gatewayInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	t.Run("InvalidKey", func(t *testing.T) {
		// Verifies that a malformed key is handled gracefully without error.
		err := controller.syncHandler("invalid/key/format")
		assert.NoError(t, err)
	})

	t.Run("NonExistentGateway", func(t *testing.T) {
		// Verifies that syncing a non-existent key is a no-op without error.
		err := controller.syncHandler("default/non-existent")
		assert.NoError(t, err)
	})
}

func TestGatewayController_WorkQueueProcessing(t *testing.T) {
	gatewayClient := gatewayfake.NewSimpleClientset()
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
	store := datastore.New()

	controller := NewGatewayController(gatewayInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	gatewayInformerFactory.Start(stop)

	t.Run("InitialSyncSignal", func(t *testing.T) {
		// Verifies that the initialSyncSignal sentinel marks the controller
		// as synced via HasSynced().
		assert.False(t, controller.HasSynced())
		controller.workqueue.Add(initialSyncSignal)
		controller.processNextWorkItem()
		assert.True(t, controller.HasSynced())
	})

	t.Run("UnknownResourceType", func(t *testing.T) {
		// Verifies that an unexpected workqueue item type is dropped
		// without crashing the worker.
		controller.workqueue.Add(12345)
		result := controller.processNextWorkItem()
		assert.True(t, result)
	})
}
