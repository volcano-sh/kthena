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

	"github.com/stretchr/testify/require"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
)

type testControllerHarness struct {
	t             *testing.T
	ctx           context.Context
	cancel        context.CancelFunc
	controller    *ModelServingController
	kubeClient    *fake.Clientset
	kthenaClient  *kthenafake.Clientset
	volcanoClient *volcanofake.Clientset
	apiextClient  *apiextfake.Clientset
}

func newTestController(t *testing.T, modelServings ...*workloadv1alpha1.ModelServing) *testControllerHarness {
	t.Helper()

	objects := make([]runtime.Object, 0, len(modelServings))
	for _, ms := range modelServings {
		objects = append(objects, ms.DeepCopy())
	}

	kubeClient := fake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset(objects...)
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextClient := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	harness := &testControllerHarness{
		t:             t,
		ctx:           ctx,
		cancel:        cancel,
		controller:    controller,
		kubeClient:    kubeClient,
		kthenaClient:  kthenaClient,
		volcanoClient: volcanoClient,
		apiextClient:  apiextClient,
	}

	go controller.Run(ctx, 0)

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(syncCancel)
	if !cache.WaitForCacheSync(syncCtx.Done(),
		controller.podsInformer.HasSynced,
		controller.servicesInformer.HasSynced,
		controller.modelServingsInformer.HasSynced,
	) {
		cancel()
		t.Fatalf("timed out waiting for informer caches to sync")
	}
	require.Eventually(t, func() bool {
		return controller.initialSync
	}, 5*time.Second, 10*time.Millisecond, "timed out waiting for initial sync")

	t.Cleanup(cancel)
	return harness
}

func namespacedKey(namespace, name string) string {
	return namespace + "/" + name
}

func (h *testControllerHarness) expectQueuedKey(key string) {
	h.t.Helper()

	var seen []interface{}
	defer func() {
		for _, item := range seen {
			h.controller.workqueue.Add(item)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.controller.workqueue.Len() == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		item, shutdown := h.controller.workqueue.Get()
		require.False(h.t, shutdown, "workqueue shut down while waiting for %s", key)
		h.controller.workqueue.Done(item)
		h.controller.workqueue.Forget(item)
		itemKey, ok := item.(string)
		if ok && itemKey == key {
			return
		}
		seen = append(seen, item)
	}
	h.t.Fatalf("timed out waiting for %s", key)
}
