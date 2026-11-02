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

package utils

import (
	"slices"
	"strings"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
)

// Compare the same workload fields as the upstream hash, without changing the
// persisted hash scheme when coordination is enabled.
func EqualRoleTemplateForRevision(a, b api.Role) bool {
	return apiequality.Semantic.DeepEqual(removeRoleReplicasForRoleTemplateHash(a), removeRoleReplicasForRoleTemplateHash(b))
}
func EqualRoleTemplatesForRevision(a, b []api.Role) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = slices.Clone(a), slices.Clone(b)
	order := func(x, y api.Role) int { return strings.Compare(x.Name, y.Name) }
	slices.SortFunc(a, order)
	slices.SortFunc(b, order)
	for i := range a {
		if !EqualRoleTemplateForRevision(a[i], b[i]) {
			return false
		}
	}
	return true
}
