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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// finishPendingDeletions retries failed API requests before any new capacity can
// reuse an identity. Services with an accepted DELETE may still have finalizers.
func (c *ModelServingController) finishPendingDeletions(ctx context.Context, ms *workload.ModelServing) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			if err := c.deleteServingGroup(ctx, ms, group.Name); err != nil {
				return err
			}
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for id, role := range instances {
				if role.Status == datastore.RoleDeleting {
					if err := c.DeleteRole(ctx, ms, group.Name, name, id); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// cleanupOrphanServices runs before new requests for this workqueue key, even
// when a surviving Role will fail reconciliation. Restart does not resurrect
// Role identities from Services. Desired slots, current Pods and store facts win.
func (c *ModelServingController) cleanupOrphanServices(ctx context.Context, ms *workload.ModelServing) error {
	services, err := c.kubeClientSet.CoreV1().Services(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{workload.ModelServingNameLabelKey: ms.Name}.String()})
	if err != nil {
		return err
	}
	for i := range services.Items {
		service := &services.Items[i]
		group, role, id, managed := plugins.ManagedHeadlessServiceIdentity(service, ms)
		if !managed || service.DeletionTimestamp != nil {
			continue
		}
		if c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role, id) != datastore.RoleNotFound {
			storeRID, _ := c.store.GetRoleInstanceID(utils.GetNamespaceName(ms), group, role, id)
			svcRID := utils.GetRoleInstanceID(service)
			if storeRID == "" || svcRID == storeRID {
				continue
			}
			// Active Role has a newer instance ID; only delete the stale Service once no Pod of svcRID remains.
			selector := labels.Set{workload.GroupNameLabelKey: group, workload.RoleLabelKey: role, workload.RoleIDKey: id}
			pods, listErr := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
			if listErr != nil {
				return listErr
			}
			inUse := false
			for j := range pods.Items {
				if utils.GetRoleInstanceID(&pods.Items[j]) == svcRID {
					inUse = true
					break
				}
			}
			if inUse {
				continue
			}
			uid := service.UID
			if delErr := c.kubeClientSet.CoreV1().Services(ms.Namespace).Delete(ctx, service.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); delErr != nil && !apierrors.IsNotFound(delErr) {
				return fmt.Errorf("cleanup stale Headless Service %s/%s: %w", ms.Namespace, service.Name, delErr)
			}
			continue
		}
		groupStatus := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group)
		// Also preserve desired slots while cache observation catches up. Orphans
		// outside the desired range (including zero-replica teardown) can be removed.
		_, groupIndex := utils.GetParentNameAndOrdinal(group)
		_, roleIndex := utils.GetParentNameAndOrdinal(id)
		needed := false
		if groupIndex < modelServingReplicas(ms) {
			for _, desired := range ms.Spec.Template.Roles {
				if desired.Name == role && roleIndex < roleReplicas(desired) {
					needed = true
				}
			}
		}
		if needed {
			// Count-based shrink can retain higher ordinals. A vacant low ordinal
			// is not a pending create when stable surviving capacity already fills it.
			full, err := c.stableCapacityFillsOrphanSlot(ctx, ms, group, role)
			if err != nil {
				return err
			}
			if !full {
				continue
			}
		}
		selector := labels.Set{workload.GroupNameLabelKey: group, workload.RoleLabelKey: role, workload.RoleIDKey: id}
		pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return err
		}
		// Even an object with uncertain ownership is reason to wait, not to delete
		// networking while another actor might be reusing this instance.
		if len(pods.Items) != 0 {
			continue
		}
		if c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role, id) != datastore.RoleNotFound {
			continue
		}
		if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group) != groupStatus {
			continue
		}
		uid := service.UID
		err = c.kubeClientSet.CoreV1().Services(ms.Namespace).Delete(ctx, service.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("cleanup orphan Headless Service %s/%s: %w", ms.Namespace, service.Name, err)
		}
	}
	return nil
}

// Only Running store identities with complete, currently Ready API Pods can
// establish that sparse surviving capacity satisfies the desired count. Creating,
// deleting, absent or uncertain units never authorize deleting a reserved slot.
func (c *ModelServingController) stableCapacityFillsOrphanSlot(ctx context.Context, ms *workload.ModelServing, targetGroup, targetRole string) (bool, error) {
	key := utils.GetNamespaceName(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	if err != nil {
		return false, nil // No usable membership observation: preserve the slot.
	}
	list, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{workload.ModelServingNameLabelKey: ms.Name}.String()})
	if err != nil {
		return false, err
	}
	pods := make(map[string]*corev1.Pod, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner != nil && owner.APIVersion == workload.SchemeGroupVersion.String() && owner.Kind == workload.ModelServingKind.Kind && owner.Name == ms.Name && owner.UID == ms.UID && pod.DeletionTimestamp == nil && utils.IsPodRunningAndReady(pod) && !inplace.Updating(pod) {
			pods[pod.Name] = pod
		}
	}
	stableRoles := func(group string, desired workload.Role) int {
		instances, err := c.store.GetRoleList(key, group, desired.Name)
		if err != nil {
			return 0
		}
		count := 0
		for _, instance := range instances {
			if instance.Status != datastore.RoleRunning {
				continue
			}
			complete := true
			for n := 0; n <= int(desired.WorkerReplicas); n++ {
				pod := pods[utils.GeneratePodName(group, instance.Name, n)]
				if pod == nil || pod.Labels[workload.GroupNameLabelKey] != group || pod.Labels[workload.RoleLabelKey] != desired.Name || pod.Labels[workload.RoleIDKey] != instance.Name {
					complete = false
					break
				}
			}
			if complete {
				count++
			}
		}
		return count
	}
	targetStatus := c.store.GetServingGroupStatus(key, targetGroup)
	if targetStatus != datastore.ServingGroupNotFound {
		if targetStatus != datastore.ServingGroupRunning {
			return false, nil
		}
		for _, role := range ms.Spec.Template.Roles {
			if role.Name == targetRole {
				return stableRoles(targetGroup, role) >= roleReplicas(role), nil
			}
		}
		return false, nil
	}
	stableGroups := 0
	for _, group := range groups {
		if group.Status != datastore.ServingGroupRunning {
			continue
		}
		complete, hasMembers := true, false
		for _, role := range ms.Spec.Template.Roles {
			count := stableRoles(group.Name, role)
			hasMembers = hasMembers || count > 0
			complete = complete && count >= roleReplicas(role)
		}
		if complete && hasMembers {
			stableGroups++
		}
	}
	return stableGroups >= modelServingReplicas(ms), nil
}

// deleteInstancePods submits named, UID-fenced deletions for controlled Pods matching matchInstance
// in deterministic order (entry Pod "-0" first, followed by workers) without relying on any
// in-memory deletion target map.
func deleteInstancePods(
	ctx context.Context,
	client kubernetes.Interface,
	ms *workload.ModelServing,
	selector labels.Selector,
	matchInstance func(*corev1.Pod) bool,
) ([]string, error) {
	if client == nil || ms == nil || selector == nil {
		return nil, fmt.Errorf("Pod deletion requires Kubernetes client, ModelServing and selector")
	}
	pods := client.CoreV1().Pods(ms.Namespace)
	existing, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("list Pods for ModelServing %s/%s deletion: %w", ms.Namespace, ms.Name, err)
	}

	var targets []*corev1.Pod
	var replacements []string
	for i := range existing.Items {
		pod := &existing.Items[i]
		if utils.IsControlledByModelServing(pod, ms) && (matchInstance == nil || matchInstance(pod)) {
			targets = append(targets, pod)
		} else if pod.DeletionTimestamp == nil {
			replacements = append(replacements, pod.Name)
		}
	}
	slices.SortFunc(targets, func(a, b *corev1.Pod) int {
		return cmp.Compare(a.Name, b.Name)
	})
	slices.Sort(replacements)

	for _, pod := range targets {
		if pod.DeletionTimestamp != nil {
			continue
		}
		uid := pod.UID
		if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("delete Pod %s/%s (UID %s): %w", ms.Namespace, pod.Name, uid, err)
		}
	}
	return replacements, nil
}

func isPodMarkedDeleting(pod *corev1.Pod) bool {
	return pod != nil && pod.DeletionTimestamp != nil
}

func (c *ModelServingController) resolveGroupInstanceIDForCreate(ctx context.Context, ms *workload.ModelServing, groupName string) (string, error) {
	nsn := utils.GetNamespaceName(ms)
	if id, ok := c.store.GetServingGroupInstanceID(nsn, groupName); ok && id != "" {
		return id, nil
	}
	if c.podsInformer != nil {
		if pods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName)); err == nil {
			for _, pod := range pods {
				if utils.IsControlledByModelServing(pod, ms) && !isPodMarkedDeleting(pod) {
					id := utils.GetGroupInstanceID(pod)
					c.store.EnsureServingGroupInstanceID(nsn, groupName, id)
					return id, nil
				}
			}
		}
	}
	if c.podGroupManager != nil && c.podGroupManager.HasPodGroupCRD() {
		if pgs, err := c.getPodGroupsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName)); err == nil {
			for _, pg := range pgs {
				if pg.Name == groupName && utils.IsControlledByModelServing(pg, ms) && pg.DeletionTimestamp == nil {
					id := utils.GetGroupInstanceID(pg)
					c.store.EnsureServingGroupInstanceID(nsn, groupName, id)
					return id, nil
				}
			}
		}
	}
	if c.kubeClientSet != nil && len(ms.Spec.Plugins) > 0 {
		svcList, err := c.kubeClientSet.CoreV1().Services(ms.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set{
				workload.ModelServingNameLabelKey: ms.Name,
				workload.GroupNameLabelKey:        groupName,
			}.String(),
		})
		if err != nil {
			return "", fmt.Errorf("list Services for ServingGroup %s identity check: %w", groupName, err)
		}
		var managed []*corev1.Service
		gids := make(map[string]struct{})
		for i := range svcList.Items {
			svc := &svcList.Items[i]
			svcGroup, _, _, ok := plugins.ManagedHeadlessServiceIdentity(svc, ms)
			if !ok || svcGroup != groupName {
				continue
			}
			if svc.DeletionTimestamp != nil {
				c.enqueueModelServingAfter(ms, enqueueAfter)
				return "", fmt.Errorf("Headless Service %s/%s in ServingGroup %s is terminating", ms.Namespace, svc.Name, groupName)
			}
			managed = append(managed, svc)
			gids[utils.GetGroupInstanceID(svc)] = struct{}{}
		}
		if len(gids) == 1 {
			for gid := range gids {
				c.store.EnsureServingGroupInstanceID(nsn, groupName, gid)
				return gid, nil
			}
		}
		if len(gids) > 1 {
			// Conflicting surviving Services with no active Pods/PodGroup in cache: verify zero live Pods, then drain stale Services.
			livePods, listErr := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labels.Set{workload.GroupNameLabelKey: groupName}.String(),
			})
			if listErr != nil {
				return "", fmt.Errorf("list Pods for ServingGroup %s identity check: %w", groupName, listErr)
			}
			for i := range livePods.Items {
				pod := &livePods.Items[i]
				if utils.IsControlledByModelServing(pod, ms) && !isPodMarkedDeleting(pod) {
					gid := utils.GetGroupInstanceID(pod)
					c.store.EnsureServingGroupInstanceID(nsn, groupName, gid)
					return gid, nil
				}
			}
			if err := c.deleteStaleManagedServices(ctx, ms, managed); err != nil {
				return "", err
			}
			c.enqueueModelServingAfter(ms, enqueueAfter)
			return "", fmt.Errorf("drained conflicting podless Headless Services for ServingGroup %s; retrying", groupName)
		}
	}
	id := utils.NewInstanceID()
	c.store.EnsureServingGroupInstanceID(nsn, groupName, id)
	return id, nil
}

func (c *ModelServingController) deleteStaleManagedServices(ctx context.Context, ms *workload.ModelServing, services []*corev1.Service) error {
	for _, svc := range services {
		uid := svc.UID
		if err := c.kubeClientSet.CoreV1().Services(ms.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale Headless Service %s/%s: %w", ms.Namespace, svc.Name, err)
		}
	}
	return nil
}

func (c *ModelServingController) resolveInstanceIDsForRoleCreate(
	ctx context.Context,
	ms *workload.ModelServing,
	groupName, roleName, roleID string,
) (string, string, error) {
	nsn := utils.GetNamespaceName(ms)
	groupInstanceID, groupIDSet := podgroupmanager.GroupInstanceIDSpecified(ctx)
	if !groupIDSet {
		if id, ok := c.store.GetServingGroupInstanceID(nsn, groupName); ok && id != "" {
			groupInstanceID, groupIDSet = id, true
		}
	}
	roleInstanceID, roleExists := c.store.GetRoleInstanceID(nsn, groupName, roleName, roleID)

	var activeSlotPod *corev1.Pod
	if c.podsInformer != nil {
		if indexed, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID)); err == nil {
			for _, pod := range indexed {
				if !utils.IsControlledByModelServing(pod, ms) {
					continue
				}
				if pod.DeletionTimestamp != nil {
					if !roleExists || (roleInstanceID != "" && utils.GetRoleInstanceID(pod) != roleInstanceID) {
						c.enqueueModelServingAfter(ms, enqueueAfter)
						return "", "", fmt.Errorf("Role %s/%s in ServingGroup %s still has terminating Pod %s", roleName, roleID, groupName, pod.Name)
					}
					continue
				}
				if activeSlotPod == nil {
					activeSlotPod = pod
				}
			}
		}
	}

	if activeSlotPod == nil && roleInstanceID == "" && c.kubeClientSet != nil {
		slotSelector := labels.Set{
			workload.GroupNameLabelKey: groupName,
			workload.RoleLabelKey:      roleName,
			workload.RoleIDKey:         roleID,
		}
		listed, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: slotSelector.String()})
		if err != nil {
			return "", "", fmt.Errorf("list Pods for Role %s/%s identity check: %w", roleName, roleID, err)
		}
		for i := range listed.Items {
			pod := &listed.Items[i]
			if !utils.IsControlledByModelServing(pod, ms) {
				continue
			}
			if pod.DeletionTimestamp != nil {
				if !roleExists {
					c.enqueueModelServingAfter(ms, enqueueAfter)
					return "", "", fmt.Errorf("Role %s/%s in ServingGroup %s still has terminating Pod %s", roleName, roleID, groupName, pod.Name)
				}
				continue
			}
			if activeSlotPod == nil {
				activeSlotPod = pod
			}
		}
	}

	if activeSlotPod != nil {
		gid := utils.GetGroupInstanceID(activeSlotPod)
		rid := utils.GetRoleInstanceID(activeSlotPod)
		c.store.EnsureServingGroupInstanceID(nsn, groupName, gid)
		c.store.EnsureRoleInstanceID(nsn, groupName, roleName, roleID, rid)
		return gid, rid, nil
	}

	if roleInstanceID == "" && c.kubeClientSet != nil && len(ms.Spec.Plugins) > 0 {
		entryName := utils.GeneratePodName(groupName, roleID, 0)
		var managedSvcs []*corev1.Service
		for _, svcName := range []string{entryName, plugins.DiscoveryServiceName(entryName)} {
			svc, err := c.kubeClientSet.CoreV1().Services(ms.Namespace).Get(ctx, svcName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return "", "", fmt.Errorf("get Headless Service %s/%s for Role %s/%s identity check: %w", ms.Namespace, svcName, roleName, roleID, err)
			}
			if svcGroup, svcRole, svcID, ok := plugins.ManagedHeadlessServiceIdentity(svc, ms); ok && svcGroup == groupName && svcRole == roleName && svcID == roleID {
				if svc.DeletionTimestamp != nil {
					c.enqueueModelServingAfter(ms, enqueueAfter)
					return "", "", fmt.Errorf("Headless Service %s/%s is terminating", ms.Namespace, svcName)
				}
				managedSvcs = append(managedSvcs, svc)
			}
		}
		if len(managedSvcs) > 0 {
			firstGID := utils.GetGroupInstanceID(managedSvcs[0])
			firstRID := utils.GetRoleInstanceID(managedSvcs[0])
			consistent := !groupIDSet || firstGID == groupInstanceID
			for _, svc := range managedSvcs[1:] {
				if utils.GetGroupInstanceID(svc) != firstGID || utils.GetRoleInstanceID(svc) != firstRID {
					consistent = false
					break
				}
			}
			if !consistent {
				// Zero live Pods exist in this slot (verified above); drain mismatched/inconsistent stale Services before creating replacement Pods.
				if err := c.deleteStaleManagedServices(ctx, ms, managedSvcs); err != nil {
					return "", "", err
				}
				c.enqueueModelServingAfter(ms, enqueueAfter)
				return "", "", fmt.Errorf("drained stale Headless Services for Role %s/%s in ServingGroup %s before recreation", roleName, roleID, groupName)
			}
			c.store.EnsureServingGroupInstanceID(nsn, groupName, firstGID)
			c.store.EnsureRoleInstanceID(nsn, groupName, roleName, roleID, firstRID)
			return firstGID, firstRID, nil
		}
	}

	if !groupIDSet {
		hasLegacyGroupObject := false
		if c.podsInformer != nil {
			if groupPods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName)); err == nil {
				for _, pod := range groupPods {
					if utils.IsControlledByModelServing(pod, ms) && !isPodMarkedDeleting(pod) {
						groupInstanceID = utils.GetGroupInstanceID(pod)
						hasLegacyGroupObject = groupInstanceID == ""
						break
					}
				}
			}
		}
		if groupInstanceID == "" && !hasLegacyGroupObject && c.podGroupManager != nil && c.podGroupManager.HasPodGroupCRD() {
			if pgs, err := c.getPodGroupsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName)); err == nil {
				for _, pg := range pgs {
					if pg.Name == groupName && utils.IsControlledByModelServing(pg, ms) && pg.DeletionTimestamp == nil {
						groupInstanceID = utils.GetGroupInstanceID(pg)
						hasLegacyGroupObject = groupInstanceID == ""
						break
					}
				}
			}
		}
		if groupInstanceID == "" && !hasLegacyGroupObject {
			groupInstanceID = utils.NewInstanceID()
		}
		c.store.EnsureServingGroupInstanceID(nsn, groupName, groupInstanceID)
	}
	if roleInstanceID == "" {
		roleInstanceID = utils.NewInstanceID()
		c.store.EnsureRoleInstanceID(nsn, groupName, roleName, roleID, roleInstanceID)
	}
	return groupInstanceID, roleInstanceID, nil
}

func (c *ModelServingController) captureRoleInstanceIDFromCache(ms *workload.ModelServing, groupName, roleName, roleID string) {
	if c.podsInformer == nil {
		return
	}
	nsn := utils.GetNamespaceName(ms)
	pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID))
	if err != nil {
		return
	}
	for _, pod := range pods {
		if utils.IsControlledByModelServing(pod, ms) {
			c.store.EnsureServingGroupInstanceID(nsn, groupName, utils.GetGroupInstanceID(pod))
			c.store.EnsureRoleInstanceID(nsn, groupName, roleName, roleID, utils.GetRoleInstanceID(pod))
			return
		}
	}
}

func (c *ModelServingController) captureGroupInstanceIDsFromCache(ms *workload.ModelServing, groupName string) {
	if c.podsInformer == nil {
		return
	}
	nsn := utils.GetNamespaceName(ms)
	pods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName))
	if err != nil {
		return
	}
	for _, pod := range pods {
		if !utils.IsControlledByModelServing(pod, ms) {
			continue
		}
		c.store.EnsureServingGroupInstanceID(nsn, groupName, utils.GetGroupInstanceID(pod))
		roleName, roleID := utils.GetRoleName(pod), utils.GetRoleID(pod)
		if roleName != "" && roleID != "" {
			c.store.EnsureRoleInstanceID(nsn, groupName, roleName, roleID, utils.GetRoleInstanceID(pod))
		}
	}
}

// recoverDeletingUnitsOnSync reconstructs Deleting store states during initial controller
// sync from durable signals:
//  1. Terminating Pods (DeletionTimestamp != nil)
//  2. Partially-deleted multi-Pod Roles under Recreate policies whose entry Pod ("-0") has
//     already disappeared while worker Pods survive.
func (c *ModelServingController) recoverDeletingUnitsOnSync(allPods []*corev1.Pod) {
	for _, pod := range allPods {
		if pod.DeletionTimestamp == nil {
			continue
		}
		ms, servingGroupName, err := c.getModelServingByChildResource(pod)
		if err != nil || c.shouldSkipHandling(ms, servingGroupName, pod) {
			continue
		}
		nsn := utils.GetNamespaceName(ms)
		roleName := utils.GetRoleName(pod)
		roleID := utils.GetRoleID(pod)
		podGID := utils.GetGroupInstanceID(pod)
		podRID := utils.GetRoleInstanceID(pod)
		if storeGID, exists := c.store.GetServingGroupInstanceID(nsn, servingGroupName); exists && storeGID != "" && storeGID != podGID {
			continue
		}
		if roleName != "" && roleID != "" {
			if storeRID, exists := c.store.GetRoleInstanceID(nsn, servingGroupName, roleName, roleID); exists && storeRID != "" && storeRID != podRID {
				continue
			}
		}
		roleTemplateHash := c.resolveRoleTemplateHash(ms, roleName, pod)
		c.store.AddServingGroupAndRole(nsn, servingGroupName, utils.ObjectRevision(pod), roleTemplateHash, roleName, roleID)
		c.store.EnsureServingGroupInstanceID(nsn, servingGroupName, podGID)
		if roleName != "" && roleID != "" {
			c.store.EnsureRoleInstanceID(nsn, servingGroupName, roleName, roleID, podRID)
		}
		c.markUnitDeletingByRecoveryPolicy(ms, servingGroupName, roleName, roleID)
	}

	// Detect partially-deleted multi-Pod units under Recreate policies whose entry Pod ("-0")
	// already disappeared while worker Pods remain. Under RecoveryPolicy=None, single-Pod entry
	// loss continues to be refilled normally.
	type roleUnitKey struct {
		ns, msName, group, role, roleID, rid string
	}
	type roleObservation struct {
		ms        *workload.ModelServing
		hasEntry  bool
		hasWorker bool
	}
	observed := make(map[roleUnitKey]*roleObservation)
	for _, pod := range allPods {
		if isPodMarkedDeleting(pod) {
			continue
		}
		ms, groupName, err := c.getModelServingByChildResource(pod)
		if err != nil || c.shouldSkipHandling(ms, groupName, pod) {
			continue
		}
		if ms.Spec.RecoveryPolicy == workload.NoneRestartPolicy {
			continue
		}
		roleName, roleID := utils.GetRoleName(pod), utils.GetRoleID(pod)
		if roleName == "" || roleID == "" {
			continue
		}
		key := roleUnitKey{ms.Namespace, ms.Name, groupName, roleName, roleID, utils.GetRoleInstanceID(pod)}
		obs := observed[key]
		if obs == nil {
			obs = &roleObservation{ms: ms}
			observed[key] = obs
		}
		if pod.Name == utils.GeneratePodName(groupName, roleID, 0) || pod.Labels[workload.EntryLabelKey] == utils.Entry {
			obs.hasEntry = true
		} else {
			obs.hasWorker = true
		}
	}
	for key, obs := range observed {
		if obs.hasWorker && !obs.hasEntry {
			c.markUnitDeletingByRecoveryPolicy(obs.ms, key.group, key.role, key.roleID)
		}
	}
}

func (c *ModelServingController) markServingGroupDeleting(ms *workload.ModelServing, servingGroupName string) {
	c.clearGroupRunningPods(ms, servingGroupName)
	if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), servingGroupName, datastore.ServingGroupDeleting); err != nil {
		klog.Warningf("failed to recover Deleting status for ServingGroup %s: %v", servingGroupName, err)
	}
}

func (c *ModelServingController) markRoleDeleting(ms *workload.ModelServing, servingGroupName, roleName, roleID string) {
	if roleName == "" || roleID == "" {
		return
	}
	if err := c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), servingGroupName, roleName, roleID, datastore.RoleDeleting); err != nil {
		klog.Warningf("failed to recover Deleting status for Role %s/%s: %v", roleName, roleID, err)
	}
}

func (c *ModelServingController) markUnitDeletingByRecoveryPolicy(ms *workload.ModelServing, servingGroupName, roleName, roleID string) {
	switch ms.Spec.RecoveryPolicy {
	case workload.ServingGroupRecreate:
		c.markServingGroupDeleting(ms, servingGroupName)
	case workload.RoleRecreate:
		c.markRoleDeleting(ms, servingGroupName, roleName, roleID)
	}
}

func (c *ModelServingController) clearGroupRunningPods(ms *workload.ModelServing, servingGroupName string) {
	if c.podsInformer == nil {
		return
	}
	pods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, servingGroupName))
	if err != nil {
		return
	}
	nsn := utils.GetNamespaceName(ms)
	for _, pod := range pods {
		c.store.DeleteRunningPodFromServingGroup(nsn, servingGroupName, pod.Name)
	}
}
