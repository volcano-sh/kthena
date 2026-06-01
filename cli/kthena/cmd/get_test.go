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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestGetModelServingStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   string
	}{
		{
			name:       "NoConditions",
			conditions: []metav1.Condition{},
			expected:   "Unknown",
		},
		{
			name: "Available",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingAvailable),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Available",
		},
		{
			name: "Progressing",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingProgressing),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Progressing",
		},
		{
			name: "Updating",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingUpdateInProgress),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Updating",
		},
		{
			name: "AvailableTakesPriorityOverProgressing",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingProgressing),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelServingAvailable),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Available",
		},
		{
			name: "UpdateInProgressTakesPriorityOverProgressing",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingUpdateInProgress),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelServingProgressing),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Updating",
		},
		{
			name: "ConditionFalseIgnored",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelServingAvailable),
					Status: metav1.ConditionFalse,
				},
			},
			expected: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getModelServingStatus(tt.conditions)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetModelBoosterStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   string
	}{
		{
			name:       "NoConditions",
			conditions: []metav1.Condition{},
			expected:   "Unknown",
		},
		{
			name: "Active",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Active",
		},
		{
			name: "Failed",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeFailed),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Failed",
		},
		{
			name: "Initialized",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeInitialized),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Initialized",
		},
		{
			name: "FailedTakesPriorityOverActive",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelStatusConditionTypeFailed),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Failed",
		},
		{
			name: "ActiveTakesPriorityOverInitialized",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeInitialized),
					Status: metav1.ConditionTrue,
				},
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionTrue,
				},
			},
			expected: "Active",
		},
		{
			name: "ConditionFalseIgnored",
			conditions: []metav1.Condition{
				{
					Type:   string(workload.ModelStatusConditionTypeActive),
					Status: metav1.ConditionFalse,
				},
			},
			expected: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getModelBoosterStatus(tt.conditions)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetAutoscalingPolicyBindingTarget(t *testing.T) {
	tests := []struct {
		name     string
		binding  workload.AutoscalingPolicyBinding
		expected string
	}{
		{
			name:     "NoTarget",
			binding:  workload.AutoscalingPolicyBinding{},
			expected: "<none>",
		},
		{
			name: "HomogeneousTarget",
			binding: workload.AutoscalingPolicyBinding{
				Spec: workload.AutoscalingPolicyBindingSpec{
					HomogeneousTarget: &workload.HomogeneousTarget{
						Target: workload.Target{
							TargetRef: corev1.ObjectReference{Name: "my-serving"},
						},
					},
				},
			},
			expected: "my-serving",
		},
		{
			name: "HeterogeneousTarget",
			binding: workload.AutoscalingPolicyBinding{
				Spec: workload.AutoscalingPolicyBindingSpec{
					HeterogeneousTarget: &workload.HeterogeneousTarget{
						Params: []workload.HeterogeneousTargetParam{
							{Target: workload.Target{TargetRef: corev1.ObjectReference{Name: "serving-a"}}},
							{Target: workload.Target{TargetRef: corev1.ObjectReference{Name: "serving-b"}}},
						},
					},
				},
			},
			expected: "serving-a,serving-b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, getAutoscalingPolicyBindingTarget(tt.binding))
		})
	}
}

func TestGetAutoscalingPolicyBindingMinMax(t *testing.T) {
	tests := []struct {
		name        string
		binding     workload.AutoscalingPolicyBinding
		expectedMin string
		expectedMax string
	}{
		{
			name:        "NoTarget",
			binding:     workload.AutoscalingPolicyBinding{},
			expectedMin: "-",
			expectedMax: "-",
		},
		{
			name: "HomogeneousTarget",
			binding: workload.AutoscalingPolicyBinding{
				Spec: workload.AutoscalingPolicyBindingSpec{
					HomogeneousTarget: &workload.HomogeneousTarget{
						MinReplicas: 2,
						MaxReplicas: 10,
					},
				},
			},
			expectedMin: "2",
			expectedMax: "10",
		},
		{
			name: "HeterogeneousTarget",
			binding: workload.AutoscalingPolicyBinding{
				Spec: workload.AutoscalingPolicyBindingSpec{
					HeterogeneousTarget: &workload.HeterogeneousTarget{},
				},
			},
			expectedMin: "-",
			expectedMax: "-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			min, max := getAutoscalingPolicyBindingMinMax(tt.binding)
			assert.Equal(t, tt.expectedMin, min)
			assert.Equal(t, tt.expectedMax, max)
		})
	}
}
