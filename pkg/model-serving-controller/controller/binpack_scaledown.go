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
	"fmt"
	"strconv"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
)

const (
	PodDeletionCostAnnotation = corev1.PodDeletionCost

	// Status priority constants: lower number = higher deletion priority (delete first)
	// Priority levels:
	//   0: Highest deletion priority - unhealthy or terminating states
	//   1: Lowest deletion priority - healthy running state
	//
	// ServingGroup priorities:
	PriorityServingGroupCreating = 0
	PriorityServingGroupDeleting = 0
	PriorityServingGroupScaling  = 0
	PriorityServingGroupNotFound = 0
	PriorityServingGroupRunning  = 1

	// Role priorities:
	PriorityRoleCreating = 0
	PriorityRoleDeleting = 0
	PriorityRoleNotFound = 0
	PriorityRoleRunning  = 1
)

// ServingGroupWithScore stores serving group deletion priority information
// Priority order: Status (primary) > Deletion cost (secondary) > Index (tertiary)
type ServingGroupWithScore struct {
	Name         string
	Priority     int // Status priority from getServingGroupStatusPriority()
	DeletionCost int // Higher cost = more protected (lower deletion priority)
	Index        int
}

// RoleWithScore stores role deletion priority information
// Priority order: Status (primary) > Deletion cost (secondary) > Index (tertiary)
type RoleWithScore struct {
	Name         string
	Priority     int // Status priority from getRoleStatusPriority()
	DeletionCost int // Higher cost = more protected (lower deletion priority)
	Index        int
}

// getServingGroupStatusPriority returns the deletion priority for a serving group status.
// Lower priority value means higher deletion priority (delete first).
func getServingGroupStatusPriority(status datastore.ServingGroupStatus) int {
	switch status {
	case datastore.ServingGroupRunning:
		return PriorityServingGroupRunning
	case datastore.ServingGroupCreating:
		return PriorityServingGroupCreating
	case datastore.ServingGroupDeleting:
		return PriorityServingGroupDeleting
	case datastore.ServingGroupScaling:
		return PriorityServingGroupScaling
	case datastore.ServingGroupNotFound:
		return PriorityServingGroupNotFound
	default:
		// Unknown status - treat as highest deletion priority (safety first)
		return 0
	}
}

// getRoleStatusPriority returns the deletion priority for a role status.
// Lower priority value means higher deletion priority (delete first).
func getRoleStatusPriority(status datastore.RoleStatus) int {
	switch status {
	case datastore.RoleRunning:
		return PriorityRoleRunning
	case datastore.RoleCreating:
		return PriorityRoleCreating
	case datastore.RoleDeleting:
		return PriorityRoleDeleting
	case datastore.RoleNotFound:
		return PriorityRoleNotFound
	default:
		// Unknown status - treat as highest deletion priority (safety first)
		return 0
	}
}

// getPodDeletionCost retrieves the pod deletion cost from the pod's annotations.
func (c *ModelServingController) getPodDeletionCost(pod *corev1.Pod) int {
	if costStr, exists := pod.Annotations[PodDeletionCostAnnotation]; exists {
		if cost, err := strconv.Atoi(costStr); err == nil {
			return cost
		}
	}
	// The implicit deletion cost for pods that don't set the annotation is 0.
	return 0
}

// calculateRoleScore calculates priority information for role scale-down that considers:
// 1. Role readiness (primary factor): Running vs not-running
// 2. Pod deletion cost (secondary factor)
func (c *ModelServingController) calculateRoleScore(ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) RoleWithScore {
	// Get role status from store
	roleStatus := c.store.GetRoleStatus(utils.GetNamespaceName(ms), groupName, roleName, roleID)
	priority := getRoleStatusPriority(roleStatus)

	// Get pod deletion cost as secondary factor
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID)
	pods, err := c.getPodsByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		_, index := utils.GetParentNameAndOrdinal(roleID)
		return RoleWithScore{
			Name:         roleID,
			Priority:     priority,
			DeletionCost: 0, // Default to 0 on error
			Index:        index,
		}
	}

	// Sum pod deletion costs (0 if not set)
	deletionCost := 0
	for _, pod := range pods {
		deletionCost += c.getPodDeletionCost(pod)
	}

	_, index := utils.GetParentNameAndOrdinal(roleID)

	return RoleWithScore{
		Name:         roleID,
		Priority:     priority,
		DeletionCost: deletionCost,
		Index:        index,
	}
}

// calculateServingGroupScore calculates priority information for serving group scale-down with two-level sorting:
// 1. Status readiness (primary): Not-ready groups should be deleted first
// 2. Pod deletion cost (secondary): Among groups with same status, lower cost = delete first
func (c *ModelServingController) calculateServingGroupScore(ms *workloadv1alpha1.ModelServing, groupName string) ServingGroupWithScore {
	// Get serving group status from store
	groupStatus := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), groupName)
	priority := getServingGroupStatusPriority(groupStatus)

	// Get pod deletion cost as secondary factor
	groupNameValue := fmt.Sprintf("%s/%s", ms.Namespace, groupName)
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		_, index := utils.GetParentNameAndOrdinal(groupName)
		return ServingGroupWithScore{
			Name:         groupName,
			Priority:     priority,
			DeletionCost: 0, // Default to 0 on error
			Index:        index,
		}
	}

	// Sum pod deletion costs (0 if not set)
	deletionCost := 0
	for _, pod := range pods {
		deletionCost += c.getPodDeletionCost(pod)
	}

	_, index := utils.GetParentNameAndOrdinal(groupName)

	return ServingGroupWithScore{
		Name:         groupName,
		Priority:     priority,
		DeletionCost: deletionCost,
		Index:        index,
	}
}
