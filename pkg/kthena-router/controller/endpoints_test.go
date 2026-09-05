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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

func staticModelServer(endpoints ...aiv1alpha1.Endpoint) *aiv1alpha1.ModelServer {
	return &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadPort:    aiv1alpha1.WorkloadPort{Port: 8000},
			Endpoints:       endpoints,
		},
	}
}

func podNames(t *testing.T, store datastore.Store, ms types.NamespacedName) []string {
	t.Helper()
	podInfos, err := store.GetPodsByModelServer(ms)
	require.NoError(t, err)
	names := make([]string, 0, len(podInfos))
	for _, podInfo := range podInfos {
		names = append(names, podInfo.GetPod().Name)
	}
	sort.Strings(names)
	return names
}

func TestSyncStaticEndpoints(t *testing.T) {
	store := newStoreWithMockBackend()
	ms := staticModelServer(
		aiv1alpha1.Endpoint{Name: "vllm-0", Address: "10.0.0.1"},
		aiv1alpha1.Endpoint{Name: "vllm-1", Address: "10.0.0.2"},
	)
	msName := utils.GetNamespaceName(ms)

	require.NoError(t, SyncStaticEndpoints(store, ms))

	assert.Equal(t, []string{"ms:vllm-0", "ms:vllm-1"}, podNames(t, store, msName))
	podInfo := store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "ms:vllm-0"})
	require.NotNil(t, podInfo)
	assert.Equal(t, "10.0.0.1", podInfo.GetPod().Status.PodIP)
	assert.Equal(t, string(aiv1alpha1.VLLM), podInfo.GetEngine())
}

func TestSyncStaticEndpointsRemovesStaleEndpoints(t *testing.T) {
	store := newStoreWithMockBackend()
	ms := staticModelServer(
		aiv1alpha1.Endpoint{Name: "vllm-0", Address: "10.0.0.1"},
		aiv1alpha1.Endpoint{Name: "vllm-1", Address: "10.0.0.2"},
	)
	msName := utils.GetNamespaceName(ms)
	require.NoError(t, SyncStaticEndpoints(store, ms))

	// Drop one endpoint and change the address of the other.
	updated := staticModelServer(aiv1alpha1.Endpoint{Name: "vllm-0", Address: "10.0.0.3"})
	require.NoError(t, SyncStaticEndpoints(store, updated))

	assert.Equal(t, []string{"ms:vllm-0"}, podNames(t, store, msName))
	assert.Nil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "ms:vllm-1"}))
	podInfo := store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "ms:vllm-0"})
	require.NotNil(t, podInfo)
	assert.Equal(t, "10.0.0.3", podInfo.GetPod().Status.PodIP)
}

func TestSyncStaticEndpointsIsolatesModelServers(t *testing.T) {
	store := newStoreWithMockBackend()
	shared := aiv1alpha1.Endpoint{Name: "vllm-0", Address: "10.0.0.1"}

	first := staticModelServer(shared)
	second := staticModelServer(shared)
	second.Name = "ms-2"

	require.NoError(t, SyncStaticEndpoints(store, first))
	require.NoError(t, SyncStaticEndpoints(store, second))

	// Endpoint names are only unique within one ModelServer, so each server
	// gets its own instance even when the endpoint names collide.
	assert.Equal(t, []string{"ms:vllm-0"}, podNames(t, store, utils.GetNamespaceName(first)))
	assert.Equal(t, []string{"ms-2:vllm-0"}, podNames(t, store, utils.GetNamespaceName(second)))

	// Removing the endpoint from the first ModelServer must not affect the
	// identically named endpoint of the second one.
	require.NoError(t, SyncStaticEndpoints(store, staticModelServer()))

	assert.Nil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "ms:vllm-0"}))
	assert.Equal(t, []string{"ms-2:vllm-0"}, podNames(t, store, utils.GetNamespaceName(second)))
	assert.NotNil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "ms-2:vllm-0"}))
}
