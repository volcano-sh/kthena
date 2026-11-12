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
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// A rollout with maxSurge creates its extra capacity at ordinals >= replicas
// and marks those Pods with surgeAnnotation. Once every outdated unit is
// replaced, the marked units stay in the expected count, so the ordinal
// vacated by the last replaced unit is recreated, and the rolling update then
// deletes the marked units within its availability budget. Without the marker
// the surge unit would become a regular replica and the vacated ordinal would
// stay empty. Unmarked units beyond the desired range (binpack scale-down
// survivors) are regular replicas.

// surgeScopeKey carries the surge scope for the Pods of a unit created as
// temporary rollout capacity.
type surgeScopeKey struct{}

func withSurgeScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, surgeScopeKey{}, scope)
}

func servingGroupRollout(ms *workloadv1alpha1.ModelServing) bool {
	return ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate
}

// independentRoleRollout reports a RoleRollingUpdate without Role
// coordination, which has its own surge handling.
func independentRoleRollout(ms *workloadv1alpha1.ModelServing) bool {
	return !servingGroupRollout(ms) && roleCoordination(ms) == nil
}

func servingGroupPodKey(ms *workloadv1alpha1.ModelServing, groupName string) string {
	return fmt.Sprintf("%s/%s", ms.Namespace, groupName)
}

func rolePodKey(ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) string {
	return fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID)
}

// markedPods returns the Pods of the indexed unit that carry the marker.
func (c *ModelServingController) markedPods(ms *workloadv1alpha1.ModelServing, scope, index, key string) ([]*corev1.Pod, error) {
	if c.podsInformer == nil {
		return nil, nil
	}
	pods, err := c.getPodsByIndex(index, key)
	if err != nil {
		return nil, err
	}
	var marked []*corev1.Pod
	for _, pod := range pods {
		if utils.IsOwnedByModelServingWithUID(pod, ms.UID) && pod.Annotations[surgeAnnotation] == scope {
			marked = append(marked, pod)
		}
	}
	return marked, nil
}

// surgeServingGroups returns the marked ServingGroups beyond the desired range.
func (c *ModelServingController) surgeServingGroups(ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup) (sets.Set[string], error) {
	surge := sets.New[string]()
	if !servingGroupRollout(ms) {
		return surge, nil
	}
	for _, group := range groups {
		if _, ordinal := utils.GetParentNameAndOrdinal(group.Name); ordinal < modelServingReplicas(ms) {
			continue
		}
		marked, err := c.markedPods(ms, surgeServingGroup, GroupNameKey, servingGroupPodKey(ms, group.Name))
		if err != nil {
			return nil, err
		}
		if len(marked) > 0 {
			surge.Insert(group.Name)
		}
	}
	return surge, nil
}

// surgeRoles returns the marked instances of a Role beyond its desired range.
func (c *ModelServingController) surgeRoles(ms *workloadv1alpha1.ModelServing, groupName string, role workloadv1alpha1.Role, instances []datastore.Role) (sets.Set[string], error) {
	surge := sets.New[string]()
	if !independentRoleRollout(ms) {
		return surge, nil
	}
	for _, instance := range instances {
		if _, ordinal := utils.GetParentNameAndOrdinal(instance.Name); ordinal < roleReplicas(role) {
			continue
		}
		marked, err := c.markedPods(ms, surgeRole, RoleIDKey, rolePodKey(ms, groupName, role.Name, instance.Name))
		if err != nil {
			return nil, err
		}
		if len(marked) > 0 {
			surge.Insert(instance.Name)
		}
	}
	return surge, nil
}

// adoptServingGroupSurge turns the marked ServingGroups that a replica
// increase moved into the desired range into regular replicas. Otherwise a
// later scale-down would treat them as surge again.
func (c *ModelServingController) adoptServingGroupSurge(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup) error {
	for _, group := range groups {
		if _, ordinal := utils.GetParentNameAndOrdinal(group.Name); ordinal >= modelServingReplicas(ms) {
			continue
		}
		marked, err := c.markedPods(ms, surgeServingGroup, GroupNameKey, servingGroupPodKey(ms, group.Name))
		if err != nil {
			return err
		}
		c.clearSurgeMarker(ctx, ms, marked)
	}
	return nil
}

// adoptRoleSurge is adoptServingGroupSurge for the instances of a Role.
func (c *ModelServingController) adoptRoleSurge(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, role workloadv1alpha1.Role, instances []datastore.Role) error {
	for _, instance := range instances {
		if _, ordinal := utils.GetParentNameAndOrdinal(instance.Name); ordinal >= roleReplicas(role) {
			continue
		}
		marked, err := c.markedPods(ms, surgeRole, RoleIDKey, rolePodKey(ms, groupName, role.Name, instance.Name))
		if err != nil {
			return err
		}
		c.clearSurgeMarker(ctx, ms, marked)
	}
	return nil
}

// clearSurgeMarker removes the marker without replacing the Pods. The UID
// precondition keeps a Pod recreated under the same name untouched. A failure
// does not block the reconcile, because the unit is a regular replica by its
// ordinal already; the removal is retried.
func (c *ModelServingController) clearSurgeMarker(ctx context.Context, ms *workloadv1alpha1.ModelServing, pods []*corev1.Pod) {
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid":         pod.UID,
			"annotations": map[string]any{surgeAnnotation: nil},
		}})
		if err == nil {
			_, err = c.kubeClientSet.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		}
		// NotFound: the Pod is gone. Invalid: the UID differs, so it was replaced.
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsInvalid(err) {
			klog.Warningf("failed to remove the surge marker from pod %s/%s: %v", pod.Namespace, pod.Name, err)
			c.enqueueModelServingAfter(ms, enqueueAfter)
		}
	}
}

// expectedWithSurge is the expected unit count once no outdated unit remains.
// The surge units stay counted until the rolling update retires them, so the
// ordinals the rollout vacated are recreated, unless the regular units alone
// exceed replicas, which is an explicit scale-down. A reduced maxSurge stops
// further creation without scaling down.
func expectedWithSurge(replicas, maxSurge, current, surge int) int {
	if surge == 0 || current-surge > replicas {
		return replicas
	}
	return min(replicas+surge, max(replicas+maxSurge, current))
}

// retireSurgeServingGroups deletes the surge ServingGroups once no outdated
// group remains. An unready group frees capacity without reducing
// availability; a Running one needs the rolling update budget and must leave
// minAvailable groups Running.
func (c *ModelServingController) retireSurgeServingGroups(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup, budget, minAvailable int) error {
	surge, err := c.surgeServingGroups(ms, groups)
	if err != nil {
		return err
	}
	// The budget does not see unready groups protected by partition.
	running := 0
	for _, group := range groups {
		if group.Status == datastore.ServingGroupRunning {
			running++
		}
	}
	budget = min(budget, running-minAvailable)
	for i := len(groups) - 1; i >= 0; i-- {
		group := groups[i]
		if !surge.Has(group.Name) || group.Status == datastore.ServingGroupDeleting {
			continue
		}
		if group.Status == datastore.ServingGroupRunning {
			if budget <= 0 {
				continue
			}
			budget--
		}
		klog.V(2).Infof("Surge ServingGroup %s will be terminated (status=%s)", group.Name, group.Status)
		if err := c.deleteServingGroup(ctx, ms, group.Name); err != nil {
			return err
		}
	}
	return nil
}

// surgeRolesToRetire is the Role counterpart of retireSurgeServingGroups.
func surgeRolesToRetire(roleName string, instances []datastore.Role, surge sets.Set[string], budget rolloutBudget) []roleToDelete {
	var retire []roleToDelete
	for i := len(instances) - 1; i >= 0; i-- {
		instance := instances[i]
		if !surge.Has(instance.Name) || instance.Status == datastore.RoleDeleting {
			continue
		}
		if instance.Status == datastore.RoleRunning && !budget.take(true) {
			continue
		}
		retire = append(retire, roleToDelete{roleName: roleName, roleID: instance.Name, surge: true})
	}
	return retire
}

// hasPendingSurge reports surge units that the rollout has not retired yet.
func (c *ModelServingController) hasPendingSurge(ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup) (bool, error) {
	surge, err := c.surgeServingGroups(ms, groups)
	if err != nil || surge.Len() > 0 || !independentRoleRollout(ms) {
		return surge.Len() > 0, err
	}
	for _, group := range groups {
		for _, role := range ms.Spec.Template.Roles {
			instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, role.Name)
			if errors.Is(err, datastore.ErrServingGroupNotFound) {
				continue
			}
			if err == nil {
				surge, err = c.surgeRoles(ms, group.Name, role, instances)
			}
			if err != nil || surge.Len() > 0 {
				return surge.Len() > 0, err
			}
		}
	}
	return false, nil
}
