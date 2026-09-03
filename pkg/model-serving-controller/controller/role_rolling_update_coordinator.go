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
	"strconv"
	"strings"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// coordinatedRoleState is a per-ServingGroup snapshot used to calculate a
// temporary effective partition for a coordinated Role rollout. The user
// partition remains the rollout target; effective partitions only limit how
// far the current reconcile may advance.
type coordinatedRoleState struct {
	roleName string

	userPartition int
	totalToUpdate int
	startedCount  int
	readyCount    int
	targetState   targetVersionState
	hasOldVersion bool
	inProgress    bool
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
	rolloutEnd         int
	effectivePartition int
	remainingDeletions int
	allowTargetStart   bool
	retainOldReplica   bool
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

func (p *roleRolloutGroupPolicy) coordinates(roleName string) bool {
	_, coordinated := p.role(roleName)
	return coordinated
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
		if ordinal >= 0 && ordinal < limits.rolloutEnd {
			updateableOldCount++
		}
		if ordinal >= limits.effectivePartition && ordinal < limits.rolloutEnd {
			eligible = append(eligible, role)
		}
	}

	maxScaleDown = min(maxScaleDown, limits.remainingDeletions)
	if limits.retainOldReplica {
		maxScaleDown = min(maxScaleDown, max(updateableOldCount-1, 0))
	}
	return eligible, maxScaleDown
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
	return existed && utils.CalRoleTemplateHash(oldRole) != utils.CalRoleTemplateHash(role)
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
	if controllerRevision == nil {
		return nil, fmt.Errorf("controller revision %s not found", revision)
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

func (c *ModelServingController) resolveRoleRolloutState(
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleSpec workloadv1alpha1.Role,
	previousDesired int,
	roleList []datastore.Role,
	partition int,
	templateChanged bool,
	terminatingReplicas map[int]string,
) coordinatedRoleState {
	desired := roleReplicas(roleSpec)
	stableEnd := min(previousDesired, desired)
	totalToUpdate := max(stableEnd-partition, 0)
	expectedHash := utils.CalRoleTemplateHash(roleSpec)
	stableTargetReady := 0
	hasTargetWork := false
	hasTargetReady := false
	remainingOldToUpdate := 0
	hasOldVersion := false
	for _, role := range roleList {
		_, ordinal := utils.GetParentNameAndOrdinal(role.Name)
		if ordinal < 0 {
			continue
		}
		inStableRange := ordinal >= partition && ordinal < stableEnd
		observedHash, resolved := c.resolveRoleTemplateHashForComparison(ms, servingGroup, roleSpec.Name, role)
		oldVersion := !resolved || observedHash != expectedHash
		if oldVersion {
			hasOldVersion = true
			if role.Status != datastore.RoleDeleting && inStableRange {
				remainingOldToUpdate++
			}
			continue
		}
		if role.Status == datastore.RoleDeleting || ordinal >= desired {
			continue
		}
		hasTargetWork = true
		if role.Status == datastore.RoleRunning {
			hasTargetReady = true
			if inStableRange {
				stableTargetReady++
			}
		}
	}

	// The datastore is rebuilt from non-terminating Pod events. Keep terminating
	// old capacity in the stable count and dependency state until all Pods vanish.
	for ordinal, hash := range terminatingReplicas {
		if ordinal < 0 {
			continue
		}
		if hash == "" || hash != expectedHash {
			hasOldVersion = true
		}
	}
	// A user partition preserves old-version stable slots. Keep the old-version
	// request path present even if a protected Role is temporarily absent while
	// its old template is being recovered.
	if templateChanged && min(partition, stableEnd) > 0 {
		hasOldVersion = true
	}

	startedCount := totalToUpdate - min(totalToUpdate, remainingOldToUpdate)
	readyCount := min(startedCount, stableTargetReady)
	targetState := targetNotStarted
	if hasTargetReady {
		targetState = targetReady
	} else if hasTargetWork || startedCount > 0 {
		targetState = targetStarted
	}

	return coordinatedRoleState{
		roleName:      roleSpec.Name,
		userPartition: partition,
		totalToUpdate: totalToUpdate,
		startedCount:  startedCount,
		readyCount:    readyCount,
		targetState:   targetState,
		hasOldVersion: hasOldVersion,
		inProgress:    templateChanged && totalToUpdate > 0 && readyCount < totalToUpdate,
	}
}

func (c *ModelServingController) terminatingRoleReplicas(
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
) map[string]map[int]string {
	result := make(map[string]map[int]string)
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
		hash := c.resolveRoleTemplateHash(ms, roleName, pod)
		if result[roleName] == nil {
			result[roleName] = make(map[int]string)
		}
		currentHash, exists := result[roleName][ordinal]
		// If any Pod for the Role ordinal has an unresolved hash, keep the
		// conservative unresolved observation.
		if !exists || (currentHash != "" && hash == "") {
			result[roleName][ordinal] = hash
		}
	}
	return result
}

func (c *ModelServingController) resolveRoleRolloutPolicy(
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
		if servingGroup.Status == datastore.ServingGroupDeleting || servingGroup.Revision == "" || servingGroup.Revision == newRevision {
			continue
		}
		_, servingGroupOrdinal := utils.GetParentNameAndOrdinal(servingGroup.Name)
		if servingGroupOrdinal >= 0 && servingGroupOrdinal < groupPartition {
			continue
		}
		oldRoleByName, cached := oldRolesByRevision[servingGroup.Revision]
		if !cached {
			oldRoleByName, err = c.roleSpecsFromRevision(ms, servingGroup.Revision)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve Role specs from ControllerRevision %s for ServingGroup %s: %w", servingGroup.Revision, servingGroup.Name, err)
			}
			oldRolesByRevision[servingGroup.Revision] = oldRoleByName
		}
		terminatingByRole := c.terminatingRoleReplicas(ms, servingGroup)
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
			states = append(states, c.resolveRoleRolloutState(
				ms,
				servingGroup,
				roleSpec,
				previousDesired,
				roleList,
				partition,
				roleTemplateChanged(oldRoleByName, roleSpec),
				terminatingByRole[roleSpec.Name],
			))
		}
		groupPolicy, err := calculateRoleRolloutLimits(states, coordination)
		if err != nil {
			return nil, fmt.Errorf("failed to coordinate Role rollout in ServingGroup %s: %w", servingGroup.Name, err)
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
		if states[i].inProgress && states[i].totalToUpdate > 0 {
			progressingRoleCount++
		}
	}
	policy := roleRolloutGroupPolicy{
		roles: make(map[string]roleRolloutLimits, len(states)),
	}
	for i := range states {
		state := &states[i]
		policy.inProgress = policy.inProgress || state.inProgress
		var dependencyBlocker, retentionBlocker, skewBlocker *coordinatedRoleBlocker
		limits := roleRolloutLimits{
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
			allowedStarted := state.totalToUpdate
			if progressingRoleCount > 1 {
				allowedStarted = allowedStartedReplicas(*state, baseline, maxSkewPercent)
			}
			// userPartition is already included in totalToUpdate, so this is the
			// final proportional boundary rather than an independent partition to
			// combine with userPartition again.
			limits.effectivePartition = state.userPartition + state.totalToUpdate - allowedStarted
			limits.remainingDeletions = max(allowedStarted-state.startedCount, 0)
			if state.startedCount >= allowedStarted && state.startedCount < state.totalToUpdate {
				skewBlocker = &coordinatedRoleBlocker{
					reason: coordinatedRoleMaxSkewLimitReached,
					message: fmt.Sprintf("Role %s has started %d of %d update-eligible replicas and reached the current maxSkew allowance of %d",
						state.roleName, state.startedCount, state.totalToUpdate, allowedStarted),
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
	baseline := coordinatedRoleState{readyCount: 1, totalToUpdate: 1}
	found := false
	for _, state := range states {
		if !state.inProgress || state.totalToUpdate == 0 {
			continue
		}
		if !found || int64(state.readyCount)*int64(baseline.totalToUpdate) < int64(baseline.readyCount)*int64(state.totalToUpdate) {
			baseline = state
			found = true
		}
	}
	return baseline
}

func allowedStartedReplicas(state, baseline coordinatedRoleState, maxSkewPercent int) int {
	if state.totalToUpdate <= 0 {
		return 0
	}
	if baseline.totalToUpdate <= 0 {
		return state.totalToUpdate
	}
	baselineProgressWithSkew := int64(baseline.readyCount)*100 + int64(maxSkewPercent)*int64(baseline.totalToUpdate)
	numerator := baselineProgressWithSkew * int64(state.totalToUpdate)
	denominator := int64(100 * baseline.totalToUpdate)
	allowed := int((numerator + denominator - 1) / denominator)
	if allowed > state.totalToUpdate {
		return state.totalToUpdate
	}
	return allowed
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
