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
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// coordinatedRoleState is a per-ServingGroup snapshot used only to decide
// which locally allowed Role rolling candidates may start together.
type coordinatedRoleState struct {
	roleName  string
	specIndex int

	target   int
	ready    int
	inFlight int
	active   bool

	candidates []roleToDelete
}

func roleRollingUpdateCoordination(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.RoleRollingUpdateCoordination {
	if ms == nil || ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate ||
		ms.Spec.RolloutStrategy.RoleRollingUpdateConfiguration == nil {
		return nil
	}
	return ms.Spec.RolloutStrategy.RoleRollingUpdateConfiguration.Coordination
}

func coordinatedRoleNames(ms *workloadv1alpha1.ModelServing, coordination *workloadv1alpha1.RoleRollingUpdateCoordination) map[string]struct{} {
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
	coordination := roleRollingUpdateCoordination(ms)
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

func (c *ModelServingController) changedRolesFromRevision(
	ms *workloadv1alpha1.ModelServing,
	revision string,
) (map[string]bool, bool) {
	if c == nil || c.kubeClientSet == nil || revision == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	controllerRevision, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil || controllerRevision == nil {
		return nil, false
	}
	oldRoles, err := utils.GetRolesFromControllerRevision(controllerRevision)
	if err != nil {
		return nil, false
	}
	oldHashByName := make(map[string]string, len(oldRoles))
	for _, role := range oldRoles {
		oldHashByName[role.Name] = utils.CalRoleTemplateHash(role)
	}
	changed := make(map[string]bool, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		oldHash, existed := oldHashByName[role.Name]
		// A newly added Role is ordinary scale-up rather than replacement of an
		// old Role, so it is intentionally not part of coordinated rolling.
		changed[role.Name] = existed && oldHash != utils.CalRoleTemplateHash(role)
	}
	return changed, true
}

func (c *ModelServingController) buildCoordinatedRoleState(
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleSpec workloadv1alpha1.Role,
	roleList []datastore.Role,
	partition int,
	specIndex int,
	active bool,
	candidates []roleToDelete,
) coordinatedRoleState {
	target := roleReplicas(roleSpec) - partition
	if target < 0 {
		target = 0
	}
	expectedHash := utils.CalRoleTemplateHash(roleSpec)
	ready := 0
	inFlight := 0
	present := 0
	for _, role := range roleList {
		_, ordinal := utils.GetParentNameAndOrdinal(role.Name)
		if ordinal < partition {
			continue
		}
		present++
		if role.Status == datastore.RoleDeleting {
			inFlight++
			continue
		}
		observedHash, resolved := c.resolveRoleTemplateHashForComparison(ms, servingGroup, roleSpec.Name, role)
		if !resolved || observedHash != expectedHash {
			continue
		}
		if role.Status == datastore.RoleRunning {
			ready++
		} else {
			inFlight++
		}
	}

	// A slot can temporarily disappear after delete completion and before the
	// replacement Role is recorded. Reserve it so another reconcile cannot reuse
	// the same proportional rollout budget.
	if missing := target - present; missing > 0 {
		inFlight += missing
	}
	if ready > target {
		ready = target
	}
	if inFlight > target-ready {
		inFlight = target - ready
	}

	return coordinatedRoleState{
		roleName:   roleSpec.Name,
		specIndex:  specIndex,
		target:     target,
		ready:      ready,
		inFlight:   inFlight,
		active:     active,
		candidates: candidates,
	}
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

func selectCoordinatedRoleCandidates(
	states []coordinatedRoleState,
	coordination *workloadv1alpha1.RoleRollingUpdateCoordination,
) ([]roleToDelete, error) {
	if coordination == nil {
		return nil, nil
	}
	maxSkewPercent, err := parseMaxSkewPercent(coordination.MaxSkew)
	if err != nil {
		return nil, err
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

	selected := make([]roleToDelete, 0)
	for {
		baseline := minimumReadyProgressState(states)
		chosenIndex := -1
		for i := range states {
			state := &states[i]
			if len(state.candidates) == 0 || !state.active || state.target == 0 {
				continue
			}
			if state.ready+state.inFlight+1 > allowedStartedReplicas(*state, baseline, maxSkewPercent) {
				continue
			}
			if !dependencyAllowsStart(state.roleName, stateByName, dependencies, dependents) {
				continue
			}
			if chosenIndex == -1 || lessStartedProgress(*state, states[chosenIndex]) {
				chosenIndex = i
			}
		}
		if chosenIndex == -1 {
			break
		}

		chosen := &states[chosenIndex]
		selected = append(selected, chosen.candidates[0])
		chosen.candidates = chosen.candidates[1:]
		chosen.inFlight++
	}
	return selected, nil
}

func minimumReadyProgressState(states []coordinatedRoleState) coordinatedRoleState {
	baseline := coordinatedRoleState{ready: 1, target: 1}
	found := false
	for _, state := range states {
		if !state.active || state.target == 0 {
			continue
		}
		if !found || int64(state.ready)*int64(baseline.target) < int64(baseline.ready)*int64(state.target) {
			baseline = state
			found = true
		}
	}
	return baseline
}

func allowedStartedReplicas(state, baseline coordinatedRoleState, maxSkewPercent int) int {
	if state.target <= 0 {
		return 0
	}
	if baseline.target <= 0 {
		return state.target
	}
	numerator := (int64(baseline.ready)*100 + int64(maxSkewPercent)*int64(baseline.target)) * int64(state.target)
	denominator := int64(100 * baseline.target)
	allowed := int((numerator + denominator - 1) / denominator)
	if allowed > state.target {
		return state.target
	}
	return allowed
}

func lessStartedProgress(left, right coordinatedRoleState) bool {
	leftStarted := left.ready + left.inFlight
	rightStarted := right.ready + right.inFlight
	leftProduct := int64(leftStarted) * int64(right.target)
	rightProduct := int64(rightStarted) * int64(left.target)
	if leftProduct != rightProduct {
		return leftProduct < rightProduct
	}
	return left.specIndex < right.specIndex
}

func dependencyAllowsStart(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependencies map[string][]string,
	dependents map[string][]string,
) bool {
	state := states[roleName]
	// A target-version Ready replica is the observable proof that this Role has
	// already completed its startup gate. In-flight work alone is not sufficient:
	// it may have been created by ordinary scaling rather than rollout admission.
	if state == nil || state.ready > 0 {
		return true
	}
	if len(dependencies[roleName]) == 0 {
		return true
	}
	if len(dependents[roleName]) > 0 && allUpstreamRolesNotStarted(roleName, states, dependents, map[string]bool{}) {
		return true
	}
	return dependencyClosureReady(roleName, states, dependencies, map[string]bool{})
}

func allUpstreamRolesNotStarted(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependents map[string][]string,
	visited map[string]bool,
) bool {
	if visited[roleName] {
		return true
	}
	visited[roleName] = true
	for _, upstream := range dependents[roleName] {
		state := states[upstream]
		if state != nil && state.ready+state.inFlight > 0 {
			return false
		}
		if !allUpstreamRolesNotStarted(upstream, states, dependents, visited) {
			return false
		}
	}
	return true
}

func dependencyClosureReady(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependencies map[string][]string,
	visited map[string]bool,
) bool {
	if visited[roleName] {
		return true
	}
	visited[roleName] = true
	for _, dependency := range dependencies[roleName] {
		state := states[dependency]
		if state == nil || state.ready == 0 {
			return false
		}
		if !dependencyClosureReady(dependency, states, dependencies, visited) {
			return false
		}
	}
	return true
}
