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

package podgroupmanager

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanoclient "volcano.sh/apis/pkg/client/clientset/versioned"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// Manager manages PodGroups for gang scheduling
type Manager struct {
	kubeClient        kubernetes.Interface
	volcanoClient     volcanoclient.Interface
	store             datastore.Store
	hasPodGroupCRD    atomic.Bool
	hasSubGroupPolicy atomic.Bool

	CrdInformer cache.SharedIndexInformer
}

// NewManager creates a new gang scheduling manager
func NewManager(kubeClient kubernetes.Interface, volcanoClient volcanoclient.Interface, apiextClient apiextclient.Interface, store datastore.Store) *Manager {
	newManager := Manager{
		kubeClient:    kubeClient,
		volcanoClient: volcanoClient,
		store:         store,
	}

	newManager.hasPodGroupCRD.Store(false)
	newManager.hasSubGroupPolicy.Store(false)

	// init the hasPodGroupCRD and hasSubGroupPolicy values
	crd, err := apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.TODO(),
		"podgroups.scheduling.volcano.sh",
		metav1.GetOptions{},
	)

	if err != nil {
		if apierrors.IsNotFound(err) {
			newManager.hasPodGroupCRD.Store(false)
			// If PodGroup CRD is not found, we can safely assume that
			// gang scheduling is not supported.
			newManager.hasSubGroupPolicy.Store(false)
		} else {
			klog.Errorf("failed to get PodGroup CRD: %v", err)
			return nil
		}
	} else {
		newManager.hasPodGroupCRD.Store(true)
		klog.Info("[CRD Added] PodGroup CRD detected")
		if podGroupCRDHasSubGroup(crd) {
			klog.Info("[CRD Added] PodGroup CRD has subGroupPolicy")
			newManager.hasSubGroupPolicy.Store(true)
		} else {
			klog.Info("[CRD Added] PodGroup CRD does not have subGroupPolicy")
			newManager.hasSubGroupPolicy.Store(false)
		}
	}

	// Set up informer to watch for PodGroup CRD changes
	factory := apiextinformers.NewSharedInformerFactory(apiextClient, 0)
	crdInformer := factory.Apiextensions().V1().CustomResourceDefinitions().Informer()
	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			crd := obj.(*apiextv1.CustomResourceDefinition)
			if crd.Name == "podgroups.scheduling.volcano.sh" {
				klog.Info("[CRD Added] PodGroup CRD detected")
				newManager.hasPodGroupCRD.Store(true)
				if podGroupCRDHasSubGroup(crd) {
					klog.Info("[CRD Added] PodGroup CRD has subGroupPolicy feature")
					newManager.hasSubGroupPolicy.Store(true)
				} else {
					klog.Info("[CRD Added] PodGroup CRD does not have subGroupPolicy feature")
					newManager.hasSubGroupPolicy.Store(false)
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newCrd, ok := newObj.(*apiextv1.CustomResourceDefinition)
			if !ok {
				klog.Error("failed to parse newCrd type when update CustomResourceDefinition")
				return
			}
			_, ok = oldObj.(*apiextv1.CustomResourceDefinition)
			if !ok {
				klog.Error("failed to parse curCrd type when update CustomResourceDefinition")
				return
			}

			if newCrd.Name != "podgroups.scheduling.volcano.sh" {
				return
			}

			newManager.hasPodGroupCRD.Store(true)

			if podGroupCRDHasSubGroup(newCrd) {
				klog.Info("[CRD Updated] PodGroup CRD has subGroupPolicy feature")
				newManager.hasSubGroupPolicy.Store(true)
			} else {
				klog.Info("[CRD Updated] PodGroup CRD does not have subGroupPolicy feature")
				newManager.hasSubGroupPolicy.Store(false)
			}
		},
		DeleteFunc: func(obj interface{}) {
			crd := obj.(*apiextv1.CustomResourceDefinition)
			if crd.Name == "podgroups.scheduling.volcano.sh" {
				klog.Info("[CRD Deleted] PodGroup CRD removed")
				newManager.hasPodGroupCRD.Store(false)
				newManager.hasSubGroupPolicy.Store(false)
			}
		},
	})

	newManager.CrdInformer = crdInformer

	return &newManager
}

// CreateOrUpdatePodGroup creates a PodGroup for the given ServingGroup if it doesn't exist,
// or updates it if it does.
func (m *Manager) CreateOrUpdatePodGroup(ctx context.Context, mi *workloadv1alpha1.ModelServing, pgName string) error {
	if !m.shouldCreatePodGroup(mi) {
		return nil
	}

	podGroup, err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Get(ctx, pgName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get PodGroup %s: %v", pgName, err)
		}
		return m.createPodGroup(ctx, mi, pgName)
	}

	return m.updatePodGroupIfNeeded(ctx, podGroup, mi)
}

// shouldCreatePodGroup checks if gang scheduling or networkTopology scheduling is enabled for the ModelServing.
// These advanced scheduling features are only effective when used with the "volcano" scheduler.
func (m *Manager) shouldCreatePodGroup(mi *workloadv1alpha1.ModelServing) bool {
	// If PodGroup CRD is not present, gang scheduling is not supported.
	if !m.hasPodGroupCRD.Load() {
		return false
	}

	schedulerName := mi.Spec.SchedulerName
	// If schedulerName is empty, Kubernetes uses the default scheduler, which doesn't support gang/network topology.
	isVolcano := schedulerName == "volcano"

	hasGangOrTopology := mi.Spec.Template.GangPolicy != nil || mi.Spec.Template.NetworkTopology != nil

	return isVolcano && hasGangOrTopology
}

// createPodGroup creates a PodGroup for group-level gang scheduling
func (m *Manager) createPodGroup(ctx context.Context, mi *workloadv1alpha1.ModelServing, podGroupName string) error {
	// Calculate total pods and resources for this ServingGroup
	// minMember: total pods across all roles
	// minRoleMember: map of roleName to number of pods in that role
	// minTaskMember: map of taskName to number of pods in that task
	// minResources: aggregated resource requirements of all pods in the ServingGroup
	minMember, minRoleMember, minTaskMember, minResources := m.calculateRequirements(mi, podGroupName)

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
			MinMember:    int32(minMember),
			MinResources: &minResources,
		},
	}

	if mi.Spec.Template.NetworkTopology != nil {
		// set NetworkTopology if configured in ModelServing
		if mi.Spec.Template.NetworkTopology.GroupPolicy != nil {
			podGroup.Spec.NetworkTopology = mi.Spec.Template.NetworkTopology.GroupPolicy
		}
	}

	if m.hasSubGroupPolicy.Load() {
		podGroup = appendSubGroupPolicy(mi, podGroup, minRoleMember)
	} else {
		podGroup.Spec.MinTaskMember = minTaskMember
	}

	_, err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Create(ctx, podGroup, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	klog.V(2).Infof("Created PodGroup %s for group-level gang scheduling", podGroupName)
	return nil
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
func (m *Manager) calculateRequirements(mi *workloadv1alpha1.ModelServing, podGroupName string) (int, map[string]int32, map[string]int32, corev1.ResourceList) {
	minMember := 0
	minRoleMember := make(map[string]int32)
	minTaskMember := make(map[string]int32)
	minResources := corev1.ResourceList{}

	// For role-level, only include roles up to MinRoleReplicas limit
	for _, role := range mi.Spec.Template.Roles {
		roleReplicas := int(*role.Replicas)
		minRoleReplicas := roleReplicas // Default to all replicas

		if mi.Spec.Template.GangPolicy != nil && mi.Spec.Template.GangPolicy.MinRoleReplicas != nil {
			if minReplicas, exists := mi.Spec.Template.GangPolicy.MinRoleReplicas[role.Name]; exists {
				minRoleReplicas = int(minReplicas)
			}
		}

		expectReplicas := min(minRoleReplicas, roleReplicas)
		roleList, err := m.store.GetRoleList(utils.GetNamespaceName(mi), podGroupName, role.Name)
		if err != nil {
			klog.V(2).Infof("Failed to get role list for role %s: %v", role.Name, err)
		}
		// During scaling operations, podGroup does not affect scaling policies.
		// Under the binpack scaling strategy, it is unknown which role replicas will be deleted.
		// Therefore, no action is taken during scaling.
		// PodGroup will updated after the role completes scaling down.
		if len(roleList) > expectReplicas {
			continue
		}

		// When length(roleList) <= expectReplicas, that is, when scaling up or updating.
		// Provide the roleNameList to be updated.
		needHandledRoleNameList := needHandledRoleNameList(expectReplicas, roleList, role.Name)

		// Only include role replicas up to the minimum required
		podsPerTask := 1 + int(role.WorkerReplicas) // entry + workers
		minRoleMember[role.Name] = int32(podsPerTask)
		minMember = minMember + (podsPerTask * expectReplicas)

		// Only include role replicas up to the minimum required
		for _, taskName := range needHandledRoleNameList {
			minTaskMember[taskName] = int32(podsPerTask)
		}

		// Aggregate resources
		m.aggregateResources(&minResources, &role.EntryTemplate.Spec, expectReplicas)
		if role.WorkerTemplate != nil {
			for i := 0; i < int(role.WorkerReplicas); i++ {
				m.aggregateResources(&minResources, &role.WorkerTemplate.Spec, expectReplicas)
			}
		}
	}
	return minMember, minRoleMember, minTaskMember, minResources
}

// aggregateResources aggregates resource requirements from a pod spec
func (m *Manager) aggregateResources(total *corev1.ResourceList, podSpec *corev1.PodSpec, replicas int) {
	if *total == nil {
		*total = corev1.ResourceList{}
	}

	for _, container := range podSpec.Containers {
		for resourceName, quantity := range container.Resources.Requests {
			quantityCopy := quantity.DeepCopy()
			quantityCopy.Mul(int64(replicas))

			if existing, exists := (*total)[resourceName]; exists {
				existing.Add(quantityCopy)
				(*total)[resourceName] = existing
			} else {
				(*total)[resourceName] = quantityCopy
			}
		}
	}
}

// GenerateTaskName generates task name
func (m *Manager) GenerateTaskName(roleName string, roleIndex int) string {
	return fmt.Sprintf("%s-%d", roleName, roleIndex)
}

// getExistingPodGroups gets existing PodGroups for a ModelServing
func (m *Manager) getExistingPodGroups(ctx context.Context, mi *workloadv1alpha1.ModelServing) (map[string]*schedulingv1beta1.PodGroup, error) {
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
	})

	// TODO: optimize by get from the cache
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
	minMember, minRoleMember, minTaskMember, minResources := m.calculateRequirements(mi, existing.GetName())

	updated := existing.DeepCopy()
	updated.Spec.MinMember = int32(minMember)
	updated.Spec.MinResources = &minResources

	// Apply network topology policy
	if m.hasSubGroupPolicy.Load() {
		updated = appendSubGroupPolicy(mi, updated, minRoleMember)
	} else {
		updated.Spec.MinTaskMember = minTaskMember
	}

	if hasPodGroupChanged(existing, updated) {
		_, err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		klog.V(2).Infof("Updated PodGroup %s for group-level gang scheduling", existing.Name)
	}

	return nil
}

func (m *Manager) DeletePodGroup(ctx context.Context, mi *workloadv1alpha1.ModelServing, servingGroupName string) error {
	if err := m.volcanoClient.SchedulingV1beta1().PodGroups(mi.Namespace).Delete(ctx, servingGroupName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
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

// NeedHandledRoleNameList is used in Role scale up scenario to get the roleName list that need scale up.
// Therefore, the default value for `expectedReplicas` is greater than `length(RoleList)`.
// Or the Role update scenario. (This scenario is This scenario is relatively rare. Since it is not permitted to modify an already configured gangPolicy,
// and in practical applications, the workerReplicas within a deployed role are rarely altered.)
func needHandledRoleNameList(expectedReplicas int, existRoleList []datastore.Role, roleName string) []string {
	scaleUpRoleNameList := make([]string, 0)

	maxIndex := -1
	for _, role := range existRoleList {
		_, index := utils.GetParentNameAndOrdinal(role.Name)
		scaleUpRoleNameList = append(scaleUpRoleNameList, role.Name)
		if index > maxIndex {
			maxIndex = index
		}
	}

	toCreate := expectedReplicas - len(scaleUpRoleNameList)
	if toCreate <= 0 {
		return scaleUpRoleNameList
	}

	for i := 0; i < toCreate; i++ {
		newIndex := maxIndex + 1 + i
		scaleUpRoleNameList = append(scaleUpRoleNameList, utils.GenerateRoleID(roleName, newIndex))
	}
	return scaleUpRoleNameList
}

// AnnotatePodWithPodGroup annotates a pod with the appropriate PodGroup information
func (m *Manager) AnnotatePodWithPodGroup(pod *corev1.Pod, mi *workloadv1alpha1.ModelServing, groupName, taskName string) {
	if !m.shouldCreatePodGroup(mi) {
		return
	}

	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	// Add volcano annotation
	pod.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey] = groupName
	pod.Annotations[batchv1alpha1.TaskSpecKey] = taskName
}

func podGroupCRDHasSubGroup(crd *apiextv1.CustomResourceDefinition) bool {
	if crd == nil {
		return false
	}

	for _, version := range crd.Spec.Versions {
		schema := version.Schema
		if schema == nil || schema.OpenAPIV3Schema == nil {
			continue
		}

		specProps, ok := schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			continue
		}

		if _, ok := specProps.Properties["subGroupPolicy"]; ok {
			return true
		}
	}
	return false
}

func hasPodGroupChanged(current, updated *schedulingv1beta1.PodGroup) bool {
	return current.Spec.MinMember != updated.Spec.MinMember ||
		!reflect.DeepEqual(current.Spec.MinResources, updated.Spec.MinResources) ||
		!reflect.DeepEqual(current.Spec.NetworkTopology, updated.Spec.NetworkTopology) ||
		!reflect.DeepEqual(current.Spec.SubGroupPolicy, updated.Spec.SubGroupPolicy)
}

// neededHandlerPodGroupNameList returns the list of PodGroup names that need to be handled
func neededHandledPodGroupNameList(expectedReplicas int, mi *workloadv1alpha1.ModelServing, servingGroupNameList []datastore.ServingGroup) []string {
	// Changes to the PodGroup will not affect Pods that have already been deployed.
	// During binpack scale down, it is unknown which ServingGroup will be deleted.
	// Therefore, return all podGroup names that exist.
	// Deletion of PodGroups is handled when ServingGroups are deleted.
	maxIndex := -1
	nameList := make([]string, 0)
	for _, group := range servingGroupNameList {
		_, index := utils.GetParentNameAndOrdinal(group.Name)
		nameList = append(nameList, group.Name)
		if index > maxIndex {
			maxIndex = index
		}
	}

	toCreate := expectedReplicas - len(nameList)
	// Scale down. After the deletion of the servingGroup is completed, proceed to delete the PodGroup.
	if toCreate <= 0 {
		return nameList
	}

	for i := 0; i < toCreate; i++ {
		newIndex := maxIndex + 1 + i
		nameList = append(nameList, utils.GenerateServingGroupName(mi.GetName(), newIndex))
	}
	return nameList
}

func appendSubGroupPolicy(mi *workloadv1alpha1.ModelServing, podGroup *schedulingv1beta1.PodGroup, minRoleMember map[string]int32) *schedulingv1beta1.PodGroup {
	subGroupPolicy := make([]schedulingv1beta1.SubGroupPolicySpec, 0, len(minRoleMember))
	for _, role := range mi.Spec.Template.Roles {
		roleReplicas := int(*role.Replicas)
		minRoleReplicas := roleReplicas

		if mi.Spec.Template.GangPolicy != nil && mi.Spec.Template.GangPolicy.MinRoleReplicas != nil {
			if minReplicas, exists := mi.Spec.Template.GangPolicy.MinRoleReplicas[role.Name]; exists {
				minRoleReplicas = int(minReplicas)
			}
		}

		minReplicas := min(minRoleReplicas, roleReplicas)
		minSubgroupSize := minRoleMember[role.Name]
		subGroupPolicy = append(subGroupPolicy, schedulingv1beta1.SubGroupPolicySpec{
			Name: role.Name,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
					workloadv1alpha1.RoleLabelKey:             role.Name,
				},
			},
			MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
			SubGroupSize:   &minSubgroupSize,
			MinSubGroups:   ptr.To(int32(minReplicas)),
		})
	}

	if mi.Spec.Template.NetworkTopology != nil {
		// set SubGroupPolicy if configured in ModelServing
		if mi.Spec.Template.NetworkTopology.RolePolicy != nil {
			for i := range subGroupPolicy {
				subGroupPolicy[i].NetworkTopology = mi.Spec.Template.NetworkTopology.RolePolicy
			}
		}
	}
	podGroup.Spec.SubGroupPolicy = subGroupPolicy
	return podGroup
}
