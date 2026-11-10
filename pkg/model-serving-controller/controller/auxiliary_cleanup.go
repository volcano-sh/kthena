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
	"context"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type auxiliaryCleanupKey struct {
	uid             types.UID
	group, role, id string
}

// finishPendingDeletions retries failed API requests before any new capacity can
// reuse an identity. Services with an accepted DELETE may still have finalizers.
func (c *ModelServingController) finishPendingDeletions(ctx context.Context, ms *workload.ModelServing) error {
	if err := c.resumeMarkedDeletions(ms); err != nil {
		return err
	}
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

// resumeMarkedDeletions rebuilds deletions that a previous controller
// committed with DeletionScopeAnnotation but did not finish. Only the marked
// Pod UIDs become targets; the selector is never resnapshotted.
func (c *ModelServingController) resumeMarkedDeletions(ms *workload.ModelServing) error {
	pods, err := c.podsLister.Pods(ms.Namespace).List(labels.SelectorFromSet(labels.Set{workload.ModelServingNameLabelKey: ms.Name}))
	if err != nil {
		return err
	}
	key := utils.GetNamespaceName(ms)
	groupScoped := map[string]bool{}
	for _, pod := range pods {
		if podOwnedForDeletion(pod, ms) && pod.Annotations[utils.DeletionScopeAnnotation] == utils.DeletionScopeServingGroup {
			groupScoped[pod.Labels[workload.GroupNameLabelKey]] = true
		}
	}
	marked := map[auxiliaryCleanupKey]*podDeletionTargets{}
	for _, pod := range pods {
		if !podOwnedForDeletion(pod, ms) || !utils.HasDeletionScope(pod) {
			continue
		}
		group, role, id := pod.Labels[workload.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
		c.store.AddServingGroupAndRole(key, group, utils.ObjectRevision(pod), c.resolveRoleTemplateHash(ms, role, pod), role, id)
		target := auxiliaryCleanupKey{uid: ms.UID, group: group}
		if groupScoped[group] {
			err = c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupDeleting)
		} else {
			target.role, target.id = role, id
			err = c.store.UpdateRoleStatus(key, group, role, id, datastore.RoleDeleting)
		}
		if err != nil {
			return err
		}
		if marked[target] == nil {
			marked[target] = &podDeletionTargets{targets: map[string]types.UID{}, done: map[string]bool{}, namespace: ms.Namespace, ownerName: ms.Name, ownerUID: ms.UID}
		}
		marked[target].targets[pod.Name] = pod.UID
	}
	for target, targets := range marked {
		c.podDeletions.LoadOrStore(target, targets)
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

func (c *ModelServingController) prepareRoleCleanup(ctx context.Context, ms *workload.ModelServing, group, role, id string) error {
	cleanup, _ := c.roleCleanups.LoadOrStore(auxiliaryCleanupKey{ms.UID, group, role, id}, &plugins.RoleCleanup{})
	return cleanup.(*plugins.RoleCleanup).Capture(ctx, &plugins.HookRequest{ModelServing: ms, ServingGroup: group, RoleName: role, RoleID: id, KubeClient: c.kubeClientSet})
}

func (c *ModelServingController) forgetGroupCleanup(uid types.UID, group string) {
	for _, records := range []*sync.Map{&c.roleCleanups, &c.podDeletions} {
		records.Range(func(key, _ any) bool {
			k := key.(auxiliaryCleanupKey)
			if k.uid == uid && k.group == group {
				records.Delete(key)
			}
			return true
		})
	}
}
