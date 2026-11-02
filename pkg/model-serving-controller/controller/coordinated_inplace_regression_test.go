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
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// The local budget permits three updates, while 25% coordination permits only
// one of four instances until both Roles have complete target Ready capacity.
// Keep real source labels throughout a patch: InPlace flips them on completion.
type coordinatedInPlaceFixture struct {
	controller *ModelServingController
	ms         *api.ModelServing
	group      string
	oldRev     string
	newRev     string
}

func newCoordinatedInPlaceFixture(t *testing.T) *coordinatedInPlaceFixture {
	t.Helper()
	old := createStandardModelServing("coordinated-inplace", 1, 4)
	old.UID, old.ResourceVersion = "coordinated-inplace-owner", "1"
	old.Spec.RolloutStrategy = &api.RolloutStrategy{
		Type: api.RoleRollingUpdate, UpdateMethod: api.InPlaceUpdateMethod,
		RoleCoordination: coordinationForTest("25%"),
	}
	base := old.Spec.Template.Roles[0]
	old.Spec.Template.Roles = nil
	for _, name := range []string{"prefill", "decode"} {
		role := *base.DeepCopy()
		role.Name = name
		role.WorkerReplicas = 1
		role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt(3)), ptr.To(intstr.FromInt(0))
		role.EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
		role.EntryTemplate.Spec.Containers[0].Image = "img:v1"
		role.WorkerTemplate = role.EntryTemplate.DeepCopy()
		old.Spec.Template.Roles = append(old.Spec.Template.Roles, role)
	}
	ms := old.DeepCopy()
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "img:v2"
		ms.Spec.Template.Roles[i].WorkerTemplate.Spec.Containers[0].Image = "img:v2"
	}
	f := &coordinatedInPlaceFixture{
		controller: newRevisionTestController(t, ms), ms: ms,
		group: ms.Name + "-0", oldRev: utils.ModelServingRevision(old), newRev: utils.ModelServingRevision(ms),
	}
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = f.oldRev, f.newRev
	require.NoError(t, f.controller.modelServingsInformer.GetIndexer().Update(ms))
	_, err := utils.CreateControllerRevision(context.Background(), f.controller.kubeClientSet, old, f.oldRev, old.Spec.Template.Roles)
	require.NoError(t, err)
	key := utils.GetNamespaceName(ms)
	f.controller.store.AddServingGroup(key, 0, f.oldRev)
	for _, role := range old.Spec.Template.Roles {
		for ordinal := 0; ordinal < int(*role.Replicas); ordinal++ {
			id, hash := utils.GenerateRoleID(role.Name, ordinal), utils.CalRoleTemplateHash(role)
			entry := utils.GenerateEntryPod(*role.DeepCopy(), old, f.group, id, f.oldRev, hash)
			worker := utils.GenerateWorkerPod(*role.DeepCopy(), old, entry, f.group, id, 1, f.oldRev, hash)
			for _, pod := range []*corev1.Pod{entry, worker} {
				pod.UID = types.UID(pod.Name + "-original")
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: pod.Spec.Containers[0].Name, Image: "img:v1", ImageID: "id-v1", ContainerID: "containerd://" + pod.Name + "-old",
				}}
				_, err := f.controller.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
				require.NoError(t, err)
				require.NoError(t, f.controller.podsInformer.GetIndexer().Add(pod))
				f.controller.store.AddRunningPodToServingGroup(key, f.group, pod.Name, f.oldRev, hash, role.Name, id)
			}
			require.NoError(t, f.controller.store.UpdateRoleStatus(key, f.group, role.Name, id, datastore.RoleRunning))
		}
	}
	require.NoError(t, f.controller.store.UpdateServingGroupStatus(key, f.group, datastore.ServingGroupRunning))
	return f
}

func (f *coordinatedInPlaceFixture) pod(t *testing.T, role string, ordinal, member int) *corev1.Pod {
	t.Helper()
	pod, err := f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).Get(context.Background(),
		utils.GeneratePodName(f.group, utils.GenerateRoleID(role, ordinal), member), metav1.GetOptions{})
	require.NoError(t, err)
	return pod
}

func (f *coordinatedInPlaceFixture) syncPodCache(t *testing.T) {
	t.Helper()
	pods, err := f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for i := range pods.Items {
		require.NoError(t, f.controller.podsInformer.GetIndexer().Update(&pods.Items[i]))
	}
}

func (f *coordinatedInPlaceFixture) state(t *testing.T, name string) coordinatedRoleState {
	t.Helper()
	for _, role := range f.ms.Spec.Template.Roles {
		if role.Name != name {
			continue
		}
		instances, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), f.group, name)
		require.NoError(t, err)
		return f.controller.resolveRoleRolloutState(context.Background(), f.ms,
			datastore.ServingGroup{Name: f.group, Revision: f.oldRev}, role, 4, instances, 0, true, nil, nil)
	}
	t.Fatalf("Role %s not found", name)
	return coordinatedRoleState{}
}

func (f *coordinatedInPlaceFixture) reconcileRollout(t *testing.T) {
	t.Helper()
	f.syncPodCache(t)
	require.NoError(t, f.controller.refreshCoordinatedAvailability(f.ms))
	policy, err := f.controller.resolveRoleRolloutPolicy(context.Background(), f.ms, f.newRev)
	require.NoError(t, err)
	require.NoError(t, f.controller.manageRollingUpdateWithPolicy(context.Background(), f.ms, f.newRev, policy))
	f.syncPodCache(t)
}

func (f *coordinatedInPlaceFixture) startedOrdinals(t *testing.T, role string) []int {
	t.Helper()
	var result []int
	for ordinal := 0; ordinal < 4; ordinal++ {
		for member := 0; member < 2; member++ {
			pod := f.pod(t, role, ordinal, member)
			if state := inplace.StateOf(pod); state != nil && state.Revision == f.newRev {
				result = append(result, ordinal)
				break
			}
		}
	}
	return result
}

func (f *coordinatedInPlaceFixture) restartMember(t *testing.T, role string, ordinal, member int) {
	t.Helper()
	pod := f.pod(t, role, ordinal, member)
	require.True(t, inplace.InProgress(pod))
	kubeletRestart("img:v2", "id-v2-"+pod.Name)(&pod.Status.ContainerStatuses[0])
	_, err := f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.syncPodCache(t)
}

func TestCoordinatedInPlaceReservationsLimitStarts(t *testing.T) {
	f := newCoordinatedInPlaceFixture(t)
	f.reconcileRollout(t)
	for _, role := range []string{"prefill", "decode"} {
		require.Equal(t, []int{3}, f.startedOrdinals(t, role))
		for member := 0; member < 2; member++ {
			pod := f.pod(t, role, 3, member)
			require.Equal(t, f.oldRev, utils.ObjectRevision(pod), "an image patch retains its source revision until completion")
			require.True(t, utils.IsPodRunningAndReady(pod), "kubelet still reports the pre-patch Ready condition")
			require.True(t, inplace.Updating(pod))
		}
	}

	// Retry with the actual old labels and stale Ready condition. Three local
	// unavailable slots must not grant a second 25%-coordinated target start.
	for round := 0; round < 2; round++ {
		f.reconcileRollout(t)
		for _, role := range []string{"prefill", "decode"} {
			state := f.state(t, role)
			require.Equal(t, 1, state.versionStarted)
			require.Zero(t, state.versionReady)
			require.Equal(t, targetStarted, state.targetState)
			require.Equal(t, []int{3}, f.startedOrdinals(t, role))
		}
	}

	// Once both complete instances are Ready, the same policy permits exactly
	// the next instance. This proves a reservation is released by real progress.
	for _, role := range []string{"prefill", "decode"} {
		for member := 0; member < 2; member++ {
			f.restartMember(t, role, 3, member)
		}
	}
	require.NoError(t, f.controller.completeInPlaceUpdates(context.Background(), f.ms, f.newRev))
	f.syncPodCache(t)
	f.reconcileRollout(t)
	for _, role := range []string{"prefill", "decode"} {
		require.Equal(t, []int{2, 3}, f.startedOrdinals(t, role))
		for member := 0; member < 2; member++ {
			pod := f.pod(t, role, 3, member)
			require.Equal(t, f.newRev, utils.ObjectRevision(pod))
			require.Equal(t, types.UID(pod.Name+"-original"), pod.UID)
		}
	}
}

func TestCoordinatedInPlacePartialPatchReservesUnit(t *testing.T) {
	for _, firstMember := range []int{0, 1} {
		name := "entry-first"
		if firstMember == 1 {
			name = "worker-first"
		}
		t.Run(name, func(t *testing.T) {
			f := newCoordinatedInPlaceFixture(t)
			pod := f.pod(t, "prefill", 3, firstMember)
			patched, err := inplace.Start(context.Background(), f.controller.kubeClientSet, pod,
				inplace.TargetImages(f.ms, pod), f.newRev, time.Now())
			require.NoError(t, err)
			require.True(t, patched)
			require.NoError(t, f.controller.markPodUnavailable(f.ms, f.group, pod))
			f.syncPodCache(t)
			require.NoError(t, f.controller.refreshCoordinatedAvailability(f.ms))
			state := f.state(t, "prefill")
			require.Equal(t, 1, state.versionStarted, "any patched required member reserves one complete formal instance")
			require.Zero(t, state.versionReady)

			// Finishing the other half of an admitted unit is allowed even when
			// that Role has no allowance to start an additional instance.
			f.reconcileRollout(t)
			require.Equal(t, []int{3}, f.startedOrdinals(t, "prefill"))
			for member := 0; member < 2; member++ {
				got := f.pod(t, "prefill", 3, member)
				require.True(t, inplace.InProgress(got))
				require.Equal(t, "img:v2", got.Spec.Containers[0].Image)
				require.Equal(t, f.oldRev, utils.ObjectRevision(got))
			}
		})
	}
}

func TestCoordinatedInPlaceUsesCompleteRuntimeReadiness(t *testing.T) {
	f := newCoordinatedInPlaceFixture(t)
	require.NoError(t, f.controller.updateRole(context.Background(), f.ms, f.group, "prefill", "prefill-3"))
	f.syncPodCache(t)
	check := func() {
		t.Helper()
		ready, err := f.controller.checkRoleReady(f.ms, f.group, "prefill", "prefill-3")
		require.NoError(t, err)
		require.False(t, ready)
		require.NoError(t, f.controller.refreshCoordinatedAvailability(f.ms))
		require.Equal(t, datastore.RoleCreating, f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, "prefill", "prefill-3"))
		require.Zero(t, f.state(t, "prefill").versionReady)
	}
	check()
	f.restartMember(t, "prefill", 3, 0)
	require.False(t, inplace.Updating(f.pod(t, "prefill", 3, 0)))
	require.True(t, inplace.Updating(f.pod(t, "prefill", 3, 1)))
	check()

	f.restartMember(t, "prefill", 3, 1)
	require.NoError(t, f.controller.completeInPlaceUpdates(context.Background(), f.ms, f.newRev))
	f.syncPodCache(t)
	require.NoError(t, f.controller.refreshCoordinatedAvailability(f.ms))
	ready, err := f.controller.checkRoleReady(f.ms, f.group, "prefill", "prefill-3")
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, 1, f.state(t, "prefill").versionReady)
}

func TestCoordinatedInPlaceExpectedRestartPreservesUIDs(t *testing.T) {
	f := newCoordinatedInPlaceFixture(t)
	f.controller.initialSync.Store(true)
	require.NoError(t, f.controller.updateRole(context.Background(), f.ms, f.group, "prefill", "prefill-3"))
	f.syncPodCache(t)
	pod := f.pod(t, "prefill", 3, 0)
	kubeletRestart("img:v2", "id-v2-entry")(&pod.Status.ContainerStatuses[0])
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	f.controller.updatePod(nil, pod)
	_, waitingForRecovery := f.controller.graceMap.Load(getPodGracePeriodKey(pod))
	require.False(t, waitingForRecovery, "the expected update restart is not a fault recovery")
	for member := 0; member < 2; member++ {
		got := f.pod(t, "prefill", 3, member)
		require.Nil(t, got.DeletionTimestamp)
		require.Equal(t, types.UID(got.Name+"-original"), got.UID)
	}
	require.Equal(t, datastore.RoleCreating, f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, "prefill", "prefill-3"))
}

func TestCoordinatedInPlaceFallbackAndIndependentMode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		coordination bool
		method       api.UpdateMethod
		nonImage     bool
		inPlace      bool
	}{
		{name: "independent-inplace", method: api.InPlaceUpdateMethod, inPlace: true},
		{name: "coordinated-recreate", coordination: true, method: api.RecreateUpdateMethod},
		{name: "independent-recreate", method: api.RecreateUpdateMethod},
		{name: "coordinated-non-image-fallback", coordination: true, method: api.InPlaceUpdateMethod, nonImage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordinatedInPlaceFixture(t)
			f.ms.Spec.RolloutStrategy.UpdateMethod = tc.method
			if !tc.coordination {
				f.ms.Spec.RolloutStrategy.RoleCoordination = nil
			}
			if tc.nonImage {
				f.ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CHANGED", Value: "true"}}
			}
			require.NoError(t, f.controller.updateRole(context.Background(), f.ms, f.group, "prefill", "prefill-3"))
			for member := 0; member < 2; member++ {
				pod := f.pod(t, "prefill", 3, member)
				if tc.inPlace {
					require.True(t, inplace.InProgress(pod))
					require.Equal(t, "img:v2", pod.Spec.Containers[0].Image)
				} else {
					require.Nil(t, inplace.StateOf(pod))
					require.Equal(t, "img:v1", pod.Spec.Containers[0].Image)
				}
			}
			want := datastore.RoleDeleting
			if tc.inPlace {
				want = datastore.RoleCreating
			}
			require.Equal(t, want, f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, "prefill", "prefill-3"))
		})
	}
}

func TestIndependentInPlaceRetainsLocalBudget(t *testing.T) {
	f := newCoordinatedInPlaceFixture(t)
	f.ms.Spec.RolloutStrategy.RoleCoordination = nil
	f.reconcileRollout(t)
	for _, role := range []string{"prefill", "decode"} {
		require.Equal(t, []int{1, 2, 3}, f.startedOrdinals(t, role), "without coordination the per-Role U3 budget permits three starts")
	}
}
