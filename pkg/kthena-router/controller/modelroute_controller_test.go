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

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestModelRouteController_Lifecycle(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	store := datastore.New()

	controller := NewModelRouteController(kthenaInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)

	//this block verifies that creating a ModelRoute via fake client causes it to
	//appear in the informer cache and get synced into the datastore.
	t.Run("ModelRouteCreate", func(t *testing.T) {
		mr := &aiv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-modelroute",
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName: "test-model",
				Rules: []*aiv1alpha1.Rule{
					{
						Name: "rule-1",
						TargetModels: []*aiv1alpha1.TargetModel{
							{ModelServerName: "test-server"},
						},
					},
				},
			},
		}

		_, err := kthenaClient.NetworkingV1alpha1().ModelRoutes("default").Create(
			context.Background(), mr, metav1.CreateOptions{})
		assert.NoError(t, err)

		if !waitForCacheSync(t, 5*time.Second, controller.modelRouteSynced) {
			t.Fatal("Failed to sync caches within timeout")
		}

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelRouteLister.ModelRoutes("default").Get("test-modelroute")
			return err == nil
		})
		assert.True(t, found, "ModelRoute should be in cache")

		key := "default/test-modelroute"
		err = controller.syncHandler(key)
		assert.NoError(t, err)

		storedRoute := store.GetModelRoute("default/test-modelroute")
		assert.NotNil(t, storedRoute)
		assert.Equal(t, "test-model", storedRoute.Spec.ModelName)
	})

	//this will verify that updating a ModelRoute's spec is reflected in the
	//informer cache and the datastore after syncHandlerr is called.
	t.Run("ModelRouteUpdate", func(t *testing.T) {
		existing, err := kthenaClient.NetworkingV1alpha1().ModelRoutes("default").Get(
			context.Background(), "test-modelroute", metav1.GetOptions{})
		assert.NoError(t, err)

		updated := existing.DeepCopy()
		updated.Spec.ModelName = "updated-model"

		_, err = kthenaClient.NetworkingV1alpha1().ModelRoutes("default").Update(
			context.Background(), updated, metav1.UpdateOptions{})
		assert.NoError(t, err)

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			mr, err := controller.modelRouteLister.ModelRoutes("default").Get("test-modelroute")
			return err == nil && mr.Spec.ModelName == "updated-model"
		})
		assert.True(t, found, "ModelRoute update should be reflected in cache")

		err = controller.syncHandler("default/test-modelroute")
		assert.NoError(t, err)

		storedRoute := store.GetModelRoute("default/test-modelroute")
		assert.NotNil(t, storedRoute)
		assert.Equal(t, "updated-model", storedRoute.Spec.ModelName)
	})

	//Verifies that deleting a ModelRoute removes it from the informer
	//cache and datastore after syncHandler is called.
	t.Run("ModelRouteDelete", func(t *testing.T) {
		err := kthenaClient.NetworkingV1alpha1().ModelRoutes("default").Delete(
			context.Background(), "test-modelroute", metav1.DeleteOptions{})
		assert.NoError(t, err)

		waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelRouteLister.ModelRoutes("default").Get("test-modelroute")
			return err != nil
		})

		err = controller.syncHandler("default/test-modelroute")
		assert.NoError(t, err)

		storedRoute := store.GetModelRoute("default/test-modelroute")
		assert.Nil(t, storedRoute)
	})
}

func TestModelRouteController_ErrorHandling(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	store := datastore.New()

	controller := NewModelRouteController(kthenaInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)

	//this will verify if a malformed key is handled without returning an error
	t.Run("InvalidKey", func(t *testing.T) {
		err := controller.syncHandler("invalid/key/format")
		assert.NoError(t, err)
	})

	//this will verify that syncing a key for a resource that doesnt exist
	// is a no-op and doesnt return an error
	t.Run("NonExistentModelRoute", func(t *testing.T) {
		err := controller.syncHandler("default/non-existent")
		assert.NoError(t, err)
	})
}

func TestModelRouteController_WorkQueueProcessing(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	store := datastore.New()

	controller := NewModelRouteController(kthenaInformerFactory, store)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)

	//verifies that processing the initialSyncSignal sentinel value
	//marks the controller as synced via HasSynced()
	t.Run("InitialSyncSignal", func(t *testing.T) {
		assert.False(t, controller.HasSynced())
		controller.workqueue.Add(initialSyncSignal)
		controller.processNextWorkItem()
		assert.True(t, controller.HasSynced())
	})

	//verifies thqat an unexpected type in the workqueue is dropped without crashing the worker
	t.Run("UnknownResourceType", func(t *testing.T) {
		controller.workqueue.Add(12345)
		result := controller.processNextWorkItem()
		assert.True(t, result)
	})
}
