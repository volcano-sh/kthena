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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestSetCondition(t *testing.T) {
	t.Run("All groups ready", func(t *testing.T) {
		mi := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{},
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{},
			},
		}

		progressingGroups := []int{}
		updatedGroups := []int{2, 3}
		currentGroups := []int{0, 1}

		shouldUpdate := SetCondition(mi, progressingGroups, updatedGroups, currentGroups)
		assert.True(t, shouldUpdate)
		assert.Len(t, mi.Status.Conditions, 1)
		cond := mi.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingAvailable), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, "AllGroupsReady", cond.Reason)
	})

	t.Run("set updating in progress", func(t *testing.T) {
		mi := &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{},
			Status: workloadv1alpha1.ModelServingStatus{
				Conditions: []metav1.Condition{},
			},
		}

		progressingGroups := []int{3}
		updatedGroups := []int{2, 3}
		currentGroups := []int{0, 1}

		shouldUpdate := SetCondition(mi, progressingGroups, updatedGroups, currentGroups)
		assert.True(t, shouldUpdate)
		assert.Len(t, mi.Status.Conditions, 1)
		cond := mi.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingUpdateInProgress), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Contains(t, cond.Message, SomeGroupsAreProgressing)
		assert.Contains(t, cond.Message, SomeGroupsAreUpdated)
	})

	t.Run("set partition, is updating", func(t *testing.T) {
		partition := int32(2)
		mi := &workloadv1alpha1.ModelServing{
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

		shouldUpdate := SetCondition(mi, progressingGroups, updatedGroups, currentGroups)
		assert.True(t, shouldUpdate)
		assert.Len(t, mi.Status.Conditions, 1)
		cond := mi.Status.Conditions[0]
		assert.Equal(t, string(workloadv1alpha1.ModelServingProgressing), cond.Type)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Contains(t, cond.Message, SomeGroupsAreProgressing)
	})
}

func TestGetRollingUpdateConfiguration(t *testing.T) {
	tests := []struct {
		name                   string
		modelServing           *workloadv1alpha1.ModelServing
		expectedPartition      int
		expectedMaxUnavailable int
		expectedMaxSurge       int
	}{
		{
			name: "Nil rollout strategy",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](5),
					Template: workloadv1alpha1.ServingGroup{},
				},
			},
			expectedPartition:      0,
			expectedMaxUnavailable: 1,
			expectedMaxSurge:       0,
		},
		{
			name: "Nil rolling update configuration",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](5),
					Template: workloadv1alpha1.ServingGroup{},
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
					},
				},
			},
			expectedPartition:      0,
			expectedMaxUnavailable: 1,
			expectedMaxSurge:       0,
		},
		{
			name: "All values set with integers",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](10),
					Template: workloadv1alpha1.ServingGroup{},
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							Partition:      ptr.To[int32](3),
							MaxUnavailable: intstr.FromInt(2),
							MaxSurge:       intstr.FromInt(1),
						},
					},
				},
			},
			expectedPartition:      3,
			expectedMaxUnavailable: 2,
			expectedMaxSurge:       1,
		},
		{
			name: "Percentage values",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](10),
					Template: workloadv1alpha1.ServingGroup{},
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							Partition:      ptr.To[int32](2),
							MaxUnavailable: intstr.FromString("20%"),
							MaxSurge:       intstr.FromString("10%"),
						},
					},
				},
			},
			expectedPartition:      2,
			expectedMaxUnavailable: 2, // 20% of 10
			expectedMaxSurge:       1, // 10% of 10
		},
		{
			name: "Zero partition with percentages",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](5),
					Template: workloadv1alpha1.ServingGroup{},
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: intstr.FromString("50%"),
							MaxSurge:       intstr.FromString("30%"),
						},
					},
				},
			},
			expectedPartition:      0,
			expectedMaxUnavailable: 2, // 50% of 5 = 2.5, rounded down
			expectedMaxSurge:       2, // 30% of 5 = 1.5, rounded up
		},
		{
			name: "Large replica count with percentages",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](100),
					Template: workloadv1alpha1.ServingGroup{},
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							Partition:      ptr.To[int32](10),
							MaxUnavailable: intstr.FromString("15%"),
							MaxSurge:       intstr.FromString("5%"),
						},
					},
				},
			},
			expectedPartition:      10,
			expectedMaxUnavailable: 15, // 15% of 100
			expectedMaxSurge:       5,  // 5% of 100
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			partition, maxUnavailable, maxSurge := GetRollingUpdateConfiguration(tt.modelServing)

			assert.Equal(t, tt.expectedPartition, partition, "Partition should match")
			assert.Equal(t, tt.expectedMaxUnavailable, maxUnavailable, "MaxUnavailable should match")
			assert.Equal(t, tt.expectedMaxSurge, maxSurge, "MaxSurge should match")
		})
	}
}
