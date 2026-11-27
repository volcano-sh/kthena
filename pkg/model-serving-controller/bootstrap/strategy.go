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

// Package bootstrap implements source-aware creation of ModelServing
// ServingGroups for peer-to-peer model weight loading (spec.bootstrapAccelerateStrategy).
package bootstrap

import (
	"slices"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Enabled reports whether ms opts into bootstrap acceleration.
func Enabled(ms *workloadv1alpha1.ModelServing) bool {
	return ms != nil && ms.Spec.BootstrapAccelerateStrategy != nil
}

// Provider returns the effective weight transfer provider.
func Provider(ms *workloadv1alpha1.ModelServing) workloadv1alpha1.BootstrapAccelerateStrategyProvider {
	if !Enabled(ms) {
		return ""
	}
	if ms.Spec.BootstrapAccelerateStrategy.Provider == "" {
		return workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress
	}
	return ms.Spec.BootstrapAccelerateStrategy.Provider
}

// InScope reports whether roleName participates in bootstrap source tracking
// and provider environment injection. An empty roles list selects all roles.
func InScope(ms *workloadv1alpha1.ModelServing, roleName string) bool {
	if !Enabled(ms) {
		return false
	}
	roles := ms.Spec.BootstrapAccelerateStrategy.Roles
	if len(roles) == 0 {
		return true
	}
	return slices.Contains(roles, roleName)
}

// ScopedRoleNames returns the explicit in-scope role names, or nil when all
// roles in spec.template.roles are in scope.
func ScopedRoleNames(ms *workloadv1alpha1.ModelServing) []string {
	if !Enabled(ms) || len(ms.Spec.BootstrapAccelerateStrategy.Roles) == 0 {
		return nil
	}
	return slices.Clone(ms.Spec.BootstrapAccelerateStrategy.Roles)
}
