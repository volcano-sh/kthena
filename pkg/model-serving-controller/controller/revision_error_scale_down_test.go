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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestRevisionPersistenceFailurePreservesCoordinatedSurge(t *testing.T) {
	for _, removeGroup := range []bool{false, true} {
		t.Run(fmt.Sprintf("remove-group-%t", removeGroup), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := mergeReadyModelServing("revision-error-surge", 1, 2)
			old.Spec.Template.Roles = old.Spec.Template.Roles[:1]
			role := &old.Spec.Template.Roles[0]
			role.WorkerReplicas, role.WorkerTemplate = 0, nil
			role.Partition = ptr.To(intstr.FromInt(2))
			role.MaxUnavailable = ptr.To(intstr.FromInt(0))
			role.MaxSurge = ptr.To(intstr.FromInt(1))
			current := old.DeepCopy()
			current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "desired-image:v2"
			current.Status.CurrentRevision, current.Status.UpdateRevision = "old", "old"
			if removeGroup {
				current.Spec.Replicas = ptr.To[int32](0)
			}
			recordTestRevision(t, c.kubeClientSet, old, "old")
			key, group := utils.GetNamespaceName(current), current.Name+"-0"
			c.store.AddServingGroup(key, 0, "old")
			originalUIDs := map[string]types.UID{}
			for i := 0; i < 3; i++ {
				pod := addMergeReadyInstance(t, c, old, group, *role, i, "old", "old", utils.CalRoleTemplateHash(*role))[0]
				if i == 1 {
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
					require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, utils.GenerateRoleID(role.Name, i), datastore.RoleCreating))
				}
				if i == 2 {
					setSurgeScope(pod, surgeRole)
				}
				updated, err := c.kubeClientSet.CoreV1().Pods(old.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(updated))
				originalUIDs[pod.Name] = pod.UID
			}
			// Ordinary coordinator retirement cannot remove the healthy surge while a
			// formal member is unready: Ready=2 is exactly N-U.
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			instances, err := c.store.GetRoleList(key, group, role.Name)
			require.NoError(t, err)
			handled, err := c.retireCoordinatedSurge(context.Background(), current, group, current.Spec.Template.Roles[0], instances, 2)
			require.NoError(t, err)
			require.True(t, handled)
			requireNoMergeReadyPodMutations(t, client.Actions())
			require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
			_, createErr := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
			require.NoError(t, createErr)
			client.PrependReactor("create", "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("injected desired revision persistence failure")
			})
			client.ClearActions()
			err = c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
			require.ErrorContains(t, err, "injected desired revision persistence failure")
			pods, err := client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			if removeGroup {
				require.Empty(t, pods.Items, "explicit ServingGroup scale-to-zero remains available")
				for _, action := range client.Actions() {
					if deletion, ok := action.(kt.DeleteAction); ok && action.GetResource().Resource == "pods" {
						require.Equal(t, originalUIDs[deletion.GetName()], *deletion.GetDeleteOptions().Preconditions.UID)
					}
				}
				return
			}
			requireNoMergeReadyPodMutations(t, client.Actions())
			require.Len(t, pods.Items, 3)
			ready := 0
			for _, pod := range pods.Items {
				require.Equal(t, originalUIDs[pod.Name], pod.UID)
				if utils.IsPodRunningAndReady(&pod) {
					ready++
				}
			}
			require.Equal(t, 2, ready, "a revision failure must not bypass the coordinator availability floor")
			require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, role.Name, utils.GenerateRoleID(role.Name, 2)))
		})
	}
}

func TestRevisionErrorScaleDownReturnsPodDeletionErrors(t *testing.T) {
	for _, failure := range []string{"desired-persistence", "applied-history"} {
		t.Run(failure, func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := appliedHistoryWorkload(api.ServingGroupRollingUpdate, "old-image", 0)
			old.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			current := old.DeepCopy()
			current.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
			current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "desired-image"
			current.Status.CurrentRevision = "missing"
			for i := 0; i < 2; i++ {
				seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, i, "missing", -1)
			}
			require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
			_, createErr := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
			require.NoError(t, createErr)
			client := c.kubeClientSet.(*kubefake.Clientset)
			if failure == "desired-persistence" {
				client.PrependReactor("create", "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("injected desired revision persistence failure")
				})
			}
			client.PrependReactor("delete", "pods", func(kt.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("injected fallback Pod deletion failure")
			})
			err := c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
			require.ErrorContains(t, err, "injected fallback Pod deletion failure", "the workqueue must retain the failed scale-down action")
			if failure == "desired-persistence" {
				require.ErrorContains(t, err, "injected desired revision persistence failure")
			} else {
				require.ErrorContains(t, err, "ControllerRevision is missing")
			}
			pods, err := client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2, "a rejected delete cannot be reported as completed")
		})
	}
}

func TestDesiredRevisionFailurePreservesServingGroupAndIndependentRoleSurge(t *testing.T) {
	for _, strategy := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		for _, failure := range []string{"list", "create"} {
			for _, protectedUnknown := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/protected-unknown-%t", strategy, failure, protectedUnknown), func(t *testing.T) {
					c := newAppliedHistoryController(t)
					old := appliedHistoryWorkload(strategy, "image:v1", 0)
					current := appliedHistoryWorkload(strategy, "image:v2", 0)
					partition := 0
					if protectedUnknown {
						partition = 1
					}
					configuration := api.RollingUpdateConfiguration{
						Partition: ptr.To(intstr.FromInt(partition)), MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1)),
					}
					if strategy == api.ServingGroupRollingUpdate {
						old.Spec.Replicas, current.Spec.Replicas = ptr.To[int32](2), ptr.To[int32](2)
						current.Spec.RolloutStrategy.RollingUpdateConfiguration = &configuration
					} else {
						old.Spec.Template.Roles[0].Replicas, current.Spec.Template.Roles[0].Replicas = ptr.To[int32](2), ptr.To[int32](2)
						current.Spec.Template.Roles[0].RollingUpdateConfiguration = configuration
					}
					current.Status.CurrentRevision, current.Status.UpdateRevision = "old", "new"
					recordTestRevision(t, c.kubeClientSet, old, "old")
					originalUIDs := map[string]types.UID{}
					for ordinal := 0; ordinal < 3; ordinal++ {
						workload, revision := old, "old"
						if ordinal == 2 {
							workload, revision = current, "new"
						} else if ordinal == 0 && protectedUnknown {
							revision = "missing"
						}
						groupOrdinal, roleOrdinal := ordinal, 0
						if strategy == api.RoleRollingUpdate {
							groupOrdinal, roleOrdinal = 0, ordinal
						}
						for name, uid := range seedAppliedRole(t, c, workload, workload.Spec.Template.Roles[0], groupOrdinal, roleOrdinal, revision, -1) {
							originalUIDs[name] = uid
						}
						group := utils.GenerateServingGroupName(current.Name, groupOrdinal)
						status := datastore.RoleRunning
						if (ordinal == 0 && protectedUnknown) || (ordinal == 2 && !protectedUnknown) {
							status = datastore.RoleCreating
							podName := utils.GeneratePodName(group, utils.GenerateRoleID("decode", roleOrdinal), 0)
							pod, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).Get(context.Background(), podName, metav1.GetOptions{})
							require.NoError(t, err)
							pod.Status.Conditions[0].Status = corev1.ConditionFalse
							_, err = c.kubeClientSet.CoreV1().Pods(current.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
							require.NoError(t, err)
							require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
						}
						require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(current), group, "decode", utils.GenerateRoleID("decode", roleOrdinal), status))
						groupStatus := datastore.ServingGroupRunning
						if status == datastore.RoleCreating {
							groupStatus = datastore.ServingGroupCreating
						}
						require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(current), group, groupStatus))
					}
					require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
					_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
					require.NoError(t, err)
					client := c.kubeClientSet.(*kubefake.Clientset)
					client.PrependReactor(failure, "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
						return true, nil, fmt.Errorf("injected desired revision %s failure", failure)
					})
					client.ClearActions()
					err = c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
					require.ErrorContains(t, err, "injected desired revision "+failure+" failure")
					requireNoMergeReadyPodMutations(t, client.Actions())
					pods, err := client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
					require.NoError(t, err)
					require.Len(t, pods.Items, 3, "an unresolved target cannot authorize reclaiming rollout surge")
					for _, pod := range pods.Items {
						require.Equal(t, originalUIDs[pod.Name], pod.UID)
					}
				})
			}
		}
	}
}

func TestDesiredRevisionFailureStillBoundsReplicaDecrease(t *testing.T) {
	for _, strategy := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		for _, test := range []struct {
			name     string
			desired  int32
			surge    int
			inPlace  bool
			expected int
		}{
			{name: "retain-possible-surge", desired: 2, surge: 1, expected: 3},
			{name: "no-surge", desired: 2, expected: 2},
			{name: "scale-to-zero", surge: 1, expected: 0},
			{name: "effective-in-place", desired: 2, surge: 1, inPlace: true, expected: 2},
			{name: "negative-surge", desired: 2, surge: -1, expected: 4},
			{name: "malformed-surge", desired: 2, expected: 4},
		} {
			t.Run(string(strategy)+"/"+test.name, func(t *testing.T) {
				c := newAppliedHistoryController(t)
				old := appliedHistoryWorkload(strategy, "image:v1", 0)
				current := appliedHistoryWorkload(strategy, "image:v1", 0)
				configuration := api.RollingUpdateConfiguration{
					Partition: ptr.To(intstr.FromInt(0)), MaxUnavailable: ptr.To(intstr.FromInt(1)), MaxSurge: ptr.To(intstr.FromInt(test.surge)),
				}
				if test.name == "malformed-surge" {
					configuration.MaxSurge = ptr.To(intstr.FromString("invalid"))
				}
				if strategy == api.ServingGroupRollingUpdate {
					old.Spec.Replicas, current.Spec.Replicas = ptr.To[int32](4), ptr.To(test.desired)
					current.Spec.RolloutStrategy.RollingUpdateConfiguration = &configuration
				} else {
					old.Spec.Template.Roles[0].Replicas, current.Spec.Template.Roles[0].Replicas = ptr.To[int32](4), ptr.To(test.desired)
					current.Spec.Template.Roles[0].RollingUpdateConfiguration = configuration
				}
				if test.inPlace {
					current.Spec.RolloutStrategy.UpdateMethod = api.InPlaceUpdateMethod
				}
				current.Status.CurrentRevision = "old"
				recordTestRevision(t, c.kubeClientSet, old, "old")
				originalUIDs := map[string]types.UID{}
				for ordinal := 0; ordinal < 4; ordinal++ {
					groupOrdinal, roleOrdinal := ordinal, 0
					if strategy == api.RoleRollingUpdate {
						groupOrdinal, roleOrdinal = 0, ordinal
					}
					for name, uid := range seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], groupOrdinal, roleOrdinal, "old", -1) {
						originalUIDs[name] = uid
					}
					group := utils.GenerateServingGroupName(current.Name, groupOrdinal)
					require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(current), group, "decode", utils.GenerateRoleID("decode", roleOrdinal), datastore.RoleRunning))
					require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(current), group, datastore.ServingGroupRunning))
				}
				require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
				_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
				require.NoError(t, err)
				client := c.kubeClientSet.(*kubefake.Clientset)
				historyUnavailable := true
				client.PrependReactor("list", "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
					if historyUnavailable {
						return true, nil, fmt.Errorf("injected desired revision list failure")
					}
					return false, nil, nil
				})
				client.ClearActions()
				err = c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
				require.ErrorContains(t, err, "injected desired revision list failure")
				deletions := 0
				for _, action := range client.Actions() {
					if action.GetResource().Resource != "pods" {
						continue
					}
					if action.GetVerb() == "patch" {
						scope := utils.DeletionScopeRole
						if strategy == api.ServingGroupRollingUpdate {
							scope = utils.DeletionScopeServingGroup
						}
						patch := action.(kt.PatchAction)
						assertDeletionScopePatch(t, action, scope, originalUIDs[patch.GetName()])
						continue
					}
					require.NotContains(t, []string{"create", "update"}, action.GetVerb())
					if deletion, ok := action.(kt.DeleteAction); ok {
						deletions++
						require.Equal(t, originalUIDs[deletion.GetName()], *deletion.GetDeleteOptions().Preconditions.UID)
					}
				}
				require.Equal(t, 4-test.expected, deletions)
				pods, err := client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, test.expected)
				for _, pod := range pods.Items {
					require.Equal(t, originalUIDs[pod.Name], pod.UID)
					require.Equal(t, "image:v1", pod.Spec.Containers[0].Image)
				}
				if test.name == "retain-possible-surge" {
					historyUnavailable = false
					require.NoError(t, c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name)))
					pods, err = client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
					require.NoError(t, err)
					require.Len(t, pods.Items, int(test.desired), "count-only shrink resumes after target history is readable")
					for _, pod := range pods.Items {
						require.Equal(t, originalUIDs[pod.Name], pod.UID)
					}
				}
			})
		}
	}
}

// API v2.11 §2.3 and SG §0.6: lowering surge alone is not an explicit
// formal count decrease and cannot bypass the live healthy floor.
func TestSurgeDesiredRevisionFailureKeepsHealthyRoleFloor(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("role-origin-error", "target")
	ms.ResourceVersion = "1"
	ms.Status.CurrentRevision = "old"
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
	ms.Spec.Template.Roles[0].RollingUpdateConfiguration = api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1)), MaxSurge: ptr.To(intstr.FromInt(0))}
	c := newRevisionTestController(t, ms)
	old := ms.DeepCopy()
	old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
	recordTestRevision(t, c.kubeClientSet, old, "old")
	group := ms.Name + "-0"
	for ordinal := 0; ordinal < 4; ordinal++ {
		seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, ordinal, "old", -1)
		id := utils.GenerateRoleID("decode", ordinal)
		require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", id, datastore.RoleRunning))
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(group, id, 0), metav1.GetOptions{})
		require.NoError(t, err)
		if ordinal < 2 {
			pod.Status.Conditions[0].Status = "False"
		}
		if ordinal == 3 {
			setSurgeScope(pod, surgeRole)
		}
		_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
		require.NoError(t, err)
		// Preserve the realistic earlier Ready projection. The desired-history
		// failure must not bypass the live healthy floor through count cleanup.
	}
	require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
	c.kubeClientSet.(*kubefake.Clientset).PrependReactor("list", "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected desired history read failure")
	})
	require.ErrorContains(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name), "injected desired history read failure")
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 4, "failed target history cannot authorize rolling cleanup or formal shrink when R is unchanged")
	ready := 0
	for i := range pods.Items {
		if utils.IsPodRunningAndReady(&pods.Items[i]) {
			ready++
		}
	}
	require.GreaterOrEqual(t, ready, 2, "R3/U1 permits no healthy deletion when the Ready surge and only one stable instance together supply floor2; current R did not shrink")
}

func TestSurgeRoleOriginIsNotFallbackFormalCapacity(t *testing.T) {
	for _, test := range []struct {
		name          string
		mode          api.RolloutStrategyType
		formal        int
		desired       int32
		surge         int
		origin        bool
		foreignMarker bool
		coordinated   bool
		expected      int
	}{
		{name: "owned-source-kept", mode: api.RoleRollingUpdate, formal: 1, desired: 1, origin: true, expected: 2},
		{name: "formal-shrink", mode: api.RoleRollingUpdate, formal: 2, desired: 1, origin: true, expected: 2},
		{name: "role-zero", mode: api.RoleRollingUpdate, formal: 2, origin: true},
		{name: "foreign-marker-not-owned-source", mode: api.RoleRollingUpdate, formal: 2, desired: 1, foreignMarker: true, expected: 1},
		{name: "coordinated-gate-unchanged", mode: api.RoleRollingUpdate, formal: 2, desired: 1, origin: true, coordinated: true, expected: 3},
		{name: "legacy-unmarked-allowance", mode: api.RoleRollingUpdate, formal: 4, desired: 2, surge: 1, expected: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			ms := semanticRevisionTestModelServing("role-fallback-count", "target")
			ms.ResourceVersion = "1"
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: test.mode}
			if test.coordinated {
				ms.Spec.RolloutStrategy.RoleCoordination = coordinationForTest("100%")
			}
			ms.Spec.Template.Roles[0].Replicas = ptr.To(test.desired)
			ms.Spec.Template.Roles[0].RollingUpdateConfiguration = api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1)), MaxSurge: ptr.To(intstr.FromInt(test.surge))}
			c := newRevisionTestController(t, ms)
			target, group := utils.ModelServingRevision(ms), ms.Name+"-0"
			count := test.formal
			if test.origin {
				count++
			}
			for ordinal := 0; ordinal < count; ordinal++ {
				seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, ordinal, target, -1)
				require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", utils.GenerateRoleID("decode", ordinal), datastore.RoleRunning))
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
			name := utils.GeneratePodName(group, utils.GenerateRoleID("decode", count-1), 0)
			anchor, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			if test.origin || test.foreignMarker {
				marked := anchor.DeepCopy()
				setSurgeScope(marked, surgeRole)
				if test.foreignMarker {
					marked.Name, marked.UID = "foreign-origin", "foreign-origin-uid"
					marked.OwnerReferences[0].UID = "different-owner"
					_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, marked, metav1.CreateOptions{})
				} else {
					_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, marked, metav1.UpdateOptions{})
				}
				require.NoError(t, err)
			}
			c.kubeClientSet.(*kubefake.Clientset).PrependReactor("list", "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("injected desired history read failure")
			})
			require.ErrorContains(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name), "injected desired history read failure")
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			owned := 0
			for i := range pods.Items {
				if utils.IsOwnedByModelServingWithUID(&pods.Items[i], ms.UID) {
					owned++
				}
			}
			require.Equal(t, test.expected, owned)
			if test.origin && test.desired > 0 {
				actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, anchor.UID, actual.UID)
				require.Equal(t, surgeRole, actual.Annotations[surgeAnnotation])
			}
			if test.foreignMarker {
				foreign, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, "foreign-origin", metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, types.UID("foreign-origin-uid"), foreign.UID)
			}
		})
	}
}
