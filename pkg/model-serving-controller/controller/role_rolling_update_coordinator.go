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
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// coordinatedRoleState is a per-ServingGroup snapshot used to calculate a
// temporary effective partition for a coordinated Role rollout. The user
// partition remains the rollout target; effective partitions only limit how
// far the current reconcile may advance.
type coordinatedRoleState struct {
	roleName         string
	stablePopulation *roleStablePopulation

	userPartition int
	totalToUpdate int
	startedCount  int
	readyCount    int
	targetState   targetVersionState
	hasOldVersion bool
	inProgress    bool
	// Version capacity is independent of the partition-limited work ledger.
	versionTotal   int
	versionReady   int
	versionStarted int
	versionOld     int
	versionChanged bool
	tailReady      bool
}

type targetVersionState uint8

const (
	targetNotStarted targetVersionState = iota
	targetStarted
	targetReady
)

type coordinatedRoleBlocker struct {
	servingGroupName string
	reason           string
	message          string
}

// roleRolloutLimits is the small execution contract consumed by the existing
// Role creation and deletion paths.
type roleRolloutLimits struct {
	stablePopulation   *roleStablePopulation
	rolloutEnd         int
	effectivePartition int
	remainingDeletions int
	allowTargetStart   bool
	retainOldReplica   bool
}

func (l roleRolloutLimits) containsStable(ordinal int) bool {
	if l.stablePopulation != nil {
		return l.stablePopulation.contains(ordinal)
	}
	return ordinal >= 0 && ordinal < l.rolloutEnd
}

// roleRolloutGroupPolicy contains the Role limits derived from one immutable
// ServingGroup snapshot.
type roleRolloutGroupPolicy struct {
	roles      map[string]roleRolloutLimits
	blocker    *coordinatedRoleBlocker
	inProgress bool
}

func (p *roleRolloutGroupPolicy) role(roleName string) (roleRolloutLimits, bool) {
	if p == nil {
		return roleRolloutLimits{}, false
	}
	role, exists := p.roles[roleName]
	return role, exists
}

// constrainRoleDeletion applies the optional cross-Role policy after the
// existing Role rolling-update path has calculated its maxUnavailable budget.
func (p *roleRolloutGroupPolicy) constrainRoleDeletion(
	roleName string,
	outdatedRoles []datastore.Role,
	maxScaleDown int,
) ([]datastore.Role, int) {
	limits, coordinated := p.role(roleName)
	if !coordinated {
		return outdatedRoles, maxScaleDown
	}

	updateableOldCount := 0
	eligible := make([]datastore.Role, 0, len(outdatedRoles))
	for _, role := range outdatedRoles {
		_, ordinal := utils.GetParentNameAndOrdinal(role.Name)
		if limits.containsStable(ordinal) {
			updateableOldCount++
		}
	}
	for _, role := range outdatedRoles {
		_, ordinal := utils.GetParentNameAndOrdinal(role.Name)
		if ordinal < limits.effectivePartition || !limits.containsStable(ordinal) {
			// The caller supplies a descending stable prefix. A blocked high
			// ordinal must never be filtered away to expose a lower candidate.
			break
		}
		eligible = append(eligible, role)
	}

	maxScaleDown = min(maxScaleDown, limits.remainingDeletions)
	if limits.retainOldReplica {
		maxScaleDown = min(maxScaleDown, max(updateableOldCount-1, 0))
	}
	return eligible, maxScaleDown
}

// selectRoleRolloutCandidates separates obsolete temporary capacity from the
// stable ordinal prefix. Both consume the same Q/B ledger, but removing a surge
// does not spend or return a stable maxSkew start allowance.
func (c *ModelServingController) selectRoleRolloutCandidates(
	ctx context.Context, ms *workloadv1alpha1.ModelServing, group datastore.ServingGroup,
	role workloadv1alpha1.Role, instances, outdated []datastore.Role, budget rolloutBudget,
	policy *roleRolloutGroupPolicy,
) ([]roleToDelete, error) {
	if roleCoordination(ms) == nil {
		return selectBudgetedRolesToDelete(role.Name, outdated, budget, false), nil
	}
	var pods []*corev1.Pod
	if c.podsInformer != nil {
		var err error
		pods, err = c.surgePods(ms, surgeRole, group.Name, role.Name)
		if err != nil {
			return nil, err
		}
	}
	temporary := markedSurgeNames(pods, surgeRole, roleReplicas(role))
	limits, _ := policy.role(role.Name)
	remainingOld := len(outdated)
	var selected []roleToDelete
	stable := make([]datastore.Role, 0, len(outdated))
	for _, instance := range outdated {
		if !temporary.Has(instance.Name) {
			stable = append(stable, instance)
			continue
		}
		// Keep a necessary last old dependency. A marked replica inside the
		// latest desired range has already been adopted and is not temporary.
		if limits.retainOldReplica && remainingOld <= 1 {
			continue
		}
		if budget.take(instance.Status == datastore.RoleRunning) {
			selected = append(selected, roleToDelete{roleName: role.Name, roleID: instance.Name})
			remainingOld--
		}
	}
	// Unknown history is a possible old stable instance, not permission to
	// skip it. Known target replicas and already-started deletions are handled
	// by the availability and progress ledgers instead.
	unknownOrdinal := -1
	for _, instance := range instances {
		_, ordinal := utils.GetParentNameAndOrdinal(instance.Name)
		if !temporary.Has(instance.Name) && instance.Status != datastore.RoleDeleting &&
			c.compareRoleTemplate(ctx, ms, group, role.Name, instance) == templateUnknown {
			unknownOrdinal = max(unknownOrdinal, ordinal)
		}
	}
	for i, instance := range stable {
		_, ordinal := utils.GetParentNameAndOrdinal(instance.Name)
		if ordinal <= unknownOrdinal {
			stable = stable[:i]
			break
		}
	}
	stable, budget.total = policy.constrainRoleDeletion(role.Name, stable, budget.total)
	selected = append(selected, selectBudgetedRolesToDelete(role.Name, stable, budget, true)...)
	return selected, nil
}

// roleRolloutPolicy is the reconcile-scoped view of optional cross-Role
// constraints. Independent Role rolling updates use the same object as a no-op
// policy, keeping coordination details out of the controller's main flow.
type roleRolloutPolicy struct {
	enabled    bool
	inProgress bool
	groups     map[string]*roleRolloutGroupPolicy
	blocker    *coordinatedRoleBlocker
}

const (
	coordinatedRoleDependencyNotReady          = "DependencyNotReady"
	coordinatedRoleMaxSkewLimitReached         = "MaxSkewLimitReached"
	coordinatedRoleOldVersionDependencyPresent = "OldVersionDependencyPresent"
	coordinatedRoleIncompatiblePartitions      = "IncompatiblePartitions"
	coordinatedRoleReplicaBaselineAnnotation   = "modelserving.volcano.sh/coordinated-role-replica-baseline"
)

func (p *roleRolloutPolicy) group(name string) *roleRolloutGroupPolicy {
	if p == nil {
		return nil
	}
	return p.groups[name]
}

func (p *roleRolloutPolicy) allowTargetStart(groupName, roleName string) bool {
	groupPolicy := p.group(groupName)
	if groupPolicy == nil {
		return true
	}
	limits, exists := groupPolicy.role(roleName)
	return !exists || limits.allowTargetStart
}

func (p *roleRolloutPolicy) addGroup(
	groupName string,
	groupPolicy roleRolloutGroupPolicy,
) {
	if p == nil || !p.enabled {
		return
	}
	if p.groups == nil {
		p.groups = make(map[string]*roleRolloutGroupPolicy)
	}
	p.groups[groupName] = &groupPolicy
	p.inProgress = p.inProgress || groupPolicy.inProgress
	// ServingGroups are resolved in ordinal order, so the first blocker is the
	// deterministic status reported for this reconcile.
	if p.blocker == nil && groupPolicy.blocker != nil {
		blocker := *groupPolicy.blocker
		blocker.servingGroupName = groupName
		p.blocker = &blocker
	}
}

func (p *roleRolloutPolicy) setCondition(ms *workloadv1alpha1.ModelServing) (bool, *metav1.Condition) {
	if ms == nil {
		return false, nil
	}
	conditionType := string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked)
	existing := apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType)
	if (p == nil || !p.enabled) && existing == nil {
		return false, nil
	}

	condition := metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		Reason:             "RolloutComplete",
		Message:            "Coordinated Role rollout is complete",
		ObservedGeneration: ms.Generation,
	}
	if p != nil && p.enabled && p.blocker != nil {
		blocker := p.blocker
		condition.Status = metav1.ConditionTrue
		condition.Reason = blocker.reason
		condition.Message = fmt.Sprintf("ServingGroup %s: %s", blocker.servingGroupName, blocker.message)
		// Keep a bounded diagnostic, independent of the number of dependencies.
		if text := []rune(condition.Message); len(text) > 512 {
			condition.Message = string(text[:509]) + "..."
		}
	} else if p != nil && p.enabled && p.inProgress {
		condition.Reason = "ProgressAvailable"
		condition.Message = "Coordinated Role rollout can make progress"
	}

	changed := existing == nil || existing.Status != condition.Status || existing.Reason != condition.Reason ||
		existing.Message != condition.Message || existing.ObservedGeneration != condition.ObservedGeneration
	apiMeta.SetStatusCondition(&ms.Status.Conditions, condition)
	updated := apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType)
	return changed, updated
}

func roleCoordination(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.RoleCoordination {
	if ms == nil || ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return nil
	}
	return ms.Spec.RolloutStrategy.RoleCoordination
}

// persistCoordinatedRoleRevision keeps the replica baseline attached to each
// template revision current. Replica-only scaling does not change the revision
// hash, while ControllerRevision.Data is immutable, so the mutable annotation
// records the latest stable size without rewriting the template snapshot.
func (c *ModelServingController) persistCoordinatedRoleRevision(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	revision string,
) error {
	if roleCoordination(ms) == nil {
		return nil
	}
	controllerRevision, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil {
		return err
	}
	if controllerRevision == nil {
		controllerRevision, err = utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, revision, ms.Spec.Template.Roles)
		if err != nil {
			return err
		}
	}

	if !metav1.IsControlledBy(controllerRevision, ms) {
		return fmt.Errorf("ControllerRevision %s belongs to another ModelServing UID", revision)
	}
	replicasByRole := make(map[string]int32, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		replicasByRole[role.Name] = int32(roleReplicas(role))
	}
	baseline, err := json.Marshal(replicasByRole)
	if err != nil {
		return fmt.Errorf("failed to marshal Role replica baseline: %w", err)
	}
	if controllerRevision.Annotations[coordinatedRoleReplicaBaselineAnnotation] == string(baseline) {
		return nil
	}
	copy := controllerRevision.DeepCopy()
	if copy.Annotations == nil {
		copy.Annotations = make(map[string]string)
	}
	copy.Annotations[coordinatedRoleReplicaBaselineAnnotation] = string(baseline)
	_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, copy, metav1.UpdateOptions{})
	return err
}

func coordinatedRoleNames(ms *workloadv1alpha1.ModelServing, coordination *workloadv1alpha1.RoleCoordination) map[string]struct{} {
	if coordination == nil {
		return nil
	}
	selected := make(map[string]struct{}, len(ms.Spec.Template.Roles))
	if len(coordination.Roles) == 0 {
		for _, role := range ms.Spec.Template.Roles {
			selected[role.Name] = struct{}{}
		}
		return selected
	}
	for _, roleName := range coordination.Roles {
		selected[roleName] = struct{}{}
	}
	return selected
}

func isCoordinatedRole(ms *workloadv1alpha1.ModelServing, roleName string) bool {
	coordination := roleCoordination(ms)
	if coordination == nil {
		return false
	}
	if len(coordination.Roles) == 0 {
		return true
	}
	for _, configuredRoleName := range coordination.Roles {
		if configuredRoleName == roleName {
			return true
		}
	}
	return false
}

func roleTemplateChanged(oldRoles map[string]workloadv1alpha1.Role, role workloadv1alpha1.Role) bool {
	oldRole, existed := oldRoles[role.Name]
	return existed && !utils.EqualRoleTemplateForRevision(oldRole, role)
}

func (c *ModelServingController) roleSpecsFromRevision(
	ms *workloadv1alpha1.ModelServing,
	revision string,
) (map[string]workloadv1alpha1.Role, error) {
	if c == nil || c.kubeClientSet == nil || revision == "" {
		return nil, fmt.Errorf("controller client and revision are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	controllerRevision, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil {
		return nil, err
	}
	if controllerRevision == nil || !metav1.IsControlledBy(controllerRevision, ms) {
		return nil, fmt.Errorf("controller revision %s is missing or not controlled by the current ModelServing UID", revision)
	}
	oldRoles, err := utils.GetRolesFromControllerRevision(controllerRevision)
	if err != nil {
		return nil, err
	}
	if baseline := controllerRevision.Annotations[coordinatedRoleReplicaBaselineAnnotation]; baseline != "" {
		var replicasByRole map[string]int32
		if err := json.Unmarshal([]byte(baseline), &replicasByRole); err != nil {
			return nil, fmt.Errorf("failed to parse Role replica baseline from ControllerRevision %s: %w", revision, err)
		}
		for i := range oldRoles {
			if replicas, exists := replicasByRole[oldRoles[i].Name]; exists {
				oldRoles[i].Replicas = &replicas
			}
		}
	}
	oldRoleByName := make(map[string]workloadv1alpha1.Role, len(oldRoles))
	for _, role := range oldRoles {
		oldRoleByName[role.Name] = role
	}
	return oldRoleByName, nil
}

// A stable population is the ordinary range with sparse retained identities
// replacing holes. Store only deviations, not O(desiredReplicas) virtual slots.
type roleStablePopulation struct {
	end   int
	holes sets.Set[int]
	extra []int
}

func (p *roleStablePopulation) contains(ordinal int) bool {
	return (ordinal >= 0 && ordinal < p.end && !p.holes.Has(ordinal)) || slices.Contains(p.extra, ordinal)
}
func (p *roleStablePopulation) countFrom(partition int) int {
	count := max(0, p.end-partition)
	for ordinal := range p.holes {
		if ordinal >= partition {
			count--
		}
	}
	for _, ordinal := range p.extra {
		if ordinal >= partition {
			count++
		}
	}
	return count
}
func (p *roleStablePopulation) upperBound() int {
	if len(p.extra) > 0 {
		return p.extra[len(p.extra)-1] + 1
	}
	return p.end
}
func (p *roleStablePopulation) boundary(partition, offset int) int {
	if offset >= p.countFrom(partition) {
		return p.upperBound()
	}
	candidate := partition + offset
	baseCount := max(0, p.end-partition)
	for _, hole := range sets.List(p.holes) {
		if hole < partition {
			continue
		}
		baseCount--
		if hole <= candidate {
			candidate++
		}
	}
	if offset < baseCount {
		return candidate
	}
	offset -= baseCount
	for _, ordinal := range p.extra {
		if ordinal < partition {
			continue
		}
		if offset == 0 {
			return ordinal
		}
		offset--
	}
	return p.upperBound()
}

// Missing replacements reserve low holes. Temporary surge never participates
// in stable progress. Observed formal expansion members join the population;
// uncreated expansion slots do not reserve a template replacement start.
func (c *ModelServingController) stableRolePopulation(ms *workloadv1alpha1.ModelServing, group string, role workloadv1alpha1.Role, previousDesired int, instances []datastore.Role, terminating map[int]templateComparison) (*roleStablePopulation, error) {
	desired := roleReplicas(role)
	var pods []*corev1.Pod
	if c.podsInformer != nil {
		var err error
		pods, err = c.surgePods(ms, surgeRole, group, role.Name)
		if err != nil {
			return nil, err
		}
	}
	temporary := markedSurgeNames(pods, surgeRole, desired)
	ordinals := sets.New[int]()
	for _, instance := range instances {
		_, ordinal := utils.GetParentNameAndOrdinal(instance.Name)
		if ordinal >= 0 && !temporary.Has(instance.Name) {
			ordinals.Insert(ordinal)
		}
	}
	for ordinal := range terminating {
		if ordinal >= 0 && !temporary.Has(utils.GenerateRoleID(role.Name, ordinal)) {
			ordinals.Insert(ordinal)
		}
	}
	ordered := sets.List(ordinals)
	if len(ordered) > desired {
		ordered = ordered[:desired]
		ordinals = sets.New(ordered...)
	}
	population := &roleStablePopulation{end: min(previousDesired, desired), holes: sets.New[int]()}
	retainedOutsideDesired := 0
	for _, ordinal := range ordered {
		if ordinal >= population.end {
			population.extra = append(population.extra, ordinal)
		}
		if ordinal >= desired {
			retainedOutsideDesired++
		}
	}
	for ordinal := population.end - 1; population.holes.Len() < retainedOutsideDesired && ordinal >= 0; ordinal-- {
		if !ordinals.Has(ordinal) {
			population.holes.Insert(ordinal)
		}
	}
	return population, nil
}

func (c *ModelServingController) resolveRoleRolloutState(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleSpec workloadv1alpha1.Role,
	previousDesired int,
	roleList []datastore.Role,
	partition int,
	templateChanged bool,
	terminatingReplicas map[int]templateComparison,
	stablePopulation *roleStablePopulation,
) coordinatedRoleState {
	desired := roleReplicas(roleSpec)
	if stablePopulation == nil {
		stablePopulation = &roleStablePopulation{end: min(previousDesired, desired)}
	}
	totalToUpdate := stablePopulation.countFrom(partition)
	protectedOld := stablePopulation.countFrom(0) > totalToUpdate
	stableTargetReady := 0
	hasTargetWork := false
	hasTargetReady := false
	hasCommittedReplacement := false
	remainingOldToUpdate := 0
	hasOldVersion := false
	versionReady, outsideEligibleStarted, versionOld, formalObserved, protectedObserved := 0, 0, 0, 0, 0
	tailReady := len(terminatingReplicas) == 0
	for _, role := range roleList {
		_, ordinal := utils.GetParentNameAndOrdinal(role.Name)
		if ordinal < 0 {
			continue
		}
		inStableRange := ordinal >= partition && stablePopulation.contains(ordinal)
		formal := stablePopulation.contains(ordinal) || (ordinal >= previousDesired && ordinal < desired)
		if formal {
			formalObserved++
		}
		if stablePopulation.contains(ordinal) && ordinal < partition {
			protectedObserved++
		}
		_, terminating := terminatingReplicas[ordinal]
		if inStableRange && (terminating || role.Status == datastore.RoleDeleting) {
			hasCommittedReplacement = true
		}
		comparison := c.compareRoleTemplate(ctx, ms, servingGroup, roleSpec.Name, role)
		oldVersion := comparison != templateEquivalent
		if comparison == templateUnknown || role.Status != datastore.RoleRunning || terminating {
			tailReady = false
		}
		if oldVersion {
			hasOldVersion = true
			if formal {
				versionOld++
			} else {
				// An old temporary/excess instance still carries an old path.
				tailReady = false
			}
			if role.Status != datastore.RoleDeleting && !terminating && inStableRange {
				remainingOldToUpdate++
			}
			continue
		}
		if formal && !inStableRange {
			outsideEligibleStarted++
		}
		if role.Status == datastore.RoleDeleting || terminating || (ordinal >= desired && !stablePopulation.contains(ordinal)) {
			continue
		}
		hasTargetWork = true
		if role.Status == datastore.RoleRunning {
			hasTargetReady = true
			if formal {
				versionReady++
			}
			if inStableRange {
				stableTargetReady++
			}
		}
	}

	// The datastore is rebuilt from non-terminating Pod events. Keep terminating
	// old capacity in the stable count and dependency state until all Pods vanish.
	for ordinal, comparison := range terminatingReplicas {
		if ordinal < 0 {
			continue
		}
		if comparison != templateEquivalent {
			hasOldVersion = true
		}
	}
	// A user partition preserves old-version stable slots. Keep the old-version
	// request path present even if a protected Role is temporarily absent while
	// its old template is being recovered.
	if templateChanged && protectedOld && protectedObserved < stablePopulation.countFrom(0)-totalToUpdate {
		hasOldVersion = true
	}

	startedCount := totalToUpdate - min(totalToUpdate, remainingOldToUpdate)
	readyCount := min(startedCount, stableTargetReady)
	targetState := targetNotStarted
	if hasTargetReady {
		targetState = targetReady
	} else if hasTargetWork || hasCommittedReplacement {
		targetState = targetStarted
	}

	return coordinatedRoleState{
		stablePopulation: stablePopulation,
		roleName:         roleSpec.Name,
		userPartition:    partition,
		totalToUpdate:    totalToUpdate,
		startedCount:     startedCount,
		readyCount:       readyCount,
		targetState:      targetState,
		hasOldVersion:    hasOldVersion,
		inProgress:       templateChanged && totalToUpdate > 0 && readyCount < totalToUpdate,
		versionTotal:     desired,
		versionReady:     min(desired, versionReady),
		versionStarted:   min(desired, startedCount+outsideEligibleStarted),
		versionOld:       versionOld,
		versionChanged:   templateChanged,
		tailReady:        tailReady && formalObserved == desired && versionReady+versionOld == desired,
	}
}

func (c *ModelServingController) terminatingRoleReplicas(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
) map[string]map[int]templateComparison {
	result := make(map[string]map[int]templateComparison)
	if c == nil || c.podsInformer == nil {
		return result
	}
	indexValue := fmt.Sprintf("%s/%s", ms.Namespace, servingGroup.Name)
	pods, err := c.getPodsByIndex(GroupNameKey, indexValue)
	if err != nil {
		// The coordination calculation remains usable in unit tests and during
		// informer initialization. A later reconcile will observe the Pods.
		return result
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil {
			continue
		}
		roleName := utils.GetRoleName(pod)
		_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(pod))
		if roleName == "" || ordinal < 0 {
			continue
		}
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		comparison := c.compareRoleTemplate(ctx, ms, servingGroup, roleName, datastore.Role{
			Revision: utils.ObjectRevision(pod), RoleTemplateHash: utils.ObjectRoleTemplateHash(pod),
		})
		if result[roleName] == nil {
			result[roleName] = make(map[int]templateComparison)
		}
		current, exists := result[roleName][ordinal]
		// Every Pod must match before terminating capacity is classified as target.
		if !exists || current == templateEquivalent {
			result[roleName][ordinal] = comparison
		}
	}
	return result
}

func (c *ModelServingController) resolveRoleRolloutPolicy(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	newRevision string,
) (*roleRolloutPolicy, error) {
	coordination := roleCoordination(ms)
	if coordination == nil {
		return &roleRolloutPolicy{}, nil
	}
	policy := &roleRolloutPolicy{enabled: true}
	servingGroups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		return nil, fmt.Errorf("failed to get ServingGroups for ModelServing %s/%s: %w", ms.Namespace, ms.Name, err)
	}
	coordinatedNames := coordinatedRoleNames(ms, coordination)
	groupPartition, _, err := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	if err != nil {
		return nil, fmt.Errorf("failed to parse ModelServing partition: %w", err)
	}
	oldRolesByRevision := make(map[string]map[string]workloadv1alpha1.Role)
	for _, servingGroup := range servingGroups {
		if servingGroup.Status == datastore.ServingGroupDeleting || servingGroup.Revision == "" {
			continue
		}
		if ms.Status.CurrentRevision == newRevision && c.compareServingGroupTemplate(ctx, ms, servingGroup, newRevision) == templateEquivalent {
			continue
		}
		_, servingGroupOrdinal := utils.GetParentNameAndOrdinal(servingGroup.Name)
		if servingGroupOrdinal >= 0 && servingGroupOrdinal < groupPartition {
			continue
		}
		// The group retains its observed identity after independent Role
		// updates. The completed ModelServing revision is the baseline for a
		// subsequent rollout, not a stale group-level label.
		baselineRevision := servingGroup.Revision
		if ms.Status.CurrentRevision != "" && ms.Status.CurrentRevision != newRevision {
			baselineRevision = ms.Status.CurrentRevision
		}
		oldRoleByName, cached := oldRolesByRevision[baselineRevision]
		if !cached {
			oldRoleByName, err = c.roleSpecsFromRevision(ms, baselineRevision)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve Role specs from ControllerRevision %s for ServingGroup %s: %w", servingGroup.Revision, servingGroup.Name, err)
			}
			oldRolesByRevision[baselineRevision] = oldRoleByName
		}
		oldRoles := make([]workloadv1alpha1.Role, 0, len(oldRoleByName))
		for _, role := range oldRoleByName {
			oldRoles = append(oldRoles, role)
		}
		if utils.EqualRoleTemplatesForRevision(oldRoles, ms.Spec.Template.Roles) {
			continue
		}
		terminatingByRole := c.terminatingRoleReplicas(ctx, ms, servingGroup)
		states := make([]coordinatedRoleState, 0, len(coordinatedNames))
		for _, roleSpec := range ms.Spec.Template.Roles {
			if _, coordinated := coordinatedNames[roleSpec.Name]; !coordinated {
				continue
			}
			roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), servingGroup.Name, roleSpec.Name)
			if err != nil {
				return nil, fmt.Errorf("failed to get roles for ServingGroup %s, role %s: %w", servingGroup.Name, roleSpec.Name, err)
			}
			partition, _, err := c.getPartition(rolePartition(ms, roleSpec), roleReplicas(roleSpec))
			if err != nil {
				return nil, fmt.Errorf("failed to parse partition for Role %s: %w", roleSpec.Name, err)
			}
			previousDesired := 0
			if oldRole, existed := oldRoleByName[roleSpec.Name]; existed {
				previousDesired = roleReplicas(oldRole)
			}
			stablePopulation, err := c.stableRolePopulation(ms, servingGroup.Name, roleSpec, previousDesired, roleList, terminatingByRole[roleSpec.Name])
			if err != nil {
				return nil, err
			}
			states = append(states, c.resolveRoleRolloutState(ctx,
				ms,
				servingGroup,
				roleSpec,
				previousDesired,
				roleList,
				partition,
				roleTemplateChanged(oldRoleByName, roleSpec),
				terminatingByRole[roleSpec.Name],
				stablePopulation,
			))
		}
		groupPolicy, err := calculateRoleRolloutLimits(states, coordination)
		if err != nil {
			return nil, fmt.Errorf("failed to coordinate Role rollout in ServingGroup %s: %w", servingGroup.Name, err)
		}
		if _, conflict := utils.CoordinatedPartitionConflict(ms); conflict != nil {
			// Preserve already-issued replacements, but do not select more old
			// stable instances for a legacy incompatible configuration.
			for name, limits := range groupPolicy.roles {
				limits.remainingDeletions = 0
				limits.effectivePartition = limits.rolloutEnd
				groupPolicy.roles[name] = limits
			}
			groupPolicy.blocker = &coordinatedRoleBlocker{reason: coordinatedRoleIncompatiblePartitions, message: conflict.Error()}
		}
		policy.addGroup(servingGroup.Name, groupPolicy)
	}
	return policy, nil
}

func parseMaxSkewPercent(value *intstr.IntOrString) (int, error) {
	if value == nil || value.Type != intstr.String || !strings.HasSuffix(value.StrVal, "%") {
		return 0, fmt.Errorf("maxSkew must be a percentage string")
	}
	percent, err := strconv.Atoi(strings.TrimSuffix(value.StrVal, "%"))
	if err != nil || percent <= 0 || percent > 100 {
		return 0, fmt.Errorf("maxSkew must be greater than 0%% and no greater than 100%%")
	}
	return percent, nil
}

// calculateRoleRolloutLimits converts dependency and maxSkew constraints into
// per-Role limits without mutating the user-supplied partition.
func calculateRoleRolloutLimits(
	states []coordinatedRoleState,
	coordination *workloadv1alpha1.RoleCoordination,
) (roleRolloutGroupPolicy, error) {
	if coordination == nil {
		return roleRolloutGroupPolicy{}, nil
	}
	maxSkewPercent, err := parseMaxSkewPercent(coordination.MaxSkew)
	if err != nil {
		return roleRolloutGroupPolicy{}, err
	}

	stateByName := make(map[string]*coordinatedRoleState, len(states))
	for i := range states {
		stateByName[states[i].roleName] = &states[i]
	}
	dependencies := make(map[string][]string, len(coordination.Dependencies))
	dependents := make(map[string][]string, len(coordination.Dependencies))
	for _, dependency := range coordination.Dependencies {
		dependencies[dependency.Role] = append(dependencies[dependency.Role], dependency.DependsOn...)
		for _, dependencyRole := range dependency.DependsOn {
			dependents[dependencyRole] = append(dependents[dependencyRole], dependency.Role)
		}
	}

	baseline := slowestReadyProgress(states)
	progressingRoleCount := 0
	for i := range states {
		if states[i].versionChanged && states[i].versionTotal > 0 {
			progressingRoleCount++
		}
	}
	terminal := fullRoleRolloutTailReady(states)
	policy := roleRolloutGroupPolicy{
		roles: make(map[string]roleRolloutLimits, len(states)),
	}
	for i := range states {
		state := &states[i]
		policy.inProgress = policy.inProgress || state.inProgress
		var dependencyBlocker, retentionBlocker, skewBlocker *coordinatedRoleBlocker
		limits := roleRolloutLimits{
			stablePopulation:   state.stablePopulation,
			rolloutEnd:         state.userPartition + state.totalToUpdate,
			effectivePartition: state.userPartition,
			allowTargetStart:   true,
		}

		// Only rollout roots wait for the dependency closure on their first
		// target-version start. Internal Roles may start together, allowing the
		// target-version dependency chain to become Ready from the bottom up.
		if state.targetState == targetNotStarted && len(dependents[state.roleName]) == 0 {
			unready := unreadyDependencyClosure(state.roleName, stateByName, dependencies)
			if len(unready) > 0 {
				limits.allowTargetStart = false
				limits.effectivePartition = state.userPartition + state.totalToUpdate
				dependencyBlocker = &coordinatedRoleBlocker{
					reason:  coordinatedRoleDependencyNotReady,
					message: fmt.Sprintf("root Role %s is waiting for target-version RoleRunning capacity from Roles %s", state.roleName, strings.Join(unready, ", ")),
				}
			}
		}

		if state.inProgress && state.totalToUpdate > 0 && limits.allowTargetStart {
			allowedStarted := state.versionTotal
			if progressingRoleCount > 1 {
				allowedStarted = allowedStartedReplicas(*state, baseline, maxSkewPercent)
			}
			limits.remainingDeletions = max(allowedStarted-state.versionStarted, 0)
			if terminal && limits.remainingDeletions == 0 && state.versionOld == 1 &&
				len(oldDirectDependents(state.roleName, stateByName, dependents)) == 0 {
				for _, name := range dependencies[state.roleName] {
					if dependency := stateByName[name]; dependency != nil && dependency.hasOldVersion {
						limits.remainingDeletions = 1
						klog.V(2).InfoS("Allowing one full-rollout terminal rounding step", "role", state.roleName,
							"targetReady", state.versionReady, "total", state.versionTotal, "maxSkew", maxSkewPercent)
						break
					}
				}
			}
			allowedEligible := min(state.totalToUpdate, state.startedCount+limits.remainingDeletions)
			limits.effectivePartition = state.userPartition + state.totalToUpdate - allowedEligible
			if limits.remainingDeletions == 0 && state.startedCount < state.totalToUpdate {
				skewBlocker = &coordinatedRoleBlocker{
					reason: coordinatedRoleMaxSkewLimitReached,
					message: fmt.Sprintf("Role %s has started %d of %d formal target replicas and reached the current maxSkew allowance of %d",
						state.roleName, state.versionStarted, state.versionTotal, allowedStarted),
				}
			}
		}

		// Keep one old dependency while a direct old-version caller remains.
		// A user partition of at least one already provides this boundary.
		oldDependents := oldDirectDependents(state.roleName, stateByName, dependents)
		if state.hasOldVersion && state.userPartition == 0 && len(oldDependents) > 0 {
			limits.retainOldReplica = true
			retentionIsBinding := limits.effectivePartition == 0
			limits.effectivePartition = max(limits.effectivePartition, 1)
			if retentionIsBinding {
				retentionBlocker = &coordinatedRoleBlocker{
					reason: coordinatedRoleOldVersionDependencyPresent,
					message: fmt.Sprintf("Role %s is retaining an old replica while old dependent Roles %s still exist",
						state.roleName, strings.Join(oldDependents, ", ")),
				}
			}
		}
		// Convert the count boundary to an actual ordinal after skew and
		// dependencies. A sparse ordinal is not a replica count.
		if state.stablePopulation != nil {
			limits.rolloutEnd = state.stablePopulation.upperBound()
			offset := max(0, limits.effectivePartition-state.userPartition)
			limits.effectivePartition = state.stablePopulation.boundary(state.userPartition, offset)
		}
		policy.roles[state.roleName] = limits
		if policy.blocker == nil {
			for _, blocker := range []*coordinatedRoleBlocker{dependencyBlocker, retentionBlocker, skewBlocker} {
				if blocker != nil {
					policy.blocker = blocker
					break
				}
			}
		}
	}
	return policy, nil
}

func slowestReadyProgress(states []coordinatedRoleState) coordinatedRoleState {
	baseline := coordinatedRoleState{versionReady: 1, versionTotal: 1}
	found := false
	for _, state := range states {
		if !state.versionChanged || state.versionTotal == 0 {
			continue
		}
		if !found || int64(state.versionReady)*int64(baseline.versionTotal) < int64(baseline.versionReady)*int64(state.versionTotal) {
			baseline = state
			found = true
		}
	}
	return baseline
}

func allowedStartedReplicas(state, baseline coordinatedRoleState, maxSkewPercent int) int {
	if state.versionTotal <= 0 {
		return 0
	}
	if baseline.versionTotal <= 0 {
		return state.versionTotal
	}
	progress := uint64(baseline.versionReady)*100 + uint64(maxSkewPercent)*uint64(baseline.versionTotal)
	denominator := uint64(baseline.versionTotal) * 100
	if progress >= denominator {
		return state.versionTotal
	}
	// The product can exceed int64 for legal int32 replica counts.
	hi, lo := bits.Mul64(progress, uint64(state.versionTotal))
	allowed, remainder := bits.Div64(hi, lo, denominator)
	if remainder != 0 {
		allowed++
	}
	return int(allowed)
}

func fullRoleRolloutTailReady(states []coordinatedRoleState) bool {
	participants := 0
	for _, state := range states {
		if state.versionTotal == 0 {
			continue
		}
		// Unchanged templates do not constrain version progress, but their
		// unavailable or unknown capacity cannot justify a terminal exception.
		if !state.tailReady {
			return false
		}
		if !state.versionChanged {
			continue
		}
		participants++
		if state.userPartition != 0 || state.versionOld > 1 ||
			state.versionReady+state.versionOld != state.versionTotal || state.versionStarted != state.versionReady {
			return false
		}
	}
	return participants > 1
}

func oldDirectDependents(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependents map[string][]string,
) []string {
	var oldDependents []string
	for _, dependentRole := range dependents[roleName] {
		dependent := states[dependentRole]
		if dependent != nil && dependent.hasOldVersion {
			oldDependents = append(oldDependents, dependentRole)
		}
	}
	return oldDependents
}

func unreadyDependencyClosure(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependencies map[string][]string,
) []string {
	visited := make(map[string]bool)
	reported := make(map[string]bool)
	var result []string
	var visit func(string)
	visit = func(current string) {
		if visited[current] {
			return
		}
		visited[current] = true
		for _, dependencyRole := range dependencies[current] {
			dependency := states[dependencyRole]
			if (dependency == nil || dependency.targetState != targetReady) && !reported[dependencyRole] {
				reported[dependencyRole] = true
				result = append(result, dependencyRole)
			}
			visit(dependencyRole)
		}
	}
	visit(roleName)
	return result
}
