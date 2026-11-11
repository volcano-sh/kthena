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
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// newSurgeFixture builds the state right after the last outdated unit of a
// rollout is gone: units 1..3 are updated and Ready, 3 carries the surge
// marker, and the desired ordinal 0 is missing. The units are ServingGroups
// for scope surgeServingGroup and Role instances of group 0 for surgeRole.
func newSurgeFixture(t *testing.T, scope string) (*ModelServingController, *workloadv1alpha1.ModelServing, string) {
	t.Helper()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "completion", Namespace: "default", UID: types.UID("completion-owner")},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type: workloadv1alpha1.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
					MaxSurge: ptr.To(intstr.FromInt(1)), MaxUnavailable: ptr.To(intstr.FromInt(0)),
				},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name: "inference", Replicas: ptr.To[int32](1),
				EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "server", Image: "new"}},
				}},
			}}},
		},
		Status: workloadv1alpha1.ModelServingStatus{CurrentRevision: "old"},
	}
	if scope == surgeRole {
		ms.Spec.Replicas = ptr.To[int32](1)
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
		ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt(1))
		ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(0))
	}
	revision := utils.ModelServingRevision(ms)
	ms.Status.UpdateRevision = revision
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(ms), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	t.Cleanup(c.workqueue.ShutDown)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	recordTestRevision(t, c.kubeClientSet, ms, revision)
	role := ms.Spec.Template.Roles[0]
	hash, err := utils.RoleRevisionHash(ms, role.Name)
	require.NoError(t, err)
	for ordinal := 1; ordinal <= 3; ordinal++ {
		pod := addSurgeFixtureUnit(t, c, ms, scope, ordinal, revision, hash, true)
		if ordinal == 3 {
			setSurgeScope(pod, scope)
			require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
	}
	return c, ms, revision
}

// addSurgeFixtureUnit adds one Running unit with an entry Pod whose readiness
// is ready. The store status is Running either way, as it is until the next
// Role sync observes the Pod.
func addSurgeFixtureUnit(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, scope string, ordinal int, revision, hash string, ready bool) *corev1.Pod {
	t.Helper()
	key := utils.GetNamespaceName(ms)
	role := ms.Spec.Template.Roles[0]
	groupOrdinal, roleOrdinal := ordinal, 0
	if scope == surgeRole {
		groupOrdinal, roleOrdinal = 0, ordinal
	}
	groupName := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
	roleID := utils.GenerateRoleID(role.Name, roleOrdinal)
	c.store.AddServingGroup(key, groupOrdinal, revision)
	c.store.AddRole(key, groupName, role.Name, roleID, revision, hash)
	require.NoError(t, c.store.UpdateRoleStatus(key, groupName, role.Name, roleID, datastore.RoleRunning))
	require.NoError(t, c.store.UpdateServingGroupStatus(key, groupName, datastore.ServingGroupRunning))
	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, groupName, roleID, revision, hash)
	pod.UID = types.UID(pod.Name)
	setPodReady(pod, ready)
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	return pod
}

func setPodReady(pod *corev1.Pod, ready bool) {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}
}

// surgeFixtureUnitDeleting reports whether the fixture unit is being deleted.
func surgeFixtureUnitDeleting(c *ModelServingController, ms *workloadv1alpha1.ModelServing, scope string, ordinal int) bool {
	key := utils.GetNamespaceName(ms)
	if scope == surgeServingGroup {
		return c.store.GetServingGroupStatus(key, utils.GenerateServingGroupName(ms.Name, ordinal)) == datastore.ServingGroupDeleting
	}
	role := ms.Spec.Template.Roles[0].Name
	return c.store.GetRoleStatus(key, utils.GenerateServingGroupName(ms.Name, 0), role, utils.GenerateRoleID(role, ordinal)) == datastore.RoleDeleting
}

// reconcileSurgeFixture runs the Role sync, which refreshes readiness from the
// Pods, followed by the rolling update step, as one reconcile does.
func reconcileSurgeFixture(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, revision string) {
	t.Helper()
	ctx := c.withRevisionHistory(context.Background(), ms)
	require.NoError(t, c.syncRoleReplicas(ctx, ms, revision, nil))
	require.NoError(t, c.manageRollingUpdateWithPolicy(ctx, ms, revision, &roleRolloutPolicy{}))
}

func syncInformerPods(t *testing.T, c *ModelServingController, namespace string) {
	t.Helper()
	pods, err := c.kubeClientSet.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for i := range pods.Items {
		require.NoError(t, c.podsInformer.GetIndexer().Update(&pods.Items[i]))
	}
}

// newRolloutHarness builds a ServingGroup rollout from revision "old" to "new"
// with unmarked Running groups at the given ordinals.
func newRolloutHarness(t *testing.T, replicas, maxSurge, maxUnavailable, partition int, initial map[int]string) (*ModelServingController, *workloadv1alpha1.ModelServing) {
	t.Helper()
	ctx := context.Background()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	t.Cleanup(c.workqueue.ShutDown)
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "sparse", Namespace: "default", UID: types.UID("sparse-owner")},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To(int32(replicas)),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type: workloadv1alpha1.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
					MaxSurge: ptr.To(intstr.FromInt(maxSurge)), MaxUnavailable: ptr.To(intstr.FromInt(maxUnavailable)),
					Partition: ptr.To(intstr.FromInt(partition)),
				},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name: "decode", Replicas: ptr.To[int32](1),
				EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "decode", Image: "new"}},
				}},
			}}},
		},
		Status: workloadv1alpha1.ModelServingStatus{CurrentRevision: "old"},
	}
	oldRoles := ms.DeepCopy().Spec.Template.Roles
	oldRoles[0].EntryTemplate.Spec.Containers[0].Image = "old"
	_, err = utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "old", oldRoles)
	require.NoError(t, err)
	recordTestRevision(t, c.kubeClientSet, ms, "new")
	key := utils.GetNamespaceName(ms)
	for ordinal, revision := range initial {
		c.store.AddServingGroup(key, ordinal, revision)
		require.NoError(t, c.store.UpdateServingGroupStatus(key, utils.GenerateServingGroupName(ms.Name, ordinal), datastore.ServingGroupRunning))
	}
	return c, ms
}

// runRollout drives replica sync and rolling update until the layout settles.
// Replica sync runs twice per round because it repeats while new Pods start.
// It checks the availability floor after every step and returns the final
// Running groups as name=revision. Groups below partition may stay old.
func runRollout(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, maxUnavailable int) []string {
	t.Helper()
	ctx := context.Background()
	key := utils.GetNamespaceName(ms)
	replicas := int(*ms.Spec.Replicas)
	resyncPods := func() {
		for _, obj := range c.podsInformer.GetIndexer().List() {
			require.NoError(t, c.podsInformer.GetIndexer().Delete(obj))
		}
		pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		for i := range pods.Items {
			require.NoError(t, c.podsInformer.GetIndexer().Add(&pods.Items[i]))
		}
	}
	running := func() []string {
		groups, err := c.store.GetServingGroupByModelServing(key)
		require.NoError(t, err)
		names := []string{}
		for _, group := range groups {
			if group.Status == datastore.ServingGroupRunning {
				names = append(names, group.Name)
			}
		}
		return names
	}
	finishDeletion := func() {
		groups, err := c.store.GetServingGroupByModelServing(key)
		require.NoError(t, err)
		for _, group := range groups {
			if group.Status != datastore.ServingGroupDeleting {
				continue
			}
			_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
			assert.False(t, group.Revision == "new" && ordinal < replicas, "desired replacement %s was deleted", group.Name)
			c.store.DeleteServingGroup(key, group.Name)
		}
		resyncPods()
		assert.GreaterOrEqual(t, len(running()), replicas-maxUnavailable, "availability floor")
	}
	var last []string
	for round := 0; round < 20; round++ {
		for range 2 {
			require.NoError(t, c.syncServingGroupReplicas(ctx, ms, "new"))
			resyncPods()
			finishDeletion()
		}
		groups, err := c.store.GetServingGroupByModelServing(key)
		require.NoError(t, err)
		for _, group := range groups {
			if group.Status == datastore.ServingGroupCreating {
				require.NoError(t, c.store.UpdateServingGroupStatus(key, group.Name, datastore.ServingGroupRunning))
			}
		}
		require.NoError(t, c.manageRollingUpdateWithPolicy(ctx, ms, "new", &roleRolloutPolicy{}))
		finishDeletion()
		groups, err = c.store.GetServingGroupByModelServing(key)
		require.NoError(t, err)
		settled := true
		current := []string{}
		partition := ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition.IntValue()
		for _, group := range groups {
			_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
			settled = settled && (group.Revision == "new" || ordinal < partition) && group.Status == datastore.ServingGroupRunning
			current = append(current, group.Name+"="+group.Revision)
		}
		if settled && len(groups) == replicas && slices.Equal(current, last) {
			return current
		}
		last = current
	}
	t.Fatalf("rollout did not settle, last layout %v", last)
	return nil
}

// A ServingGroup rollout with maxSurge ends at ordinals [0, replicas): the
// surge group is retired and the ordinal vacated by the last replaced group is
// recreated, without dropping below replicas-maxUnavailable.
func TestServingGroupRolloutRestoresOrdinals(t *testing.T) {
	all := func(n int) []string {
		want := make([]string, 0, n)
		for ordinal := 0; ordinal < n; ordinal++ {
			want = append(want, fmt.Sprintf("sparse-%d=new", ordinal))
		}
		return want
	}
	for _, tt := range []struct {
		name                                string
		replicas, surge, unavail, partition int
		initial                             map[int]string
		want                                []string
	}{
		{"maxUnavailable 0", 3, 1, 0, 0, map[int]string{0: "old", 1: "old", 2: "old"}, all(3)},
		{"maxUnavailable 1", 3, 1, 1, 0, map[int]string{0: "old", 1: "old", 2: "old"}, all(3)},
		{"maxUnavailable 2", 3, 1, 2, 0, map[int]string{0: "old", 1: "old", 2: "old"}, all(3)},
		{"maxSurge 2", 3, 2, 0, 0, map[int]string{0: "old", 1: "old", 2: "old"}, all(3)},
		{"maxSurge 2 and maxUnavailable 1", 4, 2, 1, 0, map[int]string{0: "old", 1: "old", 2: "old", 3: "old"}, all(4)},
		{"single replica", 1, 1, 0, 0, map[int]string{0: "old"}, all(1)},
		{"sparse layout left by scale-down", 2, 1, 0, 0, map[int]string{1: "old", 3: "old"}, all(2)},
		{"unmarked group beyond the desired range", 3, 1, 0, 0, map[int]string{0: "old", 1: "old", 5: "old"}, all(3)},
		{"partition protects the lowest ordinal", 3, 1, 0, 1, map[int]string{0: "old", 1: "old", 2: "old"}, []string{"sparse-0=old", "sparse-1=new", "sparse-2=new"}},
		{"partition with a sparse layout", 2, 1, 0, 1, map[int]string{0: "old", 3: "old"}, []string{"sparse-0=old", "sparse-1=new"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, ms := newRolloutHarness(t, tt.replicas, tt.surge, tt.unavail, tt.partition, tt.initial)
			assert.Equal(t, tt.want, runRollout(t, c, ms, tt.unavail))
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			for _, pod := range pods.Items {
				assert.NotContains(t, pod.Annotations, surgeAnnotation, "marker left on %s", pod.Name)
			}
		})
	}
}

// Scaling up during a rollout turns the surge group into a regular replica
// and creates a new surge group: {0,1,2S} becomes {0,1,2,3S}.
func TestScaleUpDuringRolloutCreatesNewSurge(t *testing.T) {
	c, ms := newRolloutHarness(t, 2, 1, 0, 0, map[int]string{0: "old", 1: "old"})
	ctx := context.Background()
	key := utils.GetNamespaceName(ms)
	surgeGroups := func() []string {
		syncInformerPods(t, c, ms.Namespace)
		groups, err := c.store.GetServingGroupByModelServing(key)
		require.NoError(t, err)
		surge, err := c.surgeServingGroups(ms, groups)
		require.NoError(t, err)
		return sets.List(surge)
	}
	require.NoError(t, c.syncServingGroupReplicas(ctx, ms, "new"))
	assert.Equal(t, []string{"sparse-2"}, surgeGroups())

	ms.Spec.Replicas = ptr.To[int32](3)
	require.NoError(t, c.syncServingGroupReplicas(ctx, ms, "new"))
	assert.Equal(t, []string{"sparse-3"}, surgeGroups())
	pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName("sparse-2", "decode-0", 0), metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, pod.Annotations, surgeAnnotation, "the group inside the desired range is a regular replica")

	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	for _, group := range groups {
		require.NoError(t, c.store.UpdateServingGroupStatus(key, group.Name, datastore.ServingGroupRunning))
	}
	assert.Equal(t, []string{"sparse-0=new", "sparse-1=new", "sparse-2=new"}, runRollout(t, c, ms, 0))
}

// A RoleRollingUpdate with maxSurge returns the Role instances of a group to
// ordinals [0, replicas) within the availability floor and replica ceiling.
func TestRoleRolloutRestoresOrdinals(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		replicas, surge, unavail int32
	}{
		{"maxUnavailable 0", 3, 1, 0},
		{"maxUnavailable 1", 3, 1, 1},
		{"maxUnavailable 2", 3, 1, 2},
		{"maxUnavailable equal to replicas", 3, 1, 3},
		{"maxSurge 2", 3, 2, 0},
		{"single replica", 1, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
			require.NoError(t, err)
			t.Cleanup(c.workqueue.ShutDown)
			workload := func(image string) *workloadv1alpha1.ModelServing {
				ms := &workloadv1alpha1.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: "roles", Namespace: "default", UID: types.UID("roles-owner")},
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas:        ptr.To[int32](1),
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate},
						Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
							Name: "decode", Replicas: ptr.To(tt.replicas),
							EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "decode", Image: image}},
							}},
						}}},
					},
					Status: workloadv1alpha1.ModelServingStatus{CurrentRevision: "old"},
				}
				ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt32(tt.surge))
				ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt32(tt.unavail))
				return ms
			}
			old, current := workload("old"), workload("new")
			recordTestRevision(t, c.kubeClientSet, old, "old")
			recordTestRevision(t, c.kubeClientSet, current, "new")
			require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
			key := utils.GetNamespaceName(current)
			group := utils.GenerateServingGroupName(current.Name, 0)
			role := current.Spec.Template.Roles[0]
			for ordinal := 0; ordinal < int(tt.replicas); ordinal++ {
				seedAppliedRole(t, c, old, old.Spec.Template.Roles[0], 0, ordinal, "old", -1)
				require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, utils.GenerateRoleID(role.Name, ordinal), datastore.RoleRunning))
			}
			ctx := c.withRevisionHistory(context.Background(), current)
			// deliver hands the API state to the informer and completes pending
			// Role deletions; with ready it also lets every live Pod become Ready.
			deliver := func(ready bool) {
				pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				for _, obj := range c.podsInformer.GetIndexer().List() {
					require.NoError(t, c.podsInformer.GetIndexer().Delete(obj))
				}
				for i := range pods.Items {
					pod := &pods.Items[i]
					if ready {
						setPodReady(pod, true)
						_, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
						require.NoError(t, err)
					}
					require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
				}
				instances, err := c.store.GetRoleList(key, group, role.Name)
				require.NoError(t, err)
				for _, instance := range instances {
					if instance.Status == datastore.RoleDeleting {
						c.store.DeleteRole(key, group, role.Name, instance.Name)
					}
				}
			}
			// observe refreshes readiness from the Pods and checks the bounds.
			observe := func(step string) []string {
				require.NoError(t, c.syncRoleReplicas(ctx, current, "new", nil))
				instances, err := c.store.GetRoleList(key, group, role.Name)
				require.NoError(t, err)
				names, running := []string{}, 0
				for _, instance := range instances {
					names = append(names, instance.Name+"="+instance.Revision)
					if instance.Status == datastore.RoleRunning {
						running++
					}
				}
				assert.GreaterOrEqual(t, running, int(tt.replicas-tt.unavail), "availability floor %s: %v", step, names)
				assert.LessOrEqual(t, len(names), int(tt.replicas+tt.surge), "replica ceiling %s: %v", step, names)
				return names
			}
			want := []string{}
			for ordinal := 0; ordinal < int(tt.replicas); ordinal++ {
				want = append(want, fmt.Sprintf("decode-%d=new", ordinal))
			}
			var last []string
			for round := 0; round < 40; round++ {
				names := observe("after replica sync")
				require.NoError(t, c.manageRollingUpdateWithPolicy(ctx, current, "new", &roleRolloutPolicy{}))
				// Another reconcile runs while the deleted Pods are still terminating,
				// with the Pods created so far visible to the informer: the surge
				// instance stays as long as an outdated instance exists.
				created, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				for i := range created.Items {
					if _, exists, _ := c.podsInformer.GetIndexer().Get(&created.Items[i]); !exists {
						require.NoError(t, c.podsInformer.GetIndexer().Add(&created.Items[i]))
					}
				}
				observe("while terminating")
				require.NoError(t, c.manageRollingUpdateWithPolicy(ctx, current, "new", &roleRolloutPolicy{}))
				instances, err := c.store.GetRoleList(key, group, role.Name)
				require.NoError(t, err)
				outdatedLeft := slices.ContainsFunc(instances, func(instance datastore.Role) bool { return instance.Revision == "old" })
				for _, instance := range instances {
					assert.False(t, outdatedLeft && instance.Revision == "new" && instance.Status == datastore.RoleDeleting,
						"%s is deleted while outdated instances remain", instance.Name)
				}
				// Deletions take effect before any new Pod becomes Ready.
				deliver(false)
				observe("after rolling update")
				deliver(true)
				if slices.Equal(names, want) && slices.Equal(names, last) {
					pods, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).List(ctx, metav1.ListOptions{})
					require.NoError(t, err)
					for _, pod := range pods.Items {
						assert.NotContains(t, pod.Annotations, surgeAnnotation, "marker left on %s", pod.Name)
					}
					return
				}
				last = names
			}
			t.Fatalf("Role rollout did not converge, last layout %v", last)
		})
	}
}

// A Ready surge unit is retired only while replicas-maxUnavailable units stay
// Ready. Readiness is the one observed in the same reconcile, not the one the
// store held before it.
func TestSurgeRetirementKeepsAvailability(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			c, ms, revision := newSurgeFixture(t, scope)
			hash, err := utils.RoleRevisionHash(ms, ms.Spec.Template.Roles[0].Name)
			require.NoError(t, err)
			// The restored unit 0 is not Ready yet although the store still says Running.
			restored := addSurgeFixtureUnit(t, c, ms, scope, 0, revision, hash, false)
			reconcileSurgeFixture(t, c, ms, revision)
			assert.False(t, surgeFixtureUnitDeleting(c, ms, scope, 3), "maxUnavailable=0 keeps the Ready surge until unit 0 is Ready")

			setPodReady(restored, true)
			require.NoError(t, c.podsInformer.GetIndexer().Update(restored))
			reconcileSurgeFixture(t, c, ms, revision)
			assert.True(t, surgeFixtureUnitDeleting(c, ms, scope, 3))
			for ordinal := 0; ordinal < 3; ordinal++ {
				assert.False(t, surgeFixtureUnitDeleting(c, ms, scope, ordinal), "regular unit %d", ordinal)
			}
		})
	}
	// The rolling update budget does not count an unready outdated group that
	// partition protects; the surge group must still not be retired.
	t.Run("partition protected group is not ready", func(t *testing.T) {
		c, ms, revision := newSurgeFixture(t, surgeServingGroup)
		ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
		oldRoles := ms.DeepCopy().Spec.Template.Roles
		oldRoles[0].EntryTemplate.Spec.Containers[0].Image = "old"
		_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "old", oldRoles)
		require.NoError(t, err)
		key := utils.GetNamespaceName(ms)
		c.store.AddServingGroup(key, 0, "old")
		require.NoError(t, c.manageRollingUpdateWithPolicy(context.Background(), ms, revision, &roleRolloutPolicy{}))
		assert.False(t, surgeFixtureUnitDeleting(c, ms, surgeServingGroup, 3))
	})
}

// A surge Role instance is deleted, not updated in place: its Pods already run
// the target revision, so an in-place update would never remove it.
func TestSurgeRoleIsDeletedWithInPlaceUpdateMethod(t *testing.T) {
	c, ms, revision := newSurgeFixture(t, surgeRole)
	ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
	ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
	hash, err := utils.RoleRevisionHash(ms, ms.Spec.Template.Roles[0].Name)
	require.NoError(t, err)
	addSurgeFixtureUnit(t, c, ms, surgeRole, 0, revision, hash, true)
	// In-place updates need restartPolicy Always.
	for _, obj := range c.podsInformer.GetIndexer().List() {
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
		require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
	}
	require.True(t, c.updatesInPlace(ms))
	reconcileSurgeFixture(t, c, ms, revision)
	assert.True(t, surgeFixtureUnitDeleting(c, ms, surgeRole, 3))
}

// A replica increase that covers a surge unit makes it a regular replica: the
// marker is removed and the Pods are kept.
func TestScaleUpAdoptsSurge(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			c, ms, revision := newSurgeFixture(t, scope)
			ctx := context.Background()
			group := utils.GenerateServingGroupName(ms.Name, 0)
			role := ms.Spec.Template.Roles[0]
			surgePod := utils.GeneratePodName(utils.GenerateServingGroupName(ms.Name, 3), utils.GenerateRoleID(role.Name, 0), 0)
			if scope == surgeRole {
				surgePod = utils.GeneratePodName(group, utils.GenerateRoleID(role.Name, 3), 0)
			}
			// A Pod of another owner with the same labels is left alone.
			marked, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, surgePod, metav1.GetOptions{})
			require.NoError(t, err)
			foreign := marked.DeepCopy()
			foreign.Name += "-foreign"
			foreign.OwnerReferences[0].UID = types.UID("another-owner")
			_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, foreign, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(foreign))

			if scope == surgeServingGroup {
				ms.Spec.Replicas = ptr.To[int32](4)
				require.NoError(t, c.syncServingGroupReplicas(ctx, ms, revision))
			} else {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](4)
				require.NoError(t, c.manageRoleReplicasPerGroup(ctx, ms, group, ms.Spec.Template.Roles[0], 0, revision, true, nil))
			}
			adopted, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, surgePod, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, marked.UID, adopted.UID)
			assert.NotContains(t, adopted.Annotations, surgeAnnotation)
			untouched, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, foreign.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, scope, untouched.Annotations[surgeAnnotation])
		})
	}
}

// The last outdated unit can leave the store before its ordinal is recreated.
// Counts and revisions then match, but the rollout is not complete while the
// surge unit is still to be retired.
func TestStatusWaitsForSurgeRetirement(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			c, ms, revision := newSurgeFixture(t, scope)
			require.NoError(t, c.UpdateModelServingStatus(ms, revision))
			current, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "old", current.Status.CurrentRevision)
			assert.True(t, meta.IsStatusConditionTrue(current.Status.Conditions, string(workloadv1alpha1.ModelServingUpdateInProgress)))
		})
	}
}

// Unmarked units beyond the desired range, as left by a scale-down that kept
// them, are regular replicas and are not replaced.
func TestUnmarkedSparseReplicasAreNotReplaced(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			c, ms, revision := newSurgeFixture(t, scope)
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			for i := range pods.Items {
				pod := pods.Items[i].DeepCopy()
				delete(pod.Annotations, surgeAnnotation)
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
			}
			kube := c.kubeClientSet.(*kubefake.Clientset)
			kube.ClearActions()
			if scope == surgeServingGroup {
				require.NoError(t, c.syncServingGroupReplicas(context.Background(), ms, revision))
			}
			reconcileSurgeFixture(t, c, ms, revision)
			for _, action := range kube.Actions() {
				if action.GetResource().Resource == "pods" {
					assert.NotContains(t, []string{"create", "delete", "delete-collection", "patch", "update"}, action.GetVerb())
				}
			}
		})
	}
}

// The marker goes on the entry and worker Pods of a surge unit and is not part
// of the Role template or the revision.
func TestSurgeMarkerIsNotPartOfTheRevision(t *testing.T) {
	c, ms, _ := newSurgeFixture(t, surgeRole)
	ms.Spec.Template.Roles[0].WorkerReplicas = 1
	ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	revision := utils.ModelServingRevision(ms)
	recordTestRevision(t, c.kubeClientSet, ms, revision)
	before := ms.Spec.Template.DeepCopy()
	wantHash, err := utils.RoleRevisionHash(ms, "inference")
	require.NoError(t, err)
	gotHash, err := c.createPodsByRole(withSurgeScope(context.Background(), surgeRole), ms.Spec.Template.Roles[0], ms, 4, 0, revision, wantHash)
	require.NoError(t, err)
	assert.Equal(t, *before, ms.Spec.Template)
	assert.Equal(t, wantHash, gotHash)
	assert.Equal(t, revision, utils.ModelServingRevision(ms))
	for _, name := range []string{"completion-0-inference-4-0", "completion-0-inference-4-1"} {
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), name, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, surgeRole, pod.Annotations[surgeAnnotation])
		assert.Equal(t, revision, pod.Labels[workloadv1alpha1.RevisionLabelKey])
	}
}

func TestExpectedWithSurge(t *testing.T) {
	for _, tt := range []struct {
		name                               string
		replicas, maxSurge, current, surge int
		want                               int
	}{
		{"no surge", 3, 1, 3, 0, 3},
		{"vacated ordinal is recreated", 3, 1, 3, 1, 4},
		{"surge stays until it is retired", 3, 1, 4, 1, 4},
		{"regular units above replicas are an explicit scale-down", 2, 1, 4, 1, 2},
		{"reduced maxSurge stops creation", 3, 1, 4, 2, 4},
		{"reduced maxSurge does not scale down", 3, 0, 4, 1, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, expectedWithSurge(tt.replicas, tt.maxSurge, tt.current, tt.surge))
		})
	}
}

// A failing marker removal does not delay the scale-up that made the surge
// unit a regular replica; the removal is retried.
func TestAdoptionFailureDoesNotBlockScaleUp(t *testing.T) {
	c, ms, revision := newSurgeFixture(t, surgeServingGroup)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("patch", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("patch failed"))
	})
	ms.Spec.Replicas = ptr.To[int32](5)
	require.NoError(t, c.syncServingGroupReplicas(context.Background(), ms, revision))
	for _, ordinal := range []int{0, 4} {
		_, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), utils.GeneratePodName(utils.GenerateServingGroupName(ms.Name, ordinal), "inference-0", 0), metav1.GetOptions{})
		assert.NoError(t, err, "group %d is created", ordinal)
	}
}
