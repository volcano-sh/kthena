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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestClassifyRestart(t *testing.T) {
	tests := []struct {
		name     string
		state    *State
		rawState string
		mutate   func(*corev1.Pod)
		want     RestartCause
	}{
		{
			name: "no state and no restart",
			want: NoRestart,
		},
		{
			name:   "no state and regular container restarted",
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
			want:   UnexpectedRestart,
		},
		{
			name: "no state and init container restarted",
			mutate: func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
			},
			want: UnexpectedRestart,
		},
		{
			name: "no state and pending pod restarted",
			mutate: func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses[0].RestartCount = 1
			},
			want: UnexpectedRestart,
		},
		{
			name: "failed pod is not classified as a restart",
			mutate: func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				p.Status.ContainerStatuses[0].RestartCount = 1
			},
			want: NoRestart,
		},
		{
			name:   "draining and restarted before the patch",
			state:  newTestState(PhaseDraining),
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
			want:   UnexpectedRestart,
		},
		{
			name:   "patched and the changed container restarted once on the target image",
			state:  newPatchedState(5, 5),
			mutate: func(p *corev1.Pod) { setMain(p, 6, targetImage) },
			want:   InPlaceUpdateRestart,
		},
		{
			name:   "patched and not restarted yet",
			state:  newPatchedState(5, 5),
			mutate: func(p *corev1.Pod) { setMain(p, 5, "example.com/engine:v1") },
			want:   NoRestart,
		},
		{
			name:   "patched and the changed container restarted more than once",
			state:  newPatchedState(5, 5),
			mutate: func(p *corev1.Pod) { setMain(p, 7, targetImage) },
			want:   UnexpectedRestart,
		},
		{
			name:   "patched and the changed container restarted on another image",
			state:  newPatchedState(5, 5),
			mutate: func(p *corev1.Pod) { setMain(p, 6, "example.com/engine:v1") },
			want:   UnexpectedRestart,
		},
		{
			name:  "patched and the runtime reports the target image under another name",
			state: newPatchedStateWithImageID("sha256:v1"),
			mutate: func(p *corev1.Pod) {
				setMain(p, 6, "mirror.example.com/engine:v2")
				p.Status.ContainerStatuses[0].ImageID = "sha256:v2"
			},
			want: InPlaceUpdateRestart,
		},
		{
			name:  "patched and the changed container restarted on the source image",
			state: newPatchedStateWithImageID("sha256:v1"),
			mutate: func(p *corev1.Pod) {
				setMain(p, 6, "example.com/engine:v1")
				p.Status.ContainerStatuses[0].ImageID = "sha256:v1"
			},
			want: UnexpectedRestart,
		},
		{
			name:  "patched before the container first started, and it started on the source image",
			state: newPatchedStateWithImageID(""),
			mutate: func(p *corev1.Pod) {
				setMain(p, 6, "example.com/engine:v1")
				p.Status.ContainerStatuses[0].ImageID = "sha256:v1"
			},
			want: UnexpectedRestart,
		},
		{
			name:   "patched after a restart that happened before the patch",
			state:  newPatchedState(4, 5),
			mutate: func(p *corev1.Pod) { setMain(p, 6, targetImage) },
			want:   UnexpectedRestart,
		},
		{
			name:  "patched and an unchanged container restarted",
			state: newPatchedState(5, 5),
			mutate: func(p *corev1.Pod) {
				setMain(p, 6, targetImage)
				p.Status.ContainerStatuses[1].RestartCount = 6
			},
			want: UnexpectedRestart,
		},
		{
			name:  "patched and an init container restarted",
			state: newPatchedState(5, 5),
			mutate: func(p *corev1.Pod) {
				setMain(p, 6, targetImage)
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
			},
			want: UnexpectedRestart,
		},
		{
			name: "in progress and no restart beyond baseline",
			state: func() *State {
				s := newTestState(PhasePatched)
				s.Baseline["main"] = ContainerBaseline{RestartCount: 2}
				return s
			}(),
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 2 },
			want:   NoRestart,
		},
		{
			name: "done and restart count equals baseline",
			state: func() *State {
				s := newTestState(PhaseDone)
				s.Baseline["main"] = ContainerBaseline{RestartCount: 3}
				return s
			}(),
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 3 },
			want:   NoRestart,
		},
		{
			name: "done and restarted beyond baseline",
			state: func() *State {
				s := newTestState(PhaseDone)
				s.Baseline["main"] = ContainerBaseline{RestartCount: 3}
				return s
			}(),
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 4 },
			want:   UnexpectedRestart,
		},
		{
			name:  "done and container missing from baseline restarted",
			state: newTestState(PhaseDone),
			mutate: func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
			},
			want: UnexpectedRestart,
		},
		{
			name: "in-progress state for another pod does not exempt a restart",
			state: func() *State {
				s := newTestState(PhasePatched)
				s.PodUID = "previous-pod"
				return s
			}(),
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
			want:   UnexpectedRestart,
		},
		{
			name: "in-progress state for another ModelServing does not exempt a restart",
			state: func() *State {
				s := newTestState(PhasePatched)
				s.OwnerUID = "previous-ms"
				return s
			}(),
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
			want:   UnexpectedRestart,
		},
		{
			name:     "malformed state does not exempt a restart",
			rawState: `{"version":1,"phase":`,
			mutate:   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
			want:     UnexpectedRestart,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newTestPod()
			if tt.state != nil {
				setState(t, pod, tt.state)
			}
			if tt.rawState != "" {
				setStateValue(pod, tt.rawState)
			}
			if tt.mutate != nil {
				tt.mutate(pod)
			}
			assert.Equal(t, tt.want, ClassifyRestart(pod))
		})
	}
}

const targetImage = "example.com/engine:v2"

// newPatchedState patches only "main" to targetImage.
func newPatchedState(baselineRestarts, patchRestarts int32) *State {
	state := newTestState(PhasePatched)
	state.Baseline = map[string]ContainerBaseline{
		"main":    {RestartCount: baselineRestarts},
		"sidecar": {RestartCount: baselineRestarts},
	}
	state.Patched = map[string]PatchedContainer{"main": {Image: targetImage, RestartCount: patchRestarts}}
	return state
}

func newPatchedStateWithImageID(imageID string) *State {
	state := newPatchedState(5, 5)
	patched := state.Patched["main"]
	patched.ImageID = imageID
	state.Patched["main"] = patched
	return state
}

func setMain(pod *corev1.Pod, restarts int32, image string) {
	pod.Status.ContainerStatuses[0].RestartCount = restarts
	pod.Status.ContainerStatuses[0].Image = image
}

func restartsByName(t *testing.T, pod *corev1.Pod) map[string]ContainerRestarts {
	t.Helper()
	restarts, ok := UpdateRestarts(pod)
	require.True(t, ok)
	byName := make(map[string]ContainerRestarts, len(restarts))
	for _, r := range restarts {
		byName[r.Name] = r
	}
	return byName
}

func TestUpdateRestarts(t *testing.T) {
	tests := []struct {
		name   string
		state  *State
		mutate func(*corev1.Pod)
		want   map[string]ContainerRestarts
	}{
		{
			name:  "draining: restarts beyond baseline are other",
			state: newTestState(PhaseDraining),
			mutate: func(p *corev1.Pod) {
				setMain(p, 2, "example.com/engine:v1")
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
			},
			want: map[string]ContainerRestarts{
				"init":    {Name: "init", Other: 1},
				"main":    {Name: "main", Other: 2},
				"sidecar": {Name: "sidecar"},
			},
		},
		{
			name:   "patched, before kubelet restarts the container",
			state:  newPatchedState(0, 0),
			mutate: func(p *corev1.Pod) { setMain(p, 0, "example.com/engine:v1") },
			want: map[string]ContainerRestarts{
				"main":    {Name: "main", Changed: true},
				"sidecar": {Name: "sidecar"},
			},
		},
		{
			name:   "one restart on the target image is the image change",
			state:  newPatchedState(0, 0),
			mutate: func(p *corev1.Pod) { setMain(p, 1, targetImage) },
			want: map[string]ContainerRestarts{
				"main":    {Name: "main", Changed: true, ImageChange: 1},
				"sidecar": {Name: "sidecar"},
			},
		},
		{
			name:  "restarts before the patch and after the image change are other",
			state: newPatchedState(1, 3),
			mutate: func(p *corev1.Pod) {
				setMain(p, 6, targetImage)
				p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
			},
			want: map[string]ContainerRestarts{
				"main":    {Name: "main", Changed: true, ImageChange: 1, Other: 4, LastTermination: "OOMKilled"},
				"sidecar": {Name: "sidecar"},
			},
		},
		{
			name:   "restart on an image that is not the target is other",
			state:  newPatchedState(0, 0),
			mutate: func(p *corev1.Pod) { setMain(p, 1, "example.com/engine:v1") },
			want: map[string]ContainerRestarts{
				"main":    {Name: "main", Changed: true, Other: 1},
				"sidecar": {Name: "sidecar"},
			},
		},
		{
			name:  "peer restart of an unchanged container is other",
			state: newPatchedState(0, 0),
			mutate: func(p *corev1.Pod) {
				setMain(p, 1, targetImage)
				p.Status.ContainerStatuses[1].RestartCount = 2
				p.Status.ContainerStatuses[1].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}
			},
			want: map[string]ContainerRestarts{
				"main":    {Name: "main", Changed: true, ImageChange: 1},
				"sidecar": {Name: "sidecar", Other: 2, LastTermination: "Error (exit code 1)"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newTestPod()
			setState(t, pod, tt.state)
			tt.mutate(pod)
			assert.Equal(t, tt.want, restartsByName(t, pod))
		})
	}

	t.Run("no breakdown outside the update window", func(t *testing.T) {
		pod := newTestPod()
		_, ok := UpdateRestarts(pod)
		assert.False(t, ok, "no state")

		setState(t, pod, newTestState(PhaseDone))
		_, ok = UpdateRestarts(pod)
		assert.False(t, ok, "done state")
	})
}
