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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func schedulerTestPod(name string) *datastore.PodInfo {
	return &datastore.PodInfo{
		Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}},
	}
}

func TestLeastLatencyName(t *testing.T) {
	plugin := NewLeastLatency(runtime.RawExtension{Raw: []byte(`{}`)})
	if plugin.Name() != LeastLatencyPluginName {
		t.Fatalf("expected plugin name %q, got %q", LeastLatencyPluginName, plugin.Name())
	}
}

func TestLeastLatencyScore(t *testing.T) {
	tests := []struct {
		name           string
		weightFactor   float64
		pods           []*datastore.PodInfo
		expectedScores map[string]int
	}{
		{
			name:         "empty pod list",
			weightFactor: 0.5,
			pods:         []*datastore.PodInfo{},
		},
		{
			name:         "single pod",
			weightFactor: 0.5,
			pods: []*datastore.PodInfo{
				{Pod: schedulerTestPod("pod-1").Pod, TTFT: 10, TPOT: 20},
			},
			expectedScores: map[string]int{"pod-1": 100},
		},
		{
			name:         "identical latency",
			weightFactor: 0.5,
			pods: []*datastore.PodInfo{
				{Pod: schedulerTestPod("pod-1").Pod, TTFT: 10, TPOT: 20},
				{Pod: schedulerTestPod("pod-2").Pod, TTFT: 10, TPOT: 20},
			},
			expectedScores: map[string]int{"pod-1": 100, "pod-2": 100},
		},
		{
			name:         "only tpot matters",
			weightFactor: 0.0,
			pods: []*datastore.PodInfo{
				{Pod: schedulerTestPod("pod-1").Pod, TTFT: 100, TPOT: 10},
				{Pod: schedulerTestPod("pod-2").Pod, TTFT: 1, TPOT: 20},
			},
			expectedScores: map[string]int{"pod-1": 100, "pod-2": 0},
		},
		{
			name:         "only ttft matters",
			weightFactor: 1.0,
			pods: []*datastore.PodInfo{
				{Pod: schedulerTestPod("pod-1").Pod, TTFT: 100, TPOT: 10},
				{Pod: schedulerTestPod("pod-2").Pod, TTFT: 1, TPOT: 20},
			},
			expectedScores: map[string]int{"pod-1": 0, "pod-2": 100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &LeastLatency{
				name:                 LeastLatencyPluginName,
				TTFTTPOTWeightFactor: tt.weightFactor,
			}

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

func TestNewLeastLatencyNilRawUsesDefaultWeight(t *testing.T) {
	plugin := NewLeastLatency(runtime.RawExtension{})
	if plugin.TTFTTPOTWeightFactor != 0.5 {
		t.Fatalf("expected default weight factor 0.5, got %v", plugin.TTFTTPOTWeightFactor)
	}
}

func TestNewLeastLatencyInvalidArgsUsesDefaultWeight(t *testing.T) {
	plugin := NewLeastLatency(runtime.RawExtension{Raw: []byte(`TTFTTPOTWeightFactor: [`)})
	if plugin.TTFTTPOTWeightFactor != 0.5 {
		t.Fatalf("expected default weight factor 0.5, got %v", plugin.TTFTTPOTWeightFactor)
	}
}

func TestCalculateMinMaxMetricsSkipsNegativeLatency(t *testing.T) {
	pods := []*datastore.PodInfo{
		{Pod: schedulerTestPod("pod-1").Pod, TTFT: 10, TPOT: 20},
		{Pod: schedulerTestPod("pod-2").Pod, TTFT: -1, TPOT: 5},
		{Pod: schedulerTestPod("pod-3").Pod, TTFT: 30, TPOT: 40},
		{Pod: schedulerTestPod("pod-4").Pod, TTFT: 5, TPOT: -1},
	}

	minTTFT, maxTTFT, minTPOT, maxTPOT := calculateMinMaxMetrics(pods)
	if minTTFT != 10 || maxTTFT != 30 || minTPOT != 20 || maxTPOT != 40 {
		t.Fatalf("expected min/max TTFT=(10,30) TPOT=(20,40), got TTFT=(%v,%v) TPOT=(%v,%v)", minTTFT, maxTTFT, minTPOT, maxTPOT)
	}
}
