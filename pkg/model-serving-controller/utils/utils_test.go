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

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

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
		partition := int32(2)
		ms := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{
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

func TestIsOwnedByModelServing(t *testing.T) {
	testCases := []struct {
		name      string
		ownerRefs []metav1.OwnerReference
		uid       types.UID
		expected  bool
	}{
		{
			name:      "empty owner references",
			ownerRefs: []metav1.OwnerReference{},
			uid:       types.UID("some-uid"),
			expected:  false,
		},
		{
			name: "owner reference matches UID",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "workload.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServing",
					Name:       "test-model-serving",
					UID:        types.UID("matching-uid"),
				},
			},
			uid:      types.UID("matching-uid"),
			expected: true,
		},
		{
			name: "owner reference does not match UID",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "workload.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServing",
					Name:       "test-model-serving",
					UID:        types.UID("different-uid"),
				},
			},
			uid:      types.UID("expected-uid"),
			expected: false,
		},
		{
			name: "multiple owner references with matching UID",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test-deployment",
					UID:        types.UID("non-matching-uid-1"),
				},
				{
					APIVersion: "workload.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServing",
					Name:       "test-model-serving",
					UID:        types.UID("matching-uid"),
				},
				{
					APIVersion: "v1",
					Kind:       "Service",
					Name:       "test-service",
					UID:        types.UID("non-matching-uid-2"),
				},
			},
			uid:      types.UID("matching-uid"),
			expected: true,
		},
		{
			name: "multiple owner references without matching UID",
			ownerRefs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test-deployment",
					UID:        types.UID("non-matching-uid-1"),
				},
				{
					APIVersion: "v1",
					Kind:       "Service",
					Name:       "test-service",
					UID:        types.UID("non-matching-uid-2"),
				},
			},
			uid:      types.UID("expected-uid"),
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := IsOwnedByModelServing(tc.ownerRefs, tc.uid)
			assert.Equal(t, tc.expected, result)
		})
	}
}
