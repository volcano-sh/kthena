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
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestCoordinatedVersionRatioIncludesPartitionAndCompletedRoles(t *testing.T) {
	for _, p := range []int{2, 4} {
		t.Run(string(rune('0'+p)), func(t *testing.T) {
			a := newCoordinatedRoleStateForTest("a", 4-p, 4-p, 0)
			a.userPartition, a.versionTotal, a.versionChanged = p, 4, true
			b := newCoordinatedRoleStateForTest("b", 4, 3, 0)
			policy, err := calculateRoleRolloutLimits([]coordinatedRoleState{a, b}, coordinationForTest("20%"))
			require.NoError(t, err)
			require.Zero(t, policy.roles["b"].remainingDeletions, "a remains a baseline even after its eligible work ends")
			require.Equal(t, p, policy.roles["a"].effectivePartition)
		})
	}
}

func TestCoordinatedVersionCountsProtectedTargetAndFormalExpansion(t *testing.T) {
	role := api.Role{Name: "a", Replicas: ptr.To[int32](4)}
	ms := &api.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ratios", Namespace: "default", UID: "ratios"}, Spec: api.ModelServingSpec{Template: api.ServingGroup{Roles: []api.Role{role}}}}
	c := &ModelServingController{kubeClientSet: kubefake.NewSimpleClientset()}
	recordDifferentRevision(t, c, ms, "old")
	hash := utils.CalRoleTemplateHash(role)
	state := c.resolveRoleRolloutState(context.Background(), ms, datastore.ServingGroup{Name: "ratios-0"}, role, 3,
		[]datastore.Role{
			{Name: "a-0", RoleTemplateHash: hash, Status: datastore.RoleRunning}, // protected target
			{Name: "a-1", Revision: "old", Status: datastore.RoleRunning},
			{Name: "a-2", RoleTemplateHash: hash, Status: datastore.RoleCreating},
			{Name: "a-3", RoleTemplateHash: hash, Status: datastore.RoleRunning}, // formal expansion
			{Name: "a-4", RoleTemplateHash: hash, Status: datastore.RoleRunning}, // surge
		}, 1, true, nil, nil)
	require.Equal(t, 4, state.versionTotal)
	require.Equal(t, 2, state.versionReady)
	require.Equal(t, 3, state.versionStarted)
	require.Equal(t, 2, state.totalToUpdate)
	require.Zero(t, state.readyCount, "neither protected nor expansion/surge satisfies eligible replacement readiness")
	require.False(t, state.tailReady)
}

func TestCoordinatedFullTailRounding(t *testing.T) {
	coord := coordinationForTest("10%")
	coord.Dependencies = []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}, {Role: "decode", DependsOn: []string{"fff"}}}
	for _, tc := range []struct {
		name  string
		alter func([]coordinatedRoleState)
		allow bool
	}{
		{"ready full tail", func([]coordinatedRoleState) {}, true},
		{"target not ready", func(s []coordinatedRoleState) { s[1].versionReady--; s[1].tailReady = false }, false},
		{"in flight", func(s []coordinatedRoleState) { s[0].tailReady = false }, false},
		{"partitioned", func(s []coordinatedRoleState) { s[0].userPartition = 1 }, false},
		{"old temporary or unknown", func(s []coordinatedRoleState) { s[2].tailReady = false }, false},
		{"unchanged unavailable dependency", func(s []coordinatedRoleState) { s[2].versionChanged = false; s[2].tailReady = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := []coordinatedRoleState{newCoordinatedRoleStateForTest("prefill", 8, 7, 0), newCoordinatedRoleStateForTest("decode", 4, 3, 0), newCoordinatedRoleStateForTest("fff", 10, 9, 0)}
			tc.alter(states)
			policy, err := calculateRoleRolloutLimits(states, coord)
			require.NoError(t, err)
			if tc.allow {
				require.Equal(t, 1, policy.roles["prefill"].remainingDeletions)
				require.Zero(t, policy.roles["prefill"].effectivePartition)
			} else {
				require.Zero(t, policy.roles["prefill"].remainingDeletions)
			}
			require.True(t, policy.roles["decode"].retainOldReplica)
			require.True(t, policy.roles["fff"].retainOldReplica)
			require.Equal(t, 1, policy.roles["decode"].effectivePartition)
			require.GreaterOrEqual(t, policy.roles["fff"].effectivePartition, 1)
		})
	}
}

func TestAllowedTargetStartsDoesNotOverflowInt32ReplicaDomain(t *testing.T) {
	state := coordinatedRoleState{versionTotal: 2147483647}
	baseline := coordinatedRoleState{versionTotal: 2147483647, versionReady: 1073741823}
	require.Equal(t, 1288490188, allowedStartedReplicas(state, baseline, 10))
	require.Equal(t, 2147483647, allowedStartedReplicas(state, baseline, 100))
}

func TestFullTailReleasesDependenciesOnlyAfterCallerDisappears(t *testing.T) {
	coord := coordinationForTest("10%")
	coord.Dependencies = []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b"}}, {Role: "b", DependsOn: []string{"c"}}}
	states := []coordinatedRoleState{newCoordinatedRoleStateForTest("a", 8, 8, 0), newCoordinatedRoleStateForTest("b", 4, 3, 0), newCoordinatedRoleStateForTest("c", 10, 9, 0)}
	policy, err := calculateRoleRolloutLimits(states, coord)
	require.NoError(t, err)
	require.Zero(t, policy.roles["b"].effectivePartition)
	require.False(t, policy.roles["b"].retainOldReplica)
	require.True(t, policy.roles["c"].retainOldReplica)
	states[1] = newCoordinatedRoleStateForTest("b", 4, 4, 0)
	policy, err = calculateRoleRolloutLimits(states, coord)
	require.NoError(t, err)
	require.Zero(t, policy.roles["c"].effectivePartition)
	require.Equal(t, 1, policy.roles["c"].remainingDeletions)

	// Without a retained dependency path there is no terminal cycle to break.
	states[0] = newCoordinatedRoleStateForTest("a", 8, 7, 0)
	states[1] = newCoordinatedRoleStateForTest("b", 4, 3, 0)
	policy, err = calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	require.Zero(t, policy.roles["a"].remainingDeletions)
}

func TestFullTailRequiresObservedReadyKnownCapacity(t *testing.T) {
	role := api.Role{Name: "a", Replicas: ptr.To[int32](2)}
	ms := &api.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "tail", Namespace: "default", UID: "tail"}, Spec: api.ModelServingSpec{Template: api.ServingGroup{Roles: []api.Role{role}}}}
	c := &ModelServingController{kubeClientSet: kubefake.NewSimpleClientset()}
	recordDifferentRevision(t, c, ms, "old")
	for _, tc := range []struct {
		name  string
		alter func(*[]datastore.Role, map[int]templateComparison)
		ready bool
	}{
		{"ready", func(*[]datastore.Role, map[int]templateComparison) {}, true},
		{"missing", func(r *[]datastore.Role, _ map[int]templateComparison) { *r = (*r)[:1] }, false},
		{"unready target", func(r *[]datastore.Role, _ map[int]templateComparison) { (*r)[1].Status = datastore.RoleCreating }, false},
		{"unknown history", func(r *[]datastore.Role, _ map[int]templateComparison) { (*r)[0].Revision = "missing" }, false},
		{"terminating", func(_ *[]datastore.Role, m map[int]templateComparison) { m[0] = templateDifferent }, false},
		{"old surge", func(r *[]datastore.Role, _ map[int]templateComparison) {
			*r = append(*r, datastore.Role{Name: "a-2", Revision: "old", Status: datastore.RoleRunning})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roles := []datastore.Role{{Name: "a-0", Revision: "old", Status: datastore.RoleRunning}, {Name: "a-1", RoleTemplateHash: utils.CalRoleTemplateHash(role), Status: datastore.RoleRunning}}
			terminating := map[int]templateComparison{}
			tc.alter(&roles, terminating)
			state := c.resolveRoleRolloutState(context.Background(), ms, datastore.ServingGroup{Name: "tail-0"}, role, 2, roles, 0, true, terminating, nil)
			require.Equal(t, tc.ready, state.tailReady)
		})
	}
}

func TestLegacyPartitionConflictStopsFurtherStableDeletions(t *testing.T) {
	ms := &api.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "legacy"}, Spec: api.ModelServingSpec{RolloutStrategy: &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("20%")}, Template: api.ServingGroup{Roles: []api.Role{
		{Name: "a", Replicas: ptr.To[int32](4), RollingUpdateConfiguration: api.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(2))}},
		{Name: "b", Replicas: ptr.To[int32](4)},
	}}}}
	c := &ModelServingController{kubeClientSet: kubefake.NewSimpleClientset(), store: datastore.New()}
	recordDifferentRevision(t, c, ms, "old")
	nsn := utils.GetNamespaceName(ms)
	c.store.AddServingGroup(nsn, 0, "old")
	for _, role := range ms.Spec.Template.Roles {
		for ordinal := 0; ordinal < 4; ordinal++ {
			id := fmt.Sprintf("%s-%d", role.Name, ordinal)
			c.store.AddRole(nsn, "legacy-0", role.Name, id, "old", "old-hash")
			require.NoError(t, c.store.UpdateRoleStatus(nsn, "legacy-0", role.Name, id, datastore.RoleRunning))
		}
	}
	before := ms.DeepCopy()
	policy, err := c.resolveRoleRolloutPolicy(context.Background(), ms, "new")
	require.NoError(t, err)
	group := policy.group("legacy-0")
	require.Equal(t, "IncompatiblePartitions", group.blocker.reason)
	for _, limits := range group.roles {
		require.Zero(t, limits.remainingDeletions)
		require.Equal(t, limits.rolloutEnd, limits.effectivePartition)
		require.True(t, limits.allowTargetStart, "existing reservations can finish")
	}
	require.Equal(t, before, ms)
}
