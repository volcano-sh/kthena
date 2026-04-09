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
	"strings"
	"testing"

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

		shouldUpdate := SetCondition(ms, progressingGroups, updatedGroups, currentGroups)
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

		shouldUpdate := SetCondition(ms, progressingGroups, updatedGroups, currentGroups)
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

		shouldUpdate := SetCondition(ms, progressingGroups, updatedGroups, currentGroups)
		assert.True(t, shouldUpdate)
		assert.Len(t, ms.Status.Conditions, 1)
		cond := ms.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingProgressing), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Contains(t, cond.Message, SomeGroupsAreProgressing)
	})
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

func TestModelNameAnnotationPropagation(t *testing.T) {
	baseRole := workloadv1alpha1.Role{
		Name: "prefill",
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{},
		},
	}

	tests := []struct {
		name           string
		msAnnotations  map[string]string
		expectLabel    bool
		expectedValue  string
	}{
		{
			name:          "valid model name is propagated",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: "deepseek-r1"},
			expectLabel:   true,
			expectedValue: "deepseek-r1",
		},
		{
			name:          "no annotations on ModelServing",
			msAnnotations: nil,
			expectLabel:   false,
		},
		{
			name:          "annotation key absent",
			msAnnotations: map[string]string{"other-key": "other-value"},
			expectLabel:   false,
		},
		{
			name:          "empty string value is not propagated",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: ""},
			expectLabel:   false,
		},
		{
			name:          "value with spaces is invalid and not propagated",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: "my model"},
			expectLabel:   false,
		},
		{
			name:          "value exceeding 63 chars is invalid and not propagated",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: strings.Repeat("a", 64)},
			expectLabel:   false,
		},
		{
			name:          "value with special characters is invalid and not propagated",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: "model@v1!"},
			expectLabel:   false,
		},
		{
			name:          "valid value with dots and hyphens",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: "llama-3.1-70b"},
			expectLabel:   true,
			expectedValue: "llama-3.1-70b",
		},
		{
			name:          "exactly 63 chars is valid",
			msAnnotations: map[string]string{workloadv1alpha1.ModelNameAnnotationKey: strings.Repeat("a", 63)},
			expectLabel:   true,
			expectedValue: strings.Repeat("a", 63),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ms",
					Namespace:   "default",
					Annotations: tt.msAnnotations,
				},
			}

			pod := GenerateEntryPod(baseRole, ms, "test-group", 0, "rev-1", "hash-1")
			assert.NotNil(t, pod)

			val, exists := pod.Labels[workloadv1alpha1.ModelNameAnnotationKey]
			if tt.expectLabel {
				assert.True(t, exists, "expected label to be set")
				assert.Equal(t, tt.expectedValue, val)
			} else {
				assert.False(t, exists, "expected label to NOT be set, but got: %q", val)
			}
		})
	}
}

func TestModelNameAnnotation_RoleTemplateTakesPrecedence(t *testing.T) {
	role := workloadv1alpha1.Role{
		Name: "prefill",
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Metadata: &workloadv1alpha1.Metadata{
				Labels: map[string]string{
					workloadv1alpha1.ModelNameAnnotationKey: "role-override",
				},
			},
			Spec: corev1.PodSpec{},
		},
	}

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			Annotations: map[string]string{
				workloadv1alpha1.ModelNameAnnotationKey: "from-cr-annotation",
			},
		},
	}

	pod := GenerateEntryPod(role, ms, "test-group", 0, "rev-1", "hash-1")
	assert.NotNil(t, pod)

	// Role template label applied via addPodLabelAndAnnotation overwrites the CR annotation value
	val := pod.Labels[workloadv1alpha1.ModelNameAnnotationKey]
	assert.Equal(t, "role-override", val)
}

func TestModelNameAnnotation_WorkerPod(t *testing.T) {
	role := workloadv1alpha1.Role{
		Name:           "decode",
		WorkerReplicas: 1,
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{},
		},
		WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{},
		},
	}

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			Annotations: map[string]string{
				workloadv1alpha1.ModelNameAnnotationKey: "deepseek-r1",
			},
		},
	}

	entryPod := GenerateEntryPod(role, ms, "test-group", 0, "rev-1", "hash-1")
	workerPod := GenerateWorkerPod(role, ms, entryPod, "test-group", 0, 1, "rev-1", "hash-1")

	assert.NotNil(t, workerPod)
	val, exists := workerPod.Labels[workloadv1alpha1.ModelNameAnnotationKey]
	assert.True(t, exists)
	assert.Equal(t, "deepseek-r1", val)
}
