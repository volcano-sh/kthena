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
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestChangedRolesFromRevisionExcludesUnchangedAndNewRoles(t *testing.T) {
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
	changed, resolved := controller.changedRolesFromRevision(ms, "old-revision")
	require.True(t, resolved)
	assert.Equal(t, map[string]bool{"a": true, "b": false, "c": false}, changed)
}

func TestBuildCoordinatedRoleStateUsesPartitionAndReservesMissingSlot(t *testing.T) {
	roleSpec := workloadv1alpha1.Role{Name: "a", Replicas: ptr.To[int32](4)}
	targetHash := utils.CalRoleTemplateHash(roleSpec)
	roleList := []datastore.Role{
		{Name: "a-0", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-1", RoleTemplateHash: "old", Status: datastore.RoleRunning},
		{Name: "a-2", RoleTemplateHash: targetHash, Status: datastore.RoleRunning},
		// a-3 has completed deletion but has not yet been recorded as Creating.
	}
	controller := &ModelServingController{}
	state := controller.buildCoordinatedRoleState(
		&workloadv1alpha1.ModelServing{},
		datastore.ServingGroup{Name: "test-ms-0"},
		roleSpec,
		roleList,
		2,
		0,
		true,
		nil,
	)

	assert.Equal(t, 2, state.target)
	assert.Equal(t, 1, state.ready)
	assert.Equal(t, 1, state.inFlight)
}

func TestSelectCoordinatedRoleCandidatesProportionalLimits(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 0, 100, 0, 0, 100),
		newCoordinatedRoleStateForTest("b", 1, 50, 0, 0, 50),
		newCoordinatedRoleStateForTest("c", 2, 10, 0, 0, 10),
	}

	selected, err := selectCoordinatedRoleCandidates(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 10, "b": 5, "c": 1}, selectedCountByRole(selected))
}

func TestSelectCoordinatedRoleCandidatesDependencyStartup(t *testing.T) {
	coordination := coordinationForTest("20%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "a", DependsOn: []string{"b"}},
		{Role: "b", DependsOn: []string{"c"}},
	}

	t.Run("internal Roles prestart while entry Role waits", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 0, 10, 0, 0, 10),
			newCoordinatedRoleStateForTest("b", 1, 10, 0, 0, 10),
			newCoordinatedRoleStateForTest("c", 2, 10, 0, 0, 10),
		}

		selected, err := selectCoordinatedRoleCandidates(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"b": 2, "c": 2}, selectedCountByRole(selected))
	})

	t.Run("ready dependency closure unlocks entry Role", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 0, 10, 0, 0, 10),
			newCoordinatedRoleStateForTest("b", 1, 10, 2, 0, 10),
			newCoordinatedRoleStateForTest("c", 2, 10, 2, 0, 10),
		}

		selected, err := selectCoordinatedRoleCandidates(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 2}, selectedCountByRole(selected))
	})

	t.Run("dependency order is not a permanent progress order", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 0, 10, 2, 0, 2),
			newCoordinatedRoleStateForTest("b", 1, 10, 2, 0, 2),
			newCoordinatedRoleStateForTest("c", 2, 10, 2, 0, 2),
		}

		selected, err := selectCoordinatedRoleCandidates(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 2, "b": 2, "c": 2}, selectedCountByRole(selected))
	})
}

func TestSelectCoordinatedRoleCandidatesCountsInFlightReservations(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 0, 10, 0, 1, 10),
		newCoordinatedRoleStateForTest("b", 1, 10, 0, 0, 10),
	}

	selected, err := selectCoordinatedRoleCandidates(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"b": 1}, selectedCountByRole(selected))
}

func TestSelectCoordinatedRoleCandidatesKeepsQuantizationLocal(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 0, 2, 0, 0, 2),
		newCoordinatedRoleStateForTest("b", 1, 4, 0, 0, 4),
		newCoordinatedRoleStateForTest("c", 2, 10, 0, 0, 10),
	}

	selected, err := selectCoordinatedRoleCandidates(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1}, selectedCountByRole(selected))
}

func TestSelectCoordinatedRoleCandidatesRejectsInvalidMaxSkew(t *testing.T) {
	integerSkew := intstr.FromInt(10)
	_, err := selectCoordinatedRoleCandidates(
		[]coordinatedRoleState{newCoordinatedRoleStateForTest("a", 0, 10, 0, 0, 1)},
		&workloadv1alpha1.RoleRollingUpdateCoordination{MaxSkew: &integerSkew},
	)
	require.ErrorContains(t, err, "percentage string")
}

func newCoordinatedRoleStateForTest(roleName string, specIndex, target, ready, inFlight, candidateCount int) coordinatedRoleState {
	candidates := make([]roleToDelete, 0, candidateCount)
	for i := 0; i < candidateCount; i++ {
		candidates = append(candidates, roleToDelete{roleName: roleName, roleID: fmt.Sprintf("%s-%d", roleName, candidateCount-i-1)})
	}
	return coordinatedRoleState{
		roleName:   roleName,
		specIndex:  specIndex,
		target:     target,
		ready:      ready,
		inFlight:   inFlight,
		active:     true,
		candidates: candidates,
	}
}

func coordinationForTest(maxSkew string) *workloadv1alpha1.RoleRollingUpdateCoordination {
	value := intstr.FromString(maxSkew)
	return &workloadv1alpha1.RoleRollingUpdateCoordination{MaxSkew: &value}
}

func selectedCountByRole(selected []roleToDelete) map[string]int {
	result := make(map[string]int)
	for _, role := range selected {
		result[role.roleName]++
	}
	return result
}
