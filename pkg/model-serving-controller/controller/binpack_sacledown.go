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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	PodDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
)

// ServingGroupWithScore stores serving group name and its score
type ServingGroupWithScore struct {
	Name  string
	Score int
	Index int
}

// RoleWithScore stores role name and its score
type RoleWithScore struct {
	Name  string
	Score int
	Index int
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

// calculateRoleScore calculates the total deletion cost for all pods in a given role.
func (c *ModelServingController) calculateRoleScore(mi *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) (int, error) {
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", mi.Namespace, groupName, roleName, roleID)
	pods, err := c.getPodsByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		return 0, fmt.Errorf("failed to get pods for role %s: %v", roleID, err)
	}

	score := 0
	for _, pod := range pods {
		score += c.getPodDeletionCost(pod)
	}

	return score, nil
}

// calculateServingGroupScore calculates the total deletion cost for all pods in a given serving group.
func (c *ModelServingController) calculateServingGroupScore(mi *workloadv1alpha1.ModelServing, groupName string) (int, error) {
	groupNameValue := fmt.Sprintf("%s/%s", mi.Namespace, groupName)
	pods, err := c.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		return 0, fmt.Errorf("failed to get pods for serving group %s: %v", groupName, err)
	}

	score := 0
	for _, pod := range pods {
		score += c.getPodDeletionCost(pod)
	}

	return score, nil
}

// shouldUseBinPackScaleDown checks if binpack scale down should be used.
// Returns true if any pod under the ModelServing has the PodDeletionCostAnnotation.
func (c *ModelServingController) shouldUseBinPackScaleDown(mi *workloadv1alpha1.ModelServing) (bool, error) {
	// Get all pods belonging to this ModelServing
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
	})

	pods, err := c.podsLister.Pods(mi.Namespace).List(selector)
	if err != nil {
		return false, fmt.Errorf("failed to list pods for ModelServing %s/%s: %v", mi.Namespace, mi.Name, err)
	}

	// Check if any pod has the PodDeletionCostAnnotation
	for _, pod := range pods {
		if _, exists := pod.Annotations[PodDeletionCostAnnotation]; exists {
			return true, nil
		}
	}

	return false, nil
}
