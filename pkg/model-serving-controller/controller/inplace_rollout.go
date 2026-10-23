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
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const maxInPlaceStepsPerSync = 3

// inPlaceUnit is a ServingGroup, or a Role instance under RoleRollingUpdate.
type inPlaceUnit struct {
	group       datastore.ServingGroup
	role        *inPlaceRole
	pods        []*inplace.UnitPod
	inUpdate    bool
	terminating bool
	// missingTemplate is set when a Pod has no template in the target revision.
	missingTemplate string
}

type inPlaceRole struct {
	name       string
	id         string
	status     datastore.RoleStatus
	targetHash string
}

func (u *inPlaceUnit) name() string {
	if u.role != nil {
		return u.group.Name + "/" + u.role.id
	}
	return u.group.Name
}

func (u *inPlaceUnit) String() string {
	if u.role != nil {
		return "Role instance " + u.name()
	}
	return "ServingGroup " + u.name()
}

// outdated compares Role instances by template hash, so unchanged Roles are kept.
func (u *inPlaceUnit) outdated(revision string) bool {
	if u.inUpdate {
		return true
	}
	for _, p := range u.pods {
		if u.role != nil && utils.ObjectRoleTemplateHash(p.Pod) != u.role.targetHash {
			return true
		}
		if u.role == nil && !p.HasTargetLabels(revision) {
			return true
		}
	}
	return false
}

func (u *inPlaceUnit) available() bool {
	if u.role != nil {
		return u.role.status == datastore.RoleRunning && !u.inUpdate
	}
	return u.group.Status == datastore.ServingGroupRunning && !u.inUpdate
}

func (u *inPlaceUnit) rawPods() []*corev1.Pod {
	pods := make([]*corev1.Pod, 0, len(u.pods))
	for _, p := range u.pods {
		pods = append(pods, p.Pod)
	}
	return pods
}

type inPlaceRollout struct {
	c              *ModelServingController
	ms             *workloadv1alpha1.ModelServing
	revision       string
	target         *inplace.RevisionTemplates
	updater        *inplace.PodUpdater
	sources        map[string]sourceTemplates
	eligibility    map[string]*inplace.Fallback
	revisions      []appsv1.ControllerRevision
	listed         bool
	targetRecorded bool
}

type sourceTemplates struct {
	revision  string
	ms        *workloadv1alpha1.ModelServing
	templates *inplace.RevisionTemplates
	fallback  *inplace.Fallback
}

func (c *ModelServingController) newInPlaceRollout(ms *workloadv1alpha1.ModelServing, revision string) *inPlaceRollout {
	return &inPlaceRollout{
		c:           c,
		ms:          ms,
		revision:    revision,
		target:      inplace.NewRevisionTemplates(ms, revision),
		updater:     inplace.NewPodUpdater(c.kubeClientSet),
		sources:     map[string]sourceTemplates{},
		eligibility: map[string]*inplace.Fallback{},
	}
}

func isRoleRollingUpdate(ms *workloadv1alpha1.ModelServing) bool {
	return ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate
}

// inPlaceRolloutFallback returns why no unit of ms can be updated in place, or nil.
func (c *ModelServingController) inPlaceRolloutFallback(ms *workloadv1alpha1.ModelServing) (*inplace.Fallback, error) {
	// Plugins only mutate Pods at creation, so each must support in-place updates.
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return &inplace.Fallback{Reason: inplace.ReasonIneligible, Message: fmt.Sprintf("plugins cannot be built: %v", err)}, nil
	}
	if name, ok := chain.PluginWithoutInPlaceUpdate(); ok {
		return &inplace.Fallback{Reason: inplace.ReasonIneligible, Message: fmt.Sprintf("plugin %s does not support in-place updates", name)}, nil
	}
	if isRoleRollingUpdate(ms) {
		return nil, nil
	}
	maxUnavailable, err := utils.GetMaxUnavailable(ms)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate maxUnavailable: %v", err)
	}
	if maxUnavailable == 0 {
		return &inplace.Fallback{Reason: inplace.ReasonIneligible,
			Message: "maxUnavailable resolves to 0, and an in-place update takes its ServingGroup out of service"}, nil
	}
	return nil, nil
}

func roleInPlaceFallback(role workloadv1alpha1.Role) (*inplace.Fallback, error) {
	maxUnavailable, configured, err := utils.GetMaxUnavailableForRole(role)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate maxUnavailable for role %s: %v", role.Name, err)
	}
	if configured && maxUnavailable == 0 {
		return &inplace.Fallback{Reason: inplace.ReasonIneligible,
			Message: fmt.Sprintf("maxUnavailable of Role %s resolves to 0, and an in-place update takes its Role instance out of service", role.Name)}, nil
	}
	return nil, nil
}

func (c *ModelServingController) updatesInPlace(ms *workloadv1alpha1.ModelServing) (bool, error) {
	if inplace.UpdateMethodOf(ms) != workloadv1alpha1.InPlaceUpdateMethod {
		return false, nil
	}
	fallback, err := c.inPlaceRolloutFallback(ms)
	return err == nil && fallback == nil, err
}

// needsServingGroupSurge reports whether maxSurge applies, which under InPlace is only while a ServingGroup is recreated.
func (c *ModelServingController) needsServingGroupSurge(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup, revision string, partition int) (bool, error) {
	inPlace, err := c.updatesInPlace(ms)
	if err != nil {
		return false, err
	}
	if !inPlace {
		return hasUpdateableOutdatedServingGroup(groups, revision, partition), nil
	}
	r := c.newInPlaceRollout(ms, revision)
	for index, group := range groups {
		if index < partition || group.Revision == revision {
			continue
		}
		if group.Status == datastore.ServingGroupDeleting {
			return true, nil
		}
		unit, err := r.loadUnit(group)
		if err != nil {
			return false, err
		}
		fallback, err := r.unitFallback(ctx, unit)
		if err != nil {
			return false, err
		}
		if fallback != nil {
			return true, nil
		}
	}
	return false, nil
}

// newRoleSurgeRollout returns the rollout that needsRoleSurge shares across one sync, or nil when Role instances are recreated.
// Sharing it avoids repeating revision lookups for every Role of every ServingGroup.
func (c *ModelServingController) newRoleSurgeRollout(ms *workloadv1alpha1.ModelServing) *inPlaceRollout {
	if !isRoleRollingUpdate(ms) {
		return nil
	}
	inPlace, err := c.updatesInPlace(ms)
	if err != nil {
		klog.Errorf("newRoleSurgeRollout: %v", err)
	}
	if !inPlace {
		return nil
	}
	return c.newInPlaceRollout(ms, utils.ModelServingRevision(ms))
}

// needsRoleSurge is needsServingGroupSurge for the instances of one Role.
func (c *ModelServingController) needsRoleSurge(ctx context.Context, r *inPlaceRollout, ms *workloadv1alpha1.ModelServing, groupName string, targetRole workloadv1alpha1.Role, roleList []datastore.Role) bool {
	if r == nil {
		return c.hasUpdateableOutdatedRole(ms, groupName, targetRole, roleList)
	}
	if roleFallback, err := roleInPlaceFallback(targetRole); err != nil || roleFallback != nil {
		return c.hasUpdateableOutdatedRole(ms, groupName, targetRole, roleList)
	}
	partition, partitionConfigured, err := c.getPartition(rolePartition(ms, targetRole), roleReplicas(targetRole))
	if err != nil {
		klog.Errorf("needsRoleSurge: failed to calculate partition for role %s: %v", targetRole.Name, err)
		return false
	}
	targetHash := utils.CalRoleTemplateHash(targetRole)
	group := datastore.ServingGroup{Name: groupName}
	for index, role := range roleList {
		if partitionConfigured && partition > 0 && index < partition {
			continue
		}
		if role.Status == datastore.RoleDeleting {
			if role.RoleTemplateHash != targetHash {
				return true
			}
			continue
		}
		unit, err := r.loadRoleUnit(group, targetRole.Name, role, targetHash)
		if err != nil || unit.inUpdate || !unit.outdated(r.revision) {
			continue
		}
		if fallback, err := r.unitFallback(ctx, unit); err == nil && fallback != nil {
			return true
		}
	}
	return false
}

// manageInPlaceRollout patches the images of outdated ServingGroups, and recreates those that cannot be updated in place.
// All progress is read from the Pods, so the rollout resumes after a controller restart.
func (c *ModelServingController) manageInPlaceRollout(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup, revision string) error {
	maxUnavailable, err := utils.GetMaxUnavailable(ms)
	if err != nil {
		return fmt.Errorf("failed to calculate maxUnavailable: %v", err)
	}
	partition, _, err := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	if err != nil {
		return fmt.Errorf("failed to calculate partition: %v", err)
	}

	r := c.newInPlaceRollout(ms, revision)
	var units, candidates []*inPlaceUnit
	available := 0
	for index, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		unit, err := r.loadUnit(group)
		if err != nil {
			return err
		}
		r.syncStoreRevision(unit)
		if unit.available() {
			available++
		}
		units = append(units, unit)
		if !unit.inUpdate && index >= partition && unit.outdated(revision) {
			candidates = append(candidates, unit)
		}
	}

	// Surge ServingGroups add to the budget, and missing ones take from it.
	budget := available - (modelServingReplicas(ms) - maxUnavailable)
	return r.updateUnits(ctx, units, candidates, budget)
}

// updateUnits continues the units in update, then starts candidates within the budget.
// Unavailable candidates go first, then the highest ordinals.
func (r *inPlaceRollout) updateUnits(ctx context.Context, units, candidates []*inPlaceUnit, budget int) error {
	var errs []error
	for _, unit := range units {
		if unit.inUpdate {
			errs = append(errs, r.updateUnit(ctx, unit))
		}
	}
	for _, wantAvailable := range []bool{false, true} {
		for i := len(candidates) - 1; i >= 0; i-- {
			unit := candidates[i]
			if unit.available() != wantAvailable {
				continue
			}
			if wantAvailable && budget <= 0 {
				klog.V(4).Infof("%s of ModelServing %s/%s waits for the maxUnavailable budget", unit, r.ms.Namespace, r.ms.Name)
				return errors.Join(errs...)
			}
			started, err := r.startUnit(ctx, unit)
			errs = append(errs, err)
			if started && wantAvailable {
				budget--
			}
		}
	}
	return errors.Join(errs...)
}

// manageInPlaceRoleRollout updates outdated Role instances under RoleRollingUpdate.
// Each Role uses its own maxUnavailable and partition, as under Recreate.
func (c *ModelServingController) manageInPlaceRoleRollout(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup, revision string) error {
	partition, _, err := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	if err != nil {
		return fmt.Errorf("failed to calculate partition: %v", err)
	}
	r := c.newInPlaceRollout(ms, revision)
	var errs []error
	for index, group := range groups {
		if group.Status == datastore.ServingGroupDeleting || group.Revision == revision {
			continue
		}
		// A protected ServingGroup only finishes the updates that already started.
		protected := index < partition
		hasOutdated, err := r.manageGroupRoles(ctx, group, protected)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !hasOutdated && !protected {
			c.updateServingGroupRevisionIfNoOutdatedRoles(ms, group.Name, revision)
		}
	}
	return errors.Join(errs...)
}

// manageGroupRoles reports whether any Role instance of the ServingGroup is still outdated.
func (r *inPlaceRollout) manageGroupRoles(ctx context.Context, group datastore.ServingGroup, protected bool) (bool, error) {
	c, ms := r.c, r.ms
	hasOutdated := false
	var errs []error
	for _, roleSpec := range ms.Spec.Template.Roles {
		fallback, err := roleInPlaceFallback(roleSpec)
		if err != nil {
			return true, err
		}
		key := inPlaceFallbackKey{ms: ms.UID, role: roleSpec.Name}
		if fallback == nil {
			c.inPlaceFallbackEvents.Delete(key)
		}
		if fallback == nil || protected {
			outdated, err := r.manageRoleInstances(ctx, group, roleSpec, protected)
			hasOutdated = hasOutdated || outdated
			errs = append(errs, err)
			continue
		}
		rolesToDelete, outdated, err := c.outdatedRolesToDelete(ms, group, roleSpec)
		if err != nil {
			return true, err
		}
		if outdated {
			hasOutdated = true
			c.emitInPlaceFallbackEvent(ms, key, fallback)
		}
		for _, role := range rolesToDelete {
			c.DeleteRole(ctx, ms, group.Name, role.roleName, role.roleID)
		}
	}

	if protected {
		return hasOutdated, errors.Join(errs...)
	}
	allRoles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
	if err != nil {
		return true, fmt.Errorf("failed to get roles for ServingGroup %s: %v", group.Name, err)
	}
	for _, role := range c.removedRolesToDelete(ms, allRoles) {
		hasOutdated = true
		c.DeleteRole(ctx, ms, group.Name, role.roleName, role.roleID)
	}
	return hasOutdated, errors.Join(errs...)
}

func (r *inPlaceRollout) manageRoleInstances(ctx context.Context, group datastore.ServingGroup, roleSpec workloadv1alpha1.Role, groupProtected bool) (bool, error) {
	roleList, err := r.c.store.GetRoleList(utils.GetNamespaceName(r.ms), group.Name, roleSpec.Name)
	if err != nil {
		return true, fmt.Errorf("failed to get roles for ServingGroup %s, role %s: %v", group.Name, roleSpec.Name, err)
	}
	partition, partitionConfigured, err := r.c.getPartition(rolePartition(r.ms, roleSpec), roleReplicas(roleSpec))
	if err != nil {
		return true, fmt.Errorf("failed to parse partition for role %s: %v", roleSpec.Name, err)
	}
	maxUnavailable, budgetConfigured, err := utils.GetMaxUnavailableForRole(roleSpec)
	if err != nil {
		return true, fmt.Errorf("failed to calculate maxUnavailable for role %s: %v", roleSpec.Name, err)
	}

	targetHash := utils.CalRoleTemplateHash(roleSpec)
	hasOutdated := false
	var units, candidates []*inPlaceUnit
	available := 0
	for index, role := range roleList {
		if role.Status == datastore.RoleDeleting {
			continue
		}
		unit, err := r.loadRoleUnit(group, roleSpec.Name, role, targetHash)
		if err != nil {
			return true, err
		}
		r.syncStoreRole(unit)
		if unit.available() {
			available++
		}
		units = append(units, unit)
		if !unit.outdated(r.revision) {
			continue
		}
		hasOutdated = true
		protected := groupProtected || (partitionConfigured && partition > 0 && index < partition)
		if !unit.inUpdate && !protected {
			candidates = append(candidates, unit)
		}
	}

	// Without maxUnavailable, every outdated instance updates at once, as under Recreate.
	budget := len(candidates)
	if budgetConfigured {
		budget = available - (roleReplicas(roleSpec) - maxUnavailable)
	}
	return hasOutdated, r.updateUnits(ctx, units, candidates, budget)
}

func (r *inPlaceRollout) loadUnit(group datastore.ServingGroup) (*inPlaceUnit, error) {
	pods, err := r.c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", r.ms.Namespace, group.Name))
	if err != nil {
		return nil, fmt.Errorf("cannot list pods of ServingGroup %s: %v", group.Name, err)
	}
	return r.newUnit(&inPlaceUnit{group: group}, pods), nil
}

func (r *inPlaceRollout) loadRoleUnit(group datastore.ServingGroup, roleName string, role datastore.Role, targetHash string) (*inPlaceUnit, error) {
	pods, err := r.c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", r.ms.Namespace, group.Name, roleName, role.Name))
	if err != nil {
		return nil, fmt.Errorf("cannot list pods of Role %s/%s in ServingGroup %s: %v", roleName, role.Name, group.Name, err)
	}
	unit := &inPlaceUnit{group: group, role: &inPlaceRole{name: roleName, id: role.Name, status: role.Status, targetHash: targetHash}}
	return r.newUnit(unit, pods), nil
}

func (r *inPlaceRollout) newUnit(unit *inPlaceUnit, pods []*corev1.Pod) *inPlaceUnit {
	slices.SortFunc(pods, func(a, b *corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for _, pod := range pods {
		if !utils.HasModelServingOwner(pod, r.ms.UID) {
			continue
		}
		if utils.IsPodTerminating(pod) {
			unit.terminating = true
			continue
		}
		target, ok := r.target.ForPod(pod)
		if !ok {
			unit.missingTemplate = fmt.Sprintf("pod %s belongs to no Role template of revision %s", pod.Name, r.revision)
			continue
		}
		unit.pods = append(unit.pods, &inplace.UnitPod{Pod: pod, Target: target})
	}
	unit.inUpdate = inplace.InUpdate(unit.rawPods(), r.ms.UID)
	return unit
}

func (r *inPlaceRollout) startUnit(ctx context.Context, unit *inPlaceUnit) (bool, error) {
	if unit.terminating {
		klog.V(4).Infof("%s has terminating pods, so its update waits", unit)
		return false, nil
	}
	fallback, err := r.unitFallback(ctx, unit)
	if err != nil {
		return false, err
	}
	if fallback != nil {
		return true, r.recreate(ctx, unit, fallback)
	}
	if err := r.recordTargetRevision(ctx); err != nil {
		return false, err
	}
	return true, r.updateUnit(ctx, unit)
}

func (r *inPlaceRollout) unitFallback(ctx context.Context, unit *inPlaceUnit) (*inplace.Fallback, error) {
	if unit.missingTemplate != "" {
		return &inplace.Fallback{Reason: inplace.ReasonIneligible, Message: unit.missingTemplate}, nil
	}
	for _, p := range unit.pods {
		fallback, err := r.checkPatchable(ctx, unit, p, utils.ObjectRevision(p.Pod))
		if err != nil || fallback != nil {
			return fallback, err
		}
	}
	return nil, nil
}

// recordTargetRevision stores the target revision, which a later update reads as its source.
func (r *inPlaceRollout) recordTargetRevision(ctx context.Context) error {
	if r.targetRecorded {
		return nil
	}
	if _, err := utils.CreateControllerRevision(ctx, r.c.kubeClientSet, r.ms, r.revision, r.ms.Spec.Template.Roles); err != nil {
		return fmt.Errorf("failed to create ControllerRevision for revision %s: %v", r.revision, err)
	}
	r.targetRecorded = true
	return nil
}

// recreate deletes the ServingGroup or Role instance, which is then created again on the target revision.
func (r *inPlaceRollout) recreate(ctx context.Context, unit *inPlaceUnit, fallback *inplace.Fallback) error {
	klog.V(2).Infof("%s of ModelServing %s/%s is recreated instead of updated in place to revision %s: %s",
		unit, r.ms.Namespace, r.ms.Name, r.revision, fallback)
	r.c.emitRoleStatusEvent(r.ms, corev1.EventTypeNormal, inplace.ReasonFallback,
		fmt.Sprintf("%s is recreated instead of updated in place to revision %s: %s", unit, r.revision, fallback))
	if unit.role != nil {
		r.c.DeleteRole(ctx, r.ms, unit.group.Name, unit.role.name, unit.role.id)
		return nil
	}
	return r.c.deleteServingGroup(ctx, r.ms, unit.group.Name)
}

func (r *inPlaceRollout) updateUnit(ctx context.Context, unit *inPlaceUnit) error {
	if unit.missingTemplate != "" {
		return r.recreate(ctx, unit, &inplace.Fallback{Reason: inplace.ReasonIneligible, Message: unit.missingTemplate})
	}
	// Recreate before the deadline check deletes Pods one by one.
	plan := inplace.PlanUnit(r.ms.UID, r.revision, unit.pods)
	if plan.Step == inplace.StepRecreate {
		return r.recreate(ctx, unit, &inplace.Fallback{Reason: plan.Reason, Message: plan.Message})
	}
	if unit.inUpdate {
		ended, err := r.enforceProgressDeadline(ctx, unit)
		if ended || err != nil {
			return err
		}
	}
	for step := 0; step < maxInPlaceStepsPerSync; step++ {
		if step > 0 {
			plan = inplace.PlanUnit(r.ms.UID, r.revision, unit.pods)
		}
		switch plan.Step {
		case inplace.StepNone:
			return nil
		case inplace.StepRecreate:
			return r.recreate(ctx, unit, &inplace.Fallback{Reason: plan.Reason, Message: plan.Message})
		case inplace.StepVerify:
			klog.V(4).Infof("%s of ModelServing %s/%s waits for verification: %s", unit, r.ms.Namespace, r.ms.Name, plan.Message)
			return nil
		case inplace.StepMark:
			if err := r.mark(ctx, unit, plan.Pods); err != nil {
				return err
			}
		case inplace.StepPatch:
			patched, err := r.patch(ctx, unit, plan.Pods)
			if err != nil || !patched {
				return err
			}
		case inplace.StepComplete:
			return r.complete(ctx, unit, plan.Pods)
		}
	}
	return nil
}

// enforceProgressDeadline ends the update of a unit once its deadline has passed, and then returns true.
func (r *inPlaceRollout) enforceProgressDeadline(ctx context.Context, unit *inPlaceUnit) (bool, error) {
	startedAt, ok := inplace.UnitStartedAt(unit.rawPods(), r.ms.UID)
	if !ok {
		return false, nil
	}
	deadline := inplace.ProgressDeadline(r.ms)
	if remaining := time.Until(startedAt.Add(deadline)); remaining > 0 {
		// No Pod event may arrive before the deadline, so check again when it passes.
		r.c.enqueueModelServingAfter(r.ms, remaining)
		return false, nil
	}
	unverified := inplace.UnverifiedPods(r.ms.UID, r.revision, unit.pods)
	deleted := make(map[types.UID]bool, len(unverified))
	if r.ms.Spec.RecoveryPolicy == workloadv1alpha1.NoneRestartPolicy {
		for _, p := range unverified {
			if inplace.MatchingState(p.Pod).Phase == inplace.PhaseDraining {
				return true, r.recreate(ctx, unit, &inplace.Fallback{Reason: inplace.ReasonProgressDeadlineExceeded,
					Message: fmt.Sprintf("pod %s was not patched within the progress deadline of %s", p.Pod.Name, deadline)})
			}
		}
	} else {
		for _, p := range unverified {
			err := r.c.kubeClientSet.CoreV1().Pods(p.Pod.Namespace).Delete(ctx, p.Pod.Name, *metav1.NewPreconditionDeleteOptions(string(p.Pod.UID)))
			if err != nil && !apierrors.IsNotFound(err) {
				return true, fmt.Errorf("delete pod %s after the progress deadline: %v", p.Pod.Name, err)
			}
			deleted[p.Pod.UID] = true
		}
	}
	var kept, inUpdate []*inplace.UnitPod
	for _, p := range unit.pods {
		if deleted[p.Pod.UID] {
			continue
		}
		kept = append(kept, p)
		if inplace.InUpdate([]*corev1.Pod{p.Pod}, r.ms.UID) {
			inUpdate = append(inUpdate, p)
		}
	}
	unit.pods = kept
	if len(unverified) > 0 {
		klog.V(2).Infof("In-place update of %s of ModelServing %s/%s to revision %s exceeded its progress deadline of %s with %d unverified pods, deleted %d",
			unit, r.ms.Namespace, r.ms.Name, r.revision, deadline, len(unverified), len(deleted))
		r.c.emitRoleStatusEvent(r.ms, corev1.EventTypeWarning, inplace.ReasonProgressDeadlineExceeded,
			fmt.Sprintf("%s was not updated in place to revision %s within %s, so its %d unverified pods follow the RecoveryPolicy %s",
				unit, r.revision, deadline, len(unverified), r.ms.Spec.RecoveryPolicy))
	}
	return true, r.complete(ctx, unit, inUpdate)
}

func (r *inPlaceRollout) mark(ctx context.Context, unit *inPlaceUnit, pods []*inplace.UnitPod) error {
	started := !unit.inUpdate
	for _, p := range pods {
		marked, err := r.updater.Mark(ctx, p.Pod, r.ms.UID, unit.name(), utils.ObjectRevision(p.Pod), r.revision)
		if err != nil {
			return err
		}
		p.Pod = marked
	}
	unit.inUpdate = true
	if started {
		r.c.emitRoleStatusEvent(r.ms, corev1.EventTypeNormal, "InPlaceUpdateStarted",
			fmt.Sprintf("%s started an in-place update to revision %s", unit, r.revision))
	}
	return nil
}

// patch checks every Pod before patching any, so one bad Pod never leaves the unit partly patched.
func (r *inPlaceRollout) patch(ctx context.Context, unit *inPlaceUnit, pods []*inplace.UnitPod) (bool, error) {
	for _, p := range pods {
		fallback, err := r.checkPatchable(ctx, unit, p, inplace.MatchingState(p.Pod).SourceRevision)
		if err != nil {
			return false, err
		}
		if fallback != nil {
			return false, r.recreate(ctx, unit, fallback)
		}
	}
	for _, p := range pods {
		patched, err := r.updater.PatchImages(ctx, p.Pod, p.Target.Images, p.Target.Labels(r.revision))
		if err != nil {
			return false, err
		}
		p.Pod = patched
	}
	r.c.emitRoleStatusEvent(r.ms, corev1.EventTypeNormal, "InPlaceUpdatePatched",
		fmt.Sprintf("Patched the images of %d pods in %s to revision %s", len(pods), unit, r.revision))
	return true, nil
}

func (r *inPlaceRollout) complete(ctx context.Context, unit *inPlaceUnit, pods []*inplace.UnitPod) error {
	for _, p := range pods {
		completed, err := r.updater.Complete(ctx, p.Pod)
		if err != nil {
			return err
		}
		p.Pod = completed
	}
	unit.inUpdate = false
	r.syncStore(unit)
	if len(pods) > 0 {
		r.c.emitRoleStatusEvent(r.ms, corev1.EventTypeNormal, "InPlaceUpdateCompleted",
			fmt.Sprintf("%d pods of %s completed their in-place update to revision %s", len(pods), unit, r.revision))
	}
	r.c.enqueueModelServing(r.ms)
	return nil
}

func (r *inPlaceRollout) checkPatchable(ctx context.Context, unit *inPlaceUnit, p *inplace.UnitPod, sourceRevision string) (*inplace.Fallback, error) {
	var source sourceTemplates
	var err error
	if unit.role != nil {
		source, err = r.roleSource(ctx, sourceRevision, unit.role.name, utils.ObjectRoleTemplateHash(p.Pod))
	} else {
		source, err = r.sourceTemplates(ctx, sourceRevision)
	}
	if err != nil || source.fallback != nil {
		return source.fallback, err
	}
	if fallback := r.checkEligibility(source, unit); fallback != nil {
		return fallback, nil
	}
	template, ok := source.templates.ForPod(p.Pod)
	if !ok {
		return &inplace.Fallback{Reason: inplace.ReasonIneligible,
			Message: fmt.Sprintf("pod %s belongs to no Role template of revision %s", p.Pod.Name, source.revision)}, nil
	}
	return inplace.CheckPatchable(p.Pod, template, p.Target), nil
}

// checkEligibility checks only the Role of a Role instance unit.
func (r *inPlaceRollout) checkEligibility(source sourceTemplates, unit *inPlaceUnit) *inplace.Fallback {
	if source.revision == r.revision {
		return nil
	}
	key := source.revision
	if unit.role != nil {
		key += "/" + unit.role.name
	}
	if cached, ok := r.eligibility[key]; ok {
		return cached
	}
	var eligible bool
	var err error
	if unit.role != nil {
		eligible = inplace.OnlyRoleImagesChanged(source.ms, r.ms, unit.role.name)
	} else {
		eligible, err = inplace.OnlyImagesChanged(source.ms, r.ms)
	}
	var fallback *inplace.Fallback
	switch {
	case err != nil:
		fallback = &inplace.Fallback{Reason: inplace.ReasonIneligible,
			Message: fmt.Sprintf("revision %s cannot be compared with revision %s: %v", source.revision, r.revision, err)}
	case !eligible:
		fallback = &inplace.Fallback{Reason: inplace.ReasonIneligible,
			Message: fmt.Sprintf("revision %s changes fields other than regular-container images from revision %s", r.revision, source.revision)}
	}
	r.eligibility[key] = fallback
	return fallback
}

func (r *inPlaceRollout) sourceTemplates(ctx context.Context, sourceRevision string) (sourceTemplates, error) {
	if cached, ok := r.sources[sourceRevision]; ok {
		return cached, nil
	}
	result := sourceTemplates{revision: sourceRevision}
	switch {
	case sourceRevision == r.revision:
		result.ms, result.templates = r.ms, r.target
	case sourceRevision == "":
		result.fallback = &inplace.Fallback{Reason: inplace.ReasonRevisionHistoryMissing, Message: "a pod has no revision label"}
	default:
		cr, err := utils.GetControllerRevision(ctx, r.c.kubeClientSet, r.ms, sourceRevision)
		if err != nil {
			return result, fmt.Errorf("get ControllerRevision of revision %s: %v", sourceRevision, err)
		}
		if cr == nil {
			result.fallback = &inplace.Fallback{Reason: inplace.ReasonRevisionHistoryMissing,
				Message: fmt.Sprintf("the ControllerRevision of revision %s is missing", sourceRevision)}
			break
		}
		source, err := utils.ModelServingAtRevision(r.ms, cr)
		if err != nil {
			result.fallback = &inplace.Fallback{Reason: inplace.ReasonRevisionHistoryMissing,
				Message: fmt.Sprintf("the ControllerRevision of revision %s cannot be read: %v", sourceRevision, err)}
			break
		}
		result.ms, result.templates = source, inplace.NewRevisionTemplates(source, sourceRevision)
	}
	r.sources[sourceRevision] = result
	return result, nil
}

// roleSource finds the source template of a Role instance by its template hash.
// An unchanged Role keeps an old revision label, whose ControllerRevision may be cleaned up.
func (r *inPlaceRollout) roleSource(ctx context.Context, sourceRevision, roleName, hash string) (sourceTemplates, error) {
	source, err := r.sourceTemplates(ctx, sourceRevision)
	if err != nil || (source.fallback == nil && roleTemplateHash(source.ms, roleName) == hash) {
		return source, err
	}
	if !r.listed {
		r.revisions, err = utils.ListControllerRevisions(ctx, r.c.kubeClientSet, r.ms)
		if err != nil {
			return source, err
		}
		r.listed = true
	}
	for i := range r.revisions {
		revision := r.revisions[i].Labels[utils.ControllerRevisionRevisionLabelKey]
		candidate, err := r.sourceTemplates(ctx, revision)
		if err != nil {
			return source, err
		}
		if candidate.fallback == nil && roleTemplateHash(candidate.ms, roleName) == hash {
			return candidate, nil
		}
	}
	return source, nil
}

func roleTemplateHash(ms *workloadv1alpha1.ModelServing, roleName string) string {
	for _, role := range ms.Spec.Template.Roles {
		if role.Name == roleName {
			return utils.CalRoleTemplateHash(role)
		}
	}
	return ""
}

func (r *inPlaceRollout) syncStore(unit *inPlaceUnit) {
	if unit.role != nil {
		r.syncStoreRole(unit)
		return
	}
	r.syncStoreRevision(unit)
}

// syncStoreRevision keeps a ServingGroup outdated in the store until its update completes.
func (r *inPlaceRollout) syncStoreRevision(unit *inPlaceUnit) {
	want, ok := inplace.UnitRevision(unit.rawPods(), r.ms.UID, r.revision)
	if !ok {
		if len(unit.pods) == 0 || unit.outdated(r.revision) {
			return
		}
		want = r.revision
		r.syncStoreRoles(unit)
	}
	if unit.group.Revision == want {
		return
	}
	if err := r.c.store.UpdateServingGroupRevision(utils.GetNamespaceName(r.ms), unit.group.Name, want); err != nil {
		klog.Errorf("failed to update ServingGroup %s revision: %v", unit.group.Name, err)
		return
	}
	unit.group.Revision = want
	klog.V(2).Infof("Updated ServingGroup %s revision to %s from its pods", unit.group.Name, want)
}

// syncStoreRole moves a Role instance to the target revision in the store once its update completes.
func (r *inPlaceRollout) syncStoreRole(unit *inPlaceUnit) {
	if len(unit.pods) == 0 || unit.outdated(r.revision) {
		return
	}
	if err := r.c.store.UpdateRoleRevision(utils.GetNamespaceName(r.ms), unit.group.Name, unit.role.name, unit.role.id, r.revision, unit.role.targetHash); err != nil {
		klog.Errorf("failed to update the revision of %s: %v", unit, err)
	}
}

func (r *inPlaceRollout) syncStoreRoles(unit *inPlaceUnit) {
	for _, p := range unit.pods {
		roleName, roleID := utils.GetRoleName(p.Pod), utils.GetRoleID(p.Pod)
		if err := r.c.store.UpdateRoleRevision(utils.GetNamespaceName(r.ms), unit.group.Name, roleName, roleID, r.revision, p.Target.RoleTemplateHash); err != nil {
			klog.Errorf("failed to update the revision of Role %s/%s in ServingGroup %s: %v", roleName, roleID, unit.group.Name, err)
		}
	}
}

type inPlaceFallbackKey struct {
	ms   types.UID
	role string
}

// emitInPlaceFallbackEvent reports a fallback once until its reason changes.
func (c *ModelServingController) emitInPlaceFallbackEvent(ms *workloadv1alpha1.ModelServing, key inPlaceFallbackKey, fallback *inplace.Fallback) {
	message := fmt.Sprintf("Outdated resources are recreated instead of updated in place: %s", fallback)
	if last, ok := c.inPlaceFallbackEvents.Swap(key, message); ok && last == message {
		return
	}
	klog.V(2).Infof("ModelServing %s/%s: %s", ms.Namespace, ms.Name, message)
	c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, inplace.ReasonFallback, message)
}

func inPlaceUpdateIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || !inplace.InUpdateWindow(pod) {
		return nil, nil
	}
	name, ok := pod.Labels[workloadv1alpha1.ModelServingNameLabelKey]
	if !ok {
		return nil, nil
	}
	return []string{fmt.Sprintf("%s/%s", pod.Namespace, name)}, nil
}

// syncInPlaceUnitRevisions keeps units with an unfinished in-place update outdated in the store, under any update method.
// Their Pod labels may already show the target revision, for example after the store is rebuilt from them.
func (c *ModelServingController) syncInPlaceUnitRevisions(ms *workloadv1alpha1.ModelServing, revision string) error {
	pods, err := c.getPodsByIndex(InPlaceUpdateKey, fmt.Sprintf("%s/%s", ms.Namespace, ms.Name))
	if err != nil || len(pods) == 0 {
		return err
	}
	type roleKey struct{ group, role, id string }
	groupPods := make(map[string][]*corev1.Pod)
	rolePods := make(map[roleKey][]*corev1.Pod)
	for _, pod := range pods {
		if !utils.IsPodTerminating(pod) {
			groupName := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
			groupPods[groupName] = append(groupPods[groupName], pod)
			key := roleKey{group: groupName, role: utils.GetRoleName(pod), id: utils.GetRoleID(pod)}
			rolePods[key] = append(rolePods[key], pod)
		}
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return err
	}
	for _, group := range groups {
		want, ok := inplace.UnitRevision(groupPods[group.Name], ms.UID, revision)
		if !ok || group.Revision == want {
			continue
		}
		if err := c.store.UpdateServingGroupRevision(utils.GetNamespaceName(ms), group.Name, want); err != nil {
			return err
		}
		klog.V(2).Infof("Updated ServingGroup %s revision to %s, because its in-place update is unfinished", group.Name, want)
	}
	for key, pods := range rolePods {
		hash, ok := inplace.UnitRoleTemplateHash(pods, ms.UID, roleTemplateHash(ms, key.role))
		if !ok || hash == "" {
			continue
		}
		unitRevision, _ := inplace.UnitRevision(pods, ms.UID, revision)
		if err := c.store.UpdateRoleRevision(utils.GetNamespaceName(ms), key.group, key.role, key.id, unitRevision, hash); err != nil {
			klog.Errorf("failed to keep Role %s/%s in ServingGroup %s outdated: %v", key.role, key.id, key.group, err)
		}
	}
	return nil
}
