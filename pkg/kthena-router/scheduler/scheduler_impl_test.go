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

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

// TestTopNPodInfos tests the TopNPodInfos function
func TestTopNPodInfos(t *testing.T) {
	tests := []struct {
		name     string
		scores   map[*datastore.PodInfo]int
		n        int
		expected int
	}{
		{
			name:     "empty scores map returns empty slice",
			scores:   map[*datastore.PodInfo]int{},
			n:        1,
			expected: 0,
		},
		{
			name:     "nil scores map returns empty slice",
			scores:   nil,
			n:        1,
			expected: 0,
		},
		{
			name: "single pod with n=1",
			scores: map[*datastore.PodInfo]int{
				createTestPodInfo("pod1"): 100,
			},
			n:        1,
			expected: 1,
		},
		{
			name: "multiple pods with n greater than available",
			scores: map[*datastore.PodInfo]int{
				createTestPodInfo("pod1"): 100,
				createTestPodInfo("pod2"): 50,
			},
			n:        5,
			expected: 2,
		},
		{
			name: "multiple pods returns top n by score",
			scores: map[*datastore.PodInfo]int{
				createTestPodInfo("pod1"): 100,
				createTestPodInfo("pod2"): 50,
				createTestPodInfo("pod3"): 75,
			},
			n:        2,
			expected: 2,
		},
		{
			name: "n=0 returns empty slice",
			scores: map[*datastore.PodInfo]int{
				createTestPodInfo("pod1"): 100,
			},
			n:        0,
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TopNPodInfos(tt.scores, tt.n)
			assert.Equal(t, tt.expected, len(result))
		})
	}
}

// TestTopNPodInfosOrdering verifies that TopNPodInfos returns pods in descending score order
func TestTopNPodInfosOrdering(t *testing.T) {
	pod1 := createTestPodInfo("pod1")
	pod2 := createTestPodInfo("pod2")
	pod3 := createTestPodInfo("pod3")

	scores := map[*datastore.PodInfo]int{
		pod1: 50,
		pod2: 100,
		pod3: 75,
	}

	result := TopNPodInfos(scores, 3)

	require.Equal(t, 3, len(result))
	// Verify ordering: highest score first
	assert.Equal(t, "pod2", result[0].Pod.Name)
	assert.Equal(t, "pod3", result[1].Pod.Name)
	assert.Equal(t, "pod1", result[2].Pod.Name)
}

// TestSchedulePDGroup uses table-driven tests to validate PD scheduling behavior.
// It covers both graceful degradation (no prefill pods) and the happy path (valid prefill pod).
func TestSchedulePDGroup(t *testing.T) {
	tests := []struct {
		name                   string
		includePrefillPod      bool
		wantErr                bool
		expectedDecodePodCount int
		expectedPrefillCount   int
		expectPrefillNil       bool
		expectedPrefillPodName string
	}{
		{
			name:              "empty prefill scores returns error",
			includePrefillPod: false,
			wantErr:           true,
		},
		{
			name:                   "valid prefill pod selected - happy path",
			includePrefillPod:      true,
			expectedDecodePodCount: 1,
			expectedPrefillCount:   1,
			expectPrefillNil:       false,
			expectedPrefillPodName: "prefill-pod-0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock store
			store := datastore.New()

			// Create model server with PDGroup configuration
			modelServer := &aiv1alpha1.ModelServer{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model-server",
					Namespace: "default",
				},
				Spec: aiv1alpha1.ModelServerSpec{
					WorkloadSelector: &aiv1alpha1.WorkloadSelector{
						PDGroup: &aiv1alpha1.PDGroup{
							GroupKey:      "pd-group",
							DecodeLabels:  map[string]string{"role": "decode"},
							PrefillLabels: map[string]string{"role": "prefill"},
						},
					},
				},
			}

			// Add model server to store
			modelServerName := types.NamespacedName{Namespace: "default", Name: "test-model-server"}
			err := store.AddOrUpdateModelServer(modelServer, nil)
			require.NoError(t, err)

			// Add decode pod
			decodePod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "decode-pod-0",
					Namespace: "default",
					Labels: map[string]string{
						"pd-group": "group-1",
						"role":     "decode",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.1",
				},
			}
			err = store.AddOrUpdatePod(decodePod, []*aiv1alpha1.ModelServer{modelServer})
			require.NoError(t, err)

			// Conditionally add prefill pod
			if tt.includePrefillPod {
				prefillPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prefill-pod-0",
						Namespace: "default",
						Labels: map[string]string{
							"pd-group": "group-1",
							"role":     "prefill",
						},
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.2",
					},
				}
				err = store.AddOrUpdatePod(prefillPod, []*aiv1alpha1.ModelServer{modelServer})
				require.NoError(t, err)
			}

			// Create scheduler with minimal configuration
			scheduler := NewScheduler(store, nil).(*SchedulerImpl)

			// Create scheduling context with PDGroup enabled
			ctx := &framework.Context{
				ModelServerName: modelServerName,
				PDGroup: &aiv1alpha1.PDGroup{
					GroupKey:      "pd-group",
					DecodeLabels:  map[string]string{"role": "decode"},
					PrefillLabels: map[string]string{"role": "prefill"},
				},
			}

			// Get pods for scheduling
			pods, err := store.GetPodsByModelServer(modelServerName)
			require.NoError(t, err)

			err = scheduler.Schedule(ctx, pods)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Verify decode pod count
			require.Len(t, ctx.DecodePods, tt.expectedDecodePodCount, "unexpected decode pod count")

			// Verify prefill pod count
			require.Len(t, ctx.PrefillPods, tt.expectedPrefillCount, "unexpected prefill pod count")

			// Verify prefill pod nil/non-nil status
			if tt.expectPrefillNil {
				assert.Nil(t, ctx.PrefillPods[0], "expected nil prefill pod for graceful degradation")
			} else {
				require.NotNil(t, ctx.PrefillPods[0], "expected non-nil prefill pod")
				assert.Equal(t, tt.expectedPrefillPodName, ctx.PrefillPods[0].Pod.Name, "unexpected prefill pod name")
			}
		})
	}
}

// TestScheduleNonPDGroupWithEmptyScores tests non-PD scheduling with empty scores
func TestScheduleNonPDGroupWithEmptyScores(t *testing.T) {
	store := datastore.New()
	scheduler := NewScheduler(store, nil).(*SchedulerImpl)

	ctx := &framework.Context{
		ModelServerName: types.NamespacedName{Namespace: "default", Name: "test"},
		PDGroup:         nil, // Non-PD scheduling
	}

	// Empty pods slice
	pods := []*datastore.PodInfo{}

	// This will return an error from filter plugins (all filtered out)
	// but should not panic
	assert.NotPanics(t, func() {
		_ = scheduler.Schedule(ctx, pods)
	})
}

// TestRunScorePluginsEdgeCases uses table-driven tests to validate RunScorePlugins
// handles empty and nil pods gracefully without panicking.
func TestRunScorePluginsEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		pods []*datastore.PodInfo
	}{
		{
			name: "empty pods slice",
			pods: []*datastore.PodInfo{},
		},
		{
			name: "nil pods slice",
			pods: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := datastore.New()
			scheduler := NewScheduler(store, nil).(*SchedulerImpl)

			ctx := &framework.Context{
				ModelServerName: types.NamespacedName{Namespace: "default", Name: "test"},
			}

			// Should return empty map without panic
			result := scheduler.RunScorePlugins(tt.pods, ctx)
			assert.NotNil(t, result)
			assert.Equal(t, 0, len(result))
		})
	}
}

// Helper function to create test PodInfo
func createTestPodInfo(name string) *datastore.PodInfo {
	return &datastore.PodInfo{
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				PodIP: "10.0.0.1",
			},
		},
	}
}
