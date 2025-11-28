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

package gangscheduling

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanoclient "volcano.sh/apis/pkg/client/clientset/versioned"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Manager manages PodGroups for gang scheduling
type Manager struct {
	kubeClient    kubernetes.Interface
	volcanoClient volcanoclient.Interface
}

// NewManager creates a new gang scheduling manager
func NewManager(kubeClient kubernetes.Interface, volcanoClient volcanoclient.Interface) Manager {
	return Manager{
		kubeClient:    kubeClient,
		volcanoClient: volcanoClient,
	}
}

// ManagePodGroups manages PodGroups for a ModelServing instance
func (m *Manager) ManagePodGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing) error {
	if m.isSchedulingEnabled(mi) {
		return m.managePodGroups(ctx, mi)
	}
	return nil
}

// isSchedulingEnabled checks if gang scheduling or networkTopology scheduling is enabled for the ModelServing.
// These advanced scheduling features are only effective when used with the "volcano" scheduler.
func (m *Manager) isSchedulingEnabled(mi *workloadv1alpha1.ModelServing) bool {
	schedulerName := mi.Spec.SchedulerName
	// If schedulerName is empty, Kubernetes uses the default scheduler, which doesn't support gang/network topology.
	isVolcano := schedulerName == "volcano"

	hasGangOrTopology := mi.Spec.Template.GangPolicy != nil || mi.Spec.Template.NetworkTopology != nil

	return isVolcano && hasGangOrTopology
}

// managePodGroups manages PodGroups for group-level gang scheduling
func (m *Manager) managePodGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing) error {
	expectedReplicas := int(*mi.Spec.Replicas)

	// Get existing PodGroups
	existingPodGroups, err := m.getExistingPodGroups(ctx, mi)
	if err != nil {
		return fmt.Errorf("failed to get existing PodGroups: %v", err)
	}

	// Create or update PodGroups for each ServingGroup
	for i := 0; i < expectedReplicas; i++ {
		podGroupName := m.generatePodGroupName(mi.Name, i)

		if existingPG, exists := existingPodGroups[podGroupName]; exists {
			// Update existing PodGroup if needed
			if err := m.updatePodGroupIfNeeded(ctx, existingPG, mi); err != nil {
				return fmt.Errorf("failed to update PodGroup %s: %v", podGroupName, err)
			}
		} else {
			// Create new PodGroup
			if err := m.createPodGroup(ctx, mi, i); err != nil {
				return fmt.Errorf("failed to create PodGroup %s: %v", podGroupName, err)
			}
		}
	}

	// Clean up excess PodGroups
	return m.cleanupExcessPodGroups(ctx, mi, existingPodGroups, expectedReplicas)
}

// createPodGroup creates a PodGroup for group-level gang scheduling
func (m *Manager) createPodGroup(ctx context.Context, mi *workloadv1alpha1.ModelServing, groupIndex int) error {
	podGroupName := m.generatePodGroupName(mi.Name, groupIndex)

	// Calculate total pods and resources for this ServingGroup
	minMember, minTaskMember, minResources := m.calculateRequirements(mi)

	podGroup := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podGroupName,
			Namespace: mi.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
				workloadv1alpha1.GroupNameLabelKey:        podGroupName,
			},
			Annotations: map[string]string{
				schedulingv1beta1.KubeGroupNameAnnotationKey: podGroupName,
			},
			OwnerReferences: m.buildOwnerReference(mi),
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			MinMember:     int32(minMember),
			MinTaskMember: minTaskMember,
			MinResources:  &minResources,
		},
	}

	podGroup = m.appendNetworkTopologyPolicy(mi, podGroup)

	_, err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Create(ctx, podGroup, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	klog.V(2).Infof("Created PodGroup %s for group-level gang scheduling", podGroupName)
	return nil
}

func (m *Manager) appendNetworkTopologyPolicy(mi *workloadv1alpha1.ModelServing, podGroup *schedulingv1beta1.PodGroup) *schedulingv1beta1.PodGroup {
	if mi.Spec.Template.NetworkTopology != nil {
		// set NetworkTopology if configured in ModelServing
		if mi.Spec.Template.NetworkTopology.GroupPolicy != nil {
			podGroup.Spec.NetworkTopology = mi.Spec.Template.NetworkTopology.GroupPolicy
		}

		// set SubGroupPolicy if configured in ModelServing
		if mi.Spec.Template.NetworkTopology.RolePolicy != nil {
			podGroup.Spec.SubGroupPolicy = []schedulingv1beta1.SubGroupPolicySpec{
				{
					Name:            podGroup.GetName(),
					NetworkTopology: mi.Spec.Template.NetworkTopology.RolePolicy,
					MatchPolicy: []schedulingv1beta1.MatchPolicySpec{
						{
							LabelKey: workloadv1alpha1.RoleLabelKey,
						},
						{
							LabelKey: workloadv1alpha1.RoleIDKey,
						},
					},
				},
			}
		}
	}
	return podGroup
}

// To build ownerReferences of PodGroup
func (m *Manager) buildOwnerReference(mi *workloadv1alpha1.ModelServing) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{
			APIVersion: workloadv1alpha1.GroupVersion.String(),
			Kind:       workloadv1alpha1.ModelServingKind.Kind,
			Name:       mi.Name,
			UID:        mi.UID,
			Controller: ptr.To(true),
		},
	}
}

// calculateRequirements calculates requirements for role-level gang scheduling
func (m *Manager) calculateRequirements(mi *workloadv1alpha1.ModelServing) (int, map[string]int32, corev1.ResourceList) {
	minMember := 0
	minTaskMember := make(map[string]int32)
	minResources := corev1.ResourceList{}

	// For role-level, only include roles up to MinRoleReplicas limit
	for _, role := range mi.Spec.Template.Roles {
		roleReplicas := int(*role.Replicas)
		minRoleReplicas := roleReplicas // Default to all replicas

		if mi.Spec.Template.GangPolicy.MinRoleReplicas != nil {
			if minReplicas, exists := mi.Spec.Template.GangPolicy.MinRoleReplicas[role.Name]; exists {
				minRoleReplicas = int(minReplicas)
			}
		}

		// Only include role replicas up to the minimum required
		for roleIndex := 0; roleIndex < minRoleReplicas && roleIndex < roleReplicas; roleIndex++ {
			taskName := m.GenerateTaskName(role.Name, roleIndex)
			podsPerTask := 1 + int(role.WorkerReplicas) // entry + workers
			minTaskMember[taskName] = int32(podsPerTask)
			minMember += podsPerTask

			// Aggregate resources
			m.aggregateResources(&minResources, &role.EntryTemplate.Spec)
			if role.WorkerTemplate != nil {
				for i := 0; i < int(role.WorkerReplicas); i++ {
					m.aggregateResources(&minResources, &role.WorkerTemplate.Spec)
				}
			}
		}
	}

	return minMember, minTaskMember, minResources
}

// aggregateResources aggregates resource requirements from a pod spec
func (m *Manager) aggregateResources(total *corev1.ResourceList, podSpec *corev1.PodSpec) {
	if *total == nil {
		*total = corev1.ResourceList{}
	}

	for _, container := range podSpec.Containers {
		for resourceName, quantity := range container.Resources.Requests {
			if existing, exists := (*total)[resourceName]; exists {
				existing.Add(quantity)
				(*total)[resourceName] = existing
			} else {
				(*total)[resourceName] = quantity.DeepCopy()
			}
		}
	}
}

// generatePodGroupName generates PodGroup name for group-level scheduling
func (m *Manager) generatePodGroupName(modelServingName string, groupIndex int) string {
	return fmt.Sprintf("%s-%d", modelServingName, groupIndex)
}

// GenerateTaskName generates task name for MinTaskMember
func (m *Manager) GenerateTaskName(roleName string, roleIndex int) string {
	return fmt.Sprintf("%s-%d", roleName, roleIndex)
}

// getExistingPodGroups gets existing PodGroups for a ModelServing
func (m *Manager) getExistingPodGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing) (map[string]*schedulingv1beta1.PodGroup, error) {
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
	})

	podGroupList, err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, err
	}

	result := make(map[string]*schedulingv1beta1.PodGroup)
	for i := range podGroupList.Items {
		pg := &podGroupList.Items[i]
		result[pg.Name] = pg
	}

	return result, nil
}

// updatePodGroupIfNeeded updates a PodGroup if needed for group-level scheduling
func (m *Manager) updatePodGroupIfNeeded(ctx context.Context, existing *schedulingv1beta1.PodGroup, mi *workloadv1alpha1.ModelServing) error {
	// Calculate current requirements
	minMember, minTaskMember, minResources := m.calculateRequirements(mi)

	needsUpdate := false
	updated := existing.DeepCopy()

	// Check if MinMember needs update
	if updated.Spec.MinMember != int32(minMember) {
		updated.Spec.MinMember = int32(minMember)
		needsUpdate = true
	}

	// Check if MinTaskMember needs update
	if !equalMinTaskMember(updated.Spec.MinTaskMember, minTaskMember) {
		updated.Spec.MinTaskMember = minTaskMember
		needsUpdate = true
	}

	// Check if MinResources needs update
	if !equalResourceList(updated.Spec.MinResources, &minResources) {
		updated.Spec.MinResources = &minResources
		needsUpdate = true
	}

	// Check if NetworkTopology needs update
	// Handle the case where `mi.Spec.Template.NetworkTopology == nil` to avoid a null pointer panic.
	if mi.Spec.Template.NetworkTopology == nil {
		if updated.Spec.NetworkTopology != nil {
			needsUpdate = true
		}
		if len(updated.Spec.SubGroupPolicy) > 0 {
			needsUpdate = true
		}
	} else {
		if !equalVolcanoNetworkTopology(updated.Spec.NetworkTopology, mi.Spec.Template.NetworkTopology.GroupPolicy) {
			updated.Spec.NetworkTopology = mi.Spec.Template.NetworkTopology.GroupPolicy
			needsUpdate = true
		}

		if !equalSubGroupNetworkTopology(updated.Spec.SubGroupPolicy, mi.Spec.Template.NetworkTopology.RolePolicy) {
			updated = m.appendNetworkTopologyPolicy(mi, updated)
			needsUpdate = true
		}
	}

	if needsUpdate {
		_, err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		klog.V(2).Infof("Updated PodGroup %s for group-level gang scheduling", existing.Name)
	}

	return nil
}

// cleanupExcessPodGroups cleans up excess PodGroups
func (m *Manager) cleanupExcessPodGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing, existingPodGroups map[string]*schedulingv1beta1.PodGroup, expectedReplicas int) error {
	for podGroupName, podGroup := range existingPodGroups {
		// Check if this PodGroup is still needed
		isNeeded := false
		for i := 0; i < expectedReplicas; i++ {
			expectedName := m.generatePodGroupName(mi.Name, i)
			if podGroupName == expectedName {
				isNeeded = true
				break
			}
		}

		if !isNeeded {
			err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Delete(ctx, podGroup.Name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete excess PodGroup %s: %v", podGroup.Name, err)
			}
			klog.V(2).Infof("Deleted excess PodGroup %s", podGroup.Name)
		}
	}

	return nil
}

// cleanupPodGroups cleans up all PodGroups for a ModelServing
func (m *Manager) CleanupPodGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing) error {
	existingPodGroups, err := m.getExistingPodGroups(ctx, mi)
	if err != nil {
		return fmt.Errorf("failed to get existing PodGroups for cleanup: %v", err)
	}

	for _, podGroup := range existingPodGroups {
		err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Delete(ctx, podGroup.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete PodGroup %s: %v", podGroup.Name, err)
		}
		klog.V(2).Infof("Deleted PodGroup %s (gang scheduling disabled)", podGroup.Name)
	}

	return nil
}

// AnnotatePodWithPodGroup annotates a pod with the appropriate PodGroup information
func (m *Manager) AnnotatePodWithPodGroup(pod *corev1.Pod, mi *workloadv1alpha1.ModelServing, minMember int, groupName, taskName string) {
	if !m.isSchedulingEnabled(mi) {
		return
	}

	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	// Add volcano annotation
	pod.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey] = groupName
	pod.Annotations[batchv1alpha1.TaskSpecKey] = taskName
}

// equalMinTaskMember compares two MinTaskMember maps
func equalMinTaskMember(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}

	for key, valueA := range a {
		if valueB, exists := b[key]; !exists || valueA != valueB {
			return false
		}
	}

	return true
}

// equalResourceList compares two ResourceList
func equalResourceList(a, b *corev1.ResourceList) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	aList := *a
	bList := *b

	if len(aList) != len(bList) {
		return false
	}

	for resourceName, quantityA := range aList {
		if quantityB, exists := bList[resourceName]; !exists || !quantityA.Equal(quantityB) {
			return false
		}
	}

	return true
}

// equalVolcanoNetworkTopology compares two volcano NetworkTopologySpec pointers for equality
func equalVolcanoNetworkTopology(a, b *schedulingv1beta1.NetworkTopologySpec) bool {
	// If podGroup and subgroup network topology and modelServing network topology all are nil, they are equal
	if a == nil && b == nil {
		return true
	}

	// If one is nil and the other is not, they are not equal
	if a == nil || b == nil {
		return false
	}

	// Both are non-nil, compare their values
	return a.Mode == b.Mode &&
		a.HighestTierAllowed == b.HighestTierAllowed
}

// equslSubGroupNetworkTopology compares two volcano SubGroupPolicySpec pointers for equality
func equalSubGroupNetworkTopology(a []schedulingv1beta1.SubGroupPolicySpec, b *schedulingv1beta1.NetworkTopologySpec) bool {
	if len(a) == 0 && b == nil {
		return true
	}

	if len(a) == 0 || b == nil {
		return false
	}

	if a[0].MatchPolicy == nil {
		return false
	}

	if len(a[0].MatchPolicy) < 2 || a[0].MatchPolicy[0].LabelKey != workloadv1alpha1.RoleLabelKey ||
		a[0].MatchPolicy[1].LabelKey != workloadv1alpha1.RoleIDKey {
		return false
	}

	// The podGroup.SubGroupPolicy created by modelServing has a length of 1, so only the first element needs to be compared
	return a[0].NetworkTopology.Mode == b.Mode &&
		a[0].NetworkTopology.HighestTierAllowed == b.HighestTierAllowed
}
