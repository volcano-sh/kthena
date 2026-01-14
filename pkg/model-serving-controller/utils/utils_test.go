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
