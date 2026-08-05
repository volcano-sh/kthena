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

package utils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestGenerateEntryPod_WithAnnotations(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
	}
	annotations := map[string]string{
		"test-annotation": "test-value",
	}
	role := workloadv1alpha1.Role{
		Name: "test-role",
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Metadata: &workloadv1alpha1.Metadata{
				Annotations: annotations,
			},
		},
	}

	var pod *corev1.Pod
	assert.NotPanics(t, func() {
		pod = GenerateEntryPod(role, ms, "test-group", 0, "test-revision", "role-revision")
	})
	assert.NotNil(t, pod)
	assert.Equal(t, annotations, pod.Annotations)
}

func TestGenerateWorkerPod_WithAnnotations(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
	}
	annotations := map[string]string{
		"test-annotation": "test-value",
	}
	role := workloadv1alpha1.Role{
		Name: "test-role",
		WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
			Metadata: &workloadv1alpha1.Metadata{
				Annotations: annotations,
			},
		},
	}

	entryPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-entry",
			Namespace: "default",
		},
	}
	var pod *corev1.Pod
	assert.NotPanics(t, func() {
		pod = GenerateWorkerPod(role, ms, entryPod, "test-group", 0, 1, "test-revision", "role-revision")
	})
	assert.NotNil(t, pod)
	assert.Equal(t, annotations, pod.Annotations)
}

func TestSetCondition(t *testing.T) {
	t.Run("All groups ready", func(t *testing.T) {
		ms := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{},
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{},
			},
		}

		progressingGroups := []int{}
		updatedGroups := []int{2, 3}
		currentGroups := []int{0, 1}

		shouldUpdate := SetCondition(ms, progressingGroups, updatedGroups, currentGroups, nil)
		assert.True(t, shouldUpdate)
		assert.Len(t, ms.Status.Conditions, 1)
		cond := ms.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingAvailable), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, "AllGroupsReady", cond.Reason)
	})

	t.Run("set updating in progress", func(t *testing.T) {
		ms := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{},
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{},
			},
		}

		progressingGroups := []int{3}
		updatedGroups := []int{2, 3}
		currentGroups := []int{0, 1}

		shouldUpdate := SetCondition(ms, progressingGroups, updatedGroups, currentGroups, nil)
		assert.True(t, shouldUpdate)
		assert.Len(t, ms.Status.Conditions, 1)
		cond := ms.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingUpdateInProgress), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Contains(t, cond.Message, SomeGroupsAreProgressing)
		assert.Contains(t, cond.Message, SomeGroupsAreUpdated)
	})

	t.Run("set partition, is updating", func(t *testing.T) {
		partition := intstr.FromInt32(2)
		ms := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{
				Replicas: ptr.To[int32](5),
				RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
					RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
						Partition: &partition,
					},
				},
			},
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{},
			},
		}

		progressingGroups := []int{2}
		updatedGroups := []int{2}
		currentGroups := []int{0, 1}

		shouldUpdate := SetCondition(ms, progressingGroups, updatedGroups, currentGroups, nil)
		assert.True(t, shouldUpdate)
		assert.Len(t, ms.Status.Conditions, 1)
		cond := ms.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingProgressing), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Contains(t, cond.Message, SomeGroupsAreProgressing)
	})

	t.Run("progressing with pod failure detail uses the specific reason and message", func(t *testing.T) {
		ms := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{},
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{},
			},
		}

		progressingGroups := []int{0}
		failure := &PodFailureDetail{
			Reason:  "ImagePullBackOff",
			Message: "pod test-ms-0-prefill-0-0 init container downloader: back-off pulling image",
		}

		shouldUpdate := SetCondition(ms, progressingGroups, nil, nil, failure)
		assert.True(t, shouldUpdate)
		assert.Len(t, ms.Status.Conditions, 1)
		cond := ms.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingProgressing), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		// The specific, stable failure reason replaces the generic "GroupProgressing" reason.
		assert.Equal(t, "ImagePullBackOff", cond.Reason)
		assert.Contains(t, cond.Message, SomeGroupsAreProgressing)
		assert.Contains(t, cond.Message, failure.Message)
	})

	t.Run("re-evaluating with an unchanged status still refreshes reason/message", func(t *testing.T) {
		ms := &workloadv1alpha1.ModelServing{
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{
					{
						Type:               string(workloadv1alpha1.ModelServingProgressing),
						Status:             metav1.ConditionTrue,
						Reason:             "GroupProgressing",
						Message:            "stale message",
						LastTransitionTime: metav1.NewTime(metav1.Now().Add(-time.Hour)),
					},
				},
			},
		}
		originalTransitionTime := ms.Status.Conditions[0].LastTransitionTime

		failure := &PodFailureDetail{Reason: "CrashLoopBackOff", Message: "pod p container c: crash looping"}
		shouldUpdate := SetCondition(ms, []int{0}, nil, nil, failure)
		assert.True(t, shouldUpdate, "message/reason changed even though Status stayed True, so an update is still required")

		cond := ms.Status.Conditions[0]
		assert.Equal(t, "CrashLoopBackOff", cond.Reason)
		assert.Contains(t, cond.Message, failure.Message)
		// Status didn't actually transition (True -> True), so the transition time must be preserved.
		assert.Equal(t, originalTransitionTime, cond.LastTransitionTime)
	})
}

func TestExtractPodFailureDetail(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		expectFailure  bool
		expectedReason string
	}{
		{
			name: "unschedulable pod is reported as a scheduling failure",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes are available: insufficient cpu"},
					},
				},
			},
			expectFailure:  true,
			expectedReason: "Unschedulable",
		},
		{
			name: "pod still pending on normal container creation is not a failure",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
					},
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
					},
				},
			},
			expectFailure: false,
		},
		{
			name: "init container image pull failure is reported",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "downloader", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff", Message: "back-off pulling image \"bad-registry/model:latest\"",
						}}},
					},
				},
			},
			expectFailure:  true,
			expectedReason: "ImagePullBackOff",
		},
		{
			name: "init container non-zero exit (e.g. downloader/model-path failure) is reported",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{
						{Name: "downloader", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1, Reason: "Error", Message: "model path not found",
						}}},
					},
				},
			},
			expectFailure:  true,
			expectedReason: "Error",
		},
		{
			name: "main container crash loop is reported",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "engine", RestartCount: 3, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
						}}},
					},
				},
			},
			expectFailure:  true,
			expectedReason: "CrashLoopBackOff",
		},
		{
			name: "main container OOMKilled after restart is reported from LastTerminationState",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "engine",
							RestartCount: 1,
							State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
								Reason: "OOMKilled", ExitCode: 137,
							}},
						},
					},
				},
			},
			expectFailure:  true,
			expectedReason: "OOMKilled",
		},
		{
			name: "pod failed phase without container detail falls back to PodFailed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status:     corev1.PodStatus{Phase: corev1.PodFailed},
			},
			expectFailure:  true,
			expectedReason: "PodFailed",
		},
		{
			name: "ready running pod has no failure",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "engine", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					},
				},
			},
			expectFailure: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail, ok := ExtractPodFailureDetail(tt.pod)
			assert.Equal(t, tt.expectFailure, ok)
			if tt.expectFailure {
				assert.Equal(t, tt.expectedReason, detail.Reason)
				assert.NotEmpty(t, detail.Message)
				assert.Contains(t, detail.Message, "p")
			}
		})
	}
}

func TestGetMaxUnavailable(t *testing.T) {
	tests := []struct {
		name           string
		modelServing   *workloadv1alpha1.ModelServing
		expectedResult int
		expectError    bool
	}{
		{
			name: "Default case - no rollout strategy",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](5),
				},
			},
			expectedResult: 1, // Default value
			expectError:    false,
		},
		{
			name: "Default case - rollout strategy but no rolling update config",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](10),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
					},
				},
			},
			expectedResult: 1, // Default value
			expectError:    false,
		},
		{
			name: "MaxUnavailable as integer - value 2",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](10),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromInt(2)),
						},
					},
				},
			},
			expectedResult: 2,
			expectError:    false,
		},
		{
			name: "MaxUnavailable as integer - value 0",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](5),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromInt(0)),
						},
					},
				},
			},
			expectedResult: 0,
			expectError:    false,
		},
		{
			name: "MaxUnavailable as percentage - 20%",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](10),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromString("20%")),
						},
					},
				},
			},
			expectedResult: 2, // 20% of 10 is 2
			expectError:    false,
		},
		{
			name: "MaxUnavailable as percentage - 50%",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](9),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromString("50%")),
						},
					},
				},
			},
			expectedResult: 4, // 50% of 9 is 4.5, rounded down to 4
			expectError:    false,
		},
		{
			name: "MaxUnavailable as percentage - 100%",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](3),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromString("100%")),
						},
					},
				},
			},
			expectedResult: 3, // 100% of 3 is 3
			expectError:    false,
		},
		{
			name: "MaxUnavailable as percentage - 0%",
			modelServing: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](10),
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: "ServingGroupRollingUpdate",
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromString("0%")),
						},
					},
				},
			},
			expectedResult: 0, // 0% of 10 is 0
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GetMaxUnavailable(tt.modelServing)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedResult, result)
			}
		})
	}
}

func TestGetMaxUnavailableForRole(t *testing.T) {
	tests := []struct {
		name           string
		role           workloadv1alpha1.Role
		wantValue      int
		wantConfigured bool
		wantErr        bool
	}{
		{
			name:           "unset",
			role:           workloadv1alpha1.Role{Name: "decode", Replicas: ptr.To[int32](4)},
			wantConfigured: false,
		},
		{
			name: "absolute value",
			role: workloadv1alpha1.Role{
				Name:     "decode",
				Replicas: ptr.To[int32](4),
				RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt(2)),
				},
			},
			wantValue:      2,
			wantConfigured: true,
		},
		{
			name: "percentage rounds down",
			role: workloadv1alpha1.Role{
				Name:     "decode",
				Replicas: ptr.To[int32](5),
				RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromString("50%")),
				},
			},
			wantValue:      2,
			wantConfigured: true,
		},
		{
			name: "nil replicas defaults to one",
			role: workloadv1alpha1.Role{
				Name: "decode",
				RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt(1)),
				},
			},
			wantValue:      1,
			wantConfigured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotValue, gotConfigured, err := GetMaxUnavailableForRole(tt.role)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantValue, gotValue)
			assert.Equal(t, tt.wantConfigured, gotConfigured)
		})
	}
}
