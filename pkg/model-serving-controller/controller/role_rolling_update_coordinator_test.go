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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestRoleTemplateChangedExcludesUnchangedAndNewRoles(t *testing.T) {
	role := func(name, image string) workloadv1alpha1.Role {
		return workloadv1alpha1.Role{
			Name: name,
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
			},
		}
	}
	oldRoles := []workloadv1alpha1.Role{role("a", "old"), role("b", "same")}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms", UID: "test-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{
				role("a", "new"), role("b", "same"), role("c", "new"),
			}},
		},
	}
	kubeClient := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(context.Background(), kubeClient, ms, "old-revision", oldRoles)
	require.NoError(t, err)

	controller := &ModelServingController{kubeClientSet: kubeClient}
	oldRoleByName, err := controller.roleSpecsFromRevision(ms, "old-revision")
	require.NoError(t, err)
	assert.True(t, roleTemplateChanged(oldRoleByName, ms.Spec.Template.Roles[0]))
	assert.False(t, roleTemplateChanged(oldRoleByName, ms.Spec.Template.Roles[1]))
	assert.False(t, roleTemplateChanged(oldRoleByName, ms.Spec.Template.Roles[2]))
}

func TestPersistCoordinatedRoleRevisionRefreshesReplicaBaseline(t *testing.T) {
	maxSkew := intstr.FromString("25%")
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms", UID: "test-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:             workloadv1alpha1.RoleRollingUpdate,
				RoleCoordination: &workloadv1alpha1.RoleCoordination{MaxSkew: &maxSkew},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name:     "a",
				Replicas: ptr.To[int32](2),
			}}},
		},
	}
	controller := &ModelServingController{kubeClientSet: kubefake.NewSimpleClientset()}
	revision := utils.ModelServingRevision(ms)
	require.NoError(t, controller.persistCoordinatedRoleRevision(context.Background(), ms, revision))
	initialRevision, err := utils.GetControllerRevision(context.Background(), controller.kubeClientSet, ms, revision)
	require.NoError(t, err)
	require.NotNil(t, initialRevision)
	initialData := string(initialRevision.Data.Raw)

	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](4)
	assert.Equal(t, revision, utils.ModelServingRevision(ms), "replica-only scaling must keep the template revision")
	require.NoError(t, controller.persistCoordinatedRoleRevision(context.Background(), ms, revision))

	controllerRevision, err := utils.GetControllerRevision(context.Background(), controller.kubeClientSet, ms, revision)
	require.NoError(t, err)
	require.NotNil(t, controllerRevision)
	assert.Equal(t, initialData, string(controllerRevision.Data.Raw), "the immutable template snapshot must not change")
	assert.Equal(t, int64(1), controllerRevision.Revision)

	rolesByName, err := controller.roleSpecsFromRevision(ms, revision)
	require.NoError(t, err)
	require.Contains(t, rolesByName, "a")
	require.NotNil(t, rolesByName["a"].Replicas)
	assert.Equal(t, int32(4), *rolesByName["a"].Replicas)
}

func TestResolveRoleRolloutStateUsesPartitionAndReservesStartedSlots(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](4)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	roleList := []datastore.Role{
		{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		// a-3 has completed deletion but has not yet been recorded as Creating.
	}
	controller := &ModelServingController{}
	state := controller.resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		4,
		roleList,
		2,
		true,
		nil,
	)

	assert.Equal(t, 2, state.totalToUpdate)
	assert.Equal(t, 2, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.Equal(t, targetReady, state.targetState)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutStateUsesExactRolloutOrdinalRange(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](4)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	roleList := []datastore.Role{
		{Name: "a-0", RoleTemplateHash: targetHash, Status: datastore.RoleRunning}, // Below partition.
		{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		{Name: "a-3", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-7", RoleTemplateHash: targetHash, Status: datastore.RoleRunning}, // Scale-down excess.
	}

	state := (&ModelServingController{}).resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		4,
		roleList,
		2,
		true,
		nil,
	)

	assert.Equal(t, 2, state.totalToUpdate)
	assert.Equal(t, 1, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutStateReconstructsTerminatingOldReplica(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](2)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	roleList := []datastore.Role{
		{Name: "a-0", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
	}

	state := (&ModelServingController{}).resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		roleList,
		0,
		true,
		map[int]string{
			1: "old",
			4: "old", // Scale-down excess is ignored.
		},
	)

	assert.Equal(t, 2, state.totalToUpdate)
	assert.Equal(t, 2, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestTerminatingRoleReplicasReadsPodInformerAfterRestart(t *testing.T) {
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
	})
	now := metav1.Now()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:         "default",
		Name:              "test-ms-0-a-3-0",
		DeletionTimestamp: &now,
		Labels: map[string]string{
			workloadv1alpha1.GroupNameLabelKey:        "test-ms-0",
			workloadv1alpha1.RoleLabelKey:             "a",
			workloadv1alpha1.RoleIDKey:                "a-3",
			workloadv1alpha1.RoleTemplateHashLabelKey: "old-hash",
		},
	}}
	require.NoError(t, informer.GetStore().Add(pod))
	controller := &ModelServingController{podsInformer: informer}
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms"}}

	replicas := controller.terminatingRoleReplicas(ms, datastore.ServingGroup{Name: "test-ms-0"})
	require.Contains(t, replicas, "a")
	assert.Equal(t, "old-hash", replicas["a"][3])
}

func TestResolveRoleRolloutStateDoesNotReserveUnadmittedScaleUp(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](4)}
	roleList := []datastore.Role{
		{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
	}
	controller := &ModelServingController{}
	state := controller.resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		roleList,
		0,
		true,
		nil,
	)

	assert.Equal(t, 2, state.totalToUpdate)
	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState)
}

func TestResolveRoleRolloutStateTracksReplacementReadyIndependentlyOfScaleUp(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](3)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	tests := []struct {
		name       string
		roleList   []datastore.Role
		readyCount int
	}{
		{
			name: "replacement becomes Ready before scale-up",
			roleList: []datastore.Role{
				{Name: "a-0", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
				{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
				{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleCreating},
			},
			readyCount: 1,
		},
		{
			name: "scale-up becomes Ready before replacement",
			roleList: []datastore.Role{
				{Name: "a-0", RoleTemplateHash: targetHash, Status: datastore.RoleCreating},
				{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
				{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
			},
			readyCount: 0,
		},
		{
			name: "replacement and scale-up are both Ready",
			roleList: []datastore.Role{
				{Name: "a-0", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
				{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
				{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
			},
			readyCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := (&ModelServingController{}).resolveRoleRolloutState(
				&workloadv1alpha1.ModelServing{},
				datastore.ServingGroup{Name: "test-ms-0"},
				roleSpec,
				2,
				tt.roleList,
				0,
				true,
				nil,
			)

			assert.Equal(t, 2, state.totalToUpdate)
			assert.Equal(t, 1, state.startedCount)
			assert.Equal(t, tt.readyCount, state.readyCount)
			assert.Equal(t, targetReady, state.targetState)
		})
	}
}

func TestResolveRoleRolloutStateIgnoresScaleDownExcessForReadyState(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](2)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	controller := &ModelServingController{}
	state := controller.resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		[]datastore.Role{
			{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
			{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
			{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		},
		0,
		true,
		nil,
	)

	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState)

	state = controller.resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		[]datastore.Role{
			{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
			{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		},
		0,
		true,
		nil,
	)

	assert.Equal(t, 1, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetStarted, state.targetState)
}

func TestResolveRoleRolloutStateUsesOrdinaryScaleUpForDependencyReadinessOnly(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](3)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	state := (&ModelServingController{}).resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		[]datastore.Role{
			{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
			{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
			{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		},
		0,
		true,
		nil,
	)

	assert.Equal(t, 2, state.totalToUpdate)
	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetReady, state.targetState)
}

func TestResolveRoleRolloutStateRetainsMissingPartitionProtectedOldSlot(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](2)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	state := (&ModelServingController{}).resolveRoleRolloutState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		[]datastore.Role{
			// The partition-protected old ordinal a-0 is temporarily absent.
			{Name: "a-1", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		},
		1,
		true,
		nil,
	)

	assert.Equal(t, 1, state.totalToUpdate)
	assert.Equal(t, 1, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutPolicyAppliesDependencyAndSkew(t *testing.T) {
	role := func(name, image string, replicas int32) workloadv1alpha1.Role {
		return workloadv1alpha1.Role{
			Name:     name,
			Replicas: ptr.To(replicas),
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
			},
		}
	}
	oldRoles := []workloadv1alpha1.Role{role("a", "old", 2), role("b", "old", 2)}
	maxSkew := intstr.FromString("25%")
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms", UID: "test-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type: workloadv1alpha1.RoleRollingUpdate,
				RoleCoordination: &workloadv1alpha1.RoleCoordination{
					MaxSkew: &maxSkew,
					Dependencies: []workloadv1alpha1.RoleRolloutDependency{
						{Role: "a", DependsOn: []string{"b"}},
					},
				},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{
				role("a", "new", 4), role("b", "new", 2),
			}},
		},
	}
	kubeClient := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(context.Background(), kubeClient, ms, "old-revision", oldRoles)
	require.NoError(t, err)
	controller := &ModelServingController{kubeClientSet: kubeClient, store: datastore.New()}
	nsn := utils.GetNamespaceName(ms)
	controller.store.AddServingGroup(nsn, 0, "old-revision")
	for _, roleName := range []string{"a", "b"} {
		for ordinal := 0; ordinal < 2; ordinal++ {
			roleID := fmt.Sprintf("%s-%d", roleName, ordinal)
			controller.store.AddRole(nsn, "test-ms-0", roleName, roleID, "old-revision", "old-hash")
			require.NoError(t, controller.store.UpdateRoleStatus(nsn, "test-ms-0", roleName, roleID, datastore.RoleRunning))
		}
	}
	t.Run("dependency blocks upstream target-version scale up", func(t *testing.T) {
		policy, err := controller.resolveRoleRolloutPolicy(ms, "new-revision")
		require.NoError(t, err)
		plan := policy.group("test-ms-0")
		assert.False(t, plan.roles["a"].allowTargetStart)
		assert.True(t, plan.roles["b"].allowTargetStart)
		assert.Equal(t, 2, plan.roles["a"].effectivePartition)
	})

	t.Run("ready dependency unlocks the upstream target start", func(t *testing.T) {
		bTargetHash := utils.CalRoleTemplateHash(ms.Spec.Template.Roles[1])
		controller.store.DeleteRole(nsn, "test-ms-0", "b", "b-0")
		controller.store.AddRole(nsn, "test-ms-0", "b", "b-0", "new-revision", bTargetHash)
		require.NoError(t, controller.store.UpdateRoleStatus(nsn, "test-ms-0", "b", "b-0", datastore.RoleRunning))

		policy, err := controller.resolveRoleRolloutPolicy(ms, "new-revision")
		require.NoError(t, err)
		plan := policy.group("test-ms-0")
		assert.True(t, plan.roles["a"].allowTargetStart)
		assert.Equal(t, 1, plan.roles["a"].effectivePartition)
	})
}

func TestCoordinatedRoleSelectionCountsStartedReplicasOutsideEffectiveRange(t *testing.T) {
	role := func(name string) workloadv1alpha1.Role {
		return workloadv1alpha1.Role{
			Name:     name,
			Replicas: ptr.To[int32](10),
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "new"}}},
			},
		}
	}
	maxSkew := intstr.FromString("10%")
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms"},
		Spec: workloadv1alpha1.ModelServingSpec{
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:             workloadv1alpha1.RoleRollingUpdate,
				RoleCoordination: &workloadv1alpha1.RoleCoordination{MaxSkew: &maxSkew},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{role("a"), role("b")}},
		},
	}
	store := datastore.New()
	nsn := utils.GetNamespaceName(ms)
	store.AddServingGroup(nsn, 0, "old-revision")
	for _, roleSpec := range ms.Spec.Template.Roles {
		targetHash := utils.CalRoleTemplateHash(roleSpec)
		for ordinal := 0; ordinal < 10; ordinal++ {
			hash := "old"
			if roleSpec.Name == "a" && ordinal == 0 {
				hash = targetHash
			}
			roleID := fmt.Sprintf("%s-%d", roleSpec.Name, ordinal)
			store.AddRole(nsn, "test-ms-0", roleSpec.Name, roleID, "old-revision", hash)
			require.NoError(t, store.UpdateRoleStatus(nsn, "test-ms-0", roleSpec.Name, roleID, datastore.RoleRunning))
		}
	}
	controller := &ModelServingController{store: store, kubeClientSet: kubefake.NewSimpleClientset()}
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 10, 1, 1),
		newCoordinatedRoleStateForTest("b", 10, 0, 0),
	}
	decision, err := calculateRoleRolloutLimits(states, ms.Spec.RolloutStrategy.RoleCoordination)
	require.NoError(t, err)
	plan := &decision

	selected, hasOutdated, err := controller.rolesToDeleteForRoleRollingUpdate(ms, datastore.ServingGroup{
		Name: "test-ms-0", Revision: "old-revision", Status: datastore.ServingGroupRunning,
	}, plan)
	require.NoError(t, err)
	assert.True(t, hasOutdated)
	assert.Equal(t, map[string]int{"b": 1}, selectedCountByRole(selected))
}

func TestCalculateRoleRolloutLimitsProportionalLimits(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 100, 0, 0),
		newCoordinatedRoleStateForTest("b", 50, 0, 0),
		newCoordinatedRoleStateForTest("c", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 90, "b": 45, "c": 9}, effectivePartitionsByRole(decision))
}

func TestCalculateRoleRolloutLimitsDependencyStartupAndCompletion(t *testing.T) {
	coordination := coordinationForTest("20%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "a", DependsOn: []string{"b"}},
		{Role: "b", DependsOn: []string{"c"}},
	}

	t.Run("internal Roles prestart while entry Role is fully partitioned", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 0, 0),
			newCoordinatedRoleStateForTest("b", 10, 0, 0),
			newCoordinatedRoleStateForTest("c", 10, 0, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 10, "b": 8, "c": 8}, effectivePartitionsByRole(decision))
	})

	t.Run("ready dependency closure unlocks entry Role", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 0, 0),
			newCoordinatedRoleStateForTest("b", 10, 2, 0),
			newCoordinatedRoleStateForTest("c", 10, 2, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 8, "b": 8, "c": 8}, effectivePartitionsByRole(decision))
	})

	t.Run("an admitted root is not startup-gated again", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 0, 1),
			newCoordinatedRoleStateForTest("b", 10, 0, 0),
			newCoordinatedRoleStateForTest("c", 10, 0, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, 8, decision.roles["a"].effectivePartition)
	})

	t.Run("dependency order is not a permanent progress order", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 2, 0),
			newCoordinatedRoleStateForTest("b", 10, 2, 0),
			newCoordinatedRoleStateForTest("c", 10, 2, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 6, "b": 6, "c": 6}, effectivePartitionsByRole(decision))
	})

	t.Run("dependency retains one eligible old replica until dependents complete", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 8, 0),
			newCoordinatedRoleStateForTest("b", 10, 9, 0),
			newCoordinatedRoleStateForTest("c", 10, 9, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, 1, decision.roles["b"].effectivePartition)
		assert.Equal(t, 1, decision.roles["c"].effectivePartition)
	})

	t.Run("completion releases dependency partitions in order", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 10, 0),
			newCoordinatedRoleStateForTest("b", 10, 9, 0),
			newCoordinatedRoleStateForTest("c", 10, 9, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, 0, decision.roles["b"].effectivePartition)
		assert.Equal(t, 1, decision.roles["c"].effectivePartition)
	})
}

func TestCalculateRoleRolloutLimitsInfersRootFromSelectedGraph(t *testing.T) {
	coordination := coordinationForTest("20%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "b", DependsOn: []string{"c"}},
	}
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("b", 10, 0, 0),
		newCoordinatedRoleStateForTest("c", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 10, decision.roles["b"].effectivePartition)
	assert.Equal(t, 8, decision.roles["c"].effectivePartition)
	require.NotNil(t, decision.blocker)
	assert.Equal(t, coordinatedRoleDependencyNotReady, decision.blocker.reason)
}

func TestCalculateRoleRolloutLimitsCountsInFlightReservations(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 10, 0, 1),
		newCoordinatedRoleStateForTest("b", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 9, "b": 9}, effectivePartitionsByRole(decision))
	assert.Equal(t, map[string]int{"a": 0, "b": 1}, remainingDeletionsByRole(decision))
}

func TestCalculateRoleRolloutLimitsUsesUserPartitionAsCompletionFloor(t *testing.T) {
	coordination := coordinationForTest("100%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "a", DependsOn: []string{"b"}},
	}
	states := []coordinatedRoleState{
		{roleName: "a", userPartition: 1, totalToUpdate: 3, startedCount: 2, readyCount: 2, targetState: targetReady, hasOldVersion: true, inProgress: true},
		{roleName: "b", userPartition: 2, totalToUpdate: 2, startedCount: 1, readyCount: 1, targetState: targetReady, hasOldVersion: true, inProgress: true},
	}

	decision, err := calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 2, decision.roles["b"].effectivePartition)

	states[0].readyCount = states[0].totalToUpdate
	states[0].startedCount = states[0].totalToUpdate
	states[0].hasOldVersion = false
	states[0].inProgress = false
	decision, err = calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 2, decision.roles["b"].effectivePartition)
}

func TestCalculateRoleRolloutLimitsDoesNotBypassMaxSkewAtCompletion(t *testing.T) {
	coordination := coordinationForTest("5%")
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 100, 95, 0),
		newCoordinatedRoleStateForTest("b", 10, 9, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 5, decision.roles["a"].effectivePartition)
	assert.Zero(t, decision.roles["b"].effectivePartition)
	assert.Equal(t, 0, decision.roles["a"].remainingDeletions)
	assert.Equal(t, 1, decision.roles["b"].remainingDeletions)
}

func TestCalculateRoleRolloutLimitsKeepsQuantizationLocal(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 2, 0, 0),
		newCoordinatedRoleStateForTest("b", 4, 0, 0),
		newCoordinatedRoleStateForTest("c", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 1, "b": 3, "c": 9}, effectivePartitionsByRole(decision))
}

func TestCalculateRoleRolloutLimitsRejectsInvalidMaxSkew(t *testing.T) {
	integerSkew := intstr.FromInt(10)
	_, err := calculateRoleRolloutLimits(
		[]coordinatedRoleState{newCoordinatedRoleStateForTest("a", 10, 0, 0)},
		&workloadv1alpha1.RoleCoordination{MaxSkew: &integerSkew},
	)
	require.ErrorContains(t, err, "percentage string")
}

func TestCoordinatedRoleRolloutConditionUsesDeterministicFirstBlocker(t *testing.T) {
	maxSkew := intstr.FromString("10%")
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms", Generation: 7},
		Spec: workloadv1alpha1.ModelServingSpec{
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:             workloadv1alpha1.RoleRollingUpdate,
				RoleCoordination: &workloadv1alpha1.RoleCoordination{MaxSkew: &maxSkew},
			},
		},
	}
	policy := &roleRolloutPolicy{enabled: true}
	policy.addGroup("test-ms-0", roleRolloutGroupPolicy{
		inProgress: true,
		blocker: &coordinatedRoleBlocker{
			reason:  coordinatedRoleDependencyNotReady,
			message: "first group blocker",
		},
	})
	policy.addGroup("test-ms-2", roleRolloutGroupPolicy{
		inProgress: true,
		blocker: &coordinatedRoleBlocker{
			reason:  coordinatedRoleMaxSkewLimitReached,
			message: "later group blocker",
		},
	})

	changed, condition := policy.setCondition(ms)
	require.True(t, changed)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, coordinatedRoleDependencyNotReady, condition.Reason)
	assert.Contains(t, condition.Message, "ServingGroup test-ms-0")
	assert.Equal(t, int64(7), condition.ObservedGeneration)
}

func TestCoordinatedRoleRolloutConditionClearsWhenConfigurationIsRemoved(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms", Generation: 2},
		Status: workloadv1alpha1.ModelServingStatus{Conditions: []metav1.Condition{{
			Type:   string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked),
			Status: metav1.ConditionTrue,
			Reason: coordinatedRoleDependencyNotReady,
		}}},
	}
	changed, condition := (&roleRolloutPolicy{}).setCondition(ms)
	require.True(t, changed)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "RolloutComplete", condition.Reason)
}

func newCoordinatedRoleStateForTest(roleName string, total, ready, inFlight int) coordinatedRoleState {
	started := min(total, ready+inFlight)
	targetState := targetNotStarted
	if ready > 0 {
		targetState = targetReady
	} else if started > 0 {
		targetState = targetStarted
	}
	return coordinatedRoleState{
		roleName:      roleName,
		totalToUpdate: total,
		startedCount:  started,
		readyCount:    ready,
		targetState:   targetState,
		hasOldVersion: ready < total,
		inProgress:    total > 0 && ready < total,
	}
}

func coordinationForTest(maxSkew string) *workloadv1alpha1.RoleCoordination {
	value := intstr.FromString(maxSkew)
	return &workloadv1alpha1.RoleCoordination{MaxSkew: &value}
}

func effectivePartitionsByRole(decision roleRolloutGroupPolicy) map[string]int {
	partitions := make(map[string]int, len(decision.roles))
	for roleName, limits := range decision.roles {
		partitions[roleName] = limits.effectivePartition
	}
	return partitions
}

func remainingDeletionsByRole(decision roleRolloutGroupPolicy) map[string]int {
	remaining := make(map[string]int, len(decision.roles))
	for roleName, limits := range decision.roles {
		remaining[roleName] = limits.remainingDeletions
	}
	return remaining
}

func selectedCountByRole(selected []roleToDelete) map[string]int {
	result := make(map[string]int)
	for _, role := range selected {
		result[role.roleName]++
	}
	return result
}
