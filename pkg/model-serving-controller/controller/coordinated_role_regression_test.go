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
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestProtectedTargetDoesNotRetainPhantomOldDependency(t *testing.T) {
	role := api.Role{Name: "a", Replicas: ptr.To[int32](2)}
	ms := &api.ModelServing{Spec: api.ModelServingSpec{Template: api.ServingGroup{Roles: []api.Role{role}}}}
	hash := utils.CalRoleTemplateHash(role)
	instances := []datastore.Role{{Name: "a-0", RoleTemplateHash: hash, Status: datastore.RoleRunning}, {Name: "a-1", RoleTemplateHash: hash, Status: datastore.RoleRunning}}
	ms.Name, ms.Namespace, ms.UID = "model", "default", "model-owner"
	c := &ModelServingController{kubeClientSet: kubefake.NewSimpleClientset()}
	recordTestRevision(t, c.kubeClientSet, ms, "target")
	for i := range instances {
		instances[i].Revision = "target"
	}
	state := c.resolveRoleRolloutState(c.withRevisionHistory(context.Background(), ms), ms, datastore.ServingGroup{Name: "model-0"}, role, 2, instances, 1, true, nil, nil)
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
	handled, err := c.retireCoordinatedSurge(context.Background(), ms, "cleanup-0", role, instances, 1)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 0)))
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 1)))
	require.NoError(t, c.store.UpdateRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 0), datastore.RoleRunning))
	instances, err = c.store.GetRoleList(key, "cleanup-0", role.Name)
	require.NoError(t, err)
	client := c.kubeClientSet.(*kubefake.Clientset)
	denied := true
	client.PrependReactor("delete", "pods", func(action kt.Action) (bool, runtime.Object, error) {
		deletion := action.(kt.DeleteAction)
		require.Equal(t, "cleanup-0-prefill-1-0", deletion.GetName(), "a marked surge cannot authorize deleting formal capacity")
		require.Equal(t, ms.UID, *deletion.GetDeleteOptions().Preconditions.UID)
		if denied {
			return true, nil, fmt.Errorf("injected surge DELETE failure")
		}
		return false, nil, nil
	})
	handled, err = c.retireCoordinatedSurge(context.Background(), ms, "cleanup-0", role, instances, 1)
	require.ErrorContains(t, err, "injected surge DELETE failure")
	require.True(t, handled)
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 0)))
	require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, "cleanup-0", role.Name, utils.GenerateRoleID(role.Name, 1)))
	denied = false
	require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
	_, err = client.CoreV1().Pods(ms.Namespace).Get(context.Background(), "cleanup-0-prefill-0-0", metav1.GetOptions{})
	require.NoError(t, err, "retry preserves formal capacity")
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
	require.NoError(t, c.adoptSurgeReplicas(context.Background(), ms, surgeRole, "adoption-0", role.Name, roleReplicas(role)))
	actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, pod.UID, actual.UID)
	require.NotContains(t, actual.Annotations, surgeAnnotation)
	require.Contains(t, pod.Annotations, surgeAnnotation, "informer-owned Pod must not be mutated")
}

// Deliver fake API observations explicitly. This controls event lag without
// sleeping or treating a cached Ready bit as physical entry/worker readiness.
func deliverSurgePods(t *testing.T, c *ModelServingController, ms *api.ModelServing, ready bool) []corev1.Pod {
	t.Helper()
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, cached := range c.podsInformer.GetIndexer().List() {
		require.NoError(t, c.podsInformer.GetIndexer().Delete(cached))
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.UID == "" {
			pod.UID = types.UID(fmt.Sprintf("%s-%s-%d", pod.Name, utils.ObjectRevision(pod), i))
		}
		if ready {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod.DeepCopy()))
	}
	current, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(current))
	return pods.Items
}

func TestSurgeOriginPreservesFormalAndSparseIdentities(t *testing.T) {
	for _, mode := range []string{"ServingGroup", "Role", "CoordinatedRole"} {
		for _, sparse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sparse=%t", mode, sparse), func(t *testing.T) {
				ms := semanticRevisionTestModelServing("surge-origin", "target")
				ms.ResourceVersion, ms.Status.CurrentRevision = "1", "old"
				config := api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1))}
				scope := surgeRole
				if mode == "ServingGroup" {
					ms.Spec.Replicas = ptr.To[int32](2)
					ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate, RollingUpdateConfiguration: &config}
					scope = surgeServingGroup
				} else {
					ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
					ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
					ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
					if mode == "CoordinatedRole" {
						ms.Spec.RolloutStrategy.RoleCoordination = coordinationForTest("100%")
					}
				}
				ms.Spec.Template.Roles[0].WorkerReplicas = 1
				ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
				c := newRevisionTestController(t, ms)
				old := ms.DeepCopy()
				old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
				recordTestRevision(t, c.kubeClientSet, old, "old")
				original, stableHigh := map[string]types.UID{}, map[string]types.UID{}
				for _, ordinal := range []int{0, 1} {
					workload, revision := old, "old"
					if sparse && ordinal == 1 {
						ordinal, workload, revision = 3, ms, utils.ModelServingRevision(ms)
					}
					groupOrdinal, roleOrdinal := ordinal, 0
					if scope == surgeRole {
						groupOrdinal, roleOrdinal = 0, ordinal
					}
					group := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
					uids := seedAppliedRole(t, c, workload, workload.Spec.Template.Roles[0], groupOrdinal, roleOrdinal, revision, -1)
					for name, uid := range uids {
						original[name] = uid
						if sparse && ordinal == 3 {
							stableHigh[name] = uid
						}
					}
					require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", utils.GenerateRoleID("decode", roleOrdinal), datastore.RoleRunning))
					require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
				}
				key, ctx := ms.Namespace+"/"+ms.Name, context.Background()
				require.NoError(t, c.syncModelServing(ctx, key))
				pods := deliverSurgePods(t, c, ms, false)
				marked := 0
				for _, pod := range pods {
					if pod.Annotations[surgeAnnotation] == scope {
						marked++
						if pod.Labels[api.EntryLabelKey] == utils.Entry {
							pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
							_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, &pod, metav1.UpdateOptions{})
							require.NoError(t, err)
						}
					}
				}
				require.Equal(t, 2, marked, "entry and worker carry temporary origin")
				deliverSurgePods(t, c, ms, false)
				require.NoError(t, c.syncModelServing(ctx, key))
				for name, uid := range original {
					pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, uid, pod.UID, "entry Ready alone supplies no healthy credit")
				}
				for round := 0; round < 10; round++ {
					deliverSurgePods(t, c, ms, true)
					require.NoError(t, c.syncModelServing(ctx, key))
					pods = deliverSurgePods(t, c, ms, true)
					require.LessOrEqual(t, len(pods), 6, "Terminating/in-flight occupancy never creates more than N+S complete layouts")
				}
				require.Len(t, pods, 4)
				ordinals := []int{}
				for _, pod := range pods {
					require.Empty(t, pod.Annotations[surgeAnnotation], "completed rollout retires all temporary origin")
					require.Equal(t, utils.ModelServingRevision(ms), utils.ObjectRevision(&pod))
					name := pod.Labels[api.GroupNameLabelKey]
					if scope == surgeRole {
						name = utils.GetRoleID(&pod)
					}
					_, ordinal := utils.GetParentNameAndOrdinal(name)
					if !slices.Contains(ordinals, ordinal) {
						ordinals = append(ordinals, ordinal)
					}
					if uid, stable := stableHigh[pod.Name]; stable {
						require.Equal(t, uid, pod.UID, "unmarked high stable members survive")
					}
				}
				slices.Sort(ordinals)
				want := []int{0, 1}
				if sparse {
					want = []int{0, 3}
				}
				require.Equal(t, want, ordinals)
			})
		}
	}
}

func TestSurgeAdoptionRequiresFormalRangeAndCapacity(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		for _, tc := range []struct {
			name                 string
			desired, ordinal     int
			stable               []int
			terminating, adopted bool
		}{
			{"enters new formal range", 3, 2, []int{0, 1}, false, true},
			{"still outside expanded range", 4, 4, []int{0, 1, 3}, false, false},
			{"inside range but stable capacity still occupied", 2, 1, []int{0, 3}, false, false},
			{"terminating unit cannot be adopted", 3, 2, []int{0, 1}, true, false},
		} {
			t.Run(scope+"/"+tc.name, func(t *testing.T) {
				ms := semanticRevisionTestModelServing("adopt-origin", "target")
				ms.ResourceVersion = "1"
				config := api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1))}
				if scope == surgeServingGroup {
					ms.Spec.Replicas = ptr.To(int32(tc.desired))
					ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate, RollingUpdateConfiguration: &config}
				} else {
					ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
					ms.Spec.Template.Roles[0].Replicas = ptr.To(int32(tc.desired))
					ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
				}
				c, ctx := newRevisionTestController(t, ms), context.Background()
				target := utils.ModelServingRevision(ms)
				for _, ordinal := range append(slices.Clone(tc.stable), tc.ordinal) {
					group, role := ordinal, 0
					if scope == surgeRole {
						group, role = 0, ordinal
					}
					seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], group, role, target, -1)
				}
				group, role := tc.ordinal, 0
				if scope == surgeRole {
					group, role = 0, tc.ordinal
				}
				name := utils.GeneratePodName(utils.GenerateServingGroupName(ms.Name, group), utils.GenerateRoleID("decode", role), 0)
				pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
				require.NoError(t, err)
				setSurgeScope(pod, scope)
				if tc.terminating {
					pod.DeletionTimestamp = ptr.To(metav1.Now())
				}
				_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
				groupFilter, roleFilter := "", ""
				if scope == surgeRole {
					groupFilter, roleFilter = ms.Name+"-0", "decode"
				}
				require.NoError(t, c.adoptSurgeReplicas(ctx, ms, scope, groupFilter, roleFilter, tc.desired))
				actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, pod.UID, actual.UID)
				require.Equal(t, tc.adopted, actual.Annotations[surgeAnnotation] == "")
				if tc.name == "still outside expanded range" {
					// SG-S04: total=N does not adopt out-of-range temporary4.
					require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
					formalName := ms.Name + "-2-decode-0-0"
					if scope == surgeRole {
						formalName = ms.Name + "-0-decode-2-0"
					}
					formal, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, formalName, metav1.GetOptions{})
					require.NoError(t, err)
					require.Empty(t, formal.Annotations[surgeAnnotation])
					actual, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, pod.UID, actual.UID)
					require.Equal(t, scope, actual.Annotations[surgeAnnotation])
				}
			})
		}
	}
}

func TestSurgeOriginOverridesTemplateAndPluginInput(t *testing.T) {
	ms := semanticRevisionTestModelServing("origin-input", "target")
	ms.Spec.Template.Roles[0].EntryTemplate.Metadata = &api.Metadata{Annotations: map[string]string{surgeAnnotation: surgeRole}}
	ms.Spec.Plugins = []api.PluginSpec{testRevisionPlugin("origin")}
	ms.Spec.Plugins[0].Config.Raw = []byte(fmt.Sprintf(`{"annotations":{%q:%q}}`, surgeAnnotation, surgeRole))
	c := newRevisionTestController(t, ms)
	ctx := context.Background()
	target := utils.ModelServingRevision(ms)
	require.NoError(t, c.CreatePodsForServingGroup(ctx, ms, 0, target, ms.Spec.Template.Roles))
	require.NoError(t, c.CreatePodsForServingGroup(context.WithValue(ctx, coordinatedSurgeKey{}, surgeServingGroup), ms, 1, target, ms.Spec.Template.Roles))
	for ordinal, want := range []string{"", surgeServingGroup} {
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, fmt.Sprintf("%s-%d-decode-0-0", ms.Name, ordinal), metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, want, pod.Annotations[surgeAnnotation])
	}
}

func TestSparseHighReplacementUsesLowSlotHistory(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		for _, surge := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/surge=%d", scope, surge), func(t *testing.T) {
				ms := semanticRevisionTestModelServing("sparse-history", "target")
				ms.ResourceVersion, ms.Status.CurrentRevision = "1", "old"
				config := api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1 - surge)), MaxSurge: ptr.To(intstr.FromInt(surge)), Partition: ptr.To(intstr.FromInt(2))}
				if scope == surgeServingGroup {
					ms.Spec.Replicas = ptr.To[int32](2)
					ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate, RollingUpdateConfiguration: &config}
				} else {
					ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
					ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
					ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
				}
				c, ctx := newRevisionTestController(t, ms), context.Background()
				old := ms.DeepCopy()
				old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
				recordTestRevision(t, c.kubeClientSet, old, "old")
				var stable map[string]types.UID
				for _, ordinal := range []int{0, 3} {
					groupOrdinal, roleOrdinal := ordinal, 0
					if scope == surgeRole {
						groupOrdinal, roleOrdinal = 0, ordinal
					}
					uids := seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], groupOrdinal, roleOrdinal, "old", -1)
					if ordinal == 0 {
						stable = uids
					}
					group := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
					require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", utils.GenerateRoleID("decode", roleOrdinal), datastore.RoleRunning))
					require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
				}
				for round := 0; round < 8; round++ {
					require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
					pods := deliverSurgePods(t, c, ms, true)
					require.LessOrEqual(t, len(pods), 2+surge)
				}
				pods := deliverSurgePods(t, c, ms, true)
				require.Len(t, pods, 2)
				for _, pod := range pods {
					identity := pod.Labels[api.GroupNameLabelKey]
					if scope == surgeRole {
						identity = utils.GetRoleID(&pod)
					}
					_, ordinal := utils.GetParentNameAndOrdinal(identity)
					require.Contains(t, []int{0, 1}, ordinal)
					require.Equal(t, "old", utils.ObjectRevision(&pod), "new low protected slot chooses historical layout, not high source target")
					require.Empty(t, pod.Annotations[surgeAnnotation])
					if uid, retained := stable[pod.Name]; retained {
						require.Equal(t, uid, pod.UID)
					}
				}
			})
		}
	}
}

func TestHealthyObsoleteSurgeCarriesFloorUntilFormalReady(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			ms := semanticRevisionTestModelServing("surge-floor", "v3")
			ms.ResourceVersion, ms.Status.CurrentRevision = "1", "old"
			config := api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1))}
			if scope == surgeServingGroup {
				ms.Spec.Replicas = ptr.To[int32](3)
				ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate, RollingUpdateConfiguration: &config}
			} else {
				ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
				ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
			}
			c, ctx := newRevisionTestController(t, ms), context.Background()
			old, middle := ms.DeepCopy(), ms.DeepCopy()
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "v1"
			middle.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "v2"
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, middle, "middle")
			var protectedSurge *corev1.Pod
			for ordinal, revision := range []string{"old", "old", utils.ModelServingRevision(ms), "middle"} {
				workload := old
				if ordinal == 2 {
					workload = ms
				}
				if ordinal == 3 {
					workload = middle
				}
				group, role := ordinal, 0
				if scope == surgeRole {
					group, role = 0, ordinal
				}
				seedAppliedRole(t, c, workload, workload.Spec.Template.Roles[0], group, role, revision, -1)
				groupName, roleID := utils.GenerateServingGroupName(ms.Name, group), utils.GenerateRoleID("decode", role)
				pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(groupName, roleID, 0), metav1.GetOptions{})
				require.NoError(t, err)
				if ordinal == 2 {
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
				}
				if ordinal == 3 {
					setSurgeScope(pod, scope)
					protectedSurge = pod
				}
				_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
				status := datastore.RoleRunning
				groupStatus := datastore.ServingGroupRunning
				if ordinal == 2 {
					status, groupStatus = datastore.RoleCreating, datastore.ServingGroupCreating
				}
				require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, "decode", roleID, status))
				require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, groupStatus))
			}
			for round := 0; round < 3; round++ {
				require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
				deliverSurgePods(t, c, ms, false)
				actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, protectedSurge.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, protectedSurge.UID, actual.UID, "obsolete healthy surge still supplies the floor")
			}
			deliverSurgePods(t, c, ms, true)
			require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, protectedSurge.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err), "only complete formal Ready releases permission to replace obsolete surge")
		})
	}
}

func TestExpansionAdoptsSurgeBeforeCreatingNextTemporary(t *testing.T) {
	for _, mode := range []string{"ServingGroup", "Role", "CoordinatedRole"} {
		t.Run(mode, func(t *testing.T) {
			ms := semanticRevisionTestModelServing("surge-expand", "target")
			ms.ResourceVersion, ms.Status.CurrentRevision = "1", "old"
			config := api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1)), Partition: ptr.To(intstr.FromInt(1))}
			scope := surgeRole
			if mode == "ServingGroup" {
				ms.Spec.Replicas = ptr.To[int32](3)
				ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate, RollingUpdateConfiguration: &config}
				scope = surgeServingGroup
			} else {
				ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
				ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
				if mode == "CoordinatedRole" {
					ms.Spec.RolloutStrategy.RoleCoordination = coordinationForTest("100%")
				}
			}
			c, ctx := newRevisionTestController(t, ms), context.Background()
			old := ms.DeepCopy()
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
			recordTestRevision(t, c.kubeClientSet, old, "old")
			var adopted *corev1.Pod
			for ordinal := 0; ordinal < 3; ordinal++ {
				workload, revision := old, "old"
				if ordinal == 2 {
					workload, revision = ms, utils.ModelServingRevision(ms)
				}
				group, role := ordinal, 0
				if scope == surgeRole {
					group, role = 0, ordinal
				}
				seedAppliedRole(t, c, workload, workload.Spec.Template.Roles[0], group, role, revision, -1)
				groupName, roleID := utils.GenerateServingGroupName(ms.Name, group), utils.GenerateRoleID("decode", role)
				require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, "decode", roleID, datastore.RoleRunning))
				require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, datastore.ServingGroupRunning))
				if ordinal == 2 {
					pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(groupName, roleID, 0), metav1.GetOptions{})
					require.NoError(t, err)
					setSurgeScope(pod, scope)
					adopted = pod
					_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
				}
			}
			require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
			actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, adopted.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, adopted.UID, actual.UID)
			require.Empty(t, actual.Annotations[surgeAnnotation])
			newName := ms.Name + "-3-decode-0-0"
			oldName := ms.Name + "-1-decode-0-0"
			if scope == surgeRole {
				newName, oldName = ms.Name+"-0-decode-3-0", ms.Name+"-0-decode-1-0"
			}
			created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, newName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, scope, created.Annotations[surgeAnnotation])
			_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, oldName, metav1.GetOptions{})
			require.NoError(t, err, "adopted capacity supplies formal N, so the new NotReady surge grants no healthy-delete credit")
		})
	}
}

func TestSurgePartialCreationRetainsOriginAndCapacity(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			ms := semanticRevisionTestModelServing("partial-origin", "target")
			ms.ResourceVersion, ms.Status.CurrentRevision = "1", "old"
			ms.Spec.Template.Roles[0].WorkerReplicas = 1
			ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			config := api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(0)), MaxSurge: ptr.To(intstr.FromInt(1))}
			if scope == surgeServingGroup {
				ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate, RollingUpdateConfiguration: &config}
			} else {
				ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
				ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
			}
			c, ctx := newRevisionTestController(t, ms), context.Background()
			old := ms.DeepCopy()
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
			recordTestRevision(t, c.kubeClientSet, old, "old")
			seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", -1)
			require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), ms.Name+"-0", "decode", "decode-0", datastore.RoleRunning))
			require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), ms.Name+"-0", datastore.ServingGroupRunning))
			entryName := ms.Name + "-1-decode-0-0"
			if scope == surgeRole {
				entryName = ms.Name + "-0-decode-1-0"
			}
			workerName := entryName[:len(entryName)-1] + "1"
			client, denied := c.kubeClientSet.(*kubefake.Clientset), true
			client.PrependReactor("create", "pods", func(action kt.Action) (bool, runtime.Object, error) {
				if denied && action.(kt.CreateAction).GetObject().(*corev1.Pod).Name == workerName {
					return true, nil, fmt.Errorf("injected worker creation denial")
				}
				return false, nil, nil
			})
			require.ErrorContains(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name), "injected worker creation denial")
			deliverSurgePods(t, c, ms, false)
			entry, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, entryName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, scope, entry.Annotations[surgeAnnotation])
			denied = false
			require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
			worker, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, workerName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, scope, worker.Annotations[surgeAnnotation])
			actual, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, entryName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, entry.UID, actual.UID)
			pods, err := client.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 4, "partial replay must not allocate another surge unit")
		})
	}
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
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
			setSurgeScope(pod, surgeRole)
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			c.store.AddRole(key, "final-slot-0", role.Name, id, revision, utils.CalRoleTemplateHash(*role))
			require.NoError(t, c.store.UpdateRoleStatus(key, "final-slot-0", role.Name, id, datastore.RoleRunning))
			c.manageRoleReplicasPerGroup(context.Background(), ms, "final-slot-0", *role, 0, revision, true, nil)
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

func TestSurgeExplicitZeroDoesNotRequireAppliedHistory(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			ms := semanticRevisionTestModelServing("zero-unknown", "target")
			ms.ResourceVersion = "1"
			ms.Spec.RecoveryPolicy = api.NoneRestartPolicy
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: mode}
			if mode == api.ServingGroupRollingUpdate {
				ms.Spec.Replicas = ptr.To[int32](0)
			} else {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			}
			c := newRevisionTestController(t, ms)
			seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, 0, "unresolvable-applied", -1)
			group, name := ms.Name+"-0", utils.GeneratePodName(ms.Name+"-0", "decode-0", 0)
			require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", "decode-0", datastore.RoleRunning))
			require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
			if mode == api.ServingGroupRollingUpdate {
				pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
				require.NoError(t, err)
				setSurgeScope(pod, surgeServingGroup)
				_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
			}
			_ = c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name)
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err), "explicit zero does not require the discarded unit's applied template, including a temporary SG or independent Role")
		})
	}
}

// API v2.11 §2.0 and §6.1: independent Roles use separate histories and
// budgets. Unknown history waits without proving the affected unit Ready.
func TestSurgeIndependentHistoryErrorsStayWithinRole(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("history-isolation", "target")
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = api.NoneRestartPolicy
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
	second := *ms.Spec.Template.Roles[0].DeepCopy()
	second.Name = "prefill"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
	c := newRevisionTestController(t, ms)
	old := ms.DeepCopy()
	for i := range old.Spec.Template.Roles {
		old.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "old"
	}
	recordTestRevision(t, c.kubeClientSet, old, "old")
	seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "unresolvable-applied", -1)
	seedAppliedRole(t, c, old, old.Spec.Template.Roles[1], 0, 0, "old", -1)
	group := ms.Name + "-0"
	for _, role := range ms.Spec.Template.Roles {
		require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, role.Name, role.Name+"-0", datastore.RoleRunning))
	}
	require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
	syncErr := c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name)
	require.ErrorContains(t, syncErr, "unresolvable-applied")
	require.True(t, onlyRevisionErrors(syncErr))
	instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group, "decode")
	require.NoError(t, err)
	require.Len(t, instances, 1)
	require.Equal(t, datastore.RoleCreating, instances[0].Status, "unresolved whole-unit history cannot prove availability")
	require.Equal(t, datastore.ServingGroupCreating, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group))
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(group, "prefill-0", 0), metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "independent prefill has known old history and its own legal U1 budget; another Role's unknown history only pauses that Role")
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(group, "decode-0", 0), metav1.GetOptions{})
	require.NoError(t, err, "unknown-history Role must be preserved")
}

// API v2.11 §2.3 and SG §0.6: a desired-history failure does not erase
// actual origin or authorize source cleanup. Explicit quantities still apply.
func TestSurgeOriginSurvivesDesiredRevisionFailure(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		for _, failure := range []string{"list", "create"} {
			for _, stable := range []int{1, 2} {
				for _, zero := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stable=%d/zero=%v", mode, failure, stable, zero), func(t *testing.T) {
						ctx := context.Background()
						ms := semanticRevisionTestModelServing("origin-error", "target")
						ms.ResourceVersion = "1"
						ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: mode}
						if zero {
							ms.Spec.Replicas = ptr.To[int32](0)
						}
						c := newRevisionTestController(t, ms)
						target := utils.ModelServingRevision(ms)
						if failure == "create" {
							require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, target), metav1.DeleteOptions{}))
						}
						for ordinal := 0; ordinal <= stable; ordinal++ {
							seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], ordinal, 0, target, -1)
							group := utils.GenerateServingGroupName(ms.Name, ordinal)
							require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", "decode-0", datastore.RoleRunning))
							require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
						}
						name := utils.GeneratePodName(utils.GenerateServingGroupName(ms.Name, stable), "decode-0", 0)
						pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
						require.NoError(t, err)
						setSurgeScope(pod, surgeServingGroup)
						_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
						require.NoError(t, err)
						require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
						c.kubeClientSet.(*kubefake.Clientset).PrependReactor(failure, "controllerrevisions", func(kt.Action) (bool, runtime.Object, error) {
							return true, nil, fmt.Errorf("injected desired history %s failure", failure)
						})
						require.ErrorContains(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name), "injected desired history "+failure+" failure")
						pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
						require.NoError(t, err)
						if zero {
							require.Empty(t, pods.Items, "explicit top zero discards every unit without its template")
							return
						}
						require.Len(t, pods.Items, 2, "only explicit excess formal capacity may be discarded")
						actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
						require.NoError(t, err, "unresolved target must not grant temporary source cleanup")
						require.Equal(t, pod.UID, actual.UID)
						require.Equal(t, surgeServingGroup, actual.Annotations[surgeAnnotation])
						formal, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(ms.Name+"-0", "decode-0", 0), metav1.GetOptions{})
						require.NoError(t, err)
						require.Equal(t, types.UID("original-"+formal.Name), formal.UID)
					})
				}
			}
		}
	}
}

// API §2.3: zero desired Role capacity is a quantity decrease, including its
// current-mode temporary instances; discarded history is not a prerequisite.
func TestSurgeRoleZeroDiscardsUnknownTemporaryHistory(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("role-zero-origin", "target")
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = api.NoneRestartPolicy
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
	c := newRevisionTestController(t, ms)
	seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, 0, "unresolvable-applied", -1)
	group, name := ms.Name+"-0", utils.GeneratePodName(ms.Name+"-0", "decode-0", 0)
	require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "decode", "decode-0", datastore.RoleRunning))
	require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group, datastore.ServingGroupRunning))
	pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	setSurgeScope(pod, surgeRole)
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
	_ = c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name)
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "explicit Role zero discards its temporary capacity too and does not select any new template")
}

// SG §0.6: origin belongs to a complete SG or a specific Role instance.
func TestSurgeLocalRepairDoesNotInheritOtherRoleOrigin(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("other-role-refill", "target")
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = api.NoneRestartPolicy
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
	ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(0))
	ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt(1))
	ms.Spec.Template.Roles[0].WorkerReplicas = 1
	ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	second := *ms.Spec.Template.Roles[0].DeepCopy()
	second.Name = "prefill"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
	c := newRevisionTestController(t, ms)
	target := utils.ModelServingRevision(ms)
	seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, 0, target, -1)
	unready, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(ms.Name+"-0", "decode-0", 1), metav1.GetOptions{})
	require.NoError(t, err)
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, unready, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(unready))
	original := seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, 1, target, -1)
	seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[1], 0, 0, target, 1)
	for name := range original {
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		setSurgeScope(pod, surgeRole)
		_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
	}
	require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
	workerName := utils.GeneratePodName(ms.Name+"-0", "prefill-0", 1)
	worker, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, workerName, metav1.GetOptions{})
	require.NoError(t, err, "known-history repair in the different Role remains permitted")
	require.Equal(t, target, utils.ObjectRevision(worker))
	require.Empty(t, worker.Annotations[surgeAnnotation], "decode-1 temporary Role origin must not spread to the stable prefill-0 worker through a group-wide sibling scan")
	for name, uid := range original {
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, uid, pod.UID, "the same-mode healthy surge still supplies required availability")
		require.Equal(t, surgeRole, pod.Annotations[surgeAnnotation])
	}
}

func TestSurgeLocalRenderingInheritsMatchingOrigin(t *testing.T) {
	for _, scope := range []string{surgeRole, surgeServingGroup} {
		t.Run(scope, func(t *testing.T) {
			ctx := context.Background()
			ms := semanticRevisionTestModelServing("origin-refill", "target")
			ms.ResourceVersion = "1"
			ms.Spec.Template.Roles[0].WorkerReplicas = 1
			ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			second := *ms.Spec.Template.Roles[0].DeepCopy()
			second.Name = "prefill"
			ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
			c := newRevisionTestController(t, ms)
			target := utils.ModelServingRevision(ms)
			sourceRole := ms.Spec.Template.Roles[0]
			targetRole := sourceRole
			if scope == surgeServingGroup {
				targetRole = ms.Spec.Template.Roles[1]
			}
			seedAppliedRole(t, c, ms, sourceRole, 0, 0, target, 1)
			sourceName := utils.GeneratePodName(ms.Name+"-0", "decode-0", 0)
			source, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, sourceName, metav1.GetOptions{})
			require.NoError(t, err)
			setSurgeScope(source, scope)
			_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, source, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Update(source))
			hash, err := utils.RoleRevisionHash(ms, targetRole.Name)
			require.NoError(t, err)
			// Rendering-only control: same Role instance inherits Role origin;
			// another Role in the same SG inherits whole-SG origin.
			require.NoError(t, c.CreatePodsByRole(ctx, targetRole, ms, 0, 0, target, hash))
			worker, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(ms.Name+"-0", targetRole.Name+"-0", 1), metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, scope, worker.Annotations[surgeAnnotation])
			actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, sourceName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, source.UID, actual.UID)
		})
	}
}
