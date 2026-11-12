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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// The datastore may briefly retain the replaced incarnation's revision while
// Pod events catch up. Use the owned live entry as the instance's version
// anchor, rather than treating a stale cached hash as permission to delete.
func (c *ModelServingController) coordinatedLiveRole(ctx context.Context, ms *api.ModelServing, group, name string, instance datastore.Role) (datastore.Role, *corev1.Pod, error) {
	if ms.ResourceVersion == "" {
		return instance, nil, nil
	}
	pods, err := c.coordinatedPods(ctx, ms, labels.Everything())
	if err != nil {
		return instance, nil, err
	}
	var pod *corev1.Pod
	for _, observed := range pods {
		if observed.Name == utils.GeneratePodName(group, instance.Name, 0) {
			pod = observed
			break
		}
	}
	if pod == nil {
		return instance, nil, nil
	}
	if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) || utils.GetRoleName(pod) != name || utils.GetRoleID(pod) != instance.Name || pod.Labels[api.GroupNameLabelKey] != group {
		return instance, nil, fmt.Errorf("live entry %s has a different owner or Role identity", pod.Name)
	}
	instance.Revision, instance.RoleTemplateHash = utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod)
	return instance, pod, nil
}

func (c *ModelServingController) refreshCoordinatedAvailabilityInRound(ctx context.Context, ms *api.ModelServing) error {
	if roleCoordination(ms) == nil || ms.ResourceVersion == "" {
		return nil
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err == datastore.ErrServingGroupNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for _, instance := range instances {
				if instance.Status == datastore.RoleDeleting {
					continue
				}
				ready, err := c.checkRoleReadyInRound(ctx, ms, group.Name, name, instance.Name)
				if err != nil {
					return err
				}
				status := datastore.RoleCreating
				if ready {
					status = datastore.RoleRunning
				}
				if !ready && group.Status == datastore.ServingGroupRunning {
					if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group.Name, datastore.ServingGroupCreating); err != nil {
						return err
					}
				}
				if err := c.updateRoleStatus(ms, group.Name, name, instance.Name, status); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// A total cleanup allowance cannot be transferred into permission to delete
// healthy capacity after dependency/ordinal filtering removes an unhealthy Pod.
type rolloutBudget struct{ total, healthy int }

func newRolloutBudget(n, u, active, ready, unavailable int) rolloutBudget {
	minimum := max(0, n-u)
	return rolloutBudget{max(0, active-minimum-unavailable), max(0, ready-minimum)}
}
func (b *rolloutBudget) take(ready bool) bool {
	if b.total <= 0 || (ready && b.healthy <= 0) {
		return false
	}
	b.total--
	if ready {
		b.healthy--
	}
	return true
}
func roleRolloutBudget(role api.Role, instances []datastore.Role, unavailable int) (rolloutBudget, error) {
	u, configured, err := utils.GetMaxUnavailableForRole(role)
	if err != nil {
		return rolloutBudget{}, err
	}
	if !configured {
		u = roleReplicas(role)
	}
	ready := 0
	for _, instance := range instances {
		if instance.Status == datastore.RoleRunning {
			ready++
		}
	}
	return newRolloutBudget(roleReplicas(role), u, len(instances), ready, unavailable), nil
}
func selectBudgetedRolesToDelete(name string, instances []datastore.Role, budget rolloutBudget, ordered bool) []roleToDelete {
	var result []roleToDelete
	for _, instance := range instances {
		if !budget.take(instance.Status == datastore.RoleRunning) {
			if ordered || budget.total == 0 {
				break
			}
			continue
		}
		result = append(result, roleToDelete{roleName: name, roleID: instance.Name})
	}
	return result
}

func (c *ModelServingController) rolloutDeletionPending(ms *api.ModelServing, group, role, instance string) bool {
	if c.podsInformer == nil {
		return false
	}
	pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, group, role, instance))
	if err != nil {
		return true
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil && utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			return true
		}
	}
	return false
}

// A persisted origin distinguishes temporary capacity from unmarked sparse
// binpack survivors. A marked instance inside the new desired range is formal.
type coordinatedSurgeKey struct{}

const (
	surgeAnnotation   = "modelserving.volcano.sh/surge"
	surgeServingGroup = "serving-group"
	surgeRole         = "role"
)

func setSurgeScope(pod *corev1.Pod, scope string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[surgeAnnotation] = scope
}
func (c *ModelServingController) surgePodsInRound(ctx context.Context, ms *api.ModelServing, scope, group, role string) ([]*corev1.Pod, error) {
	selector := labels.Set{api.ModelServingNameLabelKey: ms.Name, api.GroupNameLabelKey: group, api.RoleLabelKey: role}.AsSelector()
	var pods []*corev1.Pod
	if ms.ResourceVersion != "" {
		var err error
		pods, err = c.coordinatedPods(ctx, ms, selector)
		if err != nil {
			return nil, err
		}
	} else if c.podsInformer != nil {
		var err error
		pods, err = c.getPodsByIndex(GroupNameKey, ms.Namespace+"/"+group)
		if err != nil {
			return nil, err
		}
	}
	var result []*corev1.Pod
	for _, pod := range pods {
		if selector.Matches(labels.Set(pod.Labels)) && utils.IsOwnedByModelServingWithUID(pod, ms.UID) && pod.Annotations[surgeAnnotation] == scope {
			result = append(result, pod)
		}
	}
	return result, nil
}
func markedSurgeNames(pods []*corev1.Pod, scope string, n int) sets.Set[string] {
	names := sets.New[string]()
	for _, pod := range pods {
		name := utils.GetRoleID(pod)
		_, ordinal := utils.GetParentNameAndOrdinal(name)
		if ordinal >= n {
			names.Insert(name)
		}
	}
	return names
}

// Unmarked high binpack survivors already occupy formal slots. A temporary
// replica must not fill their low holes and silently displace their identity.
func forEachCoordinatedOrdinal(n, formalSlots int, existing []int, limit int, create func(int) bool) {
	occupied := sets.New(existing...)
	for ordinal := 0; ordinal < n && formalSlots > 0 && limit > 0; ordinal++ {
		if occupied.Has(ordinal) {
			continue
		}
		if !create(ordinal) {
			return
		}
		formalSlots--
		limit--
	}
	for ordinal := n; limit > 0; ordinal++ {
		if occupied.Has(ordinal) {
			continue
		}
		if !create(ordinal) {
			return
		}
		limit--
	}
}

func (c *ModelServingController) adoptCoordinatedSurge(ctx context.Context, ms *api.ModelServing, group string, role api.Role) error {
	pods, err := c.surgePodsInRound(ctx, ms, surgeRole, group, role.Name)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(pod))
		if ordinal < 0 || ordinal >= roleReplicas(role) || pod.DeletionTimestamp != nil {
			continue
		}
		patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
			"uid": pod.UID, "resourceVersion": pod.ResourceVersion,
			"annotations": map[string]interface{}{surgeAnnotation: nil},
		}})
		if err != nil {
			return err
		}
		if _, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// A temporary target can occupy the desired count after the last old instance
// disappears. Restore the missing formal slot before retiring that temporary
// capacity; object count alone does not establish rollout completion.
func (c *ModelServingController) finishCoordinatedSurge(ctx context.Context, ms *api.ModelServing, group string, role api.Role, instances []datastore.Role, groupOrdinal int, revision string, allowTargetStart bool) (bool, error) {
	pods, err := c.surgePodsInRound(ctx, ms, surgeRole, group, role.Name)
	if err != nil {
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return true, err
	}
	temporary := markedSurgeNames(pods, surgeRole, roleReplicas(role))
	if temporary.Len() == 0 {
		return false, nil
	}
	c.enqueueModelServingAfter(ms, enqueueAfter)
	surge, err := utils.GetMaxSurgeForRole(role)
	if err != nil {
		return true, err
	}
	formal := 0
	for _, instance := range instances {
		if !temporary.Has(instance.Name) {
			formal++
		}
	}
	missing := max(0, roleReplicas(role)-formal)
	toCreate := min(missing, max(0, roleReplicas(role)+surge-len(instances)))
	if toCreate > 0 {
		return true, c.scaleUpRoles(ctx, ms, group, role, instances, len(instances)+toCreate, groupOrdinal, revision, allowTargetStart)
	}
	// With surge reduced to zero, free only the availability-permitted temporary
	// slots. Their disappearance then allows ordinary formal refill.
	expected := roleReplicas(role)
	if missing > 0 && surge == 0 {
		expected = len(instances) - missing
	}
	_, err = c.retireCoordinatedSurge(ctx, ms, group, role, instances, expected)
	return true, err
}

// Cleanup never selects a formal target merely because a healthy surge makes
// total capacity exceed desired. Every healthy deletion spends actual credit.
func (c *ModelServingController) retireCoordinatedSurge(ctx context.Context, ms *api.ModelServing, group string, role api.Role, instances []datastore.Role, expected int) (bool, error) {
	pods, err := c.surgePodsInRound(ctx, ms, surgeRole, group, role.Name)
	if err != nil {
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return true, err
	}
	temporary := markedSurgeNames(pods, surgeRole, roleReplicas(role))
	if temporary.Len() == 0 {
		return false, nil
	}
	ledger := slices.Clone(instances)
	_, unavailable := c.outdatedRolesInRound(ctx, ms, datastore.ServingGroup{Name: group}, role, ledger)
	budget, err := roleRolloutBudget(role, ledger, unavailable)
	if err != nil {
		return true, err
	}
	excess := len(instances) - expected
	for i := len(instances) - 1; i >= 0 && excess > 0; i-- {
		instance := instances[i]
		if temporary.Has(instance.Name) && instance.Status != datastore.RoleDeleting && !c.rolloutDeletionPending(ms, group, role.Name, instance.Name) {
			actionCtx := ctx
			if ms.ResourceVersion != "" {
				actionCtx = withCoordinatedRound(ctx, ms)
				if err := c.refreshCoordinatedAvailabilityInRound(actionCtx, ms); err != nil {
					return true, err
				}
				current, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group, role.Name)
				if err != nil {
					return true, err
				}
				marked, err := c.surgePodsInRound(actionCtx, ms, surgeRole, group, role.Name)
				if err != nil {
					return true, err
				}
				if !markedSurgeNames(marked, surgeRole, roleReplicas(role)).Has(instance.Name) {
					continue
				}
				at := slices.IndexFunc(current, func(r datastore.Role) bool { return r.Name == instance.Name })
				if at < 0 || current[at].Status == datastore.RoleDeleting {
					continue
				}
				instance = current[at]
				_, unavailable := c.outdatedRolesInRound(actionCtx, ms, datastore.ServingGroup{Name: group}, role, current)
				budget, err = roleRolloutBudget(role, current, unavailable)
				if err != nil {
					return true, err
				}
			}
			comparison := c.compareRoleTemplate(actionCtx, ms, datastore.ServingGroup{Name: group}, role.Name, instance)
			if comparison == templateUnknown {
				continue
			}
			if (comparison == templateDifferent || instance.Status == datastore.RoleRunning) && !budget.take(instance.Status == datastore.RoleRunning) {
				continue
			}
			if err := c.DeleteRole(actionCtx, ms, group, role.Name, instance.Name); err != nil {
				c.enqueueModelServingAfter(ms, enqueueAfter)
				return true, err
			}
			excess--
		}
	}
	return true, nil
}

func (c *ModelServingController) refreshCoordinatedAvailability(ms *api.ModelServing) error {
	return c.refreshCoordinatedAvailabilityInRound(withCoordinatedRound(context.Background(), ms), ms)
}
