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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(group, instance.Name, 0), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return instance, nil, nil
	}
	if err != nil {
		return instance, nil, err
	}
	if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) || utils.GetRoleName(pod) != name || utils.GetRoleID(pod) != instance.Name || pod.Labels[api.GroupNameLabelKey] != group {
		return instance, nil, fmt.Errorf("live entry %s has a different owner or Role identity", pod.Name)
	}
	instance.Revision, instance.RoleTemplateHash = utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod)
	return instance, pod, nil
}

func (c *ModelServingController) refreshCoordinatedAvailability(ms *api.ModelServing) error {
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
				ready, err := c.checkRoleReady(ms, group.Name, name, instance.Name)
				if err != nil {
					return err
				}
				status := datastore.RoleCreating
				if ready {
					status = datastore.RoleRunning
				}
				if err := c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group.Name, name, instance.Name, status); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Keep semantic comparison local to coordination; revision identities and the
// upstream ControllerRevision format remain unchanged.
type templateComparison uint8

const (
	templateUnknown templateComparison = iota
	templateEquivalent
	templateDifferent
)

func (c *ModelServingController) compareRoleTemplate(ctx context.Context, ms *api.ModelServing, group datastore.ServingGroup, name string, instance datastore.Role) templateComparison {
	if ms.ResourceVersion != "" {
		observed, pod, err := c.coordinatedLiveRole(ctx, ms, group.Name, name, instance)
		if err != nil || pod == nil || pod.DeletionTimestamp != nil {
			return templateUnknown
		}
		instance = observed
	}
	for _, role := range ms.Spec.Template.Roles {
		if role.Name != name {
			continue
		}
		if instance.RoleTemplateHash == utils.CalRoleTemplateHash(role) {
			return templateEquivalent
		}
		revision := instance.Revision
		if revision == "" {
			revision = group.Revision
		}
		if c.kubeClientSet != nil && revision != "" {
			cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
			if err != nil || cr == nil || !utils.IsOwnedByModelServingWithUID(cr, ms.UID) {
				return templateUnknown
			}
			historical, err := utils.GetRolesFromControllerRevision(cr)
			if err != nil {
				return templateUnknown
			}
			for _, old := range historical {
				if old.Name == name {
					if utils.EqualRoleTemplateForRevision(old, role) {
						return templateEquivalent
					}
					return templateDifferent
				}
			}
			return templateUnknown
		}
		if instance.RoleTemplateHash != "" {
			return templateDifferent
		}
	}
	return templateUnknown
}

func (c *ModelServingController) compareServingGroupTemplate(ctx context.Context, ms *api.ModelServing, group datastore.ServingGroup, revision string) templateComparison {
	if group.Revision == revision {
		return templateEquivalent
	}
	old, err := c.roleSpecsFromRevision(ms, group.Revision)
	if err != nil {
		return templateUnknown
	}
	roles := make([]api.Role, 0, len(old))
	for _, role := range old {
		roles = append(roles, role)
	}
	if utils.EqualRoleTemplatesForRevision(roles, ms.Spec.Template.Roles) {
		return templateEquivalent
	}
	return templateDifferent
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
	surgeAnnotation = "modelserving.volcano.sh/surge"
	surgeRole       = "role"
)

func setSurgeScope(pod *corev1.Pod, scope string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[surgeAnnotation] = scope
}
func (c *ModelServingController) surgePods(ms *api.ModelServing, scope, group, role string) ([]*corev1.Pod, error) {
	selector := labels.Set{api.ModelServingNameLabelKey: ms.Name, api.GroupNameLabelKey: group, api.RoleLabelKey: role}.AsSelector()
	var pods []*corev1.Pod
	if ms.ResourceVersion != "" {
		live, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, err
		}
		for i := range live.Items {
			pods = append(pods, &live.Items[i])
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
	pods, err := c.surgePods(ms, surgeRole, group, role.Name)
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
func (c *ModelServingController) finishCoordinatedSurge(ctx context.Context, ms *api.ModelServing, group string, role api.Role, instances []datastore.Role, groupOrdinal int, revision string, allowTargetStart bool) bool {
	pods, err := c.surgePods(ms, surgeRole, group, role.Name)
	if err != nil {
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return true
	}
	temporary := markedSurgeNames(pods, surgeRole, roleReplicas(role))
	if temporary.Len() == 0 {
		return false
	}
	c.enqueueModelServingAfter(ms, enqueueAfter)
	surge, err := utils.GetMaxSurgeForRole(role)
	if err != nil {
		return true
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
		c.scaleUpRoles(ctx, ms, group, role, instances, len(instances)+toCreate, groupOrdinal, revision, allowTargetStart)
		return true
	}
	// With surge reduced to zero, free only the availability-permitted temporary
	// slots. Their disappearance then allows ordinary formal refill.
	expected := roleReplicas(role)
	if missing > 0 && surge == 0 {
		expected = len(instances) - missing
	}
	c.retireCoordinatedSurge(ctx, ms, group, role, instances, expected)
	return true
}

// Cleanup never selects a formal target merely because a healthy surge makes
// total capacity exceed desired. Every healthy deletion spends actual credit.
func (c *ModelServingController) retireCoordinatedSurge(ctx context.Context, ms *api.ModelServing, group string, role api.Role, instances []datastore.Role, expected int) bool {
	pods, err := c.surgePods(ms, surgeRole, group, role.Name)
	if err != nil {
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return true
	}
	temporary := markedSurgeNames(pods, surgeRole, roleReplicas(role))
	if temporary.Len() == 0 {
		return false
	}
	ledger := slices.Clone(instances)
	_, unavailable := c.outdatedRoles(ms, datastore.ServingGroup{Name: group}, role, ledger)
	budget, err := roleRolloutBudget(role, ledger, unavailable)
	if err != nil {
		return true
	}
	excess := len(instances) - expected
	for i := len(instances) - 1; i >= 0 && excess > 0; i-- {
		instance := instances[i]
		if temporary.Has(instance.Name) && instance.Status != datastore.RoleDeleting && !c.rolloutDeletionPending(ms, group, role.Name, instance.Name) {
			comparison := c.compareRoleTemplate(ctx, ms, datastore.ServingGroup{Name: group}, role.Name, instance)
			if comparison == templateUnknown {
				continue
			}
			if (comparison == templateDifferent || instance.Status == datastore.RoleRunning) && !budget.take(instance.Status == datastore.RoleRunning) {
				continue
			}
			c.DeleteRole(ctx, ms, group, role.Name, instance.Name)
			excess--
		}
	}
	return true
}
