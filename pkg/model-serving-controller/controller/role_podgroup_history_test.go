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
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type rolePodGroupRecorder struct {
	workloads []*workloadv1alpha1.ModelServing
}

func recordRolePodGroupInputs(c *ModelServingController) *rolePodGroupRecorder {
	recorder := &rolePodGroupRecorder{}
	c.podGroupManager = &fakePodGroupManager{
		hasCRD: true,
		createOrUpdateFunc: func(_ context.Context, ms *workloadv1alpha1.ModelServing, _ string) (error, time.Duration) {
			recorder.workloads = append(recorder.workloads, ms.DeepCopy())
			return nil, 0
		},
	}
	return recorder
}

func rolePodGroupWorkload(image string, workers int32, cpu string) *workloadv1alpha1.ModelServing {
	ms := appliedHistoryWorkload(workloadv1alpha1.RoleRollingUpdate, image, workers)
	ms.Spec.SchedulerName = "volcano"
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
	if workers > 0 {
		ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
	}
	return ms
}

func requireRolePodGroupTemplate(t *testing.T, projected, source *workloadv1alpha1.ModelServing, currentCount int32) {
	t.Helper()
	require.Len(t, projected.Spec.Template.Roles, 1)
	got := *projected.Spec.Template.Roles[0].DeepCopy()
	// Planned target templates may still inherit their scheduler, whereas
	// persisted snapshots contain its explicit effective value.
	if got.EntryTemplate.Spec.SchedulerName == "" {
		got.EntryTemplate.Spec.SchedulerName = projected.Spec.SchedulerName
	}
	if got.WorkerTemplate != nil && got.WorkerTemplate.Spec.SchedulerName == "" {
		got.WorkerTemplate.Spec.SchedulerName = projected.Spec.SchedulerName
	}
	expected := source.Spec.Template.Roles[0].DeepCopy()
	expected.Replicas = ptr.To(currentCount)
	// Persisted templates carry the effective scheduler, including inherited values.
	expected.EntryTemplate.Spec.SchedulerName = "volcano"
	if expected.WorkerTemplate != nil {
		expected.WorkerTemplate.Spec.SchedulerName = "volcano"
	}
	require.Equal(t, *expected, got, "the whole historical Role template supplies the transitional layout and resource requirements")
}

func TestRolePodGroupHistoricalLayoutsAndFinalTarget(t *testing.T) {
	for _, layout := range [][2]int32{{1, 2}, {2, 1}, {0, 1}, {1, 0}} {
		t.Run(fmt.Sprintf("workers-%d-to-%d", layout[0], layout[1]), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := rolePodGroupWorkload("image:v1", layout[0], "1")
			current := rolePodGroupWorkload("image:v2", layout[1], "4")
			current.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", -1)
			group := utils.GenerateServingGroupName(current.Name, 0)
			recorder := recordRolePodGroupInputs(c)
			before := current.DeepCopy()
			ctx := c.withRevisionHistory(context.Background(), current)

			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			require.Len(t, recorder.workloads, 1)
			requireRolePodGroupTemplate(t, recorder.workloads[0], old, 3)
			// A smaller desired template alone must not lower the requirement;
			// the target enters the projection only after an instance exists.
			seedAppliedRole(t, c, current, current.Spec.Template.Roles[0], 0, 1, "new", -1)
			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			require.Len(t, recorder.workloads, 2)
			floor := old
			if layout[1] < layout[0] {
				floor = current
			}
			requireRolePodGroupTemplate(t, recorder.workloads[1], floor, 3)

			// Deleting instances cannot pin a smaller historical floor.
			require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(current), group, "decode", "decode-0", datastore.RoleDeleting))
			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			require.Len(t, recorder.workloads, 3)
			requireRolePodGroupTemplate(t, recorder.workloads[2], current, 3)

			pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			for i := range pods.Items {
				pod := &pods.Items[i]
				if utils.ObjectRevision(pod) == "old" {
					require.NoError(t, c.kubeClientSet.CoreV1().Pods(current.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
					require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
				}
			}
			c.store.DeleteRole(utils.GetNamespaceName(current), group, "decode", "decode-0")
			seedAppliedRole(t, c, current, current.Spec.Template.Roles[0], 0, 0, "new", -1)
			seedAppliedRole(t, c, current, current.Spec.Template.Roles[0], 0, 2, "new", -1)
			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			require.Len(t, recorder.workloads, 4)
			requireRolePodGroupTemplate(t, recorder.workloads[3], current, 3)
			require.Equal(t, before, current, "projection must not rewrite the desired spec")
		})
	}
}

func TestRolePodGroupPreservesCurrentCountAndExplicitGang(t *testing.T) {
	for _, minimum := range []int32{0, 1, 3} {
		t.Run(fmt.Sprintf("minimum-%d", minimum), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := rolePodGroupWorkload("image:v1", 1, "1")
			current := rolePodGroupWorkload("image:v2", 2, "4")
			current.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
			current.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"decode": minimum}}
			old.Spec.Template.GangPolicy = current.Spec.Template.GangPolicy.DeepCopy()
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", -1)
			seedAppliedRole(t, c, current, current.Spec.Template.Roles[0], 0, 3, "new", -1)
			recorder := recordRolePodGroupInputs(c)
			require.NoError(t, c.syncServingGroupReplicas(c.withRevisionHistory(context.Background(), current), current, "new"))
			require.Len(t, recorder.workloads, 1)
			requireRolePodGroupTemplate(t, recorder.workloads[0], old, 3)
			require.Equal(t, current.Spec.Template.GangPolicy, recorder.workloads[0].Spec.Template.GangPolicy, "surge and worker projection must not lower or inflate explicit k")
		})
	}
}

func TestRolePodGroupMissingWorkerDoesNotShrinkHistoryOrGrantReady(t *testing.T) {
	c := newAppliedHistoryController(t)
	old := rolePodGroupWorkload("image:v1", 2, "1")
	current := rolePodGroupWorkload("image:v2", 1, "4")
	current.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	recordTestRevision(t, c.kubeClientSet, old, "old")
	recordTestRevision(t, c.kubeClientSet, current, "new")
	seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", 2)
	group := utils.GenerateServingGroupName(current.Name, 0)
	recorder := recordRolePodGroupInputs(c)
	ctx := c.withRevisionHistory(context.Background(), current)
	require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
	requireRolePodGroupTemplate(t, recorder.workloads[0], old, 2)
	ready, err := c.checkRoleReady(current, group, "decode", "decode-0")
	require.NoError(t, err)
	require.False(t, ready, "two live Pods cannot replace a historical three-member Role")

	seedAppliedRole(t, c, current, current.Spec.Template.Roles[0], 0, 1, "new", -1)
	for _, id := range []string{"decode-0", "decode-1"} {
		require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(current), group, "decode", id, datastore.RoleRunning))
	}
	require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
	requireRolePodGroupTemplate(t, recorder.workloads[1], current, 2)
	ready, err = c.checkServingGroupReady(current, group)
	require.NoError(t, err)
	require.False(t, ready, "the smaller transitional scheduling floor is not complete Role availability")
	require.NoError(t, c.syncRoleReplicas(ctx, current, "new"))
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(current), group, "decode", "decode-0"))
	worker, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).Get(ctx, utils.GeneratePodName(group, "decode-0", 2), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "old", utils.ObjectRevision(worker), "repair still creates the missing historical member")
}

func TestRolePodGroupUnknownHistorySkipsUpdateAndRetainsError(t *testing.T) {
	for _, failure := range []string{"missing", "corrupt", "foreign"} {
		t.Run(failure, func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := rolePodGroupWorkload("image:v1", 2, "1")
			current := rolePodGroupWorkload("image:v2", 1, "4")
			if failure != "missing" {
				recordTestRevision(t, c.kubeClientSet, old, "old")
				cr, err := c.kubeClientSet.AppsV1().ControllerRevisions(old.Namespace).Get(context.Background(), utils.GenerateControllerRevisionName(old.Name, "old"), metav1.GetOptions{})
				require.NoError(t, err)
				if failure == "corrupt" {
					cr.Data.Raw = []byte(`{"unexpected":"invalid history"}`)
				} else {
					cr.OwnerReferences[0].UID = "previous-owner"
				}
				_, err = c.kubeClientSet.AppsV1().ControllerRevisions(old.Namespace).Update(context.Background(), cr, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			recordTestRevision(t, c.kubeClientSet, current, "new")
			seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", 2)
			recorder := recordRolePodGroupInputs(c)
			ctx := c.withRevisionHistory(context.Background(), current)
			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			require.Empty(t, recorder.workloads, "unknown history must not overwrite existing PG requirements with target or live Pod count")
			require.Error(t, c.revisionHistory(ctx, current).errors(), "history failure must remain available for workqueue retry")
		})
	}
}

func TestRolePodGroupProjectionDoesNotAliasInputsOrHistory(t *testing.T) {
	c := newAppliedHistoryController(t)
	old := rolePodGroupWorkload("image:v1", 1, "1")
	current := rolePodGroupWorkload("image:v2", 2, "4")
	current.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
	current.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"decode": 1}}
	recordTestRevision(t, c.kubeClientSet, old, "old")
	seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", -1)
	ctx := c.withRevisionHistory(context.Background(), current)
	group := datastore.ServingGroup{Name: utils.GenerateServingGroupName(current.Name, 0), Revision: "old"}
	before := current.DeepCopy()
	historical, err := c.revisionHistory(ctx, current).workload(ctx, "old")
	require.NoError(t, err)
	projected, err := c.workloadForPodGroup(ctx, current, group)
	require.NoError(t, err)
	*projected.Spec.Template.Roles[0].Replicas = 99
	projected.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "mutated"
	projected.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("99")
	projected.Spec.Template.GangPolicy.MinRoleReplicas["decode"] = 0
	require.Equal(t, before, current)
	after, err := c.revisionHistory(ctx, current).workload(ctx, "old")
	require.NoError(t, err)
	require.Equal(t, historical, after, "projection must not mutate cached immutable history")
}

func TestRolePodGroupSuccessfulScaleUpEnqueuesSmallerLayout(t *testing.T) {
	for _, failCreate := range []bool{false, true} {
		t.Run(fmt.Sprintf("create-fails-%t", failCreate), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			t.Cleanup(c.workqueue.ShutDown)
			old := rolePodGroupWorkload("image:v1", 2, "1")
			current := rolePodGroupWorkload("image:v2", 1, "4")
			current.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, 0, "old", -1)
			group := utils.GenerateServingGroupName(current.Name, 0)
			recorder := recordRolePodGroupInputs(c)
			ctx := c.withRevisionHistory(context.Background(), current)
			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			requireRolePodGroupTemplate(t, recorder.workloads[0], old, 2)
			require.Zero(t, c.workqueue.Len())
			if failCreate {
				c.kubeClientSet.(*kubefake.Clientset).PrependReactor("create", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("injected CREATE failure")
				})
			}
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(current), group, "decode")
			require.NoError(t, err)
			c.scaleUpRoles(ctx, current, group, current.Spec.Template.Roles[0], roles, 2, 0, "new")
			if failCreate {
				require.Zero(t, c.workqueue.Len(), "a failed creation must not be treated as a newly observed layout")
				require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
				requireRolePodGroupTemplate(t, recorder.workloads[1], old, 2)
				return
			}
			require.Equal(t, 1, c.workqueue.Len(), "refresh PG before waiting for Pods blocked by the old larger floor")
			key, shutdown := c.workqueue.Get()
			require.False(t, shutdown)
			require.Equal(t, namespacedKey(current.Namespace, current.Name), key)
			c.workqueue.Done(key)
			require.NoError(t, c.syncServingGroupReplicas(c.withRevisionHistory(context.Background(), current), current, "new"))
			requireRolePodGroupTemplate(t, recorder.workloads[1], current, 2)
		})
	}
}

func TestRolePodGroupHistoricalCompleteButInPlaceUpdatingIsNotReady(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, ordinal := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/updating-pod-%d", strategy, ordinal), func(t *testing.T) {
				c := newAppliedHistoryController(t)
				ms := appliedHistoryWorkload(strategy, "image:v1", 1)
				recordTestRevision(t, c.kubeClientSet, ms, "old")
				seedAppliedRole(t, c, ms, ms.Spec.Template.Roles[0], 0, 0, "old", -1)
				group := utils.GenerateServingGroupName(ms.Name, 0)
				ready, err := c.checkRoleReady(ms, group, "decode", "decode-0")
				require.NoError(t, err)
				require.True(t, ready)
				pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), utils.GeneratePodName(group, "decode-0", ordinal), metav1.GetOptions{})
				require.NoError(t, err)
				state, err := json.Marshal(inplace.State{Revision: "new", Patched: map[string]inplace.PatchedContainer{"main": {Image: "image:v2"}}})
				require.NoError(t, err)
				pod.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: string(state)}
				require.True(t, inplace.Updating(pod), "fixture retains stale Ready while an in-place update is unfinished")
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
				ready, err = c.checkRoleReady(ms, group, "decode", "decode-0")
				require.NoError(t, err)
				require.False(t, ready, "complete historical membership cannot count stale Ready during in-place update")
			})
		}
	}
}

func TestRolePodGroupRecreatedInstanceEnqueuesSmallerLayout(t *testing.T) {
	c := newAppliedHistoryController(t)
	t.Cleanup(c.workqueue.ShutDown)
	old := rolePodGroupWorkload("image:v1", 2, "1")
	current := rolePodGroupWorkload("image:v2", 1, "4")
	recordTestRevision(t, c.kubeClientSet, old, "old")
	recordTestRevision(t, c.kubeClientSet, current, "new")
	group := utils.GenerateServingGroupName(current.Name, 0)
	hash, err := utils.RoleRevisionHash(old, "decode")
	require.NoError(t, err)
	c.store.AddServingGroup(utils.GetNamespaceName(current), 0, "old")
	c.store.AddRole(utils.GetNamespaceName(current), group, "decode", "decode-0", "old", hash)
	recorder := recordRolePodGroupInputs(c)
	ctx := c.withRevisionHistory(context.Background(), current)
	require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
	requireRolePodGroupTemplate(t, recorder.workloads[0], old, 1)
	require.Zero(t, c.workqueue.Len())
	require.NoError(t, c.syncRoleReplicas(ctx, current, "new"))
	roles, err := c.store.GetRoleList(utils.GetNamespaceName(current), group, "decode")
	require.NoError(t, err)
	require.Len(t, roles, 1)
	require.Equal(t, "new", roles[0].Revision)
	require.Equal(t, 1, c.workqueue.Len(), "resetting a stale instance also needs a PG refresh before smaller Pods can schedule")
	require.NoError(t, c.syncServingGroupReplicas(c.withRevisionHistory(context.Background(), current), current, "new"))
	requireRolePodGroupTemplate(t, recorder.workloads[1], current, 1)
}

func TestRolePodGroupEmptyInstancesUsePartitionSelectedHistory(t *testing.T) {
	for _, partition := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("partition-%d", partition), func(t *testing.T) {
			c := newAppliedHistoryController(t)
			old := rolePodGroupWorkload("image:v1", 2, "1")
			current := rolePodGroupWorkload("image:v2", 1, "4")
			current.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			current.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt(partition))
			current.Status.CurrentRevision = "old"
			current.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"decode": 1}}
			old.Spec.Template.GangPolicy = current.Spec.Template.GangPolicy.DeepCopy()
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			c.store.AddServingGroup(utils.GetNamespaceName(current), 0, "old")
			recorder := recordRolePodGroupInputs(c)
			ctx := c.withRevisionHistory(context.Background(), current)
			require.NoError(t, c.syncServingGroupReplicas(ctx, current, "new"))
			require.Len(t, recorder.workloads, 1)
			expected := current
			if partition == 2 {
				expected = old
			}
			// Rollout controls remain current even when the projected layout is historical.
			expected = expected.DeepCopy()
			expected.Spec.Template.Roles[0].Partition = ptr.To(*current.Spec.Template.Roles[0].Partition)
			requireRolePodGroupTemplate(t, recorder.workloads[0], expected, 2)
			require.Equal(t, current.Spec.Template.GangPolicy, recorder.workloads[0].Spec.Template.GangPolicy)
			require.NoError(t, c.syncRoleReplicas(ctx, current, "new"))
			group := utils.GenerateServingGroupName(current.Name, 0)
			for ordinal := 0; ordinal < 2; ordinal++ {
				pod, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).Get(ctx, utils.GeneratePodName(group, utils.GenerateRoleID("decode", ordinal), 0), metav1.GetOptions{})
				require.NoError(t, err)
				want := "new"
				if ordinal < partition {
					want = "old"
				}
				require.Equal(t, want, utils.ObjectRevision(pod))
			}
		})
	}
}
