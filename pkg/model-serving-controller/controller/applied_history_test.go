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
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func newAppliedHistoryController(t *testing.T) *ModelServingController {
	t.Helper()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	return c
}

func appliedHistoryWorkload(strategy workloadv1alpha1.RolloutStrategyType, image string, workers int32) *workloadv1alpha1.ModelServing {
	ms := semanticRevisionTestModelServing("applied-history", image)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: strategy}
	ms.Spec.Template.Roles[0].WorkerReplicas = workers
	if workers > 0 {
		ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	}
	return ms
}

// Seed actual rendered members, immutable history and datastore identities
// separately, so tests can remove a required member without changing its history.
func seedAppliedRole(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role, groupOrdinal, roleOrdinal int, revision string, missingOrdinal int) map[string]types.UID {
	t.Helper()
	group := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
	roleID := utils.GenerateRoleID(role.Name, roleOrdinal)
	hash, err := utils.RoleRevisionHash(ms, role.Name)
	require.NoError(t, err)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), groupOrdinal, revision)
	c.store.AddRole(utils.GetNamespaceName(ms), group, role.Name, roleID, revision, hash)
	entry := utils.GenerateEntryPod(role, ms, group, roleID, revision, hash)
	uids := map[string]types.UID{}
	for ordinal := 0; ordinal <= int(role.WorkerReplicas); ordinal++ {
		if ordinal == missingOrdinal {
			continue
		}
		pod := entry.DeepCopy()
		if ordinal > 0 {
			pod = utils.GenerateWorkerPod(role, ms, entry, group, roleID, ordinal, revision, hash)
		}
		pod.UID = types.UID("original-" + pod.Name)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		uids[pod.Name] = pod.UID
	}
	return uids
}

func TestAppliedHistoryWorkerLayoutAndLocalRepair(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, layout := range [][2]int32{{1, 2}, {2, 1}} {
			for _, missing := range []int{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/workers-%d-to-%d/missing-%d", strategy, layout[0], layout[1], missing), func(t *testing.T) {
					c := newAppliedHistoryController(t)
					old := appliedHistoryWorkload(strategy, "old-image", layout[0])
					current := appliedHistoryWorkload(strategy, "new-image", layout[1])
					recordTestRevision(t, c.kubeClientSet, old, "old")
					recordTestRevision(t, c.kubeClientSet, current, "new")
					uids := seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", missing)
					group := utils.GenerateServingGroupName(old.Name, 0)
					ready, err := c.checkRoleReady(current, group, "decode", "decode-0")
					require.NoError(t, err)
					require.Equal(t, missing < 0, ready, "readiness uses the applied worker layout outside partition")

					ctx := c.withRevisionHistory(context.Background(), current)
					require.NoError(t, c.syncRoleReplicas(ctx, current, "new"))
					pods, err := c.kubeClientSet.CoreV1().Pods(old.Namespace).List(ctx, metav1.ListOptions{})
					require.NoError(t, err)
					require.Len(t, pods.Items, int(layout[0])+1, "local repair must not add the target worker layout")
					for i := range pods.Items {
						pod := &pods.Items[i]
						require.Equal(t, "old", utils.ObjectRevision(pod))
						require.Equal(t, "old-image", pod.Spec.Containers[0].Image)
						if uid, found := uids[pod.Name]; found {
							require.Equal(t, uid, pod.UID, "a healthy survivor must retain its identity")
						}
						require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
					}
					ready, err = c.checkRoleReady(current, group, "decode", "decode-0")
					require.NoError(t, err)
					require.Equal(t, missing < 0, ready, "newly created members are not Ready until observed Ready")
				})
			}
		}
	}
}

func TestAppliedHistoryRaisedPartitionKeepsLocalVersion(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			baseline := appliedHistoryWorkload(strategy, "image:v1", 1)
			applied := appliedHistoryWorkload(strategy, "image:v2", 1)
			current := appliedHistoryWorkload(strategy, "image:v3", 2)
			current.Status.CurrentRevision = "v1"
			partition := intstr.FromInt32(1)
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				current.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{Partition: &partition}
			} else {
				current.Spec.Template.Roles[0].Partition = &partition
			}
			recordTestRevision(t, c.kubeClientSet, baseline, "v1")
			recordTestRevision(t, c.kubeClientSet, applied, "v2")
			recordTestRevision(t, c.kubeClientSet, current, "v3")
			uids := seedAppliedRole(t, c, applied, applied.Spec.Template.Roles[0], 0, 0, "v2", 1)
			require.NoError(t, c.syncRoleReplicas(c.withRevisionHistory(context.Background(), current), current, "v3"))
			pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2)
			for i := range pods.Items {
				pod := &pods.Items[i]
				require.Equal(t, "v2", utils.ObjectRevision(pod), "raising partition cannot roll a partial repair back to v1")
				require.Equal(t, "image:v2", pod.Spec.Containers[0].Image)
				if uid, found := uids[pod.Name]; found {
					require.Equal(t, uid, pod.UID)
				}
			}
		})
	}
}

func TestAppliedHistoryMissingRoleWithinServingGroup(t *testing.T) {
	c := newAppliedHistoryController(t)
	old := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 1)
	sibling := *old.Spec.Template.Roles[0].DeepCopy()
	sibling.Name = "prefill"
	old.Spec.Template.Roles = append(old.Spec.Template.Roles, sibling)
	current := old.DeepCopy()
	current.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	current.Spec.Template.Roles[0].WorkerReplicas = 2
	current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
	current.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "image:v2"
	recordTestRevision(t, c.kubeClientSet, old, "old")
	recordTestRevision(t, c.kubeClientSet, current, "new")
	uids := seedAppliedRole(t, c, old, sibling, 0, 0, "old", -1)
	require.NoError(t, c.syncRoleReplicas(c.withRevisionHistory(context.Background(), current), current, "new"))
	pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 6, "current two decode instances each retain the historical one-worker layout")
	for i := range pods.Items {
		pod := &pods.Items[i]
		require.Equal(t, "old", utils.ObjectRevision(pod))
		require.Equal(t, "image:v1", pod.Spec.Containers[0].Image)
		if uid, found := uids[pod.Name]; found {
			require.Equal(t, uid, pod.UID)
		}
	}
}

func TestAppliedHistoryWholeRoleRecreationUsesCurrentPartition(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(fmt.Sprintf("protected-%t", protected), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := appliedHistoryWorkload(workloadv1alpha1.RoleRollingUpdate, "image:v1", 1)
			current := appliedHistoryWorkload(workloadv1alpha1.RoleRollingUpdate, "image:v2", 2)
			current.Status.CurrentRevision = "old"
			if protected {
				current.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt32(1))
			}
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			c.store.AddServingGroup(utils.GetNamespaceName(current), 0, "old")
			require.NoError(t, c.syncRoleReplicas(c.withRevisionHistory(context.Background(), current), current, "new"))
			pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			wantRevision, wantImage, wantCount := "new", "image:v2", 3
			if protected {
				wantRevision, wantImage, wantCount = "old", "image:v1", 2
			}
			require.Len(t, pods.Items, wantCount)
			for i := range pods.Items {
				require.Equal(t, wantRevision, utils.ObjectRevision(&pods.Items[i]))
				require.Equal(t, wantImage, pods.Items[i].Spec.Containers[0].Image)
			}
		})
	}
}

func TestAppliedHistoryReadinessRequiresActualMembers(t *testing.T) {
	for _, member := range []string{"extra-ordinal", "foreign-owner", "wrong-revision", "terminating"} {
		t.Run(member, func(t *testing.T) {
			c := newAppliedHistoryController(t)
			ms := appliedHistoryWorkload(workloadv1alpha1.RoleRollingUpdate, "image:v1", 1)
			recordTestRevision(t, c.kubeClientSet, ms, "old")
			seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, 0, "old", 1)
			group := utils.GenerateServingGroupName(ms.Name, 0)
			entry, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), utils.GeneratePodName(group, "decode-0", 0), metav1.GetOptions{})
			require.NoError(t, err)
			pod := utils.GenerateWorkerPod(ms.Spec.Template.Roles[0], ms, entry, group, "decode-0", 1, "old", utils.ObjectRoleTemplateHash(entry))
			pod.Status = entry.Status
			switch member {
			case "extra-ordinal":
				pod.Name = utils.GeneratePodName(group, "decode-0", 2)
			case "foreign-owner":
				pod.OwnerReferences[0].UID = "previous-modelserving"
			case "wrong-revision":
				pod.Labels[workloadv1alpha1.RevisionLabelKey] = "another-revision"
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			}
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			ready, err := c.checkRoleReady(ms, group, "decode", "decode-0")
			require.NoError(t, err)
			require.False(t, ready, "two Ready labels cannot substitute for the required worker identity")
		})
	}
}

func TestAppliedHistorySparsePartitionEligibilityAndRefill(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := appliedHistoryWorkload(strategy, "image:v1", 0)
			current := appliedHistoryWorkload(strategy, "image:v2", 0)
			current.Status.CurrentRevision = "old"
			partition := intstr.FromInt32(2)
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				old.Spec.Replicas, current.Spec.Replicas = ptr.To[int32](3), ptr.To[int32](3)
				current.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{Partition: &partition}
			} else {
				old.Spec.Template.Roles[0].Replicas, current.Spec.Template.Roles[0].Replicas = ptr.To[int32](3), ptr.To[int32](3)
				current.Spec.Template.Roles[0].Partition = &partition
			}
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			for _, ordinal := range []int{0, 2} {
				groupOrdinal, roleOrdinal := ordinal, 0
				if strategy == workloadv1alpha1.RoleRollingUpdate {
					groupOrdinal, roleOrdinal = 0, ordinal
				}
				seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], groupOrdinal, roleOrdinal, "old", -1)
			}
			ctx := c.withRevisionHistory(context.Background(), current)
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(current))
				require.NoError(t, err)
				require.True(t, c.hasUpdateableOutdatedServingGroup(ctx, current, groups, "new", 2), "ordinal 2 is eligible even though it is the second existing record")
				require.NoError(t, c.scaleUpServingGroups(ctx, current, groups, 3, "new"))
			} else {
				group := utils.GenerateServingGroupName(current.Name, 0)
				instances, err := c.store.GetRoleList(utils.GetNamespaceName(current), group, "decode")
				require.NoError(t, err)
				require.True(t, c.hasUpdateableOutdatedRole(ctx, current, group, current.Spec.Template.Roles[0], instances))
				c.scaleUpRoles(ctx, current, group, current.Spec.Template.Roles[0], instances, 3, 0, "new")
			}
			groupOrdinal, roleOrdinal := 1, 0
			if strategy == workloadv1alpha1.RoleRollingUpdate {
				groupOrdinal, roleOrdinal = 0, 1
			}
			pod, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).Get(ctx, utils.GeneratePodName(utils.GenerateServingGroupName(current.Name, groupOrdinal), utils.GenerateRoleID("decode", roleOrdinal), 0), metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "old", utils.ObjectRevision(pod), "the newly filled absolute ordinal 1 is protected")
			require.Equal(t, "image:v1", pod.Spec.Containers[0].Image)
		})
	}
}

func TestAppliedHistoryUnavailableBlocksLocalRepair(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, failure := range []string{"missing", "corrupt", "foreign"} {
			t.Run(fmt.Sprintf("%s/%s", strategy, failure), func(t *testing.T) {
				c := newAppliedHistoryController(t)
				old := appliedHistoryWorkload(strategy, "old-image", 1)
				current := appliedHistoryWorkload(strategy, "new-image", 2)
				if failure != "missing" {
					recordTestRevision(t, c.kubeClientSet, old, "old")
					cr, err := c.kubeClientSet.AppsV1().ControllerRevisions(old.Namespace).Get(context.Background(), utils.GenerateControllerRevisionName(old.Name, "old"), metav1.GetOptions{})
					require.NoError(t, err)
					if failure == "corrupt" {
						cr.Data.Raw = []byte(`{"unexpected":"not a valid revision"}`)
					} else {
						cr.OwnerReferences[0].UID = "previous-modelserving"
					}
					_, err = c.kubeClientSet.AppsV1().ControllerRevisions(old.Namespace).Update(context.Background(), cr, metav1.UpdateOptions{})
					require.NoError(t, err)
				}
				recordTestRevision(t, c.kubeClientSet, current, "new")
				uids := seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", 1)
				ctx := c.withRevisionHistory(context.Background(), current)
				require.NoError(t, c.syncRoleReplicas(ctx, current, "new"), "unit-local history errors are deferred so other units can reconcile")
				require.Error(t, c.revisionHistory(ctx, current).errors(), "untrusted applied history must stay visible as an error")
				pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, 1, "target history cannot fill a member of an unknown historical unit")
				require.Equal(t, uids[pods.Items[0].Name], pods.Items[0].UID)
				require.Equal(t, "old-image", pods.Items[0].Spec.Containers[0].Image)
				ready, err := c.checkRoleReady(current, utils.GenerateServingGroupName(current.Name, 0), "decode", "decode-0")
				require.Error(t, err)
				require.False(t, ready)
			})
		}
	}
}

func TestAppliedHistoryAbsentUnitWithStaleStoreUsesPartition(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protected-%t", strategy, protected), func(t *testing.T) {
				c := newAppliedHistoryController(t)
				old := appliedHistoryWorkload(strategy, "image:v1", 0)
				current := appliedHistoryWorkload(strategy, "image:v2", 0)
				current.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				current.Status.CurrentRevision = "old"
				if protected {
					partition := ptr.To(intstr.FromInt32(1))
					if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
						current.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{Partition: partition}
					} else {
						current.Spec.Template.Roles[0].Partition = partition
					}
				}
				recordTestRevision(t, c.kubeClientSet, old, "old")
				recordTestRevision(t, c.kubeClientSet, current, "new")
				// No actual Pods survive the last deletion, but informer-driven
				// bookkeeping still contains the old Role and SG identities.
				seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", 0)
				ctx := c.withRevisionHistory(context.Background(), current)
				require.NoError(t, c.syncRoleReplicas(ctx, current, "new"))
				pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, 1)
				want := "new"
				if protected {
					want = "old"
				}
				require.Equal(t, want, utils.ObjectRevision(&pods.Items[0]), "a stale record cannot turn whole-unit recreation into partial repair")
			})
		}
	}
}

func TestAppliedHistoryTerminatingMemberIsNotAbsentUnit(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := appliedHistoryWorkload(strategy, "image:v1", 0)
			current := appliedHistoryWorkload(strategy, "image:v2", 0)
			current.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			uids := seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", -1)
			name := utils.GeneratePodName(utils.GenerateServingGroupName(current.Name, 0), "decode-0", 0)
			pod, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).Get(context.Background(), name, metav1.GetOptions{})
			require.NoError(t, err)
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			_, err = c.kubeClientSet.CoreV1().Pods(current.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
			require.NoError(t, c.syncRoleReplicas(c.withRevisionHistory(context.Background(), current), current, "new"))
			pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
			require.Equal(t, uids[name], pods.Items[0].UID)
			require.Equal(t, "old", utils.ObjectRevision(&pods.Items[0]), "Terminating members retain capacity and cannot select a fresh target generation")
			require.NotNil(t, pods.Items[0].DeletionTimestamp)
		})
	}
}

func TestAppliedHistoryErrorDoesNotShrinkReadySurge(t *testing.T) {
	c := newAppliedHistoryController(t)
	old := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 0)
	current := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v2", 0)
	current.Spec.Replicas = ptr.To[int32](2)
	current.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{
		Partition: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromInt32(1)), MaxUnavailable: ptr.To(intstr.FromInt32(1)),
	}
	current.Status.CurrentRevision = "unknown"
	recordTestRevision(t, c.kubeClientSet, old, "old")
	recordTestRevision(t, c.kubeClientSet, current, "new")
	uids := map[string]types.UID{}
	for ordinal, revision := range []string{"unknown", "old", "new"} {
		workload := old
		if revision == "new" {
			workload = current
		}
		for name, uid := range seedAppliedRole(t, c, workload, workload.Spec.Template.Roles[0], ordinal, 0, revision, -1) {
			uids[name] = uid
		}
		group := utils.GenerateServingGroupName(current.Name, ordinal)
		require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(current), group, "decode", "decode-0", datastore.RoleRunning))
		require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(current), group, datastore.ServingGroupRunning))
	}
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
	surgeGroup := utils.GenerateServingGroupName(current.Name, 2)
	surgeStatus := c.store.GetServingGroupStatus(utils.GetNamespaceName(current), surgeGroup)
	surgeRoles, err := c.store.GetRoleList(utils.GetNamespaceName(current), surgeGroup, "decode")
	require.NoError(t, err)
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()
	err = c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
	require.ErrorContains(t, err, "ControllerRevision is missing", "unknown protected history still requires retry after safe progress")
	oldGroup := utils.GenerateServingGroupName(current.Name, 1)
	protectedGroup := utils.GenerateServingGroupName(current.Name, 0)
	deletions := 0
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "pods" {
			require.NotEqual(t, "delete", action.GetVerb(), "rollout must use scoped unit deletion")
			if action.GetVerb() == "delete-collection" {
				// Fake DeleteCollection keeps tracker objects, so inspect the
				// actual selector as well as the surge's datastore state.
				selector := action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels
				require.False(t, selector.Matches(labels.Set{workloadv1alpha1.GroupNameLabelKey: surgeGroup}), "history failure must not reclaim target surge as ordinary scale-down")
				require.False(t, selector.Matches(labels.Set{workloadv1alpha1.GroupNameLabelKey: protectedGroup}), "unknown protected history never authorizes deletion")
				require.True(t, selector.Matches(labels.Set{workloadv1alpha1.GroupNameLabelKey: oldGroup}), "the eligible old group may roll out using the ready surge's availability")
				deletions++
			}
		}
	}
	require.Equal(t, 1, deletions, "independent rollout can progress without discarding its ready surge")
	require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(utils.GetNamespaceName(current), oldGroup))
	require.Equal(t, datastore.ServingGroupRunning, surgeStatus)
	require.Equal(t, surgeStatus, c.store.GetServingGroupStatus(utils.GetNamespaceName(current), surgeGroup), "target surge must not enter Deleting")
	afterRoles, err := c.store.GetRoleList(utils.GetNamespaceName(current), surgeGroup, "decode")
	require.NoError(t, err)
	require.Equal(t, surgeRoles, afterRoles, "surge Role identity and status must remain unchanged")
	surgeRevision, found := c.store.GetServingGroupRevision(utils.GetNamespaceName(current), surgeGroup)
	require.True(t, found)
	require.Equal(t, "new", surgeRevision)
	pods, err := client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 3, "target surge must survive while the old eligible group still needs rollout")
	for i := range pods.Items {
		require.Equal(t, uids[pods.Items[i].Name], pods.Items[i].UID)
	}
}

func TestAppliedHistoryUnavailableDoesNotBlockIndependentRollout(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, maxUnavailable := range []int32{2, 1} {
			t.Run(fmt.Sprintf("%s/maxUnavailable-%d", strategy, maxUnavailable), func(t *testing.T) {
				c := newAppliedHistoryController(t)
				old := appliedHistoryWorkload(strategy, "image:v1", 0)
				current := appliedHistoryWorkload(strategy, "image:v2", 0)
				configuration := workloadv1alpha1.RollingUpdateConfiguration{
					Partition: ptr.To(intstr.FromInt32(0)), MaxSurge: ptr.To(intstr.FromInt32(0)), MaxUnavailable: ptr.To(intstr.FromInt32(maxUnavailable)),
				}
				unknownGroup := utils.GenerateServingGroupName(current.Name, 0)
				knownGroup, knownRole := utils.GenerateServingGroupName(current.Name, 1), "decode-0"
				if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
					old.Spec.Replicas, current.Spec.Replicas = ptr.To[int32](2), ptr.To[int32](2)
					current.Spec.RolloutStrategy.RollingUpdateConfiguration = &configuration
				} else {
					old.Spec.Template.Roles[0].Replicas, current.Spec.Template.Roles[0].Replicas = ptr.To[int32](2), ptr.To[int32](2)
					current.Spec.Template.Roles[0].RollingUpdateConfiguration = configuration
					knownGroup, knownRole = unknownGroup, "decode-1"
					// The SG history is valid; only one contained Role's history is
					// unavailable, so its peer can still be compared independently.
					c.store.AddServingGroup(utils.GetNamespaceName(current), 0, "old")
				}
				current.Status.CurrentRevision = "old"
				recordTestRevision(t, c.kubeClientSet, old, "old")
				recordTestRevision(t, c.kubeClientSet, current, "new")
				for ordinal, revision := range []string{"unknown", "old"} {
					groupOrdinal, roleOrdinal := ordinal, 0
					if strategy == workloadv1alpha1.RoleRollingUpdate {
						groupOrdinal, roleOrdinal = 0, ordinal
					}
					seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], groupOrdinal, roleOrdinal, revision, -1)
					group := utils.GenerateServingGroupName(current.Name, groupOrdinal)
					require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(current), group, "decode", utils.GenerateRoleID("decode", roleOrdinal), datastore.RoleRunning))
					require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(current), group, datastore.ServingGroupRunning))
				}
				_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
				client := c.kubeClientSet.(*kubefake.Clientset)
				client.ClearActions()

				err = c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
				require.ErrorContains(t, err, "ControllerRevision is missing", "safe independent progress must still return the history error for retry")
				unknownLabels := labels.Set{
					workloadv1alpha1.GroupNameLabelKey: unknownGroup,
					workloadv1alpha1.RoleLabelKey:      "decode",
					workloadv1alpha1.RoleIDKey:         "decode-0",
				}
				knownLabels := labels.Set{
					workloadv1alpha1.GroupNameLabelKey: knownGroup,
					workloadv1alpha1.RoleLabelKey:      "decode",
					workloadv1alpha1.RoleIDKey:         knownRole,
				}
				deletions := 0
				for _, action := range client.Actions() {
					if action.GetResource().Resource != "pods" {
						continue
					}
					require.NotEqual(t, "delete", action.GetVerb(), "rollout must use scoped unit deletion")
					if action.GetVerb() != "delete-collection" {
						continue
					}
					selector := action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels
					require.False(t, selector.Matches(unknownLabels), "unknown history never authorizes deletion of that unit")
					require.True(t, selector.Matches(knownLabels), "only the independently verified old unit can roll out")
					deletions++
				}
				wantDeletions := 0
				if maxUnavailable == 2 {
					wantDeletions = 1
				}
				require.Equal(t, wantDeletions, deletions, "minAvailable=0 permits the known unit to progress; minAvailable=1 requires keeping it")
				require.Equal(t, datastore.ServingGroupCreating, c.store.GetServingGroupStatus(utils.GetNamespaceName(current), unknownGroup), "unknown applied history cannot retain Running availability credit")
				if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
					wantStatus := datastore.ServingGroupRunning
					if wantDeletions > 0 {
						wantStatus = datastore.ServingGroupDeleting
					}
					require.Equal(t, wantStatus, c.store.GetServingGroupStatus(utils.GetNamespaceName(current), knownGroup))
				} else {
					require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(current), unknownGroup, "decode", "decode-0"))
					wantStatus := datastore.RoleRunning
					if wantDeletions > 0 {
						wantStatus = datastore.RoleDeleting
					}
					require.Equal(t, wantStatus, c.store.GetRoleStatus(utils.GetNamespaceName(current), knownGroup, "decode", knownRole))
				}
			})
		}
	}
}

func TestAppliedHistoryPartialCreateWithInformerLagKeepsAppliedRevision(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := appliedHistoryWorkload(strategy, "image:v1", 0)
			applied := appliedHistoryWorkload(strategy, "image:v2", 1)
			latest := appliedHistoryWorkload(strategy, "image:v3", 2)
			applied.Status.CurrentRevision = "old"
			latest.Status.CurrentRevision = "old"
			for revision, ms := range map[string]*workloadv1alpha1.ModelServing{"old": old, "applied": applied, "latest": latest} {
				recordTestRevision(t, c.kubeClientSet, ms, revision)
			}
			// The last old Pod has disappeared, leaving only datastore records.
			seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", 0)
			group := utils.GenerateServingGroupName(old.Name, 0)
			entryName := utils.GeneratePodName(group, "decode-0", 0)
			workerName := utils.GeneratePodName(group, "decode-0", 1)
			client := c.kubeClientSet.(*kubefake.Clientset)
			workerFailures := 0
			client.PrependReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				pod := action.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
				if pod.Name == workerName && workerFailures == 0 {
					workerFailures++
					return true, nil, fmt.Errorf("injected worker creation failure")
				}
				if pod.Name == entryName {
					pod.UID = types.UID("created-entry-v2")
				}
				return false, nil, nil
			})

			err := c.syncRoleReplicas(c.withRevisionHistory(context.Background(), applied), applied, "applied")
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				require.ErrorContains(t, err, "injected worker creation failure")
			} else {
				// Role repair logs CREATE failures and retries on the next sync.
				require.NoError(t, err)
			}
			require.Equal(t, 1, workerFailures)
			pods, err := client.CoreV1().Pods(old.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1, "entry CREATE succeeds before the worker failure")
			entry := pods.Items[0]
			require.Equal(t, entryName, entry.Name)
			require.Equal(t, "applied", utils.ObjectRevision(&entry))
			require.NotEmpty(t, entry.UID)
			require.Empty(t, c.podsInformer.GetIndexer().List(), "the informer has not observed the successful CREATE")
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				revision, found := c.store.GetServingGroupRevision(utils.GetNamespaceName(old), group)
				require.True(t, found)
				require.Equal(t, "applied", revision)
			} else {
				roles, err := c.store.GetRoleList(utils.GetNamespaceName(old), group, "decode")
				require.NoError(t, err)
				require.Len(t, roles, 1)
				require.Equal(t, "applied", roles[0].Revision)
			}

			// A new target arrives before the entry reaches the informer. API
			// presence must keep this a partial repair of v2, not a fresh v3 unit.
			client.ClearActions()
			require.NoError(t, c.syncRoleReplicas(c.withRevisionHistory(context.Background(), latest), latest, "latest"))
			presenceReads := 0
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "pods" {
					continue
				}
				switch action.GetVerb() {
				case "list":
					presenceReads++
				case "create":
					pod := action.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
					require.Equal(t, "applied", utils.ObjectRevision(pod), "no member may be created from the newer target")
				case "delete", "delete-collection":
					t.Fatalf("partial repair must retain the surviving entry: %s", action.GetVerb())
				}
			}
			require.Positive(t, presenceReads, "informer absence must be checked against the API")
			pods, err = client.CoreV1().Pods(old.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2, "refill uses v2's one-worker layout, not v3's two workers")
			for i := range pods.Items {
				pod := &pods.Items[i]
				require.Equal(t, "applied", utils.ObjectRevision(pod))
				require.Equal(t, "image:v2", pod.Spec.Containers[0].Image)
				if pod.Name == entryName {
					require.Equal(t, entry.UID, pod.UID)
				}
			}
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(old), group, "decode")
			require.NoError(t, err)
			require.Len(t, roles, 1)
			require.Equal(t, "applied", roles[0].Revision, "store identity must stay pinned to the partially created unit")
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				revision, found := c.store.GetServingGroupRevision(utils.GetNamespaceName(old), group)
				require.True(t, found)
				require.Equal(t, "applied", revision)
			}
		})
	}
}

func TestAppliedHistoryReadinessSharesSnapshotAcrossInstances(t *testing.T) {
	c := newAppliedHistoryController(t)
	ms := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 0)
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](100)
	recordTestRevision(t, c.kubeClientSet, ms, "old")
	group := utils.GenerateServingGroupName(ms.Name, 0)
	for ordinal := 0; ordinal < 100; ordinal++ {
		seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, ordinal, "old", -1)
		require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", utils.GenerateRoleID("decode", ordinal), datastore.RoleRunning))
	}
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()
	ready, err := c.checkServingGroupReady(ms, group)
	require.NoError(t, err)
	require.True(t, ready)
	reads := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "controllerrevisions" {
			reads++
		}
	}
	require.Positive(t, reads, "readiness must establish the applied history")
	require.LessOrEqual(t, reads, 2, "shared history lookup must stay bounded instead of performing one GET per instance")
}
