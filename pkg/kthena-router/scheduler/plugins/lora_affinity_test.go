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
	"testing"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

func TestLoraAffinityName(t *testing.T) {
	plugin := NewLoraAffinity()
	if plugin.Name() != LoraAffinityPluginName {
		t.Fatalf("expected plugin name %q, got %q", LoraAffinityPluginName, plugin.Name())
	}
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
