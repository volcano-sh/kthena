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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const inPlaceTestGroup = "ms-0"

type inPlaceFixture struct {
	controller *ModelServingController
	kubeClient *kubefake.Clientset
	oldMS      *workloadv1alpha1.ModelServing
	newMS      *workloadv1alpha1.ModelServing
	oldRev     string
	newRev     string
}

func newInPlaceModelServing(image string, mutate func(*workloadv1alpha1.ModelServing)) *workloadv1alpha1.ModelServing {
	ms := createStandardModelServing("ms", 1, 1)
	ms.UID = types.UID("ms-uid")
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = image
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:                       workloadv1alpha1.ServingGroupRollingUpdate,
		UpdateMethod:               workloadv1alpha1.InPlaceUpdateMethod,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1))},
	}
	if mutate != nil {
		mutate(ms)
	}
	return ms
}

// newInPlaceFixture stores a v1 ServingGroup whose only Pod runs oldMS, and returns
// a controller that is asked to roll it out to newMS.
func newInPlaceFixture(t *testing.T, newMutate func(*workloadv1alpha1.ModelServing), podMutate func(*corev1.Pod)) *inPlaceFixture {
	t.Helper()
	kubeClient := kubefake.NewSimpleClientset()
	controller, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	controller.store = datastore.New()

	oldMS := newInPlaceModelServing("img:v1", nil)
	newMS := newInPlaceModelServing("img:v2", newMutate)
	f := &inPlaceFixture{
		controller: controller,
		kubeClient: kubeClient,
		oldMS:      oldMS,
		newMS:      newMS,
		oldRev:     utils.ModelServingRevision(oldMS),
		newRev:     utils.ModelServingRevision(newMS),
	}

	_, err = utils.CreateControllerRevision(context.Background(), kubeClient, oldMS, f.oldRev, oldMS.Spec.Template.Roles)
	require.NoError(t, err)

	name := utils.GetNamespaceName(oldMS)
	controller.store.AddServingGroup(name, 0, f.oldRev)
	require.NoError(t, controller.store.UpdateServingGroupStatus(name, inPlaceTestGroup, datastore.ServingGroupRunning))
	// A real ServingGroup has Role records before any member can complete.
	f.addRunningRole(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "ms-0-prefill-0-0",
			UID:       "pod-uid",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "ms",
				workloadv1alpha1.GroupNameLabelKey:        inPlaceTestGroup,
				workloadv1alpha1.RoleLabelKey:             "prefill",
				workloadv1alpha1.RoleIDKey:                "prefill-0",
				workloadv1alpha1.EntryLabelKey:            utils.Entry,
				workloadv1alpha1.RevisionLabelKey:         f.oldRev,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
				Kind:       workloadv1alpha1.ModelServingKind.Kind,
				Name:       "ms",
				UID:        oldMS.UID,
			}},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers:    []corev1.Container{{Name: "prefill-container", Image: "img:v1"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "prefill-container", Image: "img:v1", ImageID: "id-v1", ContainerID: "containerd://old"}},
		},
	}
	if podMutate != nil {
		podMutate(pod)
	}
	_, err = kubeClient.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, controller.podsInformer.GetIndexer().Add(pod))
	return f
}

// syncPodFromAPI refreshes the informer cache from the fake API server.
func (f *inPlaceFixture) syncPodFromAPI(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := f.kubeClient.CoreV1().Pods("default").Get(context.Background(), "ms-0-prefill-0-0", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, f.controller.podsInformer.GetIndexer().Update(pod))
	return pod
}

// kubeletRestart simulates kubelet restarting the container on the given image.
func kubeletRestart(image, imageID string) func(*corev1.ContainerStatus) {
	return func(status *corev1.ContainerStatus) {
		status.RestartCount++
		status.ContainerID = "containerd://" + imageID
		status.Image = image
		status.ImageID = imageID
	}
}

func TestUpdateServingGroupInPlaceOrRecreate(t *testing.T) {
	tests := []struct {
		name          string
		newMutate     func(*workloadv1alpha1.ModelServing)
		podMutate     func(*corev1.Pod)
		wantInPlace   bool
		wantRecreated bool
	}{
		{
			name:        "image-only change is patched in place",
			wantInPlace: true,
		},
		{
			name: "non-image change is recreated",
			newMutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "A", Value: "B"}}
			},
			wantRecreated: true,
		},
		{
			name: "Recreate update method is recreated",
			newMutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
			},
			wantRecreated: true,
		},
		{
			name: "zero maxUnavailable is recreated",
			newMutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = ptr.To(intstr.FromInt(0))
			},
			wantRecreated: true,
		},
		{
			name:          "restartPolicy Never is recreated",
			podMutate:     func(pod *corev1.Pod) { pod.Spec.RestartPolicy = corev1.RestartPolicyNever },
			wantRecreated: true,
		},
		{
			name: "container restartPolicy OnFailure is recreated",
			podMutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyOnFailure)
			},
			wantRecreated: true,
		},
		{
			name: "container restartPolicyRules are recreated",
			podMutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].RestartPolicyRules = []corev1.ContainerRestartRule{{
					Action:    corev1.ContainerRestartRuleActionRestart,
					ExitCodes: &corev1.ContainerRestartRuleOnExitCodes{Operator: corev1.ContainerRestartRuleOnExitCodesOpIn, Values: []int32{1}},
				}}
			},
			wantRecreated: true,
		},
		{
			name: "pod of another ModelServing is ignored and the group recreated",
			podMutate: func(pod *corev1.Pod) {
				pod.OwnerReferences[0].UID = "other-uid"
			},
			wantRecreated: true,
		},
		{
			name: "container renamed by admission is recreated",
			podMutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Name = "renamed"
				pod.Status.ContainerStatuses[0].Name = "renamed"
			},
			wantRecreated: true,
		},
		{
			name: "terminating pod waits",
			podMutate: func(pod *corev1.Pod) {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, tt.newMutate, tt.podMutate)
			sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}

			require.NoError(t, f.controller.updateServingGroup(context.Background(), f.newMS, sg, f.newRev))

			status := f.controller.store.GetServingGroupStatus(utils.GetNamespaceName(f.newMS), inPlaceTestGroup)
			pod, err := f.kubeClient.CoreV1().Pods("default").Get(context.Background(), "ms-0-prefill-0-0", metav1.GetOptions{})
			require.NoError(t, err)

			assert.Equal(t, tt.wantRecreated, status == datastore.ServingGroupDeleting)
			if !tt.wantInPlace {
				assert.Equal(t, "img:v1", pod.Spec.Containers[0].Image)
				assert.Nil(t, inplace.StateOf(pod))
				return
			}
			assert.Equal(t, "img:v2", pod.Spec.Containers[0].Image)
			assert.Equal(t, f.oldRev, utils.ObjectRevision(pod), "revision label flips only on completion")
			state := inplace.StateOf(pod)
			require.NotNil(t, state)
			assert.Equal(t, f.newRev, state.Revision)
			assert.Equal(t, map[string]inplace.PatchedContainer{
				"prefill-container": {Image: "img:v2", PrevContainerID: "containerd://old", PrevImageID: "id-v1"},
			}, state.Patched)
			assert.Equal(t, map[string]int32{"prefill-container": 1}, state.Expected)
			assert.Equal(t, datastore.ServingGroupCreating, status, "group must count against the unavailable budget")
		})
	}
}

func TestCompleteInPlaceUpdates(t *testing.T) {
	tests := []struct {
		name        string
		startedAgo  time.Duration
		policy      workloadv1alpha1.RecoveryPolicy
		status      func(*corev1.ContainerStatus)
		wantFlipped bool
	}{
		{
			name:       "extra crash on the target image waits for recovery",
			startedAgo: time.Minute,
			status: func(s *corev1.ContainerStatus) {
				kubeletRestart("img:v2", "id-v2")(s)
				kubeletRestart("img:v2", "id-v2b")(s)
			},
		},
		{
			name:       "extra crash on the target image completes with recoveryPolicy None",
			startedAgo: time.Minute,
			policy:     workloadv1alpha1.NoneRestartPolicy,
			status: func(s *corev1.ContainerStatus) {
				kubeletRestart("img:v2", "id-v2")(s)
				kubeletRestart("img:v2", "id-v2b")(s)
			},
			wantFlipped: true,
		},
		{
			name:        "restarted and ready flips the revision",
			startedAgo:  time.Minute,
			status:      kubeletRestart("img:v2", "id-v2"),
			wantFlipped: true,
		},
		{
			name:        "runtime reports a normalized image name",
			startedAgo:  time.Minute,
			status:      kubeletRestart("docker.io/library/img:v2", "id-v2"),
			wantFlipped: true,
		},
		{
			name:       "stale ready without restart keeps waiting",
			startedAgo: time.Minute,
		},
		{
			name:       "crash of the old image is not completion",
			startedAgo: time.Minute,
			status:     kubeletRestart("img:v1", "id-v1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, func(ms *workloadv1alpha1.ModelServing) {
				if tt.policy != "" {
					ms.Spec.RecoveryPolicy = tt.policy
				}
			}, nil)
			ctx := context.Background()
			sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
			require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))

			// Simulate a controller restart: only Pod state survives.
			pod := f.syncPodFromAPI(t)
			state := inplace.StateOf(pod)
			state.StartedAt = metav1.NewTime(time.Now().Add(-tt.startedAgo))
			pod = pod.DeepCopy()
			pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey] = marshalState(t, state)
			if tt.status != nil {
				tt.status(&pod.Status.ContainerStatuses[0])
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			_, err := f.kubeClient.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			f.syncPodFromAPI(t)

			require.NoError(t, f.controller.completeInPlaceUpdates(ctx, f.newMS, f.newRev))

			got, err := f.kubeClient.CoreV1().Pods("default").Get(ctx, pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, tt.wantFlipped, utils.ObjectRevision(got) == f.newRev)
			assert.Equal(t, tt.wantFlipped, !inplace.InProgress(got))
			if tt.wantFlipped {
				revision, ok := f.controller.store.GetServingGroupRevision(utils.GetNamespaceName(f.newMS), inPlaceTestGroup)
				assert.True(t, ok)
				assert.Equal(t, f.newRev, revision)
				if tt.policy != workloadv1alpha1.NoneRestartPolicy {
					assert.False(t, inplace.UnexpectedRestart(got), "the expected restart must not trigger recovery")
				}
			}
		})
	}
}

func TestSurgeDisabledWhileUpdatingInPlace(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*workloadv1alpha1.ModelServing)
		wantInUse bool
	}{
		{name: "InPlace with unavailable budget", wantInUse: true},
		{
			name: "InPlace without unavailable budget keeps surge path",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = ptr.To(intstr.FromInt(0))
			},
		},
		{
			name: "Recreate keeps surge path",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, tt.mutate, nil)
			assert.Equal(t, tt.wantInUse, f.controller.updatesInPlace(f.newMS))
		})
	}
}

func TestUpdatePodDoesNotRecoverInPlaceUpdatingPod(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	ctx := context.Background()
	f.controller.initialSync.Store(true)
	require.NoError(t, f.controller.modelServingsInformer.GetIndexer().Add(f.newMS))

	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
	require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
	pod := f.syncPodFromAPI(t)
	kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}

	f.controller.updatePod(nil, pod)

	assert.Equal(t, datastore.ServingGroupCreating, f.controller.store.GetServingGroupStatus(utils.GetNamespaceName(f.newMS), inPlaceTestGroup))
	_, loaded := f.controller.graceMap.Load(getPodGracePeriodKey(pod))
	assert.False(t, loaded, "an expected restart must not enter the recovery grace period")
}

func marshalState(t *testing.T, state *inplace.State) string {
	t.Helper()
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	return string(raw)
}

func roleModeMutate(maxUnavailable int, extra func(*workloadv1alpha1.ModelServing)) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
		ms.Spec.RolloutStrategy.RollingUpdateConfiguration = nil
		ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(maxUnavailable))
		if extra != nil {
			extra(ms)
		}
	}
}

func (f *inPlaceFixture) addRunningRole(t *testing.T) {
	t.Helper()
	name := utils.GetNamespaceName(f.oldMS)
	f.controller.store.AddRole(name, inPlaceTestGroup, "prefill", "prefill-0", f.oldRev, utils.CalRoleTemplateHash(f.oldMS.Spec.Template.Roles[0]))
	require.NoError(t, f.controller.store.UpdateRoleStatus(name, inPlaceTestGroup, "prefill", "prefill-0", datastore.RoleRunning))
}

func (f *inPlaceFixture) roleStatus() datastore.RoleStatus {
	return f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.newMS), inPlaceTestGroup, "prefill", "prefill-0")
}

func TestUpdateRoleInPlaceOrRecreate(t *testing.T) {
	tests := []struct {
		name          string
		newMutate     func(*workloadv1alpha1.ModelServing)
		wantInPlace   bool
		wantRecreated bool
	}{
		{
			name:        "image-only change is patched in place",
			newMutate:   roleModeMutate(1, nil),
			wantInPlace: true,
		},
		{
			name: "non-image change is recreated",
			newMutate: roleModeMutate(1, func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "A", Value: "B"}}
			}),
			wantRecreated: true,
		},
		{
			name:          "role maxUnavailable 0 is recreated",
			newMutate:     roleModeMutate(0, nil),
			wantRecreated: true,
		},
		{
			name: "Recreate update method is recreated",
			newMutate: roleModeMutate(1, func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
			}),
			wantRecreated: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, tt.newMutate, nil)
			f.addRunningRole(t)

			require.NoError(t, f.controller.updateRole(context.Background(), f.newMS, inPlaceTestGroup, "prefill", "prefill-0", f.newRev))

			pod, err := f.kubeClient.CoreV1().Pods("default").Get(context.Background(), "ms-0-prefill-0-0", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, tt.wantRecreated, f.roleStatus() == datastore.RoleDeleting)
			if !tt.wantInPlace {
				assert.Equal(t, "img:v1", pod.Spec.Containers[0].Image)
				assert.Nil(t, inplace.StateOf(pod))
				return
			}
			assert.Equal(t, "img:v2", pod.Spec.Containers[0].Image)
			assert.Equal(t, f.oldRev, utils.ObjectRevision(pod))
			assert.True(t, inplace.InProgress(pod))
			assert.Equal(t, datastore.RoleCreating, f.roleStatus(), "role must count against its unavailable budget")
		})
	}
}

// TestManageRollingUpdateInPlaceEndToEnd drives the whole rollout through
// manageRollingUpdate: start, a stale reconcile, the container restart, and completion.
func TestManageRollingUpdateInPlaceEndToEnd(t *testing.T) {
	tests := []struct {
		name      string
		newMutate func(*workloadv1alpha1.ModelServing)
		roleMode  bool
	}{
		{name: "ServingGroupRollingUpdate"},
		{name: "RoleRollingUpdate", newMutate: roleModeMutate(1, nil), roleMode: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, tt.newMutate, nil)
			ctx := context.Background()
			name := utils.GetNamespaceName(f.newMS)
			if tt.roleMode {
				f.addRunningRole(t)
			}

			require.NoError(t, f.controller.manageRollingUpdate(ctx, f.newMS, f.newRev))
			pod := f.syncPodFromAPI(t)
			assert.Equal(t, "img:v2", pod.Spec.Containers[0].Image)
			assert.True(t, inplace.InProgress(pod))
			resourceVersion := pod.ResourceVersion

			// Nothing has restarted yet: reconciling again must neither re-patch nor flip.
			require.NoError(t, f.controller.manageRollingUpdate(ctx, f.newMS, f.newRev))
			pod = f.syncPodFromAPI(t)
			assert.Equal(t, resourceVersion, pod.ResourceVersion)
			assert.Equal(t, f.oldRev, utils.ObjectRevision(pod))

			// kubelet restarts the container and the Pod becomes Ready again.
			pod = pod.DeepCopy()
			kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			_, err := f.kubeClient.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			f.syncPodFromAPI(t)

			require.NoError(t, f.controller.manageRollingUpdate(ctx, f.newMS, f.newRev))
			pod = f.syncPodFromAPI(t)
			assert.Equal(t, f.newRev, utils.ObjectRevision(pod))
			assert.False(t, inplace.InProgress(pod))
			assert.False(t, inplace.UnexpectedRestart(pod))
			if tt.roleMode {
				hash, err := utils.RoleRevisionHash(f.newMS, "prefill")
				require.NoError(t, err)
				assert.Equal(t, hash, utils.ObjectRoleTemplateHash(pod))
				roles, err := f.controller.store.GetRoleList(name, inPlaceTestGroup, "prefill")
				require.NoError(t, err)
				require.Len(t, roles, 1)
				assert.Equal(t, f.newRev, roles[0].Revision)
			} else {
				revision, ok := f.controller.store.GetServingGroupRevision(name, inPlaceTestGroup)
				assert.True(t, ok)
				assert.Equal(t, f.newRev, revision)
			}
			assert.NotEqual(t, datastore.ServingGroupDeleting, f.controller.store.GetServingGroupStatus(name, inPlaceTestGroup))
		})
	}
}

func podDeleted(f *inPlaceFixture) bool {
	for _, action := range f.kubeClient.Actions() {
		if action.Matches("delete", "pods") {
			return true
		}
	}
	return false
}

// expireInPlaceDeadline makes the unfinished update of the fixture Pod older than its deadline.
func expireInPlaceDeadline(t *testing.T, f *inPlaceFixture) {
	t.Helper()
	pod := f.syncPodFromAPI(t).DeepCopy()
	state := inplace.StateOf(pod)
	state.StartedAt = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey] = marshalState(t, state)
	_, err := f.kubeClient.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.syncPodFromAPI(t)
}

func TestInPlaceDeadlineFollowsRecoveryPolicy(t *testing.T) {
	tests := []struct {
		name       string
		roleMode   bool
		policy     workloadv1alpha1.RecoveryPolicy
		wantDelete bool
	}{
		{name: "ServingGroup update, RoleRecreate", policy: workloadv1alpha1.RoleRecreate, wantDelete: true},
		{name: "ServingGroup update, ServingGroupRecreate", policy: workloadv1alpha1.ServingGroupRecreate, wantDelete: true},
		{name: "ServingGroup update, None", policy: workloadv1alpha1.NoneRestartPolicy},
		{name: "Role update, RoleRecreate", roleMode: true, policy: workloadv1alpha1.RoleRecreate, wantDelete: true},
		{name: "Role update, ServingGroupRecreate", roleMode: true, policy: workloadv1alpha1.ServingGroupRecreate, wantDelete: true},
		{name: "Role update, None", roleMode: true, policy: workloadv1alpha1.NoneRestartPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutate := func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
				ms.Spec.RecoveryPolicy = tt.policy
			}
			if tt.roleMode {
				mutate = roleModeMutate(1, mutate)
			}
			f := newInPlaceFixture(t, mutate, nil)
			ctx := context.Background()
			if tt.roleMode {
				f.addRunningRole(t)
				require.NoError(t, f.controller.updateRole(ctx, f.newMS, inPlaceTestGroup, "prefill", "prefill-0", f.newRev))
			} else {
				sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
				require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
			}
			expireInPlaceDeadline(t, f)

			require.NoError(t, f.controller.completeInPlaceUpdates(ctx, f.newMS, f.newRev))

			if tt.wantDelete {
				assert.Eventually(t, func() bool { return podDeleted(f) }, 5*time.Second, 20*time.Millisecond, "the stuck Pod is handled as an error Pod")
			} else {
				assert.Never(t, func() bool { return podDeleted(f) }, 300*time.Millisecond, 20*time.Millisecond, "None must leave the Pod alone")
				assert.True(t, inplace.InProgress(f.syncPodFromAPI(t)))
			}
		})
	}
}

func TestUpdatePodRecoversCrashDuringInPlaceUpdate(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	ctx := context.Background()
	f.controller.initialSync.Store(true)
	require.NoError(t, f.controller.modelServingsInformer.GetIndexer().Add(f.newMS))
	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
	require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))

	pod := f.syncPodFromAPI(t)
	// One restart for the update and one crash on top of it.
	kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
	kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	require.True(t, inplace.UnexpectedRestart(pod))

	f.controller.updatePod(nil, pod)

	assert.Eventually(t, func() bool {
		for _, action := range f.kubeClient.Actions() {
			if action.Matches("delete", "pods") {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "a crash during the update must trigger recovery")
}

func TestUpdateServingGroupRevisionLookup(t *testing.T) {
	tests := []struct {
		name          string
		prepare       func(f *inPlaceFixture)
		wantErr       bool
		wantInPlace   bool
		wantRecreated bool
	}{
		{
			name: "API error is retried, not recreated",
			prepare: func(f *inPlaceFixture) {
				f.kubeClient.PrependReactor("get", "controllerrevisions", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("etcd timeout")
				})
			},
			wantErr: true,
		},
		{
			name: "missing source revision pauses the unit",
			prepare: func(f *inPlaceFixture) {
				require.NoError(t, f.kubeClient.AppsV1().ControllerRevisions("default").Delete(context.Background(),
					utils.GenerateControllerRevisionName("ms", f.oldRev), metav1.DeleteOptions{}))
			},
			wantErr: true,
		},
		{
			name: "existing target revision with other replicas does not block the update",
			prepare: func(f *inPlaceFixture) {
				other := f.newMS.DeepCopy()
				other.Spec.Template.Roles[0].Replicas = ptr.To[int32](5)
				_, err := utils.CreateControllerRevision(context.Background(), f.kubeClient, other, f.newRev, other.Spec.Template.Roles)
				require.NoError(t, err)
			},
			wantInPlace: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, nil, nil)
			tt.prepare(f)
			sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}

			err := f.controller.updateServingGroup(context.Background(), f.newMS, sg, f.newRev)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			status := f.controller.store.GetServingGroupStatus(utils.GetNamespaceName(f.newMS), inPlaceTestGroup)
			assert.Equal(t, tt.wantRecreated, status == datastore.ServingGroupDeleting)
			pod := f.syncPodFromAPI(t)
			assert.Equal(t, tt.wantInPlace, inplace.InProgress(pod))
		})
	}
}

func TestCompleteInPlaceUpdatesRevertedToSourceRevisionRecreates(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	ctx := context.Background()
	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
	require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
	f.syncPodFromAPI(t)

	// The user reverts the spec while the Pod already runs the patched image.
	require.NoError(t, f.controller.completeInPlaceUpdates(ctx, f.oldMS, f.oldRev))

	assert.Equal(t, datastore.ServingGroupDeleting, f.controller.store.GetServingGroupStatus(utils.GetNamespaceName(f.newMS), inPlaceTestGroup))
}

func TestCompleteInPlaceUpdatesRepairsStoreAfterLostCompleteResponse(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	ctx := context.Background()
	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
	require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))

	// The Complete patch reached the API server, but the controller never recorded it.
	pod := f.syncPodFromAPI(t)
	kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
	require.NoError(t, inplace.Complete(ctx, f.kubeClient, pod, f.newRev, ""))
	f.syncPodFromAPI(t)
	revision, _ := f.controller.store.GetServingGroupRevision(utils.GetNamespaceName(f.newMS), inPlaceTestGroup)
	require.Equal(t, f.oldRev, revision)

	require.NoError(t, f.controller.completeInPlaceUpdates(ctx, f.newMS, f.newRev))

	revision, _ = f.controller.store.GetServingGroupRevision(utils.GetNamespaceName(f.newMS), inPlaceTestGroup)
	assert.Equal(t, f.newRev, revision)
}

func TestCompleteInPlaceUpdatesStillRunsAfterSwitchToRecreate(t *testing.T) {
	f := newInPlaceFixture(t, func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
	}, nil)
	ctx := context.Background()
	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
	require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
	expireInPlaceDeadline(t, f)

	switched := f.newMS.DeepCopy()
	switched.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
	require.NoError(t, f.controller.completeInPlaceUpdates(ctx, switched, f.newRev))

	assert.Eventually(t, func() bool { return podDeleted(f) }, 5*time.Second, 20*time.Millisecond)
}

func TestInPlaceRoundSharesRevisionLookups(t *testing.T) {
	tests := []struct {
		name            string
		ctx             func() context.Context
		wantReads       int
		wantSourceReads int
	}{
		{name: "units of one reconcile share the revision reads", ctx: func() context.Context { return withInPlaceRound(context.Background()) }, wantReads: 2, wantSourceReads: 1},
		{name: "without a round every unit reads again", ctx: context.Background, wantReads: 4, wantSourceReads: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, nil, nil)
			sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
			// Real reconcile has already persisted canonical source and target
			// snapshots. Measure per-round reads independently of legacy migration.
			require.NoError(t, f.kubeClient.AppsV1().ControllerRevisions("default").Delete(context.Background(),
				utils.GenerateControllerRevisionName("ms", f.oldRev), metav1.DeleteOptions{}))
			for _, ms := range []*workloadv1alpha1.ModelServing{f.oldMS, f.newMS} {
				data, err := utils.BuildRevisionData(ms)
				require.NoError(t, err)
				_, _, err = utils.RecordModelServingRevision(context.Background(), f.kubeClient, ms, data)
				require.NoError(t, err)
			}
			f.kubeClient.ClearActions()
			ctx := tt.ctx()

			require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
			require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))

			reads, sourceReads := 0, 0
			for _, action := range f.kubeClient.Actions() {
				if action.Matches("get", "controllerrevisions") {
					reads++
					if action.(k8stesting.GetAction).GetName() == utils.GenerateControllerRevisionName(f.oldMS.Name, f.oldRev) {
						sourceReads++
					}
				}
			}
			assert.Equal(t, tt.wantReads, reads)
			assert.Equal(t, tt.wantSourceReads, sourceReads)
		})
	}
}

func TestRetargetedUnitStillHitsDeadline(t *testing.T) {
	tests := []struct {
		name       string
		policy     workloadv1alpha1.RecoveryPolicy
		finished   bool
		wantDelete bool
	}{
		{name: "stuck unit, RoleRecreate", policy: workloadv1alpha1.RoleRecreate, wantDelete: true},
		{name: "stuck unit, None", policy: workloadv1alpha1.NoneRestartPolicy},
		{name: "unit that finished its own update is left alone", policy: workloadv1alpha1.RoleRecreate, finished: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
				ms.Spec.RecoveryPolicy = tt.policy
			}, nil)
			ctx := context.Background()
			sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
			require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
			if tt.finished {
				pod := f.syncPodFromAPI(t).DeepCopy()
				kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				_, err := f.kubeClient.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			expireInPlaceDeadline(t, f)

			// The spec moves on to a third revision that no rollout selects (partition).
			third := newInPlaceModelServing("img:v3", func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
				ms.Spec.RecoveryPolicy = tt.policy
			})
			thirdRev := utils.ModelServingRevision(third)
			require.NotEqual(t, f.newRev, thirdRev)

			require.NoError(t, f.controller.completeInPlaceUpdates(ctx, third, thirdRev))

			if tt.wantDelete {
				assert.Eventually(t, func() bool { return podDeleted(f) }, 5*time.Second, 20*time.Millisecond)
			} else {
				pod := f.syncPodFromAPI(t)
				assert.Equal(t, f.oldRev, utils.ObjectRevision(pod), "never completed towards the new revision")
				assert.Never(t, func() bool { return podDeleted(f) }, 300*time.Millisecond, 20*time.Millisecond)
			}
		})
	}
}

func TestRetargetedHalfPatchedUnitHitsDeadline(t *testing.T) {
	f := newInPlaceFixture(t, func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
		ms.Spec.RecoveryPolicy = workloadv1alpha1.RoleRecreate
	}, nil)
	ctx := context.Background()
	second := f.syncPodFromAPI(t).DeepCopy()
	second.Name, second.UID, second.ResourceVersion = "ms-0-prefill-0-extra", "pod-uid-2", ""
	_, err := f.kubeClient.CoreV1().Pods("default").Create(ctx, second, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.controller.podsInformer.GetIndexer().Add(second))

	// Only the first Pod was patched, and it finished; then the spec moved on to v3.
	_, err = inplace.Start(ctx, f.kubeClient, f.syncPodFromAPI(t), map[string]string{"prefill-container": "img:v2"}, f.newRev, time.Now())
	require.NoError(t, err)
	pod := f.syncPodFromAPI(t).DeepCopy()
	kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
	_, err = f.kubeClient.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	expireInPlaceDeadline(t, f)
	third := newInPlaceModelServing("img:v3", func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
		ms.Spec.RecoveryPolicy = workloadv1alpha1.RoleRecreate
	})
	f.kubeClient.ClearActions()

	require.NoError(t, f.controller.completeInPlaceUpdates(ctx, third, utils.ModelServingRevision(third)))

	_, err = f.kubeClient.CoreV1().Pods("default").Get(ctx, second.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the never patched Pod is not left behind")
}

func TestUpdateServingGroupAlreadyStartedMakesNoAPICalls(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupCreating}
	require.NoError(t, f.controller.updateServingGroup(context.Background(), f.newMS, sg, f.newRev))
	f.syncPodFromAPI(t)
	f.kubeClient.ClearActions()

	require.NoError(t, f.controller.updateServingGroup(context.Background(), f.newMS, sg, f.newRev))

	for _, action := range f.kubeClient.Actions() {
		assert.False(t, action.Matches("get", "controllerrevisions") || action.Matches("patch", "pods"),
			"unexpected %s %s", action.GetVerb(), action.GetResource().Resource)
	}
	assert.True(t, inplace.InProgress(f.syncPodFromAPI(t)))
}

func TestRoleLevelRevertRecreatesPatchedRole(t *testing.T) {
	roleHash := utils.CalRoleTemplateHash(newInPlaceModelServing("img:v1", nil).Spec.Template.Roles[0])
	f := newInPlaceFixture(t, roleModeMutate(1, nil), func(pod *corev1.Pod) {
		pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = roleHash
	})
	ctx := context.Background()
	f.addRunningRole(t)
	require.NoError(t, f.controller.updateRole(ctx, f.newMS, inPlaceTestGroup, "prefill", "prefill-0", f.newRev))
	f.syncPodFromAPI(t)

	// The prefill template goes back to v1 while another Role changes, so the overall
	// revision matches neither the Pod's label nor the first target.
	reverted := f.oldMS.DeepCopy()
	reverted.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
	other := *reverted.Spec.Template.Roles[0].DeepCopy()
	other.Name = "decode"
	reverted.Spec.Template.Roles = append(reverted.Spec.Template.Roles, other)
	revertedRev := utils.ModelServingRevision(reverted)
	require.NotEqual(t, f.oldRev, revertedRev)

	require.NoError(t, f.controller.completeInPlaceUpdates(ctx, reverted, revertedRev))

	assert.Equal(t, datastore.RoleDeleting, f.roleStatus(), "the Role still runs the abandoned image and must be recreated")
}

func TestPartiallyStartedUnitIsResumed(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	ctx := context.Background()
	second := f.syncPodFromAPI(t).DeepCopy()
	second.Name, second.UID, second.ResourceVersion = "ms-0-prefill-0-extra", "pod-uid-2", ""
	_, err := f.kubeClient.CoreV1().Pods("default").Create(ctx, second, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.controller.podsInformer.GetIndexer().Add(second))

	// Only the first Pod was patched before the second patch failed.
	first := f.syncPodFromAPI(t)
	_, err = inplace.Start(ctx, f.kubeClient, first, map[string]string{"prefill-container": "img:v2"}, f.newRev, time.Now())
	require.NoError(t, err)
	f.syncPodFromAPI(t)

	require.NoError(t, f.controller.completeInPlaceUpdates(ctx, f.newMS, f.newRev))

	got, err := f.kubeClient.CoreV1().Pods("default").Get(ctx, second.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "img:v2", got.Spec.Containers[0].Image)
	assert.True(t, inplace.InProgress(got))
}

func TestCompleteInPlaceUpdatesFailsOnUnknownStoreError(t *testing.T) {
	f := newInPlaceFixture(t, nil, nil)
	f.controller.store = datastore.New()
	require.NoError(t, f.controller.completeInPlaceUpdates(context.Background(), f.newMS, f.newRev), "no ServingGroup yet is not an error")
}

func TestImageRewrittenAfterCreationIsRecreated(t *testing.T) {
	f := newInPlaceFixture(t, nil, func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "img:webhook" })
	sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}

	require.NoError(t, f.controller.updateServingGroup(context.Background(), f.newMS, sg, f.newRev))

	assert.Equal(t, datastore.ServingGroupDeleting, f.controller.store.GetServingGroupStatus(utils.GetNamespaceName(f.newMS), inPlaceTestGroup))
	assert.Nil(t, inplace.StateOf(f.syncPodFromAPI(t)))
}

func TestHalfPatchedUnitHitsDeadline(t *testing.T) {
	tests := []struct {
		name       string
		policy     workloadv1alpha1.RecoveryPolicy
		wantDelete bool
	}{
		{name: "RoleRecreate deletes the never patched Pod", policy: workloadv1alpha1.RoleRecreate, wantDelete: true},
		{name: "None keeps resuming", policy: workloadv1alpha1.NoneRestartPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
				ms.Spec.RecoveryPolicy = tt.policy
			}, nil)
			ctx := context.Background()
			second := f.syncPodFromAPI(t).DeepCopy()
			second.Name, second.UID, second.ResourceVersion = "ms-0-prefill-0-extra", "pod-uid-2", ""
			_, err := f.kubeClient.CoreV1().Pods("default").Create(ctx, second, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, f.controller.podsInformer.GetIndexer().Add(second))
			_, err = inplace.Start(ctx, f.kubeClient, f.syncPodFromAPI(t), map[string]string{"prefill-container": "img:v2"}, f.newRev, time.Now())
			require.NoError(t, err)
			expireInPlaceDeadline(t, f)
			f.kubeClient.ClearActions()

			require.NoError(t, f.controller.completeInPlaceUpdates(ctx, f.newMS, f.newRev))

			got, err := f.kubeClient.CoreV1().Pods("default").Get(ctx, second.Name, metav1.GetOptions{})
			if tt.wantDelete {
				assert.True(t, podDeleted(f))
				assert.True(t, apierrors.IsNotFound(err))
				return
			}
			require.NoError(t, err)
			assert.False(t, podDeleted(f))
			assert.True(t, inplace.InProgress(got), "None retries the patch")
		})
	}
}

func TestGraceTimeKeepsOnlyPodsThatFinishedTheUpdate(t *testing.T) {
	tests := []struct {
		name       string
		finished   bool
		wantDelete bool
	}{
		{name: "ready on the old container instance is still deleted", wantDelete: true},
		{name: "ready after finishing the update is kept", finished: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceFixture(t, func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](1)
			}, nil)
			ctx := context.Background()
			sg := datastore.ServingGroup{Name: inPlaceTestGroup, Revision: f.oldRev, Status: datastore.ServingGroupRunning}
			require.NoError(t, f.controller.updateServingGroup(ctx, f.newMS, sg, f.newRev))
			pod := f.syncPodFromAPI(t).DeepCopy()
			if tt.finished {
				kubeletRestart("img:v2", "id-v2")(&pod.Status.ContainerStatuses[0])
				_, err := f.kubeClient.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				pod = f.syncPodFromAPI(t)
			}
			f.kubeClient.ClearActions()

			f.controller.handlePodAfterGraceTime(f.newMS, pod)

			assert.Equal(t, tt.wantDelete, podDeleted(f))
		})
	}
}
