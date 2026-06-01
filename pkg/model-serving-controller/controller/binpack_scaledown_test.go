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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This block tests that each serving group status maps to the correct deletion priority value.
func TestGetServingGroupStatusPriority(t *testing.T) {
	tests := []struct {
		name     string
		status   datastore.ServingGroupStatus
		expected int
	}{
		{
			name:     "Running has lowest deletion priority",
			status:   datastore.ServingGroupRunning,
			expected: PriorityServingGroupRunning,
		},
		{
			name:     "Creating has highest deletion priority",
			status:   datastore.ServingGroupCreating,
			expected: PriorityServingGroupCreating,
		},
		{
			name:     "Deleting has highest deletion priority",
			status:   datastore.ServingGroupDeleting,
			expected: PriorityServingGroupDeleting,
		},
		{
			name:     "Scaling has highest deletion priority",
			status:   datastore.ServingGroupScaling,
			expected: PriorityServingGroupScaling,
		},
		{
			name:     "NotFound has highest deletion priority",
			status:   datastore.ServingGroupNotFound,
			expected: PriorityServingGroupNotFound,
		},
		{
			name:     "Unknown status defaults to highest deletion priority",
			status:   datastore.ServingGroupStatus("unknown"),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getServingGroupStatusPriority(tt.status)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// This block tests that each role status maps to the correct deletion priority value.
func TestGetRoleStatusPriority(t *testing.T) {
	tests := []struct {
		name     string
		status   datastore.RoleStatus
		expected int
	}{
		{
			name:     "Running has lowest deletion priority",
			status:   datastore.RoleRunning,
			expected: PriorityRoleRunning,
		},
		{
			name:     "Creating has highest deletion priority",
			status:   datastore.RoleCreating,
			expected: PriorityRoleCreating,
		},
		{
			name:     "Deleting has highest deletion priority",
			status:   datastore.RoleDeleting,
			expected: PriorityRoleDeleting,
		},
		{
			name:     "NotFound has highest deletion priority",
			status:   datastore.RoleNotFound,
			expected: PriorityRoleNotFound,
		},
		{
			name:     "Unknown status defaults to highest deletion priority",
			status:   datastore.RoleStatus("unknown"),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRoleStatusPriority(tt.status)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Tests that getPodDeletionCost correctly reads the annotation value,
// handles missing/invalid annotations, and defaults to 0.
func TestGetPodDeletionCost(t *testing.T) {
	c := &ModelServingController{}

	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected int
	}{
		{
			name: "pod with valid deletion cost annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						PodDeletionCostAnnotation: "100",
					},
				},
			},
			expected: 100,
		},
		{
			name: "pod with negative deletion cost",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						PodDeletionCostAnnotation: "-50",
					},
				},
			},
			expected: -50,
		},
		{
			name: "pod with no annotations defaults to 0",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expected: 0,
		},
		{
			name: "pod with invalid annotation value defaults to 0",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						PodDeletionCostAnnotation: "invalid",
					},
				},
			},
			expected: 0,
		},
		{
			name: "pod with zero deletion cost",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						PodDeletionCostAnnotation: "0",
					},
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.getPodDeletionCost(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Tests that Running serving groups are always protected from deletion
// compared to all other statuses (Creating, Deleting, Scaling, NotFound).
func TestRunningStatusHasLowerDeletionPriorityThanOthers(t *testing.T) {
	runningPriority := getServingGroupStatusPriority(datastore.ServingGroupRunning)

	nonRunningStatuses := []datastore.ServingGroupStatus{
		datastore.ServingGroupCreating,
		datastore.ServingGroupDeleting,
		datastore.ServingGroupScaling,
		datastore.ServingGroupNotFound,
	}

	for _, status := range nonRunningStatuses {
		priority := getServingGroupStatusPriority(status)
		assert.Greater(t, runningPriority, priority,
			"Running (%d) should have higher priority value than %s (%d)", runningPriority, status, priority)
	}
}

// tests that Running roles are always protected from deletion
// compared to all other statuses (Creating, Deleting, NotFound).
func TestRoleRunningStatusHasLowerDeletionPriorityThanOthers(t *testing.T) {
	runningPriority := getRoleStatusPriority(datastore.RoleRunning)

	nonRunningStatuses := []datastore.RoleStatus{
		datastore.RoleCreating,
		datastore.RoleDeleting,
		datastore.RoleNotFound,
	}

	for _, status := range nonRunningStatuses {
		priority := getRoleStatusPriority(status)
		assert.Greater(t, runningPriority, priority,
			"Running (%d) should have higher priority value than %s (%d)", runningPriority, status, priority)
	}
}
