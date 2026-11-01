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
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestProtectedTargetDoesNotRetainPhantomOldDependency(t *testing.T) {
	role := api.Role{Name: "a", Replicas: ptr.To[int32](2)}
	ms := &api.ModelServing{Spec: api.ModelServingSpec{Template: api.ServingGroup{Roles: []api.Role{role}}}}
	hash := utils.CalRoleTemplateHash(role)
	instances := []datastore.Role{{Name: "a-0", RoleTemplateHash: hash, Status: datastore.RoleRunning}, {Name: "a-1", RoleTemplateHash: hash, Status: datastore.RoleRunning}}
	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(), ms, datastore.ServingGroup{Name: "model-0"}, role, 2, instances, 1, true, nil, nil)
	require.False(t, state.hasOldVersion)
	require.Equal(t, 2, state.versionReady)
}

func TestMissingFormalCapacityDoesNotBypassDependencyStartup(t *testing.T) {
	role := api.Role{Name: "a", Replicas: ptr.To[int32](2)}
	ms := &api.ModelServing{Spec: api.ModelServingSpec{Template: api.ServingGroup{Roles: []api.Role{role}}}}
	state := (&ModelServingController{}).resolveRoleRolloutState(context.Background(), ms, datastore.ServingGroup{Name: "model-0"}, role, 2, nil, 0, true, nil, nil)
	require.Equal(t, targetNotStarted, state.targetState)
	coordination := coordinationForTest("100%")
	coordination.Dependencies = []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b"}}}
	decision, err := calculateRoleRolloutLimits([]coordinatedRoleState{state, newCoordinatedRoleStateForTest("b", 2, 0, 0)}, coordination)
	require.NoError(t, err)
	require.False(t, decision.roles["a"].allowTargetStart)
}

func TestCoordinatedSurgeCleanupWaitsForFormalReady(t *testing.T) {
	ms := createStandardModelServing("cleanup", 1, 1)
	ms.UID = "cleanup-owner"
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
	role := ms.Spec.Template.Roles[0]
	role.WorkerReplicas = 0
	role.MaxUnavailable = ptr.To(intstr.FromInt(0))
	role.MaxSurge = ptr.To(intstr.FromInt(1))
	ms.Spec.Template.Roles[0] = role
	c := newRevisionTestController(t, ms)
	key := utils.GetNamespaceName(ms)
	c.store.AddServingGroup(key, 0, utils.ModelServingRevision(ms))
	for i := 0; i < 2; i++ {
		id := utils.GenerateRoleID(role.Name, i)
		pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, "cleanup-0", id, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
		pod.UID = ms.UID
		if i == 1 {
			setSurgeScope(pod, surgeRole)
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		c.store.AddRole(key, "cleanup-0", role.Name, id, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
		if i == 1 {
			require.NoError(t, c.store.UpdateRoleStatus(key, "cleanup-0", role.Name, id, datastore.RoleRunning))
		}
	}
	instances, err := c.store.GetRoleList(key, "cleanup-0", role.Name)
	require.NoError(t, err)
	require.True(t, c.retireCoordinatedSurge(context.Background(), ms, "cleanup-0", role, instances, 1))
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 0)))
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 1)))
	require.NoError(t, c.store.UpdateRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 0), datastore.RoleRunning))
	instances, err = c.store.GetRoleList(key, "cleanup-0", role.Name)
	require.NoError(t, err)
	require.True(t, c.retireCoordinatedSurge(context.Background(), ms, "cleanup-0", role, instances, 1))
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 0)))
	require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 1)))
}

func TestCoordinatedSurgeAdoptionRemovesOriginWithoutReplacingUID(t *testing.T) {
	ms := createStandardModelServing("adoption", 1, 2)
	ms.UID = "adoption-owner"
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
	c := newRevisionTestController(t, ms)
	role := ms.Spec.Template.Roles[0]
	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, "adoption-0", utils.GenerateRoleID(role.Name, 1), utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
	pod.UID = "surviving-uid"
	setSurgeScope(pod, surgeRole)
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	require.NoError(t, c.adoptCoordinatedSurge(context.Background(), ms, "adoption-0", role))
	actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, pod.UID, actual.UID)
	require.NotContains(t, actual.Annotations, surgeAnnotation)
	require.Contains(t, pod.Annotations, surgeAnnotation, "informer-owned Pod must not be mutated")
}

func TestCoordinatedReplicaBaselineRejectsForeignRevisionOwner(t *testing.T) {
	ms := createStandardModelServing("foreign", 1, 2)
	ms.UID = "current-owner"
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("20%")}
	c := newRevisionTestController(t, ms)
	revision := utils.ModelServingRevision(ms)
	cr, err := utils.GetControllerRevision(context.Background(), c.kubeClientSet, ms, revision)
	require.NoError(t, err)
	cr.OwnerReferences[0].UID = "retired-owner"
	_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Update(context.Background(), cr, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Error(t, c.persistCoordinatedRoleRevision(context.Background(), ms, revision))
	cr, err = utils.GetControllerRevision(context.Background(), c.kubeClientSet, ms, revision)
	require.NoError(t, err)
	require.NotContains(t, cr.Annotations, coordinatedRoleReplicaBaselineAnnotation)
}

func TestSparseStableRepairDoesNotBecomeTemporarySurge(t *testing.T) {
	ms := createStandardModelServing("sparse", 1, 1)
	ms.UID = "sparse-owner"
	ms.Spec.Template.Roles[0].WorkerReplicas = 0
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
	c := newRevisionTestController(t, ms)
	role := ms.Spec.Template.Roles[0]
	require.NoError(t, c.CreatePodsByRole(context.Background(), role, ms, 5, 0, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role)))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	require.NotContains(t, pods.Items[0].Annotations, surgeAnnotation)
}

func TestTerminatingCapacityPreventsExtraCreationIntoSparseHole(t *testing.T) {
	ms := createStandardModelServing("occupied", 1, 1)
	ms.UID = "occupied-owner"
	ms.Spec.Template.Roles[0].WorkerReplicas = 0
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
	c := newRevisionTestController(t, ms)
	role := ms.Spec.Template.Roles[0]
	key := utils.GetNamespaceName(ms)
	revision := utils.ModelServingRevision(ms)
	c.store.AddServingGroup(key, 0, revision)
	c.store.AddRole(key, "occupied-0", role.Name, utils.GenerateRoleID(role.Name, 5), revision, utils.CalRoleTemplateHash(role))
	for _, i := range []int{1, 5} {
		pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, "occupied-0", utils.GenerateRoleID(role.Name, i), revision, utils.CalRoleTemplateHash(role))
		if i == 1 {
			pod.DeletionTimestamp = ptr.To(metav1.Now())
			setSurgeScope(pod, surgeRole)
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	}
	instances, err := c.store.GetRoleList(key, "occupied-0", role.Name)
	require.NoError(t, err)
	c.scaleUpRoles(context.Background(), ms, "occupied-0", role, instances, 2, 0, revision, true)
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2)
	for _, pod := range pods.Items {
		require.NotEqual(t, utils.GenerateRoleID(role.Name, 0), utils.GetRoleID(&pod))
	}
}

func TestCoordinatedOrdinalSelectionPreservesSparseFormalIdentities(t *testing.T) {
	var selected []int
	forEachCoordinatedOrdinal(2, 0, []int{0, 5}, 1, func(i int) bool { selected = append(selected, i); return true })
	require.Equal(t, []int{2}, selected)
	selected = nil
	forEachCoordinatedOrdinal(3, 1, []int{0, 2}, 1, func(i int) bool { selected = append(selected, i); return true })
	require.Equal(t, []int{1}, selected)
}

func TestCoordinatedSurgeCompletionRefillsFormalHoleAtDesiredCount(t *testing.T) {
	for _, surge := range []int{0, 1} {
		t.Run(string(rune('0'+surge)), func(t *testing.T) {
			ms := createStandardModelServing("final-slot", 1, 1)
			ms.UID = "final-slot-owner"
			role := &ms.Spec.Template.Roles[0]
			role.WorkerReplicas = 0
			role.MaxSurge = ptr.To(intstr.FromInt(surge))
			role.MaxUnavailable = ptr.To(intstr.FromInt(1 - surge))
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
			c := newRevisionTestController(t, ms)
			revision := utils.ModelServingRevision(ms)
			key := utils.GetNamespaceName(ms)
			c.store.AddServingGroup(key, 0, revision)
			id := utils.GenerateRoleID(role.Name, 1)
			pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, "final-slot-0", id, revision, utils.CalRoleTemplateHash(*role))
			pod.UID = "temporary-target"
			setSurgeScope(pod, surgeRole)
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			c.store.AddRole(key, "final-slot-0", role.Name, id, revision, utils.CalRoleTemplateHash(*role))
			require.NoError(t, c.store.UpdateRoleStatus(key, "final-slot-0", role.Name, id, datastore.RoleRunning))
			c.manageRoleReplicasPerGroup(context.Background(), ms, "final-slot-0", *role, 0, revision, true)
			if surge == 1 {
				pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, 2)
				require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, "final-slot-0", role.Name, utils.GenerateRoleID(role.Name, 0)))
				require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, "final-slot-0", role.Name, id))
			} else {
				require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, "final-slot-0", role.Name, id))
			}
		})
	}
}

func TestCoordinatedAvailabilityUsesLivePodDespiteStaleDatastoreAndInformer(t *testing.T) {
	ms := createStandardModelServing("live", 1, 1)
	ms.UID = "live-owner"
	ms.ResourceVersion = "live-rv"
	ms.Spec.Template.Roles[0].WorkerReplicas = 0
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
	c := newRevisionTestController(t, ms)
	role := ms.Spec.Template.Roles[0]
	key := utils.GetNamespaceName(ms)
	id := utils.GenerateRoleID(role.Name, 0)
	c.store.AddServingGroup(key, 0, "missing-retired-revision")
	c.store.AddRole(key, "live-0", role.Name, id, "missing-retired-revision", "stale-hash")
	require.NoError(t, c.store.UpdateRoleStatus(key, "live-0", role.Name, id, datastore.RoleRunning))
	actual := utils.GenerateEntryPod(*role.DeepCopy(), ms, "live-0", id, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
	actual.Status.Phase = corev1.PodRunning
	actual.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), actual, metav1.CreateOptions{})
	require.NoError(t, err)
	stale := actual.DeepCopy()
	stale.Labels[api.RevisionLabelKey] = "missing-retired-revision"
	stale.Labels[api.RoleTemplateHashLabelKey] = "stale-hash"
	require.NoError(t, c.podsInformer.GetIndexer().Add(stale))
	instances, err := c.store.GetRoleList(key, "live-0", role.Name)
	require.NoError(t, err)
	require.Equal(t, templateEquivalent, c.compareRoleTemplate(context.Background(), ms, datastore.ServingGroup{Name: "live-0"}, role.Name, instances[0]))
	projected, err := c.coordinatedPodGroupLayout(ms, "live-0")
	require.NoError(t, err)
	require.Equal(t, role.WorkerReplicas, projected.Spec.Template.Roles[0].WorkerReplicas)
	actual.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), actual, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.refreshCoordinatedAvailability(ms))
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, "live-0", role.Name, id), "cached Ready from the retired incarnation must not grant deletion credit")
	ready, err := c.checkRoleReady(ms, "live-0", role.Name, id)
	require.NoError(t, err)
	require.False(t, ready)
}
