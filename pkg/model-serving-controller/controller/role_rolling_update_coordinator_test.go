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
	state := controller.resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		4,
		roleList,
		2,
		true,
		nil, nil)

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

	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		4,
		roleList,
		2,
		true,
		nil, nil)

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

	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		roleList,
		0,
		true,
		map[int]templateComparison{
			1: templateDifferent,
			4: templateDifferent, // Scale-down excess is ignored.
		}, nil)

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
		OwnerReferences:   []metav1.OwnerReference{{APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: "ModelServing", Name: "test-ms", UID: "test-uid", Controller: ptr.To(true)}},
		Namespace:         "default",
		Name:              "test-ms-0-a-3-0",
		DeletionTimestamp: &now,
		Labels: map[string]string{
			workloadv1alpha1.GroupNameLabelKey:        "test-ms-0",
			workloadv1alpha1.RevisionLabelKey:         "legacy",
			workloadv1alpha1.RoleLabelKey:             "a",
			workloadv1alpha1.RoleIDKey:                "a-3",
			workloadv1alpha1.RoleTemplateHashLabelKey: "old-hash",
		},
	}}
	require.NoError(t, informer.GetStore().Add(pod))
	controller := &ModelServingController{podsInformer: informer, kubeClientSet: kubefake.NewSimpleClientset()}
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-ms", UID: "test-uid"}, Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{Name: "a"}}}}}
	recordDifferentRevision(t, controller, ms, "legacy")

	replicas := controller.terminatingRoleReplicas(context.Background(), ms, datastore.ServingGroup{Name: "test-ms-0"})
	require.Contains(t, replicas, "a")
	assert.Equal(t, templateDifferent, replicas["a"][3])
}

func TestResolveRoleRolloutStateDoesNotReserveUnadmittedScaleUp(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](4)}
	roleList := []datastore.Role{
		{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
	}
	controller := &ModelServingController{}
	state := controller.resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		roleList,
		0,
		true,
		nil, nil)

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
			state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(),
				&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
				datastore.ServingGroup{Name: "test-ms-0"},
				roleSpec,
				2,
				tt.roleList,
				0,
				true,
				nil, nil)

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
	state := controller.resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
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
		nil, nil)

	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState)

	state = controller.resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		[]datastore.Role{
			{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
			{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		},
		0,
		true,
		nil, nil)

	assert.Equal(t, 1, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState, "a missing formal slot does not prove dependency admission; excess target capacity cannot open the gate")
}

func TestResolveRoleRolloutStateUsesOrdinaryScaleUpForDependencyReadinessOnly(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](3)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
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
		nil, nil)

	assert.Equal(t, 2, state.totalToUpdate)
	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetReady, state.targetState)
}

func TestResolveRoleRolloutStateRetainsMissingPartitionProtectedOldSlot(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](2)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(),
		&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{roleSpec}}}},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		2,
		[]datastore.Role{
			// The partition-protected old ordinal a-0 is temporarily absent.
			{Name: "a-1", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		},
		1,
		true,
		nil, nil)

	assert.Equal(t, 1, state.totalToUpdate)
	assert.Equal(t, 1, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestStableRolePopulationIncludesObservedExpansion(t *testing.T) {
	role := createStandardModelServing("expansion", 1, 10).Spec.Template.Roles[0]
	role.Name = "a"
	targetHash := utils.CalRoleTemplateHash(role)
	tests := []struct {
		name                  string
		ordinals              []int
		targets               []int
		terminating           map[int]templateComparison
		missingOld            bool
		count, started, ready int
	}{
		{name: "all expanded old members are eligible", ordinals: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, count: 10},
		{name: "uncreated expansion does not reserve starts", ordinals: []int{0, 1, 2, 3, 4, 9}, count: 6},
		{name: "target expansion is counted once", ordinals: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, targets: []int{5, 6, 7, 8, 9}, count: 10, started: 5, ready: 5},
		{name: "missing prior replacement remains reserved", ordinals: []int{0, 1, 2, 3, 5}, targets: []int{5}, missingOld: true, count: 6, started: 2, ready: 1},
		{name: "terminating expanded replacement remains reserved", ordinals: []int{0, 1, 2, 3, 4}, terminating: map[int]templateComparison{9: templateDifferent}, count: 6, started: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instances := []datastore.Role{}
			for _, ordinal := range tt.ordinals {
				hash := "old"
				for _, target := range tt.targets {
					if ordinal == target {
						hash = targetHash
					}
				}
				instances = append(instances, datastore.Role{Name: utils.GenerateRoleID(role.Name, ordinal), RoleTemplateHash: hash, Status: datastore.RoleRunning})
			}
			ms := &workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{role}}}}
			c := &ModelServingController{}
			population, err := c.stableRolePopulation(ms, "expansion-0", role, 5, instances, tt.terminating)
			require.NoError(t, err)
			require.NotNil(t, population)
			assert.Equal(t, tt.count, population.countFrom(0))
			for _, ordinal := range tt.ordinals {
				assert.True(t, population.contains(ordinal))
			}
			if tt.missingOld {
				assert.True(t, population.contains(4), "missing original replacement holds its start")
			}
			state := c.resolveRoleRolloutState(context.Background(), ms, datastore.ServingGroup{Name: "expansion-0"}, role, 5, instances, 0, true, tt.terminating, population)
			assert.Equal(t, tt.started, state.startedCount)
			assert.Equal(t, tt.ready, state.readyCount)
			assert.Equal(t, tt.started, state.versionStarted, "formal target capacity must not be counted twice")
			assert.Equal(t, 10, state.versionTotal)
		})
	}
}

func TestStableRoleExpansionDistinguishesAdoptedAndTemporarySurge(t *testing.T) {
	ms := createStandardModelServing("surge-expansion", 1, 10)
	ms.UID = "surge-expansion-owner"
	c := newRevisionTestController(t, ms)
	role, group := ms.Spec.Template.Roles[0], "surge-expansion-0"
	instances := []datastore.Role{}
	for i := 0; i < 5; i++ {
		instances = append(instances, datastore.Role{Name: utils.GenerateRoleID(role.Name, i), Status: datastore.RoleRunning})
	}
	for _, ordinal := range []int{5, 10} {
		id := utils.GenerateRoleID(role.Name, ordinal)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: utils.GeneratePodName(group, id, 0), Namespace: ms.Namespace,
			Labels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey: group, workloadv1alpha1.RoleLabelKey: role.Name, workloadv1alpha1.RoleIDKey: id},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))},
		}}
		setSurgeScope(pod, surgeRole)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		instances = append(instances, datastore.Role{Name: id, Status: datastore.RoleRunning})
	}
	population, err := c.stableRolePopulation(ms, group, role, 5, instances, nil)
	require.NoError(t, err)
	assert.Equal(t, 6, population.countFrom(0))
	assert.True(t, population.contains(5), "an adopted formal expansion member participates")
	assert.False(t, population.contains(10), "temporary surge never joins stable progress")
	assert.Equal(t, 4, population.countFrom(2), "partition protects absolute original ordinals")
}

func TestCoordinatedRoleExpansionIgnoresIncompleteUnselectedRole(t *testing.T) {
	ctx := context.Background()
	initial := createStandardModelServing("independent-expansion", 1, 5)
	initial.UID = "expansion-owner"
	for _, name := range []string{"decode", "fff"} {
		role := *initial.Spec.Template.Roles[0].DeepCopy()
		role.Name = name
		initial.Spec.Template.Roles = append(initial.Spec.Template.Roles, role)
	}
	initial.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.RoleRollingUpdate,
		RoleCoordination: &workloadv1alpha1.RoleCoordination{
			Roles: []string{"prefill", "decode"}, MaxSkew: ptr.To(intstr.FromString("20%")),
			Dependencies: []workloadv1alpha1.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}},
		},
	}
	c := newRevisionTestController(t, initial)
	completed := utils.ModelServingRevision(initial)
	require.NoError(t, c.persistCoordinatedRoleRevision(ctx, initial, completed))
	expanded := initial.DeepCopy()
	expanded.Status.CurrentRevision = completed
	expanded.Spec.Template.Roles[2].EntryTemplate.Spec.Containers[0].Image = "fff:missing"
	for i := range expanded.Spec.Template.Roles {
		expanded.Spec.Template.Roles[i].Replicas = ptr.To[int32](10)
	}
	require.NoError(t, c.persistCoordinatedRoleRevision(ctx, expanded, utils.ModelServingRevision(expanded)))
	historical, err := c.roleSpecsFromRevision(expanded, completed)
	require.NoError(t, err)
	require.Equal(t, int32(5), *historical["decode"].Replicas, "global completed baseline remains unchanged")
	key, group := utils.GetNamespaceName(expanded), "independent-expansion-0"
	c.store.AddServingGroup(key, 0, completed)
	for _, role := range expanded.Spec.Template.Roles {
		for i := 0; i < 10; i++ {
			id := utils.GenerateRoleID(role.Name, i)
			c.store.AddRole(key, group, role.Name, id, utils.ModelServingRevision(expanded), utils.CalRoleTemplateHash(role))
			if role.Name != "fff" {
				require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, id, datastore.RoleRunning))
			}
		}
	}
	for _, image := range []string{"pd:B", "pd:C"} {
		target := expanded.DeepCopy()
		for i := 0; i < 2; i++ {
			target.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = image
		}
		policy, err := c.resolveRoleRolloutPolicy(ctx, target, utils.ModelServingRevision(target))
		require.NoError(t, err)
		_, coordinated := policy.group(group).role("fff")
		require.False(t, coordinated)
		for _, role := range expanded.Spec.Template.Roles[:2] {
			limits, ok := policy.group(group).role(role.Name)
			require.True(t, ok)
			require.Equal(t, 10, limits.rolloutEnd)
			candidates := []datastore.Role{}
			for i := 9; i >= 0; i-- {
				candidates = append(candidates, datastore.Role{Name: utils.GenerateRoleID(role.Name, i), Status: datastore.RoleRunning})
			}
			selected, budget := policy.group(group).constrainRoleDeletion(role.Name, candidates, 1)
			if role.Name == "decode" {
				require.NotEmpty(t, selected)
				require.Equal(t, "decode-9", selected[0].Name)
				require.Equal(t, 1, budget)
			} else {
				require.Empty(t, selected, "prefill retains its explicit startup dependency")
			}
		}
	}
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
		policy, err := controller.resolveRoleRolloutPolicy(context.Background(), ms, "new-revision")
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

		policy, err := controller.resolveRoleRolloutPolicy(context.Background(), ms, "new-revision")
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

	recordDifferentRevision(t, controller, ms, "old-revision")
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
		{roleName: "a", userPartition: 1, totalToUpdate: 3, startedCount: 2, readyCount: 2, targetState: targetReady, hasOldVersion: true, inProgress: true, versionChanged: true, versionTotal: 4, versionStarted: 2, versionReady: 2},
		{roleName: "b", userPartition: 2, totalToUpdate: 2, startedCount: 1, readyCount: 1, targetState: targetReady, hasOldVersion: true, inProgress: true, versionChanged: true, versionTotal: 4, versionStarted: 1, versionReady: 1},
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
		versionTotal:  total, versionReady: ready, versionStarted: started,
		versionOld: total - started, versionChanged: total > 0, tailReady: ready == started,
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

func TestRoleRolloutAvailabilityAndOrderedCandidates(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		n, u, partition                            int
		coordinated, exhausted, retained, excluded bool
		unready, temporary, target, terminating    []int
		count                                      int
		unknown                                    int // one-based ordinal, zero disables
		want                                       []string
	}{
		{name: "terminating low Pod reserves budget after restart", n: 3, u: 1, count: 3, unready: []int{0, 2}, terminating: []int{0}},
		{name: "independent skips healthy high for low old fault", n: 3, u: 1, count: 3, unready: []int{0}, want: []string{"prefill-0"}},
		{name: "coordination waits at healthy high", n: 3, u: 1, count: 3, coordinated: true, unready: []int{0}},
		{name: "nonparticipating Role still ordered when coordination configured", n: 3, u: 1, count: 3, coordinated: true, excluded: true, unready: []int{0}},
		{name: "all old failed repairs highest", n: 3, u: 1, count: 3, coordinated: true, unready: []int{0, 1, 2}, want: []string{"prefill-2"}},
		{name: "new failed replacement consumes allowance", n: 3, u: 1, count: 3, unready: []int{0, 1, 2}, target: []int{2}},
		{name: "protected fault cannot fund healthy deletion", n: 3, u: 1, count: 3, partition: 1, unready: []int{0}},
		{name: "049 single obsolete surge only", n: 1, u: 0, count: 2, coordinated: true, unready: []int{1}, temporary: []int{1}, want: []string{"prefill-1"}},
		{name: "049 triple obsolete surge only", n: 3, u: 0, count: 4, coordinated: true, unready: []int{3}, temporary: []int{3}, want: []string{"prefill-3"}},
		{name: "obsolete surge cleanup does not need stable start credit", n: 3, u: 0, count: 4, coordinated: true, exhausted: true, unready: []int{3}, temporary: []int{3}, want: []string{"prefill-3"}},
		{name: "target surge unavailable is not rolled again", n: 1, u: 0, count: 2, coordinated: true, unready: []int{1}, temporary: []int{1}, target: []int{1}},
		{name: "healthy surge retains availability for new stable fault", n: 1, u: 0, count: 2, coordinated: true, unready: []int{0}, temporary: []int{1}, target: []int{0}},
		{name: "ready target surge permits stable replacement", n: 1, u: 0, count: 2, coordinated: true, temporary: []int{1}, target: []int{1}, want: []string{"prefill-0"}},
		{name: "unmarked high ordinal is not obsolete surge", n: 3, u: 1, count: 4, coordinated: true, unready: []int{3}},
		{name: "absorbed surge obeys stable start limit", n: 3, u: 1, count: 3, coordinated: true, exhausted: true, unready: []int{2}, temporary: []int{2}},
		{name: "necessary last old dependency survives", n: 1, u: 1, count: 2, coordinated: true, retained: true, unready: []int{1}, temporary: []int{1}, target: []int{0}},
		{name: "unknown high history blocks lower old fault", n: 3, u: 1, count: 3, coordinated: true, unknown: 3, unready: []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := createStandardModelServing("role-budget", 1, int32(tc.n))
			ms.UID = "role-budget-uid"
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
			if tc.coordinated {
				ms.Spec.RolloutStrategy.RoleCoordination = coordinationForTest("20%")
			}
			role := &ms.Spec.Template.Roles[0]
			role.WorkerReplicas = 0
			role.MaxUnavailable = ptr.To(intstr.FromInt(tc.u))
			role.MaxSurge = ptr.To(intstr.FromInt(1))
			role.Partition = ptr.To(intstr.FromInt(tc.partition))
			c := newRevisionTestController(t, ms)
			recordDifferentRevision(t, c, ms, "old")
			target := utils.ModelServingRevision(ms)
			key := utils.GetNamespaceName(ms)
			c.store.AddServingGroup(key, 0, "old")
			contains := func(xs []int, i int) bool {
				for _, x := range xs {
					if i == x {
						return true
					}
				}
				return false
			}
			for ordinal := 0; ordinal < tc.count; ordinal++ {
				id := utils.GenerateRoleID(role.Name, ordinal)
				revision := "old"
				if contains(tc.target, ordinal) {
					revision = target
				}
				if ordinal+1 == tc.unknown {
					revision = "missing"
				}
				hash := "legacy-hash"
				if revision == target {
					hash = utils.CalRoleTemplateHash(*role)
				}
				pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, "role-budget-0", id, revision, hash)
				if contains(tc.temporary, ordinal) {
					setSurgeScope(pod, surgeRole)
				}
				if contains(tc.terminating, ordinal) {
					pod.DeletionTimestamp = ptr.To(metav1.Now())
				}
				require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
				c.store.AddRole(key, "role-budget-0", role.Name, id, revision, hash)
				status := datastore.RoleRunning
				if contains(tc.unready, ordinal) {
					status = datastore.RoleCreating
				}
				require.NoError(t, c.store.UpdateRoleStatus(key, "role-budget-0", role.Name, id, status))
			}
			var policy *roleRolloutGroupPolicy
			if tc.coordinated && !tc.excluded {
				remaining := tc.n
				if tc.exhausted {
					remaining = 0
				}
				policy = &roleRolloutGroupPolicy{roles: map[string]roleRolloutLimits{role.Name: {rolloutEnd: tc.n, effectivePartition: tc.partition, remainingDeletions: remaining, retainOldReplica: tc.retained}}}
			}
			groups, err := c.store.GetServingGroupByModelServing(key)
			require.NoError(t, err)
			selected, _, err := c.rolesToDeleteForRoleRollingUpdate(ms, groups[0], policy)
			require.NoError(t, err)
			var names []string
			for _, candidate := range selected {
				names = append(names, candidate.roleID)
			}
			require.Equal(t, tc.want, names)
			// Selected deletions remain physically present. Their reservation must
			// prevent another stable deletion before replacement readiness.
			for _, candidate := range selected {
				require.NoError(t, c.store.UpdateRoleStatus(key, "role-budget-0", candidate.roleName, candidate.roleID, datastore.RoleDeleting))
			}
			selected, _, err = c.rolesToDeleteForRoleRollingUpdate(ms, groups[0], policy)
			require.NoError(t, err)
			require.Empty(t, selected)
		})
	}
}

func TestResolveRoleRolloutStateKeepsTerminatingStartAfterRestart(t *testing.T) {
	role := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](2)}
	ms := &workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{role}}}}
	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(), ms,
		datastore.ServingGroup{Name: "test-0"}, role, 2,
		[]datastore.Role{
			{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleCreating},
			{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		}, 0, true, map[int]templateComparison{0: templateDifferent}, nil)
	require.Equal(t, 1, state.startedCount)
	require.Zero(t, state.readyCount)
	require.True(t, state.hasOldVersion)
	policy, err := calculateRoleRolloutLimits([]coordinatedRoleState{state, newCoordinatedRoleStateForTest("b", 2, 0, 0)}, coordinationForTest("20%"))
	require.NoError(t, err)
	require.Zero(t, policy.roles["a"].remainingDeletions)
}
