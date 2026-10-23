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

package plugins

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/discovery"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

func TestLoraAffinityName(t *testing.T) {
	plugin := NewLoraAffinity()
	if plugin.Name() != LoraAffinityPluginName {
		t.Fatalf("expected plugin name %q, got %q", LoraAffinityPluginName, plugin.Name())
	}
}

type coldStartInspector struct{ calls atomic.Int32 }

func (i *coldStartInspector) GetPodMetrics(string, *corev1.Pod, uint32, map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	i.calls.Add(1)
	return map[string]float64{utils.KVCacheUsage: 0.5}, nil
}

func (i *coldStartInspector) GetPodModels(string, *corev1.Pod, uint32) ([]string, error) {
	i.calls.Add(1)
	return []string{"base-model"}, nil
}

func TestDiscoveredEndpointColdStart(t *testing.T) {
	i := &coldStartInspector{}
	s := datastore.New(datastore.WithPodRuntimeInspector(i), datastore.WithDeferredRuntimeInspection())
	ms := &api.ModelServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "model"},
		Spec: api.ModelServerSpec{InferenceEngine: api.VLLM, WorkloadPort: api.WorkloadPort{Port: 8000}}}
	require.NoError(t, discovery.SyncEndpoints(s, ms, []api.Endpoint{{Name: "a", Address: "10.0.0.1"}}))
	pods, err := s.GetPodsByModelServer(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.Len(t, pods, 1)
	info := pods[0]
	ctx := &framework.Context{Model: "base-model"}
	require.Zero(t, i.calls.Load(), "membership updates do not inspect the runtime")
	require.Empty(t, NewLoraAffinity().Filter(ctx, append([]*datastore.PodInfo(nil), pods...)))
	require.Equal(t, 100, NewGPUCacheUsage().Score(ctx, pods)[info], "missing metrics currently score as zero usage")
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Run(runCtx)
	require.Eventually(t, s.HasSynced, 5*time.Second, time.Millisecond)
	require.GreaterOrEqual(t, i.calls.Load(), int32(2))
	require.Len(t, NewLoraAffinity().Filter(ctx, append([]*datastore.PodInfo(nil), pods...)), 1)
	require.Equal(t, 50, NewGPUCacheUsage().Score(ctx, pods)[info])
}

func TestLoraAffinityFilter(t *testing.T) {
	matchingPod := schedulerTestPod("matching")
	matchingPod.UpdateModels([]string{"base-model", "adapter-a"})
	otherPod := schedulerTestPod("other")
	otherPod.UpdateModels([]string{"base-model", "adapter-b"})
	emptyModelPod := schedulerTestPod("empty-model")
	emptyModelPod.UpdateModels([]string{""})

	tests := []struct {
		name          string
		model         string
		pods          []*datastore.PodInfo
		expectedNames []string
	}{
		{
			name:          "empty pod list",
			model:         "adapter-a",
			pods:          []*datastore.PodInfo{},
			expectedNames: []string{},
		},
		{
			name:          "keeps pods containing requested model",
			model:         "adapter-a",
			pods:          []*datastore.PodInfo{matchingPod, otherPod},
			expectedNames: []string{"matching"},
		},
		{
			name:          "filters pods without requested model",
			model:         "missing-adapter",
			pods:          []*datastore.PodInfo{matchingPod, otherPod},
			expectedNames: []string{},
		},
		{
			name:          "empty model name uses contains semantics",
			model:         "",
			pods:          []*datastore.PodInfo{matchingPod, emptyModelPod},
			expectedNames: []string{"empty-model"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := NewLoraAffinity()
			filtered := plugin.Filter(&framework.Context{Model: tt.model}, append([]*datastore.PodInfo(nil), tt.pods...))
			if len(filtered) != len(tt.expectedNames) {
				t.Fatalf("expected %d pods, got %d", len(tt.expectedNames), len(filtered))
			}

			for i, pod := range filtered {
				if pod.Pod.Name != tt.expectedNames[i] {
					t.Errorf("index %d: expected pod %q, got %q", i, tt.expectedNames[i], pod.Pod.Name)
				}
			}
		})
	}
}
