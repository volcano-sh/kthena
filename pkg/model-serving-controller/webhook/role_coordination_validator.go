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

package webhook

import (
	"fmt"
	"strconv"
	"strings"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func validateRoleCoordination(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.RoleCoordination == nil {
		return allErrs
	}

	coordination := ms.Spec.RolloutStrategy.RoleCoordination
	coordinationPath := field.NewPath("spec", "rolloutStrategy", "roleCoordination")
	if ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		allErrs = append(allErrs, field.Forbidden(coordinationPath,
			"coordination is only valid when rolloutStrategy.type is RoleRollingUpdate"))
	}

	maxSkewPath := coordinationPath.Child("maxSkew")
	if coordination.MaxSkew == nil {
		allErrs = append(allErrs, field.Required(maxSkewPath, "maxSkew is required"))
	} else if coordination.MaxSkew.Type != intstr.String {
		allErrs = append(allErrs, field.Invalid(maxSkewPath, coordination.MaxSkew, "must be a percentage string, for example 10%"))
	} else {
		percentErrors := validation.IsValidPercent(coordination.MaxSkew.StrVal)
		for _, msg := range percentErrors {
			allErrs = append(allErrs, field.Invalid(maxSkewPath, coordination.MaxSkew, msg))
		}
		if len(percentErrors) == 0 {
			percent, err := strconv.Atoi(strings.TrimSuffix(coordination.MaxSkew.StrVal, "%"))
			if err != nil || percent <= 0 || percent > 100 {
				allErrs = append(allErrs, field.Invalid(maxSkewPath, coordination.MaxSkew, "must be greater than 0% and no greater than 100%"))
			}
		}
	}

	roleNames := make(map[string]struct{}, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		roleNames[role.Name] = struct{}{}
	}

	selectedRoles := make(map[string]struct{}, len(roleNames))
	rolesPath := coordinationPath.Child("roles")
	if len(coordination.Roles) == 0 {
		for roleName := range roleNames {
			selectedRoles[roleName] = struct{}{}
		}
	} else {
		for i, roleName := range coordination.Roles {
			rolePath := rolesPath.Index(i)
			if _, exists := selectedRoles[roleName]; exists {
				allErrs = append(allErrs, field.Duplicate(rolePath, roleName))
				continue
			}
			if _, exists := roleNames[roleName]; !exists {
				allErrs = append(allErrs, field.NotFound(rolePath, roleName))
				continue
			}
			selectedRoles[roleName] = struct{}{}
		}
	}
	if len(selectedRoles) < 2 {
		allErrs = append(allErrs, field.Invalid(rolesPath, coordination.Roles, "at least two Roles must participate in coordination"))
	}

	graph := make(map[string][]string, len(selectedRoles))
	dependencyOwnerSeen := make(map[string]struct{}, len(coordination.Dependencies))
	dependenciesPath := coordinationPath.Child("dependencies")
	for i, dependency := range coordination.Dependencies {
		dependencyPath := dependenciesPath.Index(i)
		if _, exists := dependencyOwnerSeen[dependency.Role]; exists {
			allErrs = append(allErrs, field.Duplicate(dependencyPath.Child("role"), dependency.Role))
		} else {
			dependencyOwnerSeen[dependency.Role] = struct{}{}
		}
		if _, exists := roleNames[dependency.Role]; !exists {
			allErrs = append(allErrs, field.NotFound(dependencyPath.Child("role"), dependency.Role))
		} else if _, selected := selectedRoles[dependency.Role]; !selected {
			allErrs = append(allErrs, field.Invalid(dependencyPath.Child("role"), dependency.Role, "must belong to the coordinated Role set"))
		}

		dependsOnSeen := make(map[string]struct{}, len(dependency.DependsOn))
		for j, dependencyRole := range dependency.DependsOn {
			dependsOnPath := dependencyPath.Child("dependsOn").Index(j)
			if _, exists := dependsOnSeen[dependencyRole]; exists {
				allErrs = append(allErrs, field.Duplicate(dependsOnPath, dependencyRole))
				continue
			}
			dependsOnSeen[dependencyRole] = struct{}{}
			if dependencyRole == dependency.Role {
				allErrs = append(allErrs, field.Invalid(dependsOnPath, dependencyRole, "a Role cannot depend on itself"))
				continue
			}
			if _, exists := roleNames[dependencyRole]; !exists {
				allErrs = append(allErrs, field.NotFound(dependsOnPath, dependencyRole))
				continue
			}
			if _, selected := selectedRoles[dependencyRole]; !selected {
				allErrs = append(allErrs, field.Invalid(dependsOnPath, dependencyRole, "must belong to the coordinated Role set"))
				continue
			}
			graph[dependency.Role] = append(graph[dependency.Role], dependencyRole)
		}
	}
	if roleDependencyGraphHasCycle(graph, selectedRoles) {
		allErrs = append(allErrs, field.Invalid(dependenciesPath, coordination.Dependencies, "dependency graph must be acyclic"))
	}

	return allErrs
}

// validateRoleCoordinationUpdate rejects updates whose dependency graph cannot
// produce target-version capacity from the stable Role population. It is
// intentionally update-only: the old Role templates and replica counts define
// both the old request path and the replacement population.
func validateRoleCoordinationUpdate(
	oldMS, newMS *workloadv1alpha1.ModelServing,
) field.ErrorList {
	var allErrs field.ErrorList
	coordination := roleCoordination(newMS)
	if oldMS == nil || coordination == nil {
		return allErrs
	}

	oldRoles := roleSpecsByName(oldMS.Spec.Template.Roles)
	newRoles := roleSpecsByName(newMS.Spec.Template.Roles)
	selected := selectedRoleNames(coordination, newRoles)
	requirements := dependencyCapacityRequirements(coordination, selected, oldRoles, newRoles)
	if coordinatedRoleRolloutMayBeActive(oldMS) {
		allErrs = append(allErrs, validateInProgressDependencyCapacity(newMS, coordination, selected, oldRoles)...)
	}

	rolesPath := field.NewPath("spec", "template", "roles")
	for roleIndex, newRole := range newMS.Spec.Template.Roles {
		roleName := newRole.Name
		retainOld, required := requirements[roleName]
		if !required {
			continue
		}
		newReplicas := replicasOrDefault(newRole.Replicas)
		oldReplicas := int32(0)
		if oldRole, existed := oldRoles[roleName]; existed {
			oldReplicas = replicasOrDefault(oldRole.Replicas)
		}
		partition := 0
		if newRole.Partition != nil {
			resolved, err := intstr.GetScaledValueFromIntOrPercent(newRole.Partition, int(newReplicas), true)
			if err != nil {
				continue
			}
			partition = resolved
		}
		rolePath := rolesPath.Index(roleIndex)
		if partition >= int(newReplicas) {
			allErrs = append(allErrs, field.Invalid(rolePath.Child("partition"), newRole.Partition,
				fmt.Sprintf("changed dependency Role %q must leave target-version capacity outside partition", roleName)))
			continue
		}

		retentionFloor := 0
		if retainOld {
			retentionFloor = 1
		}
		stableBase := min(int(oldReplicas), int(newReplicas))
		replacementCapacity := max(stableBase-max(partition, retentionFloor), 0)
		expansionCapacity := max(int(newReplicas)-max(int(oldReplicas), partition), 0)
		if replacementCapacity+expansionCapacity == 0 {
			allErrs = append(allErrs, field.Invalid(rolePath, roleName,
				fmt.Sprintf("changed dependency Role %q cannot create target-version capacity while retaining the old request path; increase replicas or stage a scale-up before changing the Role template", roleName)))
		}
	}
	return allErrs
}

// coordinatedRoleRolloutMayBeActive is conservative while status is stale: a
// spec update can arrive before the controller has observed the generation and
// published the coordinated rollout condition.
func coordinatedRoleRolloutMayBeActive(ms *workloadv1alpha1.ModelServing) bool {
	if ms == nil || roleCoordination(ms) == nil {
		return false
	}
	if ms.Status.ObservedGeneration < ms.Generation {
		return true
	}
	condition := apiMeta.FindStatusCondition(ms.Status.Conditions, string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked))
	return condition != nil && condition.Reason != "RolloutComplete"
}

// validateInProgressDependencyCapacity prevents a staged scale-down or
// partition increase from removing the target-version bootstrap slot after a
// coordinated rollout has already started. A dependency with old callers must
// be able to retain one old stable slot and still expose one target slot.
func validateInProgressDependencyCapacity(
	newMS *workloadv1alpha1.ModelServing,
	coordination *workloadv1alpha1.RoleCoordination,
	selected map[string]struct{},
	oldRoles map[string]workloadv1alpha1.Role,
) field.ErrorList {
	var allErrs field.ErrorList
	dependencyRoles := make(map[string]struct{})
	for _, dependency := range coordination.Dependencies {
		if _, participates := selected[dependency.Role]; !participates {
			continue
		}
		for _, dependencyRole := range dependency.DependsOn {
			if _, participates := selected[dependencyRole]; participates {
				dependencyRoles[dependencyRole] = struct{}{}
			}
		}
	}

	rolesPath := field.NewPath("spec", "template", "roles")
	for roleIndex, newRole := range newMS.Spec.Template.Roles {
		if _, isDependency := dependencyRoles[newRole.Name]; !isDependency {
			continue
		}
		oldRole, existed := oldRoles[newRole.Name]
		if !existed || !roleCapacitySettingChanged(oldRole, newRole) {
			continue
		}
		newReplicas := int(replicasOrDefault(newRole.Replicas))
		partition, err := resolvedRolePartition(newRole)
		if err != nil {
			continue
		}
		if max(partition, 1) < newReplicas {
			continue
		}
		allErrs = append(allErrs, field.Invalid(rolesPath.Index(roleIndex), newRole.Name,
			fmt.Sprintf("dependency Role %q must retain capacity for one old and one target replica while coordinated rollout is in progress; wait for rollout completion or increase replicas/reduce partition", newRole.Name)))
	}
	return allErrs
}

func roleCapacitySettingChanged(oldRole, newRole workloadv1alpha1.Role) bool {
	if replicasOrDefault(oldRole.Replicas) != replicasOrDefault(newRole.Replicas) {
		return true
	}
	oldPartition, oldErr := resolvedRolePartition(oldRole)
	newPartition, newErr := resolvedRolePartition(newRole)
	return oldErr == nil && newErr == nil && oldPartition != newPartition
}

func resolvedRolePartition(role workloadv1alpha1.Role) (int, error) {
	if role.Partition == nil {
		return 0, nil
	}
	return intstr.GetScaledValueFromIntOrPercent(role.Partition, int(replicasOrDefault(role.Replicas)), true)
}

func roleSpecsByName(roles []workloadv1alpha1.Role) map[string]workloadv1alpha1.Role {
	result := make(map[string]workloadv1alpha1.Role, len(roles))
	for _, role := range roles {
		result[role.Name] = role
	}
	return result
}

func selectedRoleNames(
	coordination *workloadv1alpha1.RoleCoordination,
	roles map[string]workloadv1alpha1.Role,
) map[string]struct{} {
	selected := make(map[string]struct{}, len(roles))
	if len(coordination.Roles) == 0 {
		for roleName := range roles {
			selected[roleName] = struct{}{}
		}
		return selected
	}
	for _, roleName := range coordination.Roles {
		if _, exists := roles[roleName]; exists {
			selected[roleName] = struct{}{}
		}
	}
	return selected
}

// dependencyCapacityRequirements returns Roles that must provide target-version
// capacity. The bool value is true when one old-version replica must also remain.
func dependencyCapacityRequirements(
	coordination *workloadv1alpha1.RoleCoordination,
	selected map[string]struct{},
	oldRoles, newRoles map[string]workloadv1alpha1.Role,
) map[string]bool {
	dependencies := make(map[string][]string, len(coordination.Dependencies))
	hasDependent := make(map[string]bool, len(selected))
	for _, dependency := range coordination.Dependencies {
		if _, exists := selected[dependency.Role]; !exists {
			continue
		}
		for _, dependencyRole := range dependency.DependsOn {
			if _, exists := selected[dependencyRole]; !exists {
				continue
			}
			dependencies[dependency.Role] = append(dependencies[dependency.Role], dependencyRole)
			hasDependent[dependencyRole] = true
		}
	}

	templateChanged := func(roleName string) bool {
		newRole, exists := newRoles[roleName]
		if !exists {
			return false
		}
		oldRole, existed := oldRoles[roleName]
		return !existed || utils.CalRoleTemplateHash(oldRole) != utils.CalRoleTemplateHash(newRole)
	}

	// Presence means target capacity is required; true additionally means old
	// capacity must be retained.
	required := make(map[string]bool)
	for roleName := range selected {
		if hasDependent[roleName] || !templateChanged(roleName) {
			continue
		}
		visited := make(map[string]bool)
		var visit func(string)
		visit = func(current string) {
			if visited[current] {
				return
			}
			visited[current] = true
			for _, dependencyRole := range dependencies[current] {
				if templateChanged(dependencyRole) {
					if _, exists := required[dependencyRole]; !exists {
						required[dependencyRole] = false
					}
				}
				visit(dependencyRole)
			}
		}
		visit(roleName)
	}

	for caller, dependencyRoles := range dependencies {
		if !templateChanged(caller) {
			continue
		}
		if _, existed := oldRoles[caller]; !existed {
			continue
		}
		for _, dependencyRole := range dependencyRoles {
			if templateChanged(dependencyRole) {
				required[dependencyRole] = true
			}
		}
	}
	return required
}

func roleCoordination(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.RoleCoordination {
	if ms == nil || ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return nil
	}
	return ms.Spec.RolloutStrategy.RoleCoordination
}

func roleDependencyGraphHasCycle(graph map[string][]string, roles map[string]struct{}) bool {
	const (
		visiting = iota + 1
		visited
	)
	state := make(map[string]int, len(roles))
	var visit func(string) bool
	visit = func(roleName string) bool {
		switch state[roleName] {
		case visiting:
			return true
		case visited:
			return false
		}
		state[roleName] = visiting
		for _, dependency := range graph[roleName] {
			if visit(dependency) {
				return true
			}
		}
		state[roleName] = visited
		return false
	}
	for roleName := range roles {
		if visit(roleName) {
			return true
		}
	}
	return false
}
