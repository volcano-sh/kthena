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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestRoleRolloutScaleUpStartsNewServingGroupFromCurrentRevision(t *testing.T) {
	tests := []struct {
		name        string
		coordinated bool
		partition   *intstr.IntOrString
		finished    bool
		wantCurrent bool
	}{
		{name: "coordinated canary", coordinated: true, partition: ptr.To(intstr.FromInt(1)), wantCurrent: true},
		{name: "partitioned rollout", partition: ptr.To(intstr.FromInt(1)), wantCurrent: true},
		{name: "coordinated rollout", coordinated: true, wantCurrent: true},
		{name: "independent rollout uses target"},
		{name: "zero partition uses target", partition: ptr.To(intstr.FromInt(0))},
		{name: "finished rollout uses target", coordinated: true, partition: ptr.To(intstr.FromInt(1)), finished: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := createStandardModelServing("scale-up", 1, 2)
			old.UID = "scale-up-owner"
			decode := *old.Spec.Template.Roles[0].DeepCopy()
			decode.Name = "decode"
			old.Spec.Template.Roles = append(old.Spec.Template.Roles, decode)
			old.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
			if tt.coordinated {
				old.Spec.RolloutStrategy.RoleCoordination = &api.RoleCoordination{
					MaxSkew:      ptr.To(intstr.FromString("50%")),
					Dependencies: []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}},
				}
			}
			ms := old.DeepCopy()
			ms.Spec.Replicas = ptr.To[int32](2)
			for i := range ms.Spec.Template.Roles {
				ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "test-image:v2"
				ms.Spec.Template.Roles[i].Partition = tt.partition
			}
			oldRevision, newRevision := utils.ModelServingRevision(old), utils.ModelServingRevision(ms)
			existing := old
			ms.Status.CurrentRevision = oldRevision
			if tt.finished {
				existing = ms
				ms.Status.CurrentRevision = newRevision
			}
			ms.Status.UpdateRevision = newRevision
			c := newRevisionTestController(t, ms)
			recordTestRevision(t, c.kubeClientSet, old, oldRevision)
			require.NoError(t, c.persistCoordinatedRoleRevision(context.Background(), ms, newRevision))
			c.podGroupManager = &fakePodGroupManager{}
			for _, role := range existing.Spec.Template.Roles {
				for ordinal := 0; ordinal < 2; ordinal++ {
					addExpansionInstance(t, c, existing, "scale-up-0", role.Name, ordinal, true)
				}
			}

			ctx := c.withRevisionHistory(context.Background(), ms)
			require.NoError(t, c.syncServingGroupReplicas(ctx, ms, newRevision))

			want := newRevision
			if tt.wantCurrent {
				want = oldRevision
			}
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: api.GroupNameLabelKey + "=scale-up-1"})
			require.NoError(t, err)
			require.Len(t, pods.Items, 4)
			for _, pod := range pods.Items {
				require.Equal(t, want, utils.ObjectRevision(&pod), "Pod %s", pod.Name)
			}
			revision, ok := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), "scale-up-1")
			require.True(t, ok)
			require.Equal(t, want, revision)
		})
	}
}
