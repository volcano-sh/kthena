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
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestCoordinationPresenceAndPolicyAreImmutable(t *testing.T) {
	old := &api.ModelServing{Spec: api.ModelServingSpec{RolloutStrategy: &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: &api.RoleCoordination{
		Roles: []string{"a", "b", "c"}, MaxSkew: ptr.To(intstr.FromString("20%")), Dependencies: []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b", "c"}}, {Role: "b", DependsOn: []string{"c"}}},
	}}}}
	require.Empty(t, validateRoleCoordinationImmutable(nil, old), "creation accepts the initial policy")
	empty := old.DeepCopy()
	empty.Spec.RolloutStrategy.RoleCoordination = nil
	require.NotEmpty(t, validateRoleCoordinationImmutable(empty, old), "adding policy after creation is forbidden")
	require.NotEmpty(t, validateRoleCoordinationImmutable(old, empty), "removing policy is forbidden")
	for _, mutate := range []func(*api.RoleCoordination){
		func(c *api.RoleCoordination) { c.MaxSkew = ptr.To(intstr.FromString("30%")) },
		func(c *api.RoleCoordination) { c.Roles = []string{"a", "b"} },
		func(c *api.RoleCoordination) { c.Dependencies = c.Dependencies[:1] },
		func(c *api.RoleCoordination) { c.Dependencies[0].DependsOn = []string{"c"} },
	} {
		updated := old.DeepCopy()
		mutate(updated.Spec.RolloutStrategy.RoleCoordination)
		require.NotEmpty(t, validateRoleCoordinationImmutable(old, updated))
	}
	updated := old.DeepCopy()
	c := updated.Spec.RolloutStrategy.RoleCoordination
	c.Roles = []string{"c", "b", "a"}
	c.Dependencies[0].DependsOn = []string{"c", "b"}
	c.Dependencies[0], c.Dependencies[1] = c.Dependencies[1], c.Dependencies[0]
	before := updated.DeepCopy()
	require.Empty(t, validateRoleCoordinationImmutable(old, updated))
	require.Equal(t, before, updated)
}

func TestEmptyCoordinationSelectorCannotChangeThroughRoleNames(t *testing.T) {
	old := &api.ModelServing{Spec: api.ModelServingSpec{RolloutStrategy: &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: &api.RoleCoordination{MaxSkew: ptr.To(intstr.FromString("20%"))}}, Template: api.ServingGroup{Roles: []api.Role{{Name: "a"}, {Name: "b"}}}}}
	changed := old.DeepCopy()
	changed.Spec.Template.Roles[0].Name = "x"
	require.NotEmpty(t, validateRoleCoordinationImmutable(old, changed))
	changed = old.DeepCopy()
	changed.Spec.Template.Roles = append(changed.Spec.Template.Roles, api.Role{Name: "c"})
	require.NotEmpty(t, validateRoleCoordinationImmutable(old, changed))
	changed = old.DeepCopy()
	changed.Spec.Template.Roles = changed.Spec.Template.Roles[:1]
	require.NotEmpty(t, validateRoleCoordinationImmutable(old, changed))
	changed = old.DeepCopy()
	changed.Spec.Template.Roles[0], changed.Spec.Template.Roles[1] = changed.Spec.Template.Roles[1], changed.Spec.Template.Roles[0]
	require.Empty(t, validateRoleCoordinationImmutable(old, changed))
}
