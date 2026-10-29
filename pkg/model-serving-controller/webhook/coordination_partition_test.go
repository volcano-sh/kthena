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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestCoordinatedPartitionNominalProportions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		n     []int32
		p     []intstr.IntOrString
		valid bool
	}{
		{"reported mismatch", []int32{4, 4, 4}, []intstr.IntOrString{intstr.FromInt(2), intstr.FromInt(0), intstr.FromInt(0)}, false},
		{"equal percent rounded independently", []int32{8, 4, 10}, []intstr.IntOrString{intstr.FromString("10%"), intstr.FromString("10%"), intstr.FromString("10%")}, true},
		{"mixed rounding", []int32{8, 4, 10}, []intstr.IntOrString{intstr.FromString("10%"), intstr.FromInt(1), intstr.FromInt(1)}, true},
		{"integer common interval", []int32{8, 4, 10}, []intstr.IntOrString{intstr.FromInt(1), intstr.FromInt(1), intstr.FromInt(1)}, true},
		{"open boundary rejects", []int32{2, 4}, []intstr.IntOrString{intstr.FromInt(1), intstr.FromInt(3)}, false},
		{"zero does not constrain", []int32{0, 4}, []intstr.IntOrString{intstr.FromInt(0), intstr.FromInt(2)}, true},
		{"different nominal percentages", []int32{4, 4}, []intstr.IntOrString{intstr.FromString("10%"), intstr.FromString("20%")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &api.ModelServing{Spec: api.ModelServingSpec{RolloutStrategy: &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: &api.RoleCoordination{}}}}
			for i, n := range tc.n {
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, api.Role{Name: string(rune('a' + i)), Replicas: ptr.To(n), RollingUpdateConfiguration: api.RollingUpdateConfiguration{Partition: ptr.To(tc.p[i])}})
			}
			before := ms.DeepCopy()
			_, err := utils.CoordinatedPartitionConflict(ms)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "common nominal proportion")
			}
			require.Equal(t, before, ms)
		})
	}
}

func TestLegacyPartitionMismatchAllowsOnlyUnrelatedUpdates(t *testing.T) {
	ms := &api.ModelServing{Spec: api.ModelServingSpec{RolloutStrategy: &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: &api.RoleCoordination{}}, Template: api.ServingGroup{Roles: []api.Role{
		{Name: "a", Replicas: ptr.To[int32](4), RollingUpdateConfiguration: api.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(2))}}, {Name: "b", Replicas: ptr.To[int32](4)},
	}}}}
	require.NotEmpty(t, validateCoordinatedPartitions(nil, ms))
	updated := ms.DeepCopy()
	updated.Annotations = map[string]string{"note": "unrelated"}
	require.Empty(t, validateCoordinatedPartitions(ms, updated))
	ms.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"a", "b"}
	updated.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"b", "a"}
	require.Empty(t, validateCoordinatedPartitions(ms, updated), "semantic set reordering is still permitted")
	updated.Spec.Template.Roles[1].Replicas = ptr.To[int32](5)
	require.NotEmpty(t, validateCoordinatedPartitions(ms, updated))
	updated = ms.DeepCopy()
	updated.Spec.Template.Roles[0].EntryTemplate.Metadata = &api.Metadata{Annotations: map[string]string{"version": "new"}}
	require.NotEmpty(t, validateCoordinatedPartitions(ms, updated))
	updated.Spec.Template.Roles[1].Partition = ptr.To(intstr.FromInt(2))
	require.Empty(t, validateCoordinatedPartitions(ms, updated))
	ms.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"b"}
	require.Empty(t, validateCoordinatedPartitions(nil, ms), "unselected a does not constrain the selected partition ratio")
}
