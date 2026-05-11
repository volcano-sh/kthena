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
)

func TestGPUCacheUsageName(t *testing.T) {
	plugin := NewGPUCacheUsage()
	if plugin.Name() != GPUCacheUsagePluginName {
		t.Fatalf("expected plugin name %q, got %q", GPUCacheUsagePluginName, plugin.Name())
	}
}

func TestGPUCacheUsageScore(t *testing.T) {
	tests := []struct {
		name           string
		pods           []*datastore.PodInfo
		expectedScores map[string]int
	}{
		{
			name: "empty pod list",
			pods: []*datastore.PodInfo{},
		},
		{
			name: "scores inverse gpu cache usage",
			pods: []*datastore.PodInfo{
				{Pod: schedulerTestPod("idle").Pod, GPUCacheUsage: 0.0},
				{Pod: schedulerTestPod("half").Pod, GPUCacheUsage: 0.5},
				{Pod: schedulerTestPod("full").Pod, GPUCacheUsage: 1.0},
			},
			expectedScores: map[string]int{"idle": 100, "half": 50, "full": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := NewGPUCacheUsage()
			scores := plugin.Score(nil, tt.pods)
			if len(scores) != len(tt.pods) {
				t.Fatalf("expected %d scores, got %d", len(tt.pods), len(scores))
			}

			for _, pod := range tt.pods {
				expected := tt.expectedScores[pod.Pod.Name]
				if scores[pod] != expected {
					t.Errorf("pod %s: expected score %d, got %d", pod.Pod.Name, expected, scores[pod])
				}
			}
		})
	}
}
