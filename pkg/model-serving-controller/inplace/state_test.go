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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	testOwnerUID = types.UID("ms-uid")
	testPodUID   = types.UID("pod-uid")
)

func newTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "ms-0-server-0-0",
			UID:             testPodUID,
			ResourceVersion: "1",
			Labels: map[string]string{
				workloadv1alpha1.RevisionLabelKey: "rev-1",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
				Kind:       workloadv1alpha1.ModelServingKind.Kind,
				Name:       "ms",
				UID:        testOwnerUID,
			}},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{Name: "main", Image: "example.com/engine:v1"},
				{Name: "sidecar", Image: "example.com/sidecar:v1"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", ContainerID: "containerd://main-1"},
				{Name: "sidecar", ContainerID: "containerd://sidecar-1"},
			},
		},
	}
}

func newTestState(phase Phase) *State {
	return &State{
		Version:        StateVersion,
		OwnerUID:       testOwnerUID,
		PodUID:         testPodUID,
		Unit:           "ms-0",
		SourceRevision: "rev-1",
		TargetRevision: "rev-2",
		Phase:          phase,
		Baseline: map[string]ContainerBaseline{
			"main":    {ContainerID: "containerd://main-1"},
			"sidecar": {ContainerID: "containerd://sidecar-1"},
		},
	}
}

func setState(t *testing.T, pod *corev1.Pod, state *State) {
	t.Helper()
	value, err := state.Encode()
	require.NoError(t, err)
	setStateValue(pod, value)
}

func setStateValue(pod *corev1.Pod, value string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey] = value
}

func TestParseState(t *testing.T) {
	tests := []struct {
		name      string
		value     *string
		wantState bool
		wantErr   bool
	}{
		{name: "no annotation"},
		{name: "valid state", value: ptrTo(`{"version":1,"phase":"Patched"}`), wantState: true},
		{name: "malformed JSON", value: ptrTo(`{`), wantErr: true},
		{name: "unsupported version", value: ptrTo(`{"version":2,"phase":"Done"}`), wantErr: true},
		{name: "unknown phase", value: ptrTo(`{"version":1,"phase":"Unknown"}`), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newTestPod()
			if tt.value != nil {
				setStateValue(pod, *tt.value)
			}
			state, err := ParseState(pod)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, state)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantState, state != nil)
		})
	}
}

func TestStateRoundTrip(t *testing.T) {
	pod := newTestPod()
	want := newTestState(PhaseDraining)
	want.PreviousTargets = []string{"rev-0"}
	want.StartedAt = metav1.Unix(1700000000, 0)
	want.PatchedAt = ptrTo(metav1.Unix(1700000060, 0))
	want.Patched = map[string]PatchedContainer{"main": {Image: "example.com/engine:v2", RestartCount: 1}}
	setState(t, pod, want)

	got, err := ParseState(pod)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestMatchingState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Pod, *State)
		want   bool
	}{
		{name: "matching", want: true},
		{name: "pod UID differs", mutate: func(_ *corev1.Pod, s *State) { s.PodUID = "other-pod" }},
		{name: "pod UID missing", mutate: func(_ *corev1.Pod, s *State) { s.PodUID = "" }},
		{name: "owner UID differs", mutate: func(_ *corev1.Pod, s *State) { s.OwnerUID = "other-ms" }},
		{name: "pod not owned by a ModelServing", mutate: func(p *corev1.Pod, _ *State) { p.OwnerReferences = nil }},
		{name: "owner UID missing", mutate: func(p *corev1.Pod, s *State) {
			s.OwnerUID = ""
			p.OwnerReferences[0].UID = ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newTestPod()
			state := newTestState(PhaseDraining)
			if tt.mutate != nil {
				tt.mutate(pod, state)
			}
			setState(t, pod, state)
			assert.Equal(t, tt.want, MatchingState(pod) != nil)
		})
	}

	t.Run("malformed state", func(t *testing.T) {
		pod := newTestPod()
		setStateValue(pod, "not-json")
		assert.Nil(t, MatchingState(pod))
	})
}

func TestInUpdateWindow(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		want  bool
	}{
		{name: "draining", phase: PhaseDraining, want: true},
		{name: "patched", phase: PhasePatched, want: true},
		{name: "done", phase: PhaseDone, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newTestPod()
			setState(t, pod, newTestState(tt.phase))
			assert.Equal(t, tt.want, InUpdateWindow(pod))
		})
	}

	t.Run("state written for another pod", func(t *testing.T) {
		pod := newTestPod()
		state := newTestState(PhasePatched)
		state.PodUID = "other-pod"
		setState(t, pod, state)
		assert.False(t, InUpdateWindow(pod))
	})

	t.Run("no state", func(t *testing.T) {
		assert.False(t, InUpdateWindow(newTestPod()))
	})
}

func ptrTo[T any](v T) *T {
	return &v
}
