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

func makePodWithOnFlight(name string, onFlight int64) *datastore.PodInfo {
	p := &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}}
	p.SetOnFlightRequestNum(onFlight)
	return p
}

// makePodWithRunning creates a PodInfo with both an on-flight count and an
// engine-reported running count. The scorer uses max(onFlight-running, 0) as
// an estimate of requests that are in-flight at the router but not yet
// executing at the engine (i.e. waiting in the engine queue).
func makePodWithRunning(name string, onFlight int64, running float64) *datastore.PodInfo {
	p := &datastore.PodInfo{
		Pod:               &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}},
		RequestRunningNum: running,
	}
	p.SetOnFlightRequestNum(onFlight)
	return p
}

func TestLeastRequestScore(t *testing.T) {
	tests := []struct {
		name           string
		pods           []*datastore.PodInfo
		expectedScores map[string]int
	}{
		{
			name: "all pods idle",
			pods: []*datastore.PodInfo{
				makePodWithOnFlight("pod-1", 0),
				makePodWithOnFlight("pod-2", 0),
				makePodWithOnFlight("pod-3", 0),
			},
			expectedScores: map[string]int{"pod-1": 100, "pod-2": 100, "pod-3": 100},
		},
		{
			name: "single pod idle",
			pods: []*datastore.PodInfo{
				makePodWithOnFlight("pod-1", 0),
			},
			expectedScores: map[string]int{"pod-1": 100},
		},
		{
			name: "mixed on-flight load",
			pods: []*datastore.PodInfo{
				makePodWithOnFlight("pod-1", 0),
				makePodWithOnFlight("pod-2", 10),
				makePodWithOnFlight("pod-3", 5),
			},
			expectedScores: map[string]int{"pod-1": 100, "pod-2": 0, "pod-3": 50},
		},
		{
			name: "normal non-zero case",
			pods: []*datastore.PodInfo{
				makePodWithOnFlight("pod-1", 1),
				makePodWithOnFlight("pod-2", 2),
				makePodWithOnFlight("pod-3", 3),
			},
			expectedScores: map[string]int{"pod-1": 66, "pod-2": 33, "pod-3": 0},
		},
		{
			// Demonstrates that the estimated engine-queue depth (onFlight - running)
			// dominates scoring. pod-2 has 5 in-flight but none running yet, so all
			// 5 are estimated as waiting → base = 5 + 100*5 = 505.
			// pod-1 has the same on-flight count but all are already running → base = 5.
			// pod-3 is idle → base = 0.
			// Scores: pod-3=100, pod-1=int(500/505*100)=99, pod-2=0.
			name: "estimated waiting dominates score",
			pods: []*datastore.PodInfo{
				makePodWithRunning("pod-1", 5, 5), // onFlight=running → estimated waiting=0 → base=5
				makePodWithRunning("pod-2", 5, 0), // onFlight=5, running=0 → estimated waiting=5 → base=505
				makePodWithRunning("pod-3", 0, 0), // idle → base=0
			},
			expectedScores: map[string]int{"pod-1": 99, "pod-2": 0, "pod-3": 100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := NewLeastRequest(runtime.RawExtension{Raw: []byte(`{}`)})
			scores := plugin.Score(nil, tt.pods)

			for _, pod := range tt.pods {
				podName := pod.Pod.Name
				expected := tt.expectedScores[podName]
				actual := scores[pod]
				if actual != expected {
					t.Errorf("pod %s: expected score %d, got %d", podName, expected, actual)
				}
			}
		})
	}
}
