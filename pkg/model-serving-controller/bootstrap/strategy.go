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

// Package bootstrap implements source-aware creation of ModelServing role
// replicas for peer-to-peer model weight loading (spec.bootstrapAccelerateStrategy).
package bootstrap

import (
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Enabled reports whether ms opts into bootstrap acceleration.
func Enabled(ms *workloadv1alpha1.ModelServing) bool {
	return ms != nil && ms.Spec.BootstrapAccelerateStrategy != nil && len(ms.Spec.BootstrapAccelerateStrategy.Roles) > 0
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

// RoleConfig returns the bootstrap configuration of roleName, if it is in scope.
func RoleConfig(ms *workloadv1alpha1.ModelServing, roleName string) (*workloadv1alpha1.BootstrapRole, bool) {
	if !Enabled(ms) {
		return nil, false
	}
	for i := range ms.Spec.BootstrapAccelerateStrategy.Roles {
		if ms.Spec.BootstrapAccelerateStrategy.Roles[i].Name == roleName {
			return &ms.Spec.BootstrapAccelerateStrategy.Roles[i], true
		}
	}
	return nil, false
}

// ScopedRoleNames returns the in-scope role names in declaration order.
func ScopedRoleNames(ms *workloadv1alpha1.ModelServing) []string {
	if !Enabled(ms) {
		return nil
	}
	names := make([]string, 0, len(ms.Spec.BootstrapAccelerateStrategy.Roles))
	for _, role := range ms.Spec.BootstrapAccelerateStrategy.Roles {
		names = append(names, role.Name)
	}
	return names
}
