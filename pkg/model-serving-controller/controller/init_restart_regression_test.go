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

// newRestartPod creates an entry Pod of ms in the given phase whose init and
// entry containers have been restarted the given number of times. initDone
// marks the init container as having since exited cleanly.
func newRestartPod(t *testing.T, c *ModelServingController, ms *api.ModelServing, phase corev1.PodPhase, initRestarts int32, initDone bool, containerRestarts int32) *corev1.Pod {
	t.Helper()
	role := ms.Spec.Template.Roles[0]
	group, revision := ms.Name+"-0", utils.ModelServingRevision(ms)
	id, hash := utils.GenerateRoleID(role.Name, 0), utils.CalRoleTemplateHash(role)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, revision)
	c.store.AddRole(utils.GetNamespaceName(ms), group, role.Name, id, revision, hash)

	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, group, id, revision, hash)
	pod.UID = types.UID(pod.Name + "-uid")
	pod.Status.Phase = phase
	initStatus := corev1.ContainerStatus{Name: "downloader", RestartCount: initRestarts}
	if initDone {
		initStatus.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0}
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{initStatus}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         role.EntryTemplate.Spec.Containers[0].Name,
		RestartCount: containerRestarts,
	}}
	created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(created))
	return created
}

// recovered reports whether updatePod sent the Pod to handleErrorPod, which
// claims the grace period entry before it deletes the Pod.
func recovered(c *ModelServingController, pod *corev1.Pod) bool {
	_, ok := c.graceMap.Load(getPodGracePeriodKey(pod))
	return ok
}

// An init container restart is not a crash while kubelet is still retrying it,
// nor once it has exited cleanly, because the count never resets.
func TestUnexpectedRestartRecovery(t *testing.T) {
	cases := []struct {
		name              string
		phase             corev1.PodPhase
		initRestarts      int32
		initDone          bool
		containerRestarts int32
		recover           bool
	}{
		{"pending pod, init container being retried", corev1.PodPending, 1, false, 0, false},
		{"running pod, init container retried then succeeded", corev1.PodRunning, 1, true, 0, false},
		{"running pod, init container still down", corev1.PodRunning, 1, false, 0, true},
		{"pending pod, nothing restarted", corev1.PodPending, 0, false, 0, false},
		{"pending pod, container restarted", corev1.PodPending, 0, false, 1, true},
		{"running pod, container restarted", corev1.PodRunning, 0, false, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := createStandardModelServing("restart", 1, 1)
			ms.UID, ms.ResourceVersion = types.UID("restart-owner"), "1"
			c := newRevisionTestController(t, ms)
			pod := newRestartPod(t, c, ms, tc.phase, tc.initRestarts, tc.initDone, tc.containerRestarts)

			c.updatePod(nil, pod)

			require.Equal(t, tc.recover, recovered(c, pod))
		})
	}
}
