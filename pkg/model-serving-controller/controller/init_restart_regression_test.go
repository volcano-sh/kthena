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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// initRestartPod builds a live entry Pod of ms in the given phase, with one
// init container that kubelet has restarted restarts times.
func initRestartPod(t *testing.T, c *ModelServingController, ms *api.ModelServing, phase corev1.PodPhase, initRestarts, containerRestarts int32) *corev1.Pod {
	t.Helper()
	role := ms.Spec.Template.Roles[0]
	group, revision := ms.Name+"-0", utils.ModelServingRevision(ms)
	id, hash := utils.GenerateRoleID(role.Name, 0), utils.CalRoleTemplateHash(role)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, revision)
	c.store.AddRole(utils.GetNamespaceName(ms), group, role.Name, id, revision, hash)

	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, group, id, revision, hash)
	pod.UID = types.UID(pod.Name + "-uid")
	pod.Status.Phase = phase
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:         "downloader",
		RestartCount: initRestarts,
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         role.EntryTemplate.Spec.Containers[0].Name,
		RestartCount: containerRestarts,
	}}
	created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(created))
	return created
}

// inGracePeriod reports whether updatePod routed the Pod to handleErrorPod.
// handleErrorPod claims the grace-period entry before it starts the goroutine
// that deletes the Pod, so this is the synchronous half of the recovery path.
func inGracePeriod(c *ModelServingController, pod *corev1.Pod) bool {
	_, ok := c.graceMap.Load(getPodGracePeriodKey(pod))
	return ok
}

// A model weight download that fails once is retried by kubelet under the Pod's
// restartPolicy, and the Pod stays Pending while it runs. Treating that restart
// as a crash sends the Pod to handleErrorPod, which with the default
// restartGracePeriodSeconds of 0 deletes it immediately and starts the download
// over. Six of the example ModelServings in examples/model-serving download
// weights in exactly such an init container.
func TestPendingInitContainerRetryIsNotRecovered(t *testing.T) {
	ms := createStandardModelServing("init-retry", 1, 1)
	ms.UID, ms.ResourceVersion = types.UID("init-retry-owner"), "1"
	c := newRevisionTestController(t, ms)
	pod := initRestartPod(t, c, ms, corev1.PodPending, 1, 0)

	c.updatePod(nil, pod)

	require.False(t, inGracePeriod(c, pod),
		"a Pod still initializing was sent to recovery because kubelet retried its init container")

	live, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Nil(t, live.DeletionTimestamp)
}

// The fix must not stop recovering real crashes, so the cases that should still
// reach handleErrorPod are asserted next to the one that should not.
func TestRestartsThatStillReachRecovery(t *testing.T) {
	cases := []struct {
		name              string
		phase             corev1.PodPhase
		initRestarts      int32
		containerRestarts int32
		recovered         bool
	}{
		// An init container that restarts once the Pod is Running is a sidecar:
		// it is not kubelet finishing startup, it went down under us.
		{"running pod, init container restarted", corev1.PodRunning, 1, 0, true},
		// Unchanged: a regular container restart with no in-place update to
		// account for it is a crash in either phase.
		{"running pod, container restarted", corev1.PodRunning, 0, 1, true},
		{"pending pod, container restarted", corev1.PodPending, 0, 1, true},
		// The case the fix is about.
		{"pending pod, init container retried", corev1.PodPending, 1, 0, false},
		// Nothing restarted at all.
		{"pending pod, nothing restarted", corev1.PodPending, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := createStandardModelServing("recovery", 1, 1)
			ms.UID, ms.ResourceVersion = types.UID("recovery-owner"), "1"
			c := newRevisionTestController(t, ms)
			pod := initRestartPod(t, c, ms, tc.phase, tc.initRestarts, tc.containerRestarts)

			c.updatePod(nil, pod)

			require.Equal(t, tc.recovered, inGracePeriod(c, pod))
		})
	}
}
