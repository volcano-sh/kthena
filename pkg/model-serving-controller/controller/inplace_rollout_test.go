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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	inPlaceRolloutMSUID          = types.UID("inplace-rollout-uid")
	inPlaceRolloutSourceRevision = "rev-source"
	inPlaceRolloutOldImage       = "example.com/engine:v1"
	inPlaceRolloutNewImage       = "example.com/engine:v2"
)

// newInPlaceRolloutModelServing returns a ModelServing whose ServingGroups each have one entry and one worker Pod.
func newInPlaceRolloutModelServing(image string, replicas int32) *workloadv1alpha1.ModelServing {
	template := func() workloadv1alpha1.PodTemplateSpec {
		return workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "engine", Image: image}},
		}}
	}
	workerTemplate := template()
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "llama", UID: inPlaceRolloutMSUID},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To(replicas),
			SchedulerName: "volcano",
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:         workloadv1alpha1.ServingGroupRollingUpdate,
				UpdateMethod: workloadv1alpha1.InPlaceUpdateMethod,
				RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name:           "server",
				Replicas:       ptr.To[int32](1),
				EntryTemplate:  template(),
				WorkerReplicas: 1,
				WorkerTemplate: &workerTemplate,
			}}},
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
		},
	}
}

type inPlaceRolloutFixture struct {
	t          *testing.T
	ctx        context.Context
	controller *ModelServingController
	kubeClient *kubefake.Clientset
	recorder   *record.FakeRecorder
	ms         *workloadv1alpha1.ModelServing
	revision   string
}

// newInPlaceRolloutFixture returns a controller with Running ServingGroups whose Pods run the old image, and a spec with the new image.
func newInPlaceRolloutFixture(t *testing.T, replicas int32) *inPlaceRolloutFixture {
	t.Helper()
	source := newInPlaceRolloutModelServing(inPlaceRolloutOldImage, replicas)
	f := newInPlaceFixture(t, source, newInPlaceRolloutModelServing(inPlaceRolloutNewImage, replicas))

	role := source.Spec.Template.Roles[0]
	hash := utils.CalRoleTemplateHash(role)
	msKey := utils.GetNamespaceName(f.ms)
	for i := 0; i < int(replicas); i++ {
		groupName := utils.GenerateServingGroupName(f.ms.Name, i)
		roleID := utils.GenerateRoleID(role.Name, 0)
		entry := utils.GenerateEntryPod(*role.DeepCopy(), f.ms, groupName, roleID, inPlaceRolloutSourceRevision, hash)
		worker := utils.GenerateWorkerPod(*role.DeepCopy(), f.ms, entry, groupName, roleID, 1, inPlaceRolloutSourceRevision, hash)
		for _, pod := range []*corev1.Pod{entry, worker} {
			f.createRunningPod(pod)
		}
		f.controller.store.AddServingGroupAndRole(msKey, groupName, inPlaceRolloutSourceRevision, hash, role.Name, roleID)
		require.NoError(t, f.controller.store.UpdateServingGroupStatus(msKey, groupName, datastore.ServingGroupRunning))
	}
	return f
}

// newInPlaceFixture returns a controller without Pods, with source recorded as the source revision and ms as the spec.
func newInPlaceFixture(t *testing.T, source, ms *workloadv1alpha1.ModelServing) *inPlaceRolloutFixture {
	t.Helper()
	ctx := context.Background()
	kubeClient := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(ctx, kubeClient, ms, inPlaceRolloutSourceRevision, source.Spec.Template.Roles)
	require.NoError(t, err)

	controller, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(),
		volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	t.Cleanup(controller.workqueue.ShutDown)
	recorder := record.NewFakeRecorder(100)
	controller.recorder = recorder
	controller.initialSync.Store(true)

	f := &inPlaceRolloutFixture{
		t:          t,
		ctx:        ctx,
		controller: controller,
		kubeClient: kubeClient,
		recorder:   recorder,
		ms:         ms,
		revision:   utils.ModelServingRevision(ms),
	}
	f.setModelServing(ms)
	return f
}

// createRunningPod creates the Pod as the API server and kubelet would leave it: defaulted, running, and ready.
func (f *inPlaceRolloutFixture) createRunningPod(pod *corev1.Pod) {
	pod.UID = types.UID(pod.Name)
	pod.ResourceVersion = "1"
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
	}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		container.ImagePullPolicy = corev1.PullIfNotPresent
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:        container.Name,
			Image:       container.Image,
			ContainerID: "containerd://" + pod.Name + "-0",
			Ready:       true,
		})
	}
	_, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Create(f.ctx, pod, metav1.CreateOptions{})
	require.NoError(f.t, err)
	require.NoError(f.t, f.controller.podsInformer.GetIndexer().Add(pod.DeepCopy()))
}

func (f *inPlaceRolloutFixture) setModelServing(ms *workloadv1alpha1.ModelServing) {
	f.ms = ms
	require.NoError(f.t, f.controller.modelServingsInformer.GetIndexer().Update(ms))
}

// sync runs one rollout step and lets the informer cache observe the patched Pods.
func (f *inPlaceRolloutFixture) sync() error {
	err := f.controller.manageRollingUpdate(f.ctx, f.ms, f.revision)
	f.refreshCache()
	return err
}

func (f *inPlaceRolloutFixture) refreshCache() {
	pods, err := f.kubeClient.CoreV1().Pods("default").List(f.ctx, metav1.ListOptions{})
	require.NoError(f.t, err)
	for i := range pods.Items {
		require.NoError(f.t, f.controller.podsInformer.GetIndexer().Update(pods.Items[i].DeepCopy()))
	}
}

// runKubelet restarts every container of the ServingGroup whose spec image differs from the image it runs, as kubelet does.
func (f *inPlaceRolloutFixture) runKubelet(groupName string) {
	for _, pod := range f.pods(groupName) {
		f.runKubeletOn(pod)
	}
	f.refreshCache()
}

func (f *inPlaceRolloutFixture) runKubeletOn(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		status := &pod.Status.ContainerStatuses[i]
		if status.Image == pod.Spec.Containers[i].Image {
			continue
		}
		status.RestartCount++
		status.ContainerID = fmt.Sprintf("containerd://%s-%d", pod.Name, status.RestartCount)
		status.Image = pod.Spec.Containers[i].Image
	}
	_, err := f.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(f.ctx, pod, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

func (f *inPlaceRolloutFixture) pods(groupName string) []*corev1.Pod {
	pods, err := f.kubeClient.CoreV1().Pods("default").List(f.ctx, metav1.ListOptions{
		LabelSelector: workloadv1alpha1.GroupNameLabelKey + "=" + groupName,
	})
	require.NoError(f.t, err)
	result := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		result = append(result, &pods.Items[i])
	}
	require.Len(f.t, result, 2, "a ServingGroup keeps its entry and worker Pods")
	return result
}

func (f *inPlaceRolloutFixture) phases(groupName string) []inplace.Phase {
	var phases []inplace.Phase
	for _, pod := range f.pods(groupName) {
		state, err := inplace.ParseState(pod)
		require.NoError(f.t, err)
		if state == nil {
			phases = append(phases, "")
			continue
		}
		phases = append(phases, state.Phase)
	}
	return phases
}

func (f *inPlaceRolloutFixture) storeRevision(groupName string) string {
	revision, ok := f.controller.store.GetServingGroupRevision(utils.GetNamespaceName(f.ms), groupName)
	require.True(f.t, ok)
	return revision
}

func (f *inPlaceRolloutFixture) events() []string {
	var reasons []string
	for _, event := range f.eventMessages() {
		reasons = append(reasons, strings.Fields(event)[1])
	}
	return reasons
}

func (f *inPlaceRolloutFixture) eventMessages() []string {
	var events []string
	for {
		select {
		case event := <-f.recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func (f *inPlaceRolloutFixture) groupStatus(groupName string) datastore.ServingGroupStatus {
	return f.controller.store.GetServingGroupStatus(utils.GetNamespaceName(f.ms), groupName)
}

func (f *inPlaceRolloutFixture) assertRecreated(groupName, wantReason string) {
	f.t.Helper()
	assert.Equal(f.t, datastore.ServingGroupDeleting, f.groupStatus(groupName))
	var fallbacks []string
	for _, event := range f.eventMessages() {
		if strings.Contains(event, inplace.ReasonFallback) {
			fallbacks = append(fallbacks, event)
		}
	}
	require.Len(f.t, fallbacks, 1)
	assert.Contains(f.t, fallbacks[0], "ServingGroup "+groupName+" is recreated")
	assert.Contains(f.t, fallbacks[0], wantReason)
}

func TestInPlaceRolloutUpdatesServingGroupsWithinBudget(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 2)
	podUIDs := map[types.UID]bool{}
	for _, group := range []string{"llama-0", "llama-1"} {
		for _, pod := range f.pods(group) {
			podUIDs[pod.UID] = true
		}
	}

	// The highest ordinal is marked and patched in one sync, and the budget of one keeps the other untouched.
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-1"))
	assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-0"))
	for _, pod := range f.pods("llama-1") {
		assert.Equal(t, inPlaceRolloutNewImage, pod.Spec.Containers[0].Image)
		assert.Equal(t, f.revision, utils.ObjectRevision(pod))
		assert.Equal(t, utils.CalRoleTemplateHash(f.ms.Spec.Template.Roles[0]), utils.ObjectRoleTemplateHash(pod))
		state := inplace.MatchingState(pod)
		require.NotNil(t, state)
		assert.Equal(t, inPlaceRolloutSourceRevision, state.SourceRevision)
		assert.Equal(t, "llama-1", state.Unit)
		assert.Equal(t, map[string]inplace.PatchedContainer{"engine": {Image: inPlaceRolloutNewImage}}, state.Patched)
	}
	assert.Equal(t, inPlaceRolloutSourceRevision, f.storeRevision("llama-1"), "a unit in update stays on its source revision")
	assert.Equal(t, []string{"InPlaceUpdateStarted", "InPlaceUpdatePatched"}, f.events())

	// Until kubelet restarts the containers, the unit waits and keeps its slot.
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-1"))
	assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-0"))

	f.runKubelet("llama-1")
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone}, f.phases("llama-1"))
	for _, pod := range f.pods("llama-1") {
		assert.Equal(t, inplace.ContainerBaseline{ContainerID: "containerd://" + pod.Name + "-1", RestartCount: 1},
			inplace.MatchingState(pod).Baseline["engine"], "the image-change restart moves into the baseline")
	}
	assert.Equal(t, f.revision, f.storeRevision("llama-1"))
	roles, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), "llama-1", "server")
	require.NoError(t, err)
	require.Len(t, roles, 1)
	assert.Equal(t, f.revision, roles[0].Revision, "the Role moves to the target revision with its ServingGroup")
	assert.Equal(t, utils.CalRoleTemplateHash(f.ms.Spec.Template.Roles[0]), roles[0].RoleTemplateHash)
	assert.Equal(t, []string{"InPlaceUpdateCompleted"}, f.events())

	// The completed unit is Running again, so the next unit starts.
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
	f.runKubelet("llama-0")
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone}, f.phases("llama-0"))
	assert.Equal(t, f.revision, f.storeRevision("llama-0"))

	for _, group := range []string{"llama-0", "llama-1"} {
		for _, pod := range f.pods(group) {
			assert.True(t, podUIDs[pod.UID], "pod %s was replaced", pod.Name)
		}
	}
	patches := f.podPatches()
	require.NoError(t, f.sync())
	assert.Equal(t, patches, f.podPatches(), "a finished rollout patches nothing")
}

func (f *inPlaceRolloutFixture) podPatches() int {
	count := 0
	for _, action := range f.kubeClient.Actions() {
		if action.Matches("patch", "pods") {
			count++
		}
	}
	return count
}

func TestInPlaceRolloutPartition(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 2)
	ms := f.ms.DeepCopy()
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt32(1))
	f.setModelServing(ms)

	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-1"))

	// Raising the partition does not stop a unit that has started.
	ms = f.ms.DeepCopy()
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt32(2))
	f.setModelServing(ms)
	f.runKubelet("llama-1")
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone}, f.phases("llama-1"))

	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-0"), "partition protects units that have not started")
}

func TestInPlaceRolloutStartsUnavailableUnitsFirst(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 3)
	require.NoError(t, f.controller.store.UpdateServingGroupStatus(utils.GetNamespaceName(f.ms), "llama-0", datastore.ServingGroupCreating))

	// llama-0 is already unavailable, so it uses the only slot.
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
	assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-1"))
	assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-2"))
}

func TestInPlaceRolloutCountsMissingServingGroups(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *inPlaceRolloutFixture)
	}{
		{
			name: "a ServingGroup is being deleted",
			setup: func(f *inPlaceRolloutFixture) {
				require.NoError(t, f.controller.store.UpdateServingGroupStatus(utils.GetNamespaceName(f.ms), "llama-0", datastore.ServingGroupDeleting))
			},
		},
		{
			name: "a ServingGroup is not created yet",
			setup: func(f *inPlaceRolloutFixture) {
				ms := f.ms.DeepCopy()
				ms.Spec.Replicas = ptr.To[int32](4)
				f.setModelServing(ms)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 3)
			tt.setup(f)

			require.NoError(t, f.sync())
			assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-1"))
			assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-2"), "the only slot is already used")
		})
	}
}

func TestInPlaceRolloutFallsBackWhenMaxUnavailableIsZero(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 2)
	ms := f.ms.DeepCopy()
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = ptr.To(intstr.FromString("10%"))
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = ptr.To(intstr.FromInt32(1))
	f.setModelServing(ms)

	require.NoError(t, f.sync())
	for _, group := range []string{"llama-0", "llama-1"} {
		assert.Equal(t, []inplace.Phase{"", ""}, f.phases(group), "an in-place update would take a ServingGroup out of service")
		assert.Equal(t, datastore.ServingGroupRunning, f.groupStatus(group), "Recreate waits for the surge ServingGroup")
	}
	assert.Equal(t, []string{inplace.ReasonFallback}, f.events())

	require.NoError(t, f.controller.syncServingGroupReplicas(f.ctx, f.ms, f.revision))
	groups, err := f.controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(f.ms))
	require.NoError(t, err)
	assert.Len(t, groups, 3, "Recreate surges")

	require.NoError(t, f.sync())
	assert.NotContains(t, f.events(), inplace.ReasonFallback, "the reason is reported once")
}

func TestInPlaceRolloutCountsSurgeServingGroups(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 2)
	msKey := utils.GetNamespaceName(f.ms)
	f.controller.store.AddServingGroupAndRole(msKey, "llama-2", f.revision, utils.CalRoleTemplateHash(f.ms.Spec.Template.Roles[0]), "server", "server-0")
	require.NoError(t, f.controller.store.UpdateServingGroupStatus(msKey, "llama-2", datastore.ServingGroupRunning))

	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-1"))
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
}

func TestInPlaceRolloutRecreatesIneligibleUnits(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(f *inPlaceRolloutFixture)
		wantReason string
	}{
		{
			name: "live image differs from the source revision",
			setup: func(f *inPlaceRolloutFixture) {
				pod := f.pods("llama-1")[0]
				pod.Spec.Containers[0].Image = "example.com/engine:hotfix"
				_, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Update(f.ctx, pod, metav1.UpdateOptions{})
				require.NoError(f.t, err)
				f.refreshCache()
			},
			wantReason: inplace.ReasonUnexplainedImageDrift,
		},
		{
			name: "ControllerRevision of the source revision is missing",
			setup: func(f *inPlaceRolloutFixture) {
				require.NoError(f.t, f.kubeClient.AppsV1().ControllerRevisions("default").Delete(f.ctx,
					utils.GenerateControllerRevisionName(f.ms.Name, inPlaceRolloutSourceRevision), metav1.DeleteOptions{}))
			},
			wantReason: inplace.ReasonRevisionHistoryMissing,
		},
		{
			name: "source revision differs in more than images",
			setup: func(f *inPlaceRolloutFixture) {
				ms := f.ms.DeepCopy()
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Args = []string{"--new-flag"}
				f.setModelServing(ms)
				f.revision = utils.ModelServingRevision(ms)
			},
			wantReason: inplace.ReasonIneligible,
		},
		{
			name: "target restartPolicy is not Always",
			setup: func(f *inPlaceRolloutFixture) {
				ms := f.ms.DeepCopy()
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
				f.setModelServing(ms)
				f.revision = utils.ModelServingRevision(ms)
			},
			wantReason: inplace.ReasonIneligible,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 2)
			tt.setup(f)

			require.NoError(t, f.sync())
			assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-1"), "a unit that cannot be updated in place is never marked")
			f.assertRecreated("llama-1", tt.wantReason)
			assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-0"), "the recreated unit uses the only slot")
			assert.Equal(t, datastore.ServingGroupRunning, f.groupStatus("llama-0"))

			require.NoError(t, f.sync())
			assert.NotContains(t, f.events(), inplace.ReasonFallback, "a ServingGroup is recreated once")
		})
	}
}

func TestInPlaceRolloutSurgesForRecreatedUnits(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(f *inPlaceRolloutFixture)
		wantCount int
	}{
		{
			name:      "every outdated ServingGroup can be updated in place",
			wantCount: 2,
		},
		{
			name: "an outdated ServingGroup is recreated",
			setup: func(f *inPlaceRolloutFixture) {
				pod := f.pods("llama-1")[0]
				pod.Spec.Containers[0].Image = "example.com/engine:hotfix"
				_, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Update(f.ctx, pod, metav1.UpdateOptions{})
				require.NoError(f.t, err)
				f.refreshCache()
			},
			wantCount: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 2)
			ms := f.ms.DeepCopy()
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = ptr.To(intstr.FromInt32(1))
			f.setModelServing(ms)
			if tt.setup != nil {
				tt.setup(f)
			}

			require.NoError(t, f.controller.syncServingGroupReplicas(f.ctx, f.ms, f.revision))
			groups, err := f.controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(f.ms))
			require.NoError(t, err)
			assert.Len(t, groups, tt.wantCount)
		})
	}
}

// TestInPlaceRolloutResumesAfterCrash starts from a unit whose entry Pod was patched before the controller stopped.
func TestInPlaceRolloutResumesAfterCrash(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	workerName := utils.GeneratePodName("llama-0", utils.GenerateRoleID("server", 0), 1)
	failWorkerPatch := true
	f.kubeClient.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		patch := action.(ktesting.PatchAction)
		if failWorkerPatch && patch.GetName() == workerName && strings.Contains(string(patch.GetPatch()), `"replace"`) {
			return true, nil, fmt.Errorf("connection lost")
		}
		return false, nil, nil
	})

	require.Error(t, f.sync())
	assert.ElementsMatch(t, []inplace.Phase{inplace.PhasePatched, inplace.PhaseDraining}, f.phases("llama-0"))
	entry := f.pods("llama-0")[0]
	entryPatchedAt := inplace.MatchingState(entry).PatchedAt

	failWorkerPatch = false
	require.NoError(t, f.controller.store.UpdateServingGroupRevision(utils.GetNamespaceName(f.ms), "llama-0", f.revision))
	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
	assert.Equal(t, entryPatchedAt, inplace.MatchingState(f.pods("llama-0")[0]).PatchedAt, "the patched entry Pod is not patched again")
	assert.Equal(t, inPlaceRolloutSourceRevision, f.storeRevision("llama-0"), "the store follows the unit's source revision")
}

func TestInPlaceRolloutRejectsStaleCache(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	// The Pod changed after the informer cache observed it.
	pod := f.pods("llama-0")[0]
	pod.ResourceVersion = "2"
	_, err := f.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(f.ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Error(t, f.controller.manageRollingUpdate(f.ctx, f.ms, f.revision))
	stale, err := f.kubeClient.CoreV1().Pods(pod.Namespace).Get(f.ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, inplace.MatchingState(stale), "a pod read from a stale cache is never marked")
}

func TestInPlaceRolloutFallsBackForUnsupportedConfigurations(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(ms *workloadv1alpha1.ModelServing)
		wantEvents []string
		wantStatus datastore.ServingGroupStatus
	}{
		{
			name: "plugin without in-place support",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: plugins.DemoPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn}}
			},
			wantEvents: []string{inplace.ReasonFallback},
			wantStatus: datastore.ServingGroupDeleting,
		},
		{
			name: "plugin that is not registered",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "unknown", Type: workloadv1alpha1.PluginTypeBuiltIn}}
			},
			wantEvents: []string{inplace.ReasonFallback},
			wantStatus: datastore.ServingGroupDeleting,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 1)
			ms := f.ms.DeepCopy()
			tt.mutate(ms)
			f.setModelServing(ms)
			f.revision = utils.ModelServingRevision(ms)

			require.NoError(t, f.sync())
			assert.Equal(t, []inplace.Phase{"", ""}, f.phases("llama-0"))
			assert.Equal(t, tt.wantStatus, f.groupStatus("llama-0"), "Recreate replaces the outdated resources")
			assert.Equal(t, tt.wantEvents, f.events())

			require.NoError(t, f.sync())
			assert.NotContains(t, f.events(), inplace.ReasonFallback, "the reason is reported once")
		})
	}
}

func TestInPlaceRolloutWithSupportedPlugin(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	ms := f.ms.DeepCopy()
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: plugins.LWSLabelsPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn}}
	f.setModelServing(ms)
	f.revision = utils.ModelServingRevision(ms)

	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
	assert.NotContains(t, f.events(), inplace.ReasonFallback)
}

// TestRecreateReplacesUnfinishedInPlaceUpdate switches to Recreate while a unit is in update.
func TestRecreateReplacesUnfinishedInPlaceUpdate(t *testing.T) {
	tests := []struct {
		name string
		// revert moves the spec back to the revision the update started from.
		revert bool
		// storeRevision is the store revision of the unit in update before the switch is synced.
		storeRevision func(f *inPlaceRolloutFixture) string
	}{
		{
			name:          "after the store is rebuilt from the Pod labels",
			storeRevision: func(f *inPlaceRolloutFixture) string { return f.revision },
		},
		{
			name:          "after a revert to the source revision",
			revert:        true,
			storeRevision: func(*inPlaceRolloutFixture) string { return inPlaceRolloutSourceRevision },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 2)
			require.NoError(t, f.sync())
			require.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-1"))

			ms := f.ms.DeepCopy()
			ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
			f.setModelServing(ms)
			revision := f.revision
			if tt.revert {
				revision = inPlaceRolloutSourceRevision
			}
			msKey := utils.GetNamespaceName(f.ms)
			require.NoError(t, f.controller.store.UpdateServingGroupRevision(msKey, "llama-1", tt.storeRevision(f)))

			require.NoError(t, f.controller.syncInPlaceUnitRevisions(f.ms, revision))
			groups, err := f.controller.store.GetServingGroupByModelServing(msKey)
			require.NoError(t, err)
			assert.True(t, hasUpdateableOutdatedServingGroup(groups, revision, 0), "the unit in update is outdated, so maxSurge applies")
			unitRevision, _ := f.controller.store.GetServingGroupRevision(msKey, "llama-1")
			assert.NotEqual(t, revision, unitRevision)

			require.NoError(t, f.controller.manageRollingUpdate(f.ctx, f.ms, revision))
			assert.Equal(t, datastore.ServingGroupDeleting, f.controller.store.GetServingGroupStatus(msKey, "llama-1"))
			assert.Equal(t, datastore.ServingGroupRunning, f.controller.store.GetServingGroupStatus(msKey, "llama-0"))
		})
	}
}

func TestRecreateDoesNotExemptUnfinishedInPlaceUpdate(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	require.NoError(t, f.sync())
	ms := f.ms.DeepCopy()
	ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
	f.setModelServing(ms)

	// Kubelet has stopped the container to apply the new image, so the pod is not ready and has not restarted yet.
	pod := f.pods("llama-0")[0]
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse}}
	require.True(t, inplace.InUpdateWindow(pod))
	f.controller.updatePod(nil, pod)
	msKey := utils.GetNamespaceName(f.ms)
	assert.Equal(t, datastore.ServingGroupRunning, f.controller.store.GetServingGroupStatus(msKey, "llama-0"),
		"under Recreate, the Pod is handled as any other Pod that is not ready")
	assert.Zero(t, f.controller.restartGracePeriod(f.ms, pod), "only InPlace extends the recovery grace period")
}

func TestInPlaceRolloutSurgesRoles(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(f *inPlaceRolloutFixture, role *workloadv1alpha1.Role)
		wantRoles int
	}{
		{
			name:      "every outdated Role instance can be updated in place",
			wantRoles: 1,
		},
		{
			name: "an outdated Role instance is recreated",
			mutate: func(f *inPlaceRolloutFixture, role *workloadv1alpha1.Role) {
				role.EntryTemplate.Spec.Containers[0].Args = []string{"--new-flag"}
			},
			wantRoles: 2,
		},
		{
			name: "maxUnavailable of the Role resolves to 0",
			mutate: func(f *inPlaceRolloutFixture, role *workloadv1alpha1.Role) {
				role.MaxUnavailable = ptr.To(intstr.FromInt32(0))
			},
			wantRoles: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 1)
			ms := f.ms.DeepCopy()
			ms.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration = nil
			ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt32(1))
			if tt.mutate != nil {
				tt.mutate(f, &ms.Spec.Template.Roles[0])
			}
			f.setModelServing(ms)
			f.revision = utils.ModelServingRevision(ms)

			f.controller.manageRoleReplicasPerGroup(f.ctx, f.ms, "llama-0", f.ms.Spec.Template.Roles[0], 0, f.revision, f.controller.newRoleSurgeRollout(f.ms))
			roles, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), "llama-0", "server")
			require.NoError(t, err)
			assert.Len(t, roles, tt.wantRoles)
		})
	}
}

// TestInPlaceRoleSurgeSharesRevisionLookups checks that one sync reads a source revision once for all ServingGroups.
func TestInPlaceRoleSurgeSharesRevisionLookups(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 2)
	ms := f.ms.DeepCopy()
	ms.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration = nil
	ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt32(1))
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Args = []string{"--new-flag"}
	f.setModelServing(ms)
	f.revision = utils.ModelServingRevision(ms)
	f.kubeClient.ClearActions()

	require.NoError(t, f.controller.syncRoleReplicas(f.ctx, f.ms, f.revision))
	gets := 0
	for _, action := range f.kubeClient.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "controllerrevisions" {
			gets++
		}
	}
	assert.Equal(t, 1, gets)
	for _, group := range []string{"llama-0", "llama-1"} {
		roles, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), group, "server")
		require.NoError(t, err)
		assert.Len(t, roles, 2, "ServingGroup %s surges its recreated Role", group)
	}
}

func TestUpdatePodEnqueuesInPlaceUpdate(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	require.NoError(t, f.sync())
	for f.controller.workqueue.Len() > 0 {
		item, _ := f.controller.workqueue.Get()
		f.controller.workqueue.Done(item)
	}

	// Kubelet has stopped the container to apply the new image, so the pod is not ready and has not restarted yet.
	pod := f.pods("llama-0")[0]
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse}}
	f.controller.updatePod(nil, pod)
	require.Equal(t, 1, f.controller.workqueue.Len(), "a pod event moves its in-place update forward")
	item, _ := f.controller.workqueue.Get()
	assert.Equal(t, "default/llama", item)
}

func (f *inPlaceRolloutFixture) startUpdateAt(groupName string, startedAt time.Time) {
	for _, pod := range f.pods(groupName) {
		state := inplace.MatchingState(pod)
		require.NotNil(f.t, state)
		state.StartedAt = metav1.NewTime(startedAt)
		value, err := state.Encode()
		require.NoError(f.t, err)
		pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey] = value
		_, err = f.kubeClient.CoreV1().Pods(pod.Namespace).Update(f.ctx, pod, metav1.UpdateOptions{})
		require.NoError(f.t, err)
	}
	f.refreshCache()
}

func (f *inPlaceRolloutFixture) remainingPods(groupName string) []string {
	pods, err := f.kubeClient.CoreV1().Pods("default").List(f.ctx, metav1.ListOptions{
		LabelSelector: workloadv1alpha1.GroupNameLabelKey + "=" + groupName,
	})
	require.NoError(f.t, err)
	var names []string
	for i := range pods.Items {
		names = append(names, pods.Items[i].Name)
	}
	return names
}

func TestInPlaceRolloutProgressDeadline(t *testing.T) {
	entry := utils.GeneratePodName("llama-0", utils.GenerateRoleID("server", 0), 0)
	worker := utils.GeneratePodName("llama-0", utils.GenerateRoleID("server", 0), 1)
	tests := []struct {
		name string
		// deadlineSeconds sets rolloutStrategy.progressDeadlineSeconds, or keeps the default when 0.
		deadlineSeconds int32
		// startedAgo is how long before the sync the update of the unit started.
		startedAgo time.Duration
		// prepare runs after the unit is patched and before its start time is moved.
		prepare       func(f *inPlaceRolloutFixture)
		wantRemaining []string
		wantPhases    []inplace.Phase
		wantEvent     bool
	}{
		{
			name:          "unit waiting for kubelet within the default deadline is kept",
			startedAgo:    29 * time.Minute,
			wantRemaining: []string{entry, worker},
			wantPhases:    []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched},
		},
		{
			name:       "unverified pods are deleted after the default deadline",
			startedAgo: 31 * time.Minute,
			wantEvent:  true,
		},
		{
			name:            "configured deadline",
			deadlineSeconds: 60,
			startedAgo:      2 * time.Minute,
			wantEvent:       true,
		},
		{
			name:       "verified unit completes after the deadline",
			startedAgo: time.Hour,
			prepare: func(f *inPlaceRolloutFixture) {
				f.runKubelet("llama-0")
			},
			wantRemaining: []string{entry, worker},
			wantPhases:    []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone},
		},
		{
			name:       "only unverified pods are deleted",
			startedAgo: time.Hour,
			prepare: func(f *inPlaceRolloutFixture) {
				for _, pod := range f.pods("llama-0") {
					if pod.Name == entry {
						f.runKubeletOn(pod)
					}
				}
				f.refreshCache()
			},
			wantRemaining: []string{entry},
			wantEvent:     true,
		},
		{
			name:       "RecoveryPolicy None keeps unverified pods",
			startedAgo: time.Hour,
			prepare: func(f *inPlaceRolloutFixture) {
				ms := f.ms.DeepCopy()
				ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				f.setModelServing(ms)
			},
			wantRemaining: []string{entry, worker},
			wantPhases:    []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone},
			wantEvent:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 1)
			if tt.deadlineSeconds > 0 {
				ms := f.ms.DeepCopy()
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To(tt.deadlineSeconds)
				f.setModelServing(ms)
			}
			require.NoError(t, f.sync())
			require.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
			if tt.prepare != nil {
				tt.prepare(f)
			}
			f.startUpdateAt("llama-0", time.Now().Add(-tt.startedAgo))
			f.events()

			require.NoError(t, f.sync())
			assert.ElementsMatch(t, tt.wantRemaining, f.remainingPods("llama-0"))
			if tt.wantPhases != nil {
				assert.Equal(t, tt.wantPhases, f.phases("llama-0"))
			}
			if tt.wantEvent {
				assert.Contains(t, f.events(), inplace.ReasonProgressDeadlineExceeded)
				pods, err := f.kubeClient.CoreV1().Pods("default").List(f.ctx, metav1.ListOptions{})
				require.NoError(t, err)
				for i := range pods.Items {
					assert.False(t, inplace.InUpdateWindow(&pods.Items[i]), "pod %s that is kept completes its update", pods.Items[i].Name)
				}
			} else {
				assert.NotContains(t, f.events(), inplace.ReasonProgressDeadlineExceeded)
			}
		})
	}
}

// TestInPlaceRolloutDeadlineEndsUpdate checks that Pods recreated after the deadline are not deleted again.
func TestInPlaceRolloutDeadlineEndsUpdate(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 2)
	require.NoError(t, f.sync())
	require.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-1"))

	// The unit becomes protected by the partition while it is in update, and only its entry Pod restarts on the new image.
	ms := f.ms.DeepCopy()
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt32(2))
	f.setModelServing(ms)
	entry := utils.GeneratePodName("llama-1", utils.GenerateRoleID("server", 0), 0)
	for _, pod := range f.pods("llama-1") {
		if pod.Name == entry {
			f.runKubeletOn(pod)
		}
	}
	f.refreshCache()
	f.startUpdateAt("llama-1", time.Now().Add(-time.Hour))
	f.events()

	require.NoError(t, f.sync())
	assert.Equal(t, []string{entry}, f.remainingPods("llama-1"))
	assert.Equal(t, inplace.PhaseDone, inplace.MatchingState(f.pods1(entry)).Phase, "the verified Pod completes its update")
	assert.Equal(t, f.revision, f.storeRevision("llama-1"), "the remaining Pods are on the target revision")

	// The recovered worker Pod starts from the source revision, as a protected ServingGroup could recreate it.
	source := newInPlaceRolloutModelServing(inPlaceRolloutOldImage, 2)
	role := source.Spec.Template.Roles[0]
	roleID := utils.GenerateRoleID(role.Name, 0)
	entryPod := utils.GenerateEntryPod(*role.DeepCopy(), f.ms, "llama-1", roleID, inPlaceRolloutSourceRevision, utils.CalRoleTemplateHash(role))
	f.createRunningPod(utils.GenerateWorkerPod(*role.DeepCopy(), f.ms, entryPod, "llama-1", roleID, 1, inPlaceRolloutSourceRevision, utils.CalRoleTemplateHash(role)))
	f.events()

	for i := 0; i < 3; i++ {
		require.NoError(t, f.sync())
	}
	assert.Len(t, f.remainingPods("llama-1"), 2, "the recovered Pod is not deleted again")
	assert.Equal(t, []inplace.Phase{inplace.PhaseDone, ""}, f.phases("llama-1"), "the partition keeps the recovered Pod out of a new update")
	assert.NotContains(t, f.events(), inplace.ReasonProgressDeadlineExceeded)
}

// TestInPlaceRolloutDeadlineRecreatesUnpatchedUnit checks that RecoveryPolicy None does not restart the update of a Pod that was never patched.
func TestInPlaceRolloutDeadlineRecreatesUnpatchedUnit(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	ms := f.ms.DeepCopy()
	ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
	f.setModelServing(ms)
	workerName := utils.GeneratePodName("llama-0", utils.GenerateRoleID("server", 0), 1)
	f.kubeClient.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		patch := action.(ktesting.PatchAction)
		if patch.GetName() == workerName && strings.Contains(string(patch.GetPatch()), `"replace"`) {
			return true, nil, fmt.Errorf("patch rejected")
		}
		return false, nil, nil
	})

	require.Error(t, f.sync())
	require.ElementsMatch(t, []inplace.Phase{inplace.PhasePatched, inplace.PhaseDraining}, f.phases("llama-0"))
	f.startUpdateAt("llama-0", time.Now().Add(-time.Hour))
	f.events()

	require.NoError(t, f.sync())
	f.assertRecreated("llama-0", inplace.ReasonProgressDeadlineExceeded)
	assert.Equal(t, inplace.PhaseDraining, inplace.MatchingState(f.pods1(workerName)).Phase, "the unpatched Pod is not completed")
}

func TestInPlaceRolloutRecreatesUnitInUpdate(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(f *inPlaceRolloutFixture)
		wantReason string
	}{
		{
			name: "the target changes during the update",
			setup: func(f *inPlaceRolloutFixture) {
				ms := f.ms.DeepCopy()
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v3"
				ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "example.com/engine:v3"
				f.setModelServing(ms)
				f.revision = utils.ModelServingRevision(ms)
			},
			wantReason: inplace.ReasonRetargetNotSupported,
		},
		{
			name: "a Pod belongs to no target template",
			setup: func(f *inPlaceRolloutFixture) {
				role := f.ms.Spec.Template.Roles[0]
				role.Name = "retired"
				f.createRunningPod(utils.GenerateEntryPod(*role.DeepCopy(), f.ms, "llama-0", utils.GenerateRoleID(role.Name, 0), f.revision, utils.CalRoleTemplateHash(role)))
			},
			wantReason: "belongs to no Role template",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRolloutFixture(t, 1)
			require.NoError(t, f.sync())
			require.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
			f.events()
			tt.setup(f)

			require.NoError(t, f.sync())
			f.assertRecreated("llama-0", tt.wantReason)
		})
	}
}

func TestInPlaceRolloutUpdatesTwice(t *testing.T) {
	f := newInPlaceRolloutFixture(t, 1)
	require.NoError(t, f.sync())
	f.runKubelet("llama-0")
	require.NoError(t, f.sync())
	require.Equal(t, []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone}, f.phases("llama-0"))
	cr, err := utils.GetControllerRevision(f.ctx, f.kubeClient, f.ms, f.revision)
	require.NoError(t, err)
	assert.NotNil(t, cr, "the target revision is recorded, although no ServingGroup was created on it")

	ms := f.ms.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v3"
	ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "example.com/engine:v3"
	f.setModelServing(ms)
	f.revision = utils.ModelServingRevision(ms)
	f.events()

	require.NoError(t, f.sync())
	assert.Equal(t, []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}, f.phases("llama-0"))
	assert.NotContains(t, f.events(), inplace.ReasonFallback)
}

func (f *inPlaceRolloutFixture) pods1(name string) *corev1.Pod {
	pod, err := f.kubeClient.CoreV1().Pods("default").Get(f.ctx, name, metav1.GetOptions{})
	require.NoError(f.t, err)
	return pod
}

const inPlaceRoleGroup = "llama-0"

// newInPlaceRoleModelServing returns a ModelServing under RoleRollingUpdate with one ServingGroup, two prefill instances and one decode instance.
func newInPlaceRoleModelServing(prefillImage, decodeImage string) *workloadv1alpha1.ModelServing {
	ms := newInPlaceRolloutModelServing(prefillImage, 1)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:         workloadv1alpha1.RoleRollingUpdate,
		UpdateMethod: workloadv1alpha1.InPlaceUpdateMethod,
	}
	prefill := *ms.Spec.Template.Roles[0].DeepCopy()
	prefill.Name = "prefill"
	prefill.Replicas = ptr.To[int32](2)
	prefill.MaxUnavailable = ptr.To(intstr.FromInt32(1))
	decode := *prefill.DeepCopy()
	decode.Name = "decode"
	decode.Replicas = ptr.To[int32](1)
	decode.MaxUnavailable = nil
	decode.EntryTemplate.Spec.Containers[0].Image = decodeImage
	decode.WorkerTemplate.Spec.Containers[0].Image = decodeImage
	ms.Spec.Template.Roles = []workloadv1alpha1.Role{prefill, decode}
	return ms
}

// newInPlaceRoleFixture returns Running Role instances on the old images, and a spec that changes only the prefill image.
// The decode Pods carry decodeRevision, which may differ from the recorded source revision.
func newInPlaceRoleFixture(t *testing.T, decodeRevision string) *inPlaceRolloutFixture {
	t.Helper()
	source := newInPlaceRoleModelServing(inPlaceRolloutOldImage, inPlaceRolloutOldImage)
	f := newInPlaceFixture(t, source, newInPlaceRoleModelServing(inPlaceRolloutNewImage, inPlaceRolloutOldImage))
	msKey := utils.GetNamespaceName(f.ms)
	for _, role := range source.Spec.Template.Roles {
		revision := inPlaceRolloutSourceRevision
		if role.Name == "decode" {
			revision = decodeRevision
		}
		hash := utils.CalRoleTemplateHash(role)
		for i := 0; i < int(*role.Replicas); i++ {
			roleID := utils.GenerateRoleID(role.Name, i)
			entry := utils.GenerateEntryPod(*role.DeepCopy(), f.ms, inPlaceRoleGroup, roleID, revision, hash)
			worker := utils.GenerateWorkerPod(*role.DeepCopy(), f.ms, entry, inPlaceRoleGroup, roleID, 1, revision, hash)
			f.createRunningPod(entry)
			f.createRunningPod(worker)
			f.controller.store.AddServingGroupAndRole(msKey, inPlaceRoleGroup, inPlaceRolloutSourceRevision, hash, role.Name, roleID)
			require.NoError(t, f.controller.store.UpdateRoleStatus(msKey, inPlaceRoleGroup, role.Name, roleID, datastore.RoleRunning))
		}
	}
	require.NoError(t, f.controller.store.UpdateServingGroupStatus(msKey, inPlaceRoleGroup, datastore.ServingGroupRunning))
	return f
}

func (f *inPlaceRolloutFixture) rolePods(roleID string) []*corev1.Pod {
	pods, err := f.kubeClient.CoreV1().Pods("default").List(f.ctx, metav1.ListOptions{
		LabelSelector: workloadv1alpha1.RoleIDKey + "=" + roleID,
	})
	require.NoError(f.t, err)
	result := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		result = append(result, &pods.Items[i])
	}
	return result
}

func (f *inPlaceRolloutFixture) rolePhases(roleID string) []inplace.Phase {
	var phases []inplace.Phase
	for _, pod := range f.rolePods(roleID) {
		state, err := inplace.ParseState(pod)
		require.NoError(f.t, err)
		if state == nil {
			phases = append(phases, "")
			continue
		}
		phases = append(phases, state.Phase)
	}
	return phases
}

func (f *inPlaceRolloutFixture) runKubeletOnRole(roleID string) {
	for _, pod := range f.rolePods(roleID) {
		f.runKubeletOn(pod)
	}
	f.refreshCache()
}

func (f *inPlaceRolloutFixture) storeRole(roleName, roleID string) datastore.Role {
	roles, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), inPlaceRoleGroup, roleName)
	require.NoError(f.t, err)
	for _, role := range roles {
		if role.Name == roleID {
			return role
		}
	}
	require.Failf(f.t, "role not found", "%s/%s", roleName, roleID)
	return datastore.Role{}
}

var (
	patchedPhases   = []inplace.Phase{inplace.PhasePatched, inplace.PhasePatched}
	donePhases      = []inplace.Phase{inplace.PhaseDone, inplace.PhaseDone}
	untouchedPhases = []inplace.Phase{"", ""}
)

func TestInPlaceRoleRolloutUpdatesChangedRole(t *testing.T) {
	f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
	prefillHash := utils.CalRoleTemplateHash(f.ms.Spec.Template.Roles[0])

	// maxUnavailable of prefill is 1, so its highest ordinal goes first.
	require.NoError(t, f.sync())
	assert.Equal(t, patchedPhases, f.rolePhases("prefill-1"))
	assert.Equal(t, untouchedPhases, f.rolePhases("prefill-0"))
	assert.Equal(t, untouchedPhases, f.rolePhases("decode-0"), "an unchanged Role is not updated")
	for _, pod := range f.rolePods("prefill-1") {
		state := inplace.MatchingState(pod)
		require.NotNil(t, state)
		assert.Equal(t, inPlaceRoleGroup+"/prefill-1", state.Unit)
		assert.NotEqual(t, prefillHash, state.SourceRoleTemplateHash)
	}
	assert.NotEqual(t, prefillHash, f.storeRole("prefill", "prefill-1").RoleTemplateHash, "a Role instance in update stays outdated")

	f.runKubeletOnRole("prefill-1")
	require.NoError(t, f.sync())
	assert.Equal(t, donePhases, f.rolePhases("prefill-1"))
	assert.Equal(t, prefillHash, f.storeRole("prefill", "prefill-1").RoleTemplateHash)
	assert.Equal(t, f.revision, f.storeRole("prefill", "prefill-1").Revision)

	require.NoError(t, f.sync())
	assert.Equal(t, patchedPhases, f.rolePhases("prefill-0"))
	assert.Equal(t, inPlaceRolloutSourceRevision, f.storeRevision(inPlaceRoleGroup), "the ServingGroup waits for its outdated Roles")
	f.runKubeletOnRole("prefill-0")
	require.NoError(t, f.sync())
	require.NoError(t, f.sync())
	assert.Equal(t, donePhases, f.rolePhases("prefill-0"))
	assert.Equal(t, f.revision, f.storeRevision(inPlaceRoleGroup))
	assert.Equal(t, untouchedPhases, f.rolePhases("decode-0"))
	assert.NotContains(t, f.events(), inplace.ReasonFallback)
}

func TestInPlaceRoleRolloutBudget(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(role *workloadv1alpha1.Role)
		wantPrefill [][]inplace.Phase
	}{
		{
			name:        "maxUnavailable is not set, so every instance updates at once",
			mutate:      func(role *workloadv1alpha1.Role) { role.MaxUnavailable = nil },
			wantPrefill: [][]inplace.Phase{patchedPhases, patchedPhases},
		},
		{
			name:        "partition protects the first instance",
			mutate:      func(role *workloadv1alpha1.Role) { role.Partition = ptr.To(intstr.FromInt32(1)) },
			wantPrefill: [][]inplace.Phase{untouchedPhases, patchedPhases},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
			ms := f.ms.DeepCopy()
			tt.mutate(&ms.Spec.Template.Roles[0])
			f.setModelServing(ms)

			require.NoError(t, f.sync())
			assert.Equal(t, tt.wantPrefill, [][]inplace.Phase{f.rolePhases("prefill-0"), f.rolePhases("prefill-1")})
		})
	}
}

func TestInPlaceRoleRolloutRecreatesIneligibleInstance(t *testing.T) {
	f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
	ms := f.ms.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Args = []string{"--new-flag"}
	f.setModelServing(ms)
	f.revision = utils.ModelServingRevision(ms)

	require.NoError(t, f.sync())
	assert.Equal(t, datastore.RoleDeleting, f.storeRole("prefill", "prefill-1").Status)
	assert.Equal(t, datastore.RoleRunning, f.storeRole("prefill", "prefill-0").Status, "the recreated instance uses the only slot")
	assert.Equal(t, datastore.RoleRunning, f.storeRole("decode", "decode-0").Status)
	var fallbacks []string
	for _, event := range f.eventMessages() {
		if strings.Contains(event, inplace.ReasonFallback) {
			fallbacks = append(fallbacks, event)
		}
	}
	require.Len(t, fallbacks, 1)
	assert.Contains(t, fallbacks[0], "Role instance "+inPlaceRoleGroup+"/prefill-1 is recreated")
}

func TestInPlaceRoleRolloutFallsBackWhenMaxUnavailableIsZero(t *testing.T) {
	f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
	ms := f.ms.DeepCopy()
	ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt32(0))
	ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt32(1))
	f.setModelServing(ms)

	require.NoError(t, f.sync())
	assert.Equal(t, untouchedPhases, f.rolePhases("prefill-0"))
	assert.Equal(t, untouchedPhases, f.rolePhases("prefill-1"))
	assert.Equal(t, datastore.RoleRunning, f.storeRole("prefill", "prefill-1").Status, "Recreate waits for the surge instance")
	assert.Equal(t, []string{inplace.ReasonFallback}, f.events())

	require.NoError(t, f.sync())
	assert.Empty(t, f.events(), "the reason is reported once")

	f.controller.manageRoleReplicasPerGroup(f.ctx, f.ms, inPlaceRoleGroup, f.ms.Spec.Template.Roles[0], 0, f.revision, f.controller.newRoleSurgeRollout(f.ms))
	roles, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), inPlaceRoleGroup, "prefill")
	require.NoError(t, err)
	assert.Len(t, roles, 3, "Recreate surges the Role")
}

// TestInPlaceRoleRolloutFindsSourceByRoleHash covers decode Pods whose revision was cleaned up while decode did not change.
func TestInPlaceRoleRolloutFindsSourceByRoleHash(t *testing.T) {
	f := newInPlaceRoleFixture(t, "rev-cleaned-up")
	ms := f.ms.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = inPlaceRolloutOldImage
	ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = inPlaceRolloutOldImage
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = inPlaceRolloutNewImage
	ms.Spec.Template.Roles[1].WorkerTemplate.Spec.Containers[0].Image = inPlaceRolloutNewImage
	f.setModelServing(ms)
	f.revision = utils.ModelServingRevision(ms)

	require.NoError(t, f.sync())
	assert.Equal(t, patchedPhases, f.rolePhases("decode-0"))
	assert.NotContains(t, f.events(), inplace.ReasonFallback)
}

// TestInPlaceRoleRolloutKeepsUnfinishedUpdateOutdated rebuilds the store from Pod labels while a Role instance is in update.
func TestInPlaceRoleRolloutKeepsUnfinishedUpdateOutdated(t *testing.T) {
	f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
	require.NoError(t, f.sync())
	require.Equal(t, patchedPhases, f.rolePhases("prefill-1"))
	sourceHash := f.storeRole("prefill", "prefill-1").RoleTemplateHash
	prefillHash := utils.CalRoleTemplateHash(f.ms.Spec.Template.Roles[0])
	msKey := utils.GetNamespaceName(f.ms)
	require.NoError(t, f.controller.store.UpdateRoleRevision(msKey, inPlaceRoleGroup, "prefill", "prefill-1", f.revision, prefillHash))

	require.NoError(t, f.controller.syncInPlaceUnitRevisions(f.ms, f.revision))
	assert.Equal(t, sourceHash, f.storeRole("prefill", "prefill-1").RoleTemplateHash)
	assert.Equal(t, inPlaceRolloutSourceRevision, f.storeRole("prefill", "prefill-1").Revision)
	assert.Equal(t, inPlaceRolloutSourceRevision, f.storeRevision(inPlaceRoleGroup))
}

func TestInPlaceRoleRolloutDeletesRemovedRole(t *testing.T) {
	f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
	ms := f.ms.DeepCopy()
	ms.Spec.Template.Roles = ms.Spec.Template.Roles[:1]
	f.setModelServing(ms)
	f.revision = utils.ModelServingRevision(ms)

	require.NoError(t, f.sync())
	assert.Equal(t, datastore.RoleDeleting, f.storeRole("decode", "decode-0").Status)
	assert.Equal(t, patchedPhases, f.rolePhases("prefill-1"), "the remaining Role is still updated in place")
	assert.Equal(t, inPlaceRolloutSourceRevision, f.storeRevision(inPlaceRoleGroup))
}

func TestInPlaceRoleRolloutFinishesUpdateAfterPartitionRaised(t *testing.T) {
	f := newInPlaceRoleFixture(t, inPlaceRolloutSourceRevision)
	require.NoError(t, f.sync())
	require.Equal(t, patchedPhases, f.rolePhases("prefill-1"))

	ms := f.ms.DeepCopy()
	ms.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt32(2))
	f.setModelServing(ms)
	f.runKubeletOnRole("prefill-1")
	require.NoError(t, f.sync())
	assert.Equal(t, donePhases, f.rolePhases("prefill-1"), "a started update finishes")

	require.NoError(t, f.sync())
	assert.Equal(t, untouchedPhases, f.rolePhases("prefill-0"), "partition protects instances that have not started")
}
