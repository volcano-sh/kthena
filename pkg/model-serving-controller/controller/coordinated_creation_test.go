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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func newCoordinatedCreationFixture(t *testing.T) (*ModelServingController, *api.ModelServing, api.Role, string, string) {
	t.Helper()
	ms := mergeReadyModelServing("creation-guard", 1, 1)
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].WorkerReplicas = 1
		ms.Spec.Template.Roles[i].WorkerTemplate = ms.Spec.Template.Roles[i].EntryTemplate.DeepCopy()
	}
	revision := utils.ModelServingRevision(ms)
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = revision, revision
	c := newRevisionTestController(t, ms)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, revision)
	return c, ms, ms.Spec.Template.Roles[0], ms.Name + "-0", revision
}

func hiddenCoordinatedMember(t *testing.T, c *ModelServingController, ms *api.ModelServing, role api.Role, group, revision string, ordinal, member int, mutate func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	id, hash := utils.GenerateRoleID(role.Name, ordinal), utils.CalRoleTemplateHash(role)
	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, group, id, revision, hash)
	if member > 0 {
		pod = utils.GenerateWorkerPod(*role.DeepCopy(), ms, group, id, member, revision, hash)
	}
	pod.UID = types.UID(pod.Name + "-live")
	if mutate != nil {
		mutate(pod)
	}
	created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	return created
}

func TestCoordinatedCreationRejectsHiddenOccupiedAndTerminatingSlots(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ordinal     int
		member      int
		terminating bool
		oldRevision bool
		wrongHash   bool
	}{
		{name: "hidden-entry-occupies-capacity", ordinal: 5},
		{name: "hidden-worker-occupies-capacity", ordinal: 5, member: 1},
		{name: "terminating-entry-occupies-capacity", ordinal: 5, terminating: true},
		{name: "terminating-worker-occupies-capacity", ordinal: 5, member: 1, terminating: true},
		{name: "same-slot-terminating-is-not-replay", terminating: true},
		{name: "same-slot-old-revision-is-not-replay", oldRevision: true},
		{name: "same-slot-wrong-hash-is-not-replay", member: 1, wrongHash: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ms, role, group, revision := newCoordinatedCreationFixture(t)
			occupant := hiddenCoordinatedMember(t, c, ms, role, group, revision, tc.ordinal, tc.member, func(pod *corev1.Pod) {
				if tc.terminating {
					pod.DeletionTimestamp = ptr.To(metav1.Now())
				}
				if tc.oldRevision {
					pod.Labels[api.RevisionLabelKey] = "different-revision"
				}
				if tc.wrongHash {
					pod.Labels[api.RoleTemplateHashLabelKey] = "different-template"
				}
			})
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			planning := withCoordinatedRound(context.Background(), ms)
			_, err := c.coordinatedCreationContext(planning, ms, group, role, role, 0, revision)
			require.Error(t, err)
			c.scaleUpRoles(planning, ms, group, role, nil, 1, 0, revision, true)
			requireNoMergeReadyPodMutations(t, client.Actions())
			require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role.Name, "prefill-0"))
			actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), occupant.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, occupant, actual)
			if tc.terminating && tc.ordinal == 5 {
				// Occupancy is released only after the final actual member is gone.
				require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(context.Background(), occupant.Name, metav1.DeleteOptions{}))
				c.scaleUpRoles(planning, ms, group, role, nil, 1, 0, revision, true)
				for member := 0; member < 2; member++ {
					pod, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), utils.GeneratePodName(group, "prefill-0", member), metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, revision, utils.ObjectRevision(pod))
				}
			}
		})
	}
}

func TestCoordinatedCreationReplaysOnlyTheSameAppliedIncarnation(t *testing.T) {
	for _, member := range []int{0, 1} {
		name := "entry-accepted"
		if member == 1 {
			name = "worker-accepted"
		}
		t.Run(name, func(t *testing.T) {
			c, ms, role, group, revision := newCoordinatedCreationFixture(t)
			accepted := hiddenCoordinatedMember(t, c, ms, role, group, revision, 0, member, nil)
			planning := withCoordinatedRound(context.Background(), ms)
			_, err := c.coordinatedCreationContext(planning, ms, group, role, role, 0, revision)
			require.NoError(t, err, "an accepted member may finish its missing siblings at full physical capacity")
			c.scaleUpRoles(planning, ms, group, role, nil, 1, 0, revision, true)
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2)
			for _, pod := range pods.Items {
				require.Equal(t, revision, utils.ObjectRevision(&pod))
				if pod.Name == accepted.Name {
					require.Equal(t, utils.ObjectRoleTemplateHash(accepted), utils.ObjectRoleTemplateHash(&pod))
				} else {
					require.Equal(t, canonicalRoleHashForTest(t, ms, role.Name), utils.ObjectRoleTemplateHash(&pod))
				}
				require.True(t, utils.IsOwnedByModelServingWithUID(&pod, ms.UID))
				if pod.Name == accepted.Name {
					require.Equal(t, accepted.UID, pod.UID)
				}
			}
			require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role.Name, "prefill-0"))
		})
	}
}

// Reproduce the exact Role hash emitted by upstream/main 2eff0aee before
// canonical JSON hashing; the current CalRoleTemplateHash is not that hash.
func TestCoordinatedCreationReplaysPreCanonicalSpewHash(t *testing.T) {
	for _, member := range []int{0, 1} {
		t.Run(fmt.Sprintf("accepted-member-%d", member), func(t *testing.T) {
			c, ms, role, group, revision := newCoordinatedCreationFixture(t)
			role.EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: api.GroupSizeEnv, Value: "legacy-user-value"}}
			role.EntryTemplate.Spec.SchedulerName = ""
			ms.Spec.Template.Roles[0] = role
			require.Equal(t, "volcano", ms.Spec.SchedulerName)
			require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(context.Background(), utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
			_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, revision, ms.Spec.Template.Roles)
			require.NoError(t, err)
			legacyRole := *role.DeepCopy()
			legacyRole.Replicas = nil
			legacyRole.RollingUpdateConfiguration = api.RollingUpdateConfiguration{}
			legacyHash := utils.Revision(legacyRole)
			require.NotEqual(t, utils.CalRoleTemplateHash(role), legacyHash)
			require.NotEqual(t, canonicalRoleHashForTest(t, ms, role.Name), legacyHash)
			accepted := hiddenCoordinatedMember(t, c, ms, role, group, revision, 0, member, func(pod *corev1.Pod) { pod.Labels[api.RoleTemplateHashLabelKey] = legacyHash })
			ctx := c.withRevisionHistory(withCoordinatedRound(context.Background(), ms), ms)
			require.NoError(t, c.scaleUpRoles(ctx, ms, group, role, nil, 1, 0, revision, true))
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2, "legacy partial CREATE must finish the missing sibling")
			for _, pod := range pods.Items {
				require.Equal(t, revision, utils.ObjectRevision(&pod))
				if pod.Name == accepted.Name {
					require.Equal(t, accepted.UID, pod.UID)
					require.Equal(t, legacyHash, utils.ObjectRoleTemplateHash(&pod))
				} else {
					require.Equal(t, canonicalRoleHashForTest(t, ms, role.Name), utils.ObjectRoleTemplateHash(&pod))
				}
			}
		})
	}
}

func TestCoordinatedCreationDoesNotAdoptForeignSameNamePod(t *testing.T) {
	c, ms, role, group, revision := newCoordinatedCreationFixture(t)
	foreign := hiddenCoordinatedMember(t, c, ms, role, group, revision, 0, 0, func(pod *corev1.Pod) {
		pod.OwnerReferences[0].UID = "previous-model-serving-owner"
	})
	c.scaleUpRoles(withCoordinatedRound(context.Background(), ms), ms, group, role, nil, 1, 0, revision, true)
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// The stale same-named entry is deleted rather than adopted, and it must not grant
	// permission to create target workers or adopt the Role.
	require.Empty(t, pods.Items, "a foreign entry must not grant permission to create target workers or adopt the Role")
	require.NotEmpty(t, foreign.Name)
	require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role.Name, "prefill-0"))
}

func TestCoordinatedCreationRejectsLiveForeignDespiteStaleOwnedLister(t *testing.T) {
	c, ms, role, group, revision := newCoordinatedCreationFixture(t)
	foreign := hiddenCoordinatedMember(t, c, ms, role, group, revision, 0, 0, func(pod *corev1.Pod) {
		pod.OwnerReferences[0].UID = "previous-model-serving-owner"
	})
	stale := foreign.DeepCopy()
	stale.UID = "stale-owned-incarnation"
	stale.OwnerReferences[0].UID = ms.UID
	require.NoError(t, c.podsInformer.GetIndexer().Add(stale))
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()

	// Exercise the AlreadyExists boundary directly: scaleUpRoles would already
	// see the stale entry's indexed ordinal as occupied and never reach it.
	err := c.CreatePodsByRole(withCoordinatedRound(context.Background(), ms), *role.DeepCopy(), ms, 0, 0, revision, utils.CalRoleTemplateHash(role))
	require.ErrorContains(t, err, "does not match expected identity")
	require.ErrorContains(t, err, "owner=false")
	entryAttempted, entryReadLive := false, false
	for _, action := range client.Actions() {
		if action.GetResource().Resource != "pods" {
			continue
		}
		if creation, ok := action.(clienttesting.CreateAction); ok {
			pod := creation.GetObject().(*corev1.Pod)
			require.Equal(t, foreign.Name, pod.Name, "a rejected entry cannot grant permission to create workers")
			entryAttempted = true
		}
		if read, ok := action.(clienttesting.GetAction); ok && read.GetName() == foreign.Name {
			entryReadLive = true
		}
	}
	require.True(t, entryAttempted, "the test must reach an actual AlreadyExists response")
	require.True(t, entryReadLive, "coordinated replay verification must read live identity despite a matching stale lister copy")
	pods, err := client.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, pods.Items, "the stale same-named entry is deleted, and no workers are created")
	require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role.Name, "prefill-0"))
}

func TestCoordinatedCreationRechecksDependenciesAfterPlanning(t *testing.T) {
	f := newCoordinatedInPlaceFixture(t)
	f.ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}
	for i := range f.ms.Spec.Template.Roles {
		f.ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](5)
	}
	decode := f.ms.Spec.Template.Roles[1]
	for member := 0; member < 2; member++ {
		pod := f.pod(t, decode.Name, 3, member)
		pod.Labels[api.RevisionLabelKey] = f.newRev
		pod.Labels[api.RoleTemplateHashLabelKey] = utils.CalRoleTemplateHash(decode)
		pod.Spec.Containers[0].Image = "img:v2"
		_, err := f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	planning := withCoordinatedRound(context.Background(), f.ms)
	require.NoError(t, f.controller.refreshCoordinatedAvailabilityInRound(planning, f.ms))
	policy, err := f.controller.resolveRoleRolloutPolicy(planning, f.ms, f.newRev)
	require.NoError(t, err)
	require.True(t, policy.allowTargetStart(f.group, "prefill"))
	prefill := f.ms.Spec.Template.Roles[0]
	instances, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), f.group, prefill.Name)
	require.NoError(t, err)
	worker := f.pod(t, decode.Name, 3, 1)
	worker.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err = f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).UpdateStatus(context.Background(), worker, metav1.UpdateOptions{})
	require.NoError(t, err)
	client := f.controller.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()
	f.controller.scaleUpRoles(planning, f.ms, f.group, prefill, instances, 5, 0, f.newRev, true)
	requireNoMergeReadyPodMutations(t, client.Actions())
	require.Equal(t, datastore.RoleNotFound, f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, prefill.Name, "prefill-4"))
}

func TestCoordinatedCreationEstablishesInitialDefaultGangBeforeDependenciesAreReady(t *testing.T) {
	ms := mergeReadyModelServing("initial-default-gang", 1, 2)
	ms.Status = api.ModelServingStatus{}
	ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		role.WorkerReplicas = 1
		role.EntryTemplate.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "test.kthena.io/initial-ready"}}
		role.WorkerTemplate = role.EntryTemplate.DeepCopy()
	}
	require.Nil(t, ms.Spec.Template.GangPolicy, "the initial Gang uses the default full membership")
	require.Equal(t, "1", ms.ResourceVersion)
	revision := utils.ModelServingRevision(ms)
	c := newRevisionTestController(t, ms)
	key, group := utils.GetNamespaceName(ms), ms.Name+"-0"
	c.store.AddServingGroup(key, 0, revision)
	crd := &apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "podgroups.scheduling.volcano.sh"},
		Spec: apiextv1.CustomResourceDefinitionSpec{Versions: []apiextv1.CustomResourceDefinitionVersion{{
			Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Properties: map[string]apiextv1.JSONSchemaProps{
				"spec": {Properties: map[string]apiextv1.JSONSchemaProps{"subGroupPolicy": {}}},
			}}},
		}}}}
	volcano := volcanofake.NewSimpleClientset()
	c.podGroupManager = podgroupmanager.NewManager(c.kubeClientSet, volcano, apiextfake.NewSimpleClientset(crd), nil)
	planning := withCoordinatedRound(context.Background(), ms)
	require.NoError(t, c.createOrUpdatePodGroupByServingGroup(planning, ms, group))
	pg, err := volcano.SchedulingV1beta1().PodGroups(ms.Namespace).Get(context.Background(), group, metav1.GetOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 8, pg.Spec.MinMember)
	require.Len(t, pg.Spec.SubGroupPolicy, 2)
	for _, subgroup := range pg.Spec.SubGroupPolicy {
		require.EqualValues(t, 2, *subgroup.MinSubGroups)
		require.EqualValues(t, 2, *subgroup.SubGroupSize)
	}
	// Prefill is intentionally first, with no existing decode Ready capacity.
	// Initial population must not depend on a previous rollout's startup gate.
	for _, role := range ms.Spec.Template.Roles {
		c.scaleUpRoles(planning, ms, group, role, nil, 2, 0, revision, true)
		instances, err := c.store.GetRoleList(key, group, role.Name)
		require.NoError(t, err)
		require.Len(t, instances, 2)
		for _, instance := range instances {
			require.Equal(t, datastore.RoleCreating, instance.Status)
		}
	}
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 8, "all initial members must exist before any dependency has acquired Ready credit")
	for _, pod := range pods.Items {
		require.True(t, utils.IsOwnedByModelServingWithUID(&pod, ms.UID))
		require.Equal(t, revision, utils.ObjectRevision(&pod))
		require.Equal(t, "volcano", pod.Spec.SchedulerName)
		require.False(t, utils.IsPodRunningAndReady(&pod))
		require.Equal(t, []corev1.PodReadinessGate{{ConditionType: "test.kthena.io/initial-ready"}}, pod.Spec.ReadinessGates)
	}
	require.Empty(t, ms.Status.CurrentRevision, "no historical rollout baseline or dependency Ready credit was invented")
	require.Nil(t, ms.Spec.Template.GangPolicy, "creating the initial set does not lower the default Gang")
}

func TestCoordinatedSurgeCleanupRechecksHealthyCapacityAfterPlanning(t *testing.T) {
	for _, lostWorker := range []bool{false, true} {
		name := "healthy-credit"
		if lostWorker {
			name = "credit-consumed-by-worker-failure"
		}
		t.Run(name, func(t *testing.T) {
			ms := mergeReadyModelServing("surge-fresh-budget", 1, 2)
			for i := range ms.Spec.Template.Roles {
				role := &ms.Spec.Template.Roles[i]
				role.WorkerReplicas = 1
				role.WorkerTemplate = role.EntryTemplate.DeepCopy()
				role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt(0)), ptr.To(intstr.FromInt(1))
			}
			revision := utils.ModelServingRevision(ms)
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = revision, revision
			c := newRevisionTestController(t, ms)
			key, group := utils.GetNamespaceName(ms), ms.Name+"-0"
			c.store.AddServingGroup(key, 0, revision)
			var sibling, temporary []*corev1.Pod
			for _, role := range ms.Spec.Template.Roles {
				limit := 2
				if role.Name == "prefill" {
					limit = 3
				}
				for ordinal := 0; ordinal < limit; ordinal++ {
					pods := addMergeReadyInstance(t, c, ms, group, role, ordinal, revision, revision, utils.CalRoleTemplateHash(role))
					if role.Name == "prefill" && ordinal == 0 {
						sibling = pods
					}
					if ordinal == 2 {
						temporary = pods
						for _, pod := range temporary {
							setSurgeScope(pod, surgeRole)
							_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
							require.NoError(t, err)
							require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
						}
					}
				}
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			planning := withCoordinatedRound(context.Background(), ms)
			require.NoError(t, c.refreshCoordinatedAvailabilityInRound(planning, ms))
			prefill := ms.Spec.Template.Roles[0]
			instances, err := c.store.GetRoleList(key, group, prefill.Name)
			require.NoError(t, err)
			budget, err := roleRolloutBudget(prefill, instances, 0)
			require.NoError(t, err)
			require.Equal(t, 1, budget.healthy, "planning snapshot grants one healthy cleanup")
			if lostWorker {
				worker := sibling[1].DeepCopy()
				worker.Status.Conditions[0].Status = corev1.ConditionFalse
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), worker, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			handled, err := c.retireCoordinatedSurge(planning, ms, group, prefill, instances, 2)
			require.NoError(t, err)
			require.True(t, handled)
			if lostWorker {
				requireNoMergeReadyPodMutations(t, client.Actions())
				require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, prefill.Name, "prefill-2"))
				require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, group, prefill.Name, "prefill-0"))
				for _, before := range temporary {
					actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), before.Name, metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, before.UID, actual.UID)
					require.Nil(t, actual.DeletionTimestamp)
				}
				return
			}
			deleted := false
			for _, action := range client.Actions() {
				if _, ok := action.(clienttesting.DeleteCollectionAction); ok && action.GetResource().Resource == "pods" {
					deleted = true
				}
			}
			require.True(t, deleted, "healthy positive control must retire the temporary Role")
			require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, group, prefill.Name, "prefill-2"))
		})
	}
}
