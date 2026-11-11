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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func mergeReadyModelServing(name string, groups, replicas int32) *api.ModelServing {
	ms := createStandardModelServing(name, groups, replicas)
	ms.UID, ms.ResourceVersion = types.UID(name+"-owner"), "1"
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
	second := *ms.Spec.Template.Roles[0].DeepCopy()
	second.Name = "decode"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].MaxUnavailable = ptr.To(intstr.FromInt(1))
	}
	ms.Status.CurrentRevision = utils.ModelServingRevision(ms)
	ms.Status.UpdateRevision = ms.Status.CurrentRevision
	return ms
}

// The live entry/worker Pods and datastore can intentionally describe different
// incarnations. This models informer delivery lag without modifying live Pods.
func addMergeReadyInstance(t *testing.T, c *ModelServingController, ms *api.ModelServing, group string, role api.Role, ordinal int, liveRevision, cachedRevision, cachedHash string) []*corev1.Pod {
	t.Helper()
	key := utils.GetNamespaceName(ms)
	id, liveHash := utils.GenerateRoleID(role.Name, ordinal), utils.CalRoleTemplateHash(role)
	c.store.AddRole(key, group, role.Name, id, cachedRevision, cachedHash)
	var pods []*corev1.Pod
	for worker := 0; worker <= int(role.WorkerReplicas); worker++ {
		var pod *corev1.Pod
		if worker == 0 {
			pod = utils.GenerateEntryPod(*role.DeepCopy(), ms, group, id, liveRevision, liveHash)
		} else {
			pod = utils.GenerateWorkerPod(*role.DeepCopy(), ms, group, id, worker, liveRevision, liveHash)
		}
		pod.UID = types.UID(pod.Name + "-uid")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(created))
		c.store.AddRunningPodToServingGroup(key, group, created.Name, cachedRevision, cachedHash, role.Name, id)
		pods = append(pods, created)
	}
	require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, id, datastore.RoleRunning))
	return pods
}

func requireNoMergeReadyPodMutations(t *testing.T, actions []clienttesting.Action) {
	t.Helper()
	for _, action := range actions {
		if action.GetResource().Resource != "pods" {
			continue
		}
		require.Contains(t, []string{"get", "list", "watch"}, action.GetVerb(), "unexpected Pod mutation during observation: %s", action.GetVerb())
	}
}

func TestServingGroupRolloutRechecksCompleteLiveMembers(t *testing.T) {
	for _, variant := range []string{"complete", "worker unready", "worker missing", "worker terminating", "foreign worker", "replacement target"} {
		t.Run(variant, func(t *testing.T) {
			ms := semanticRevisionTestModelServing("live-budget", "target")
			ms.ResourceVersion = "1"
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1))}}
			ms.Spec.Template.Roles[0].WorkerReplicas = 1
			ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			c := newRevisionTestController(t, ms)
			old := ms.DeepCopy()
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
			recordTestRevision(t, c.kubeClientSet, old, "old")
			key, target := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms)
			var instances [][]*corev1.Pod
			for ordinal, revision := range []string{"old", target} {
				group, role := fmt.Sprintf("%s-%d", ms.Name, ordinal), ms.Spec.Template.Roles[0]
				if ordinal == 0 {
					role = old.Spec.Template.Roles[0]
				}
				c.store.AddServingGroup(key, ordinal, revision)
				instances = append(instances, addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role)))
				require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			}
			worker := instances[1][1].DeepCopy()
			client := c.kubeClientSet.(*kubefake.Clientset)
			switch variant {
			case "worker unready":
				worker.Status.Conditions[0].Status = corev1.ConditionFalse
				_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), worker, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "worker missing":
				require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(context.Background(), worker.Name, metav1.DeleteOptions{}))
			case "worker terminating":
				worker.DeletionTimestamp = ptr.To(metav1.Now())
				_, err := client.CoreV1().Pods(ms.Namespace).Update(context.Background(), worker, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "foreign worker":
				worker.OwnerReferences[0].UID = "another-owner"
				_, err := client.CoreV1().Pods(ms.Namespace).Update(context.Background(), worker, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "replacement target":
				for _, pod := range instances[0] {
					replacement := pod.DeepCopy()
					replacement.UID += "-replacement"
					replacement.Labels[api.RevisionLabelKey] = target
					require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}))
					_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), replacement, metav1.CreateOptions{})
					require.NoError(t, err)
				}
			}
			// Deliberately leave informer/store Ready, revision and UID stale.
			client.ClearActions()
			require.NoError(t, c.manageRollingUpdate(context.Background(), ms, target))
			if variant == "complete" {
				_, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), instances[0][0].Name, metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err), "complete surge supplies one healthy-delete allowance")
			} else {
				requireNoMergeReadyPodMutations(t, client.Actions())
				actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), instances[0][0].Name, metav1.GetOptions{})
				require.NoError(t, err)
				if variant == "replacement target" {
					require.NotEqual(t, instances[0][0].UID, actual.UID)
				} else {
					require.Equal(t, instances[0][0].UID, actual.UID)
				}
			}
		})
	}
}

func TestOrdinaryReadinessLossEnqueuesWithoutRecovery(t *testing.T) {
	ms := semanticRevisionTestModelServing("probe-readiness", "target")
	ms.ResourceVersion = "1"
	ms.Status.CurrentRevision = utils.ModelServingRevision(ms)
	c := newRevisionTestController(t, ms)
	c.initialSync.Store(true)
	key, group, revision := utils.GetNamespaceName(ms), ms.Name+"-0", utils.ModelServingRevision(ms)
	c.store.AddServingGroup(key, 0, revision)
	pod := addMergeReadyInstance(t, c, ms, group, ms.Spec.Template.Roles[0], 0, revision, revision, utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0]))[0]
	require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
	client := c.kubeClientSet.(*kubefake.Clientset)
	for _, ready := range []bool{false, true, false} {
		updated := pod.DeepCopy()
		updated.Status.Conditions[0].Status = corev1.ConditionFalse
		if ready {
			updated.Status.Conditions[0].Status = corev1.ConditionTrue
		}
		_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), updated, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Update(updated))
		client.ClearActions()
		c.updatePod(pod, updated)
		require.NotZero(t, c.workqueue.Len(), "both directions must reconcile complete availability")
		require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
		actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
		require.NoError(t, err)
		if ready {
			require.EqualValues(t, 1, actual.Status.AvailableReplicas)
		} else {
			require.Zero(t, actual.Status.AvailableReplicas)
		}
		requireNoMergeReadyPodMutations(t, client.Actions())
	}
	require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupDeleting))
	require.NoError(t, c.handleReadyPod(ms, group, pod))
	require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, group))
}

func TestCoordinatedStableReconcileReadsDoNotGrowPerInstance(t *testing.T) {
	for _, replicas := range []int32{1, 10, 50} {
		t.Run(fmt.Sprintf("instances-%d", 2*replicas), func(t *testing.T) {
			ms := mergeReadyModelServing("stable-reads", 1, replicas)
			c := newRevisionTestController(t, ms)
			key, revision, group := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms), ms.Name+"-0"
			c.store.AddServingGroup(key, 0, revision)
			for _, role := range ms.Spec.Template.Roles {
				for i := 0; i < int(replicas); i++ {
					addMergeReadyInstance(t, c, ms, group, role, i, revision, revision, utils.CalRoleTemplateHash(role))
				}
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
			actions := client.Actions()
			reads := map[string]int{}
			for _, action := range actions {
				if action.GetVerb() == "get" || action.GetVerb() == "list" {
					reads[action.GetVerb()+" "+action.GetResource().Resource]++
				}
			}
			t.Logf("stable %d instances: %v", 2*replicas, reads)
			// Fixed headroom allows target-history bookkeeping without permitting
			// an API request per entry, worker, or repeated template comparison.
			require.LessOrEqual(t, reads["list pods"], 4)
			require.LessOrEqual(t, reads["get pods"], 2)
			require.LessOrEqual(t, reads["get controllerrevisions"], 4)
			requireNoMergeReadyPodMutations(t, actions)
			actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.EqualValues(t, 1, actual.Status.AvailableReplicas)
			require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, group))
		})
	}
}

func TestCoordinatedMissingHistoryDoesNotBlockExplicitScaleToZero(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprintf("retain-group-%t", retain), func(t *testing.T) {
			desired := int32(0)
			if retain {
				desired = 1
			}
			ms := mergeReadyModelServing("missing-history", desired, 1)
			c := newRevisionTestController(t, ms)
			key, revision, group := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms), ms.Name+"-0"
			c.store.AddServingGroup(key, 0, revision)
			for _, role := range ms.Spec.Template.Roles {
				addMergeReadyInstance(t, c, ms, group, role, 0, "missing-applied-history", "missing-applied-history", utils.CalRoleTemplateHash(role))
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			err := c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name)
			if retain {
				require.ErrorContains(t, err, "missing-applied-history")
				require.NotEqual(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, group))
				requireNoMergeReadyPodMutations(t, client.Actions())
				return
			}
			require.NoError(t, err)
			require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, group))
			deleted := 0
			for _, action := range client.Actions() {
				if deletion, ok := action.(clienttesting.DeleteAction); ok && action.GetResource().Resource == "pods" {
					require.Contains(t, []string{utils.GeneratePodName(group, "prefill-0", 0), utils.GeneratePodName(group, "decode-0", 0)}, deletion.GetName())
					require.NotNil(t, deletion.GetDeleteOptions().Preconditions.UID)
					deleted++
				}
			}
			require.Equal(t, 2, deleted, "explicit zero replicas deletes each observed member without decoding its old template")
		})
	}
}

func TestCoordinatedSparseOrdinalOutsidePartitionCanCreateSurge(t *testing.T) {
	ms := mergeReadyModelServing("sparse-partition", 1, 3)
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		role.Partition = ptr.To(intstr.FromInt(1))
		role.MaxUnavailable = ptr.To(intstr.FromInt(0))
		role.MaxSurge = ptr.To(intstr.FromInt(1))
	}
	ms.Status.CurrentRevision = "old"
	ms.Status.UpdateRevision = utils.ModelServingRevision(ms)
	c := newRevisionTestController(t, ms)
	recordDifferentRevision(t, c, ms, "old")
	oldRoles, err := c.roleSpecsFromRevision(ms, "old")
	require.NoError(t, err)
	key, revision, group := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms), ms.Name+"-0"
	c.store.AddServingGroup(key, 0, "old")
	prefill, decode := ms.Spec.Template.Roles[0], ms.Spec.Template.Roles[1]
	var original []*corev1.Pod
	for _, ordinal := range []int{2, 4, 5} {
		applied, appliedRevision := prefill, revision
		if ordinal == 2 {
			applied, appliedRevision = oldRoles[prefill.Name], "old"
		}
		original = append(original, addMergeReadyInstance(t, c, ms, group, applied, ordinal, appliedRevision, appliedRevision, utils.CalRoleTemplateHash(applied))...)
	}
	for i := 0; i < 3; i++ {
		addMergeReadyInstance(t, c, ms, group, decode, i, revision, revision, utils.CalRoleTemplateHash(decode))
	}
	require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
	require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: api.RoleLabelKey + "=" + prefill.Name})
	require.NoError(t, err)
	require.Len(t, pods.Items, 4, "ordinal 2 is outside P=1; U=0/S=1 must establish target surge before deleting healthy capacity")
	originalUIDs := map[string]types.UID{}
	for _, pod := range original {
		originalUIDs[pod.Name] = pod.UID
	}
	surgeCount := 0
	for _, pod := range pods.Items {
		if uid, exists := originalUIDs[pod.Name]; exists {
			require.Equal(t, uid, pod.UID)
			require.NotContains(t, pod.Annotations, surgeAnnotation, "unmarked high formal survivors retain their identity")
			require.Nil(t, pod.DeletionTimestamp)
			require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, prefill.Name, utils.GetRoleID(&pod)))
			continue
		}
		surgeCount++
		require.Equal(t, surgeRole, pod.Annotations[surgeAnnotation])
		require.Equal(t, revision, utils.ObjectRevision(&pod))
		require.Equal(t, canonicalRoleHashForTest(t, ms, prefill.Name), utils.ObjectRoleTemplateHash(&pod))
	}
	require.Equal(t, 1, surgeCount)
}

func TestCoordinatedLiveTemplateOverridesBothCachedHashDirections(t *testing.T) {
	for _, liveOld := range []bool{true, false} {
		t.Run(fmt.Sprintf("live-old-%t", liveOld), func(t *testing.T) {
			ms := mergeReadyModelServing("cache-direction", 1, 1)
			c := newRevisionTestController(t, ms)
			recordDifferentRevision(t, c, ms, "old")
			oldRoles, err := c.roleSpecsFromRevision(ms, "old")
			require.NoError(t, err)
			key, revision, group := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms), ms.Name+"-0"
			c.store.AddServingGroup(key, 0, revision)
			prefill, decode := ms.Spec.Template.Roles[0], ms.Spec.Template.Roles[1]
			applied, liveRevision, cachedRevision, cachedHash := prefill, revision, "old", utils.CalRoleTemplateHash(oldRoles[prefill.Name])
			if liveOld {
				applied, liveRevision, cachedRevision, cachedHash = oldRoles[prefill.Name], "old", revision, utils.CalRoleTemplateHash(prefill)
			}
			addMergeReadyInstance(t, c, ms, group, applied, 0, liveRevision, cachedRevision, cachedHash)
			addMergeReadyInstance(t, c, ms, group, decode, 0, revision, revision, utils.CalRoleTemplateHash(decode))
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			instances, err := c.store.GetRoleList(key, group, prefill.Name)
			require.NoError(t, err)
			outdated, unavailable := c.outdatedRoles(ms, datastore.ServingGroup{Name: group, Revision: revision}, prefill, instances)
			require.Zero(t, unavailable)
			if liveOld {
				require.Len(t, outdated, 1, "known old live entry remains a candidate when the cached incarnation has the target hash")
			} else {
				require.Empty(t, outdated, "known target live entry is never replaced because a cached incarnation was old")
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
			if liveOld {
				require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, group, prefill.Name, utils.GenerateRoleID(prefill.Name, 0)))
			} else {
				require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, prefill.Name, utils.GenerateRoleID(prefill.Name, 0)))
				requireNoMergeReadyPodMutations(t, client.Actions())
			}
		})
	}
}

func TestCoordinatedProbeFailureUpdatesParentAvailabilityWithoutReplacingPods(t *testing.T) {
	for _, failWorker := range []bool{false, true} {
		t.Run(fmt.Sprintf("worker-%t", failWorker), func(t *testing.T) {
			ms := mergeReadyModelServing("probe-availability", 1, 1)
			for i := range ms.Spec.Template.Roles {
				ms.Spec.Template.Roles[i].WorkerReplicas = 1
				ms.Spec.Template.Roles[i].WorkerTemplate = ms.Spec.Template.Roles[i].EntryTemplate.DeepCopy()
			}
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = utils.ModelServingRevision(ms), utils.ModelServingRevision(ms)
			c := newRevisionTestController(t, ms)
			c.initialSync.Store(true)
			key, revision, group := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms), ms.Name+"-0"
			c.store.AddServingGroup(key, 0, revision)
			var observed []*corev1.Pod
			for _, role := range ms.Spec.Template.Roles {
				observed = append(observed, addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role))...)
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			syncAndRead := func() *api.ModelServing {
				t.Helper()
				require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
				actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NoError(t, c.modelServingsInformer.GetIndexer().Update(actual))
				return actual
			}
			require.EqualValues(t, 1, syncAndRead().Status.AvailableReplicas)
			failing := observed[0].DeepCopy()
			if failWorker {
				failing = observed[1].DeepCopy()
			}
			failing.Status.Conditions[0].Status = corev1.ConditionFalse
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), failing, metav1.UpdateOptions{})
			require.NoError(t, err)
			// Deliberately keep the informer copy Ready. Reconciliation must use
			// current owned Pods rather than stale Role/parent Running flags.
			c.updatePod(nil, failing)
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			for round := 0; round < 3; round++ {
				require.Zero(t, syncAndRead().Status.AvailableReplicas)
				require.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, group))
				require.NotEqual(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, "prefill", "prefill-0"))
			}
			requireNoMergeReadyPodMutations(t, client.Actions())
			failing.Status.Conditions[0].Status = corev1.ConditionTrue
			_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), failing, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Update(failing))
			c.updatePod(nil, failing)
			client.ClearActions()
			require.EqualValues(t, 1, syncAndRead().Status.AvailableReplicas)
			require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, group))
			requireNoMergeReadyPodMutations(t, client.Actions())
			for _, pod := range observed {
				actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, pod.UID, actual.UID)
				require.Nil(t, actual.DeletionTimestamp)
				require.Equal(t, corev1.PodRunning, actual.Status.Phase)
				require.Zero(t, len(actual.Status.ContainerStatuses), "fixture has no restart/Failed evidence")
			}
		})
	}
}

func TestCoordinatedScaleToZeroClearsCountsAfterLastGroupDisappears(t *testing.T) {
	ms := mergeReadyModelServing("zero-complete", 0, 1)
	ms.Generation = 7
	ms.Status.Replicas, ms.Status.AvailableReplicas = 1, 1
	ms.Status.CurrentReplicas, ms.Status.UpdatedReplicas = 1, 1
	ms.Status.ObservedGeneration = 6
	c := newRevisionTestController(t, ms)
	require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
	actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Zero(t, actual.Status.Replicas)
	require.Zero(t, actual.Status.AvailableReplicas)
	require.Zero(t, actual.Status.CurrentReplicas)
	require.Zero(t, actual.Status.UpdatedReplicas)
	require.Equal(t, ms.Generation, actual.Status.ObservedGeneration)
}

func TestCoordinatedScaleToZeroPreservesLastCompletedRevision(t *testing.T) {
	for _, hasCompletedRevision := range []bool{true, false} {
		for _, groupWasDeleted := range []bool{false, true} {
			name := "last-completed-a"
			if !hasCompletedRevision {
				name = "no-completed-revision"
			}
			if groupWasDeleted {
				name += "/empty-map-after-delete"
			} else {
				name += "/never-added-group"
			}
			t.Run(name, func(t *testing.T) {
				ms := mergeReadyModelServing("zero-unfinished-target", 0, 1)
				ms.Generation = 8
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "test-image:unfinished-b"
				}
				targetRevision := utils.ModelServingRevision(ms)
				completed := ms.DeepCopy()
				for i := range completed.Spec.Template.Roles {
					completed.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "test-image:completed-a"
				}
				completedRevision := utils.ModelServingRevision(completed)
				require.NotEqual(t, completedRevision, targetRevision)
				ms.Status.CurrentRevision, ms.Status.UpdateRevision = "", ""
				if hasCompletedRevision {
					ms.Status.CurrentRevision, ms.Status.UpdateRevision = completedRevision, targetRevision
					ms.Status.Replicas, ms.Status.AvailableReplicas = 2, 1
					ms.Status.CurrentReplicas, ms.Status.UpdatedReplicas = 1, 1
					ms.Status.ObservedGeneration = 7
				}
				c := newRevisionTestController(t, ms)
				if hasCompletedRevision {
					_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, completed, completedRevision, completed.Spec.Template.Roles)
					require.NoError(t, err)
				}
				key := utils.GetNamespaceName(ms)
				if groupWasDeleted {
					c.store.AddServingGroup(key, 0, completedRevision)
					c.store.DeleteServingGroup(key, ms.Name+"-0")
					groups, err := c.store.GetServingGroupByModelServing(key)
					require.NoError(t, err)
					require.Empty(t, groups, "the last group's removal leaves an existing empty datastore map")
				} else {
					_, err := c.store.GetServingGroupByModelServing(key)
					require.ErrorIs(t, err, datastore.ErrServingGroupNotFound)
				}
				require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
				actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, ms.Status.CurrentRevision, actual.Status.CurrentRevision, "zero groups cannot supply evidence that the unfinished B template completed")
				require.Equal(t, targetRevision, actual.Status.UpdateRevision)
				require.Zero(t, actual.Status.Replicas)
				require.Zero(t, actual.Status.AvailableReplicas)
				require.Zero(t, actual.Status.CurrentReplicas)
				require.Zero(t, actual.Status.UpdatedReplicas)
				require.Equal(t, ms.Generation, actual.Status.ObservedGeneration)

				if hasCompletedRevision {
					// A later protected Role refill still needs the actual completed A
					// template, rather than the B target that was never made Ready.
					next := actual.DeepCopy()
					next.Spec.Replicas = ptr.To[int32](1)
					for i := range next.Spec.Template.Roles {
						next.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromInt(1))
					}
					group := next.Name + "-0"
					c.store.AddServingGroup(utils.GetNamespaceName(next), 0, completedRevision)
					role := next.Spec.Template.Roles[0]
					c.scaleUpRoles(withCoordinatedRound(context.Background(), next), next, group, role, nil, 1, 0, targetRevision, true)
					pod, err := c.kubeClientSet.CoreV1().Pods(next.Namespace).Get(context.Background(), utils.GeneratePodName(group, "prefill-0", 0), metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, completedRevision, utils.ObjectRevision(pod))
					require.Equal(t, canonicalRoleHashForTest(t, completed, completed.Spec.Template.Roles[0].Name), utils.ObjectRoleTemplateHash(pod))
					require.Equal(t, "test-image:completed-a", pod.Spec.Containers[0].Image)
				}
			})
		}
	}
}
