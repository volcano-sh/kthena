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

package inplace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

var updaterTestTime = metav1.Unix(1700000000, 0)

func newTestUpdater(pod *corev1.Pod) (*PodUpdater, *kubefake.Clientset) {
	client := kubefake.NewSimpleClientset(pod.DeepCopy())
	updater := NewPodUpdater(client)
	updater.now = func() metav1.Time { return updaterTestTime }
	return updater, client
}

func mustParseState(t *testing.T, pod *corev1.Pod) *State {
	t.Helper()
	state, err := ParseState(pod)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

func TestPodUpdaterLifecycle(t *testing.T) {
	ctx := context.Background()
	pod := newTestPod()
	pod.Status.ContainerStatuses[0].RestartCount = 2
	pod.Status.ContainerStatuses[0].ImageID = "sha256:v1"
	updater, client := newTestUpdater(pod)

	marked, err := updater.Mark(ctx, pod, testOwnerUID, "ms-0", "rev-1", "rev-2")
	require.NoError(t, err)
	assert.Equal(t, &State{
		Version:        StateVersion,
		OwnerUID:       testOwnerUID,
		PodUID:         testPodUID,
		Unit:           "ms-0",
		SourceRevision: "rev-1",
		TargetRevision: "rev-2",
		Phase:          PhaseDraining,
		StartedAt:      updaterTestTime,
		Baseline: map[string]ContainerBaseline{
			"main":    {ContainerID: "containerd://main-1", RestartCount: 2},
			"sidecar": {ContainerID: "containerd://sidecar-1"},
		},
	}, mustParseState(t, marked), "the baseline holds the restart counts from before the update")
	assert.Equal(t, "example.com/engine:v1", marked.Spec.Containers[0].Image, "marking does not change images")

	again, err := updater.Mark(ctx, marked, testOwnerUID, "ms-0", "rev-1", "rev-2")
	require.NoError(t, err)
	assert.Same(t, marked, again, "marking for the same target is a no-op")

	patched, err := updater.PatchImages(ctx, marked,
		map[string]string{"main": "example.com/engine:v2", "sidecar": "example.com/sidecar:v1"},
		map[string]string{workloadv1alpha1.RevisionLabelKey: "rev-2"})
	require.NoError(t, err)
	assert.Equal(t, "example.com/engine:v2", patched.Spec.Containers[0].Image)
	assert.Equal(t, "example.com/sidecar:v1", patched.Spec.Containers[1].Image)
	assert.Equal(t, "rev-2", patched.Labels[workloadv1alpha1.RevisionLabelKey])
	state := mustParseState(t, patched)
	assert.Equal(t, PhasePatched, state.Phase)
	assert.Equal(t, &updaterTestTime, state.PatchedAt)
	assert.Equal(t, map[string]PatchedContainer{
		"main": {Image: "example.com/engine:v2", RestartCount: 2, ImageID: "sha256:v1"},
	}, state.Patched, "only the changed container is recorded, with its restart count and image ID at patch time")

	again, err = updater.PatchImages(ctx, patched, map[string]string{"main": "example.com/engine:v3"}, nil)
	require.NoError(t, err)
	assert.Same(t, patched, again, "a patched pod is not patched again")

	// Kubelet restarts the changed container on the new image.
	patched.Status.ContainerStatuses[0].RestartCount = 3
	patched.Status.ContainerStatuses[0].ContainerID = "containerd://main-2"
	_, err = client.CoreV1().Pods(patched.Namespace).UpdateStatus(ctx, patched, metav1.UpdateOptions{})
	require.NoError(t, err)
	current, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)

	completed, err := updater.Complete(ctx, current)
	require.NoError(t, err)
	state = mustParseState(t, completed)
	assert.Equal(t, PhaseDone, state.Phase)
	assert.Equal(t, ContainerBaseline{ContainerID: "containerd://main-2", RestartCount: 3}, state.Baseline["main"],
		"the baseline moves to the restart counts after the update")
	assert.Nil(t, state.Patched, "the patched record is cleared at completion")
	assert.False(t, InUpdateWindow(completed))

	again, err = updater.Complete(ctx, completed)
	require.NoError(t, err)
	assert.Same(t, completed, again, "completing a finished update is a no-op")
}

func TestPodUpdaterMarkRejects(t *testing.T) {
	ctx := context.Background()

	t.Run("an unfinished update to another target", func(t *testing.T) {
		pod := newTestPod()
		setState(t, pod, newTestState(PhasePatched))
		updater, _ := newTestUpdater(pod)
		_, err := updater.Mark(ctx, pod, testOwnerUID, "ms-0", "rev-1", "rev-3")
		require.ErrorContains(t, err, "already updating to revision rev-2")
	})

	t.Run("a pod of another ModelServing", func(t *testing.T) {
		pod := newTestPod()
		updater, _ := newTestUpdater(pod)
		_, err := updater.Mark(ctx, pod, "other-uid", "ms-0", "rev-1", "rev-2")
		require.ErrorContains(t, err, "is not owned by ModelServing other-uid")
	})

	t.Run("a finished update is replaced by a new one", func(t *testing.T) {
		pod := newTestPod()
		setState(t, pod, newTestState(PhaseDone))
		updater, _ := newTestUpdater(pod)
		marked, err := updater.Mark(ctx, pod, testOwnerUID, "ms-0", "rev-2", "rev-3")
		require.NoError(t, err)
		state := mustParseState(t, marked)
		assert.Equal(t, PhaseDraining, state.Phase)
		assert.Equal(t, "rev-3", state.TargetRevision)
	})
}

func TestPodUpdaterRejectsStalePod(t *testing.T) {
	ctx := context.Background()
	pod := newTestPod()
	updater, client := newTestUpdater(pod)

	// The Pod changes after it was read, for example when kubelet reports a restart.
	live := pod.DeepCopy()
	live.ResourceVersion = "2"
	live.Status.ContainerStatuses[0].RestartCount = 1
	_, err := client.CoreV1().Pods(live.Namespace).UpdateStatus(ctx, live, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = updater.Mark(ctx, pod, testOwnerUID, "ms-0", "rev-1", "rev-2")
	require.Error(t, err, "a baseline read from a stale pod is never written")
	current, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, current.Annotations)
}

func TestPatchImagesRequiresAnUnfinishedUpdate(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []Phase{"", PhaseDone} {
		pod := newTestPod()
		if phase != "" {
			setState(t, pod, newTestState(phase))
		}
		updater, _ := newTestUpdater(pod)
		_, err := updater.PatchImages(ctx, pod, map[string]string{"main": "example.com/engine:v2"}, nil)
		require.ErrorContains(t, err, "has no in-progress in-place update", "phase %q", phase)
	}
}

func TestBaselineFromStatus(t *testing.T) {
	pod := newTestPod()
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "download", ContainerID: "containerd://download-1", RestartCount: 1}}
	pod.Status.ContainerStatuses[1].RestartCount = 4
	assert.Equal(t, map[string]ContainerBaseline{
		"download": {ContainerID: "containerd://download-1", RestartCount: 1},
		"main":     {ContainerID: "containerd://main-1"},
		"sidecar":  {ContainerID: "containerd://sidecar-1", RestartCount: 4},
	}, BaselineFromStatus(pod))
}
