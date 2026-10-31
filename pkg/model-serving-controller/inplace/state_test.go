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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func podWithState(t *testing.T, state *State, ready bool, status corev1.ContainerStatus) *corev1.Pod {
	t.Helper()
	status.Name = "main"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{status},
		},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	if state != nil {
		raw, err := json.Marshal(state)
		require.NoError(t, err)
		pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey] = string(raw)
		// The live spec carries the image the update asked for.
		pod.Spec.Containers = []corev1.Container{{Name: "main", Image: state.Patched["main"].Image}}
	}
	return pod
}

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestStatePredicates(t *testing.T) {
	// "main" was patched to v2 while running container c1 with image id1, after one earlier update restart.
	started := &State{
		Revision: "v2",
		Patched:  map[string]PatchedContainer{"main": {Image: "registry/app:v2", PrevContainerID: "c1", PrevImageID: "id1"}},
		Expected: map[string]int32{"main": 2},
	}
	done := &State{Revision: "v2", Expected: map[string]int32{"main": 2}, Done: true}
	old := corev1.ContainerStatus{RestartCount: 1, ContainerID: "c1", Image: "registry/app:v1", ImageID: "id1"}
	updated := corev1.ContainerStatus{RestartCount: 2, ContainerID: "c2", Image: "registry/app:v2", ImageID: "id2"}

	tests := []struct {
		name                  string
		state                 *State
		ready                 bool
		status                corev1.ContainerStatus
		wantInProgress        bool
		wantCompleted         bool
		wantUpdating          bool
		wantUnexpectedRestart bool
	}{
		{name: "no state, no restarts"},
		{name: "no state, crash loop", status: corev1.ContainerStatus{RestartCount: 1}, wantUnexpectedRestart: true},
		{name: "started, stale ready before kubelet notices", state: started, ready: true, status: old, wantInProgress: true, wantUpdating: true},
		{name: "started, restarted but not ready", state: started, status: updated, wantInProgress: true, wantUpdating: true},
		{name: "started, restarted and ready", state: started, ready: true, status: updated, wantInProgress: true, wantCompleted: true},
		{
			name: "started, old image crashed and restarted", state: started, ready: true, wantInProgress: true, wantUpdating: true, wantUnexpectedRestart: true,
			status: corev1.ContainerStatus{RestartCount: 2, ContainerID: "c2", Image: "registry/app:v1", ImageID: "id1"},
		},
		{
			name: "started, extra crash on the target image is not completed", state: started, ready: true, wantInProgress: true, wantUpdating: true, wantUnexpectedRestart: true,
			status: corev1.ContainerStatus{RestartCount: 3, ContainerID: "c3", Image: "registry/app:v2", ImageID: "id2"},
		},
		{
			name: "started, runtime reports normalized name", state: started, ready: true, wantInProgress: true, wantCompleted: true,
			status: corev1.ContainerStatus{RestartCount: 2, ContainerID: "c2", Image: "registry/app:v2", ImageID: "id2"},
		},
		{
			name: "started, runtime reports a digest instead of the name", state: started, ready: true, wantInProgress: true, wantCompleted: true,
			status: corev1.ContainerStatus{RestartCount: 2, ContainerID: "c2", Image: "sha256:abcd", ImageID: "id2"},
		},
		{
			name: "started, crashed again after the update restart", state: started, status: corev1.ContainerStatus{RestartCount: 3, ContainerID: "c3", Image: "registry/app:v2", ImageID: "id2"},
			wantInProgress: true, wantUpdating: true, wantUnexpectedRestart: true,
		},
		{
			name: "first start after fixing a bad image has no restart", wantInProgress: true, wantCompleted: true, ready: true,
			state:  &State{Revision: "v2", Patched: map[string]PatchedContainer{"main": {Image: "app:v2"}}, Expected: map[string]int32{"main": 1}},
			status: corev1.ContainerStatus{ContainerID: "c1", Image: "docker.io/library/app:v2", ImageID: "id2"},
		},
		{
			name: "patched before first start, old image started meanwhile", ready: true, wantInProgress: true, wantUpdating: true,
			state:  &State{Revision: "v2", Patched: map[string]PatchedContainer{"main": {Image: "app:v2"}}, Expected: map[string]int32{"main": 1}},
			status: corev1.ContainerStatus{ContainerID: "c1", Image: "sha256:old", ImageID: "id1"},
		},
		{
			name: "digest-pinned target matches a tagless runtime report", ready: true, wantInProgress: true, wantCompleted: true,
			state:  &State{Revision: "v2", Patched: map[string]PatchedContainer{"main": {Image: "app:v2@sha256:" + digest, PrevContainerID: "c1", PrevImageID: "id1"}}, Expected: map[string]int32{"main": 2}},
			status: corev1.ContainerStatus{RestartCount: 2, ContainerID: "c2", Image: "docker.io/library/app@sha256:" + digest, ImageID: "id2"},
		},
		{name: "done, only the expected restarts", state: done, ready: true, status: updated},
		{name: "done, crashed afterwards", state: done, ready: true, status: corev1.ContainerStatus{RestartCount: 3}, wantUnexpectedRestart: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := podWithState(t, tt.state, tt.ready, tt.status)
			assert.Equal(t, tt.wantInProgress, InProgress(pod))
			assert.Equal(t, tt.wantCompleted, Completed(pod))
			assert.Equal(t, tt.wantUpdating, Updating(pod))
			assert.Equal(t, tt.wantUnexpectedRestart, UnexpectedRestart(pod))
		})
	}
}

func TestStartKeepsExpectedRestartsAcrossUpdates(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid", ResourceVersion: "7"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a", Image: "a:v2"}, {Name: "b", Image: "b:v1"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "a", RestartCount: 1, ContainerID: "a1", ImageID: "ia2"},
			{Name: "b", ContainerID: "b0", ImageID: "ib1"},
		}},
	}
	pod.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: `{"revision":"v2","startedAt":null,"expected":{"a":1},"done":true}`}
	client := kubefake.NewSimpleClientset(pod)

	started, err := Start(context.Background(), client, pod, map[string]string{"a": "a:v2", "b": "b:v2"}, "v3", time.Now())
	require.NoError(t, err)
	require.True(t, started)

	got, err := client.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
	require.NoError(t, err)
	state := StateOf(got)
	require.NotNil(t, state)
	assert.Equal(t, map[string]int32{"a": 1, "b": 1}, state.Expected, "an earlier update restart stays expected")
	assert.Equal(t, map[string]PatchedContainer{"b": {Image: "b:v2", PrevContainerID: "b0", PrevImageID: "ib1"}}, state.Patched)
	assert.Equal(t, "b:v2", got.Spec.Containers[1].Image)
}

func TestUnexpectedRestartInitContainer(t *testing.T) {
	pod := podWithState(t, nil, true, corev1.ContainerStatus{})
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
	assert.True(t, UnexpectedRestart(pod))

	pod.Status.Phase = corev1.PodSucceeded
	assert.False(t, UnexpectedRestart(pod))
}

func TestTargetImages(t *testing.T) {
	template := func(image string) workloadv1alpha1.PodTemplateSpec {
		return workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}}}
	}
	worker := template("worker:v2")
	ms := &workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{
		Roles: []workloadv1alpha1.Role{{Name: "r", EntryTemplate: template("entry:v2"), WorkerTemplate: &worker}},
	}}}

	tests := []struct {
		name   string
		labels map[string]string
		want   map[string]string
	}{
		{name: "entry pod", labels: map[string]string{workloadv1alpha1.RoleLabelKey: "r", workloadv1alpha1.EntryLabelKey: "true"}, want: map[string]string{"main": "entry:v2"}},
		{name: "worker pod", labels: map[string]string{workloadv1alpha1.RoleLabelKey: "r"}, want: map[string]string{"main": "worker:v2"}},
		{name: "unknown role", labels: map[string]string{workloadv1alpha1.RoleLabelKey: "gone"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}
			assert.Equal(t, tt.want, TargetImages(ms, pod))
		})
	}
}

func TestStateOfMigratesLegacyAnnotation(t *testing.T) {
	annotate := func(raw string) *corev1.Pod {
		pod := podWithState(t, nil, true, corev1.ContainerStatus{RestartCount: 1, ContainerID: "c2", Image: "app:v2", ImageID: "id2"})
		pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey] = raw
		pod.Spec.Containers = []corev1.Container{{Name: "main", Image: "app:v2"}}
		return pod
	}
	tests := []struct {
		name           string
		raw            string
		wantInProgress bool
		wantCompleted  bool
		wantUnexpected bool
		wantRevision   string
	}{
		{
			name:         "finished update keeps its expected restart",
			raw:          `{"version":1,"phase":"Done","targetRevision":"v2","baseline":{"main":{"containerID":"c2","restartCount":1}}}`,
			wantRevision: "v2",
		},
		{
			name:           "unfinished update that already restarted completes",
			raw:            `{"version":1,"phase":"Patched","targetRevision":"v2","baseline":{"main":{"containerID":"c1","restartCount":0}},"patched":{"main":{"image":"app:v2","imageID":"id1"}}}`,
			wantInProgress: true, wantCompleted: true, wantRevision: "v2",
		},
		{
			name:           "unfinished update whose container has not restarted keeps waiting",
			raw:            `{"version":1,"phase":"Patched","targetRevision":"v2","baseline":{"main":{"containerID":"c2","restartCount":1}},"patched":{"main":{"image":"app:v2","imageID":"id2"}}}`,
			wantInProgress: true, wantRevision: "v2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := annotate(tt.raw)
			state := StateOf(pod)
			require.NotNil(t, state)
			assert.Equal(t, tt.wantRevision, state.Revision)
			assert.Equal(t, tt.wantInProgress, InProgress(pod))
			assert.Equal(t, tt.wantCompleted, Completed(pod))
			assert.Equal(t, tt.wantUnexpected, UnexpectedRestart(pod))
		})
	}
}

func TestCompleteDropsUnusedRestartAllowance(t *testing.T) {
	tests := []struct {
		name         string
		restartCount int32
		want         int32
	}{
		{name: "first start needed no restart", restartCount: 0, want: 0},
		{name: "the update restart was used", restartCount: 1, want: 1},
		{name: "extra crash is not absorbed", restartCount: 3, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &State{Revision: "v2", Patched: map[string]PatchedContainer{"main": {Image: "app:v2"}}, Expected: map[string]int32{"main": 1}}
			pod := podWithState(t, state, true, corev1.ContainerStatus{RestartCount: tt.restartCount, ContainerID: "c1"})
			pod.Namespace, pod.Name = "ns", "p"
			client := kubefake.NewSimpleClientset(pod)

			require.NoError(t, Complete(context.Background(), client, pod, "v2", ""))

			got, err := client.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
			require.NoError(t, err)
			done := StateOf(got)
			require.NotNil(t, done)
			assert.True(t, done.Done)
			assert.Equal(t, tt.want, done.Expected["main"])
		})
	}
}

func TestStartBaselinesEarlierRestarts(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "uid", ResourceVersion: "1"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "app:v1"}, {Name: "side", Image: "side:v1"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", RestartCount: 1, ContainerID: "c1", ImageID: "id1"},
				{Name: "side", ContainerID: "s1"},
			},
		},
	}
	client := kubefake.NewSimpleClientset(pod)

	_, err := Start(context.Background(), client, pod, map[string]string{"main": "app:v2", "side": "side:v1"}, "v2", time.Now())
	require.NoError(t, err)
	got, err := client.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
	require.NoError(t, err)
	state := StateOf(got)
	require.NotNil(t, state)
	assert.Equal(t, map[string]int32{"main": 2}, state.Expected, "earlier restart plus the update restart")

	// kubelet restarts main for the update, which is not a crash.
	got.Status = pod.Status
	kubeletRestart := &got.Status.ContainerStatuses[0]
	kubeletRestart.RestartCount, kubeletRestart.ContainerID, kubeletRestart.Image, kubeletRestart.ImageID = 2, "c2", "app:v2", "id2"
	assert.False(t, UnexpectedRestart(got))
	assert.True(t, Completed(got))
}

func TestCompletedRequiresLiveSpecToKeepTargetImage(t *testing.T) {
	state := &State{
		Revision: "v2",
		Patched:  map[string]PatchedContainer{"main": {Image: "registry/app:v2", PrevContainerID: "c1", PrevImageID: "id1"}},
		Expected: map[string]int32{"main": 1},
	}
	// A new container with a changed image ID would pass the runtime checks on its own.
	status := corev1.ContainerStatus{RestartCount: 1, ContainerID: "c2", Image: "registry/app:v3", ImageID: "id3"}
	tests := []struct {
		name       string
		containers []corev1.Container
		want       bool
	}{
		{name: "spec still asks for the target image", containers: []corev1.Container{{Name: "main", Image: "registry/app:v2"}}, want: true},
		{name: "spec was edited to another image", containers: []corev1.Container{{Name: "main", Image: "registry/app:v3"}}},
		{name: "container is gone from the spec", containers: []corev1.Container{{Name: "other", Image: "registry/app:v2"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := podWithState(t, state, true, status)
			pod.Spec.Containers = tt.containers
			assert.Equal(t, tt.want, Completed(pod))
		})
	}
}

func TestMissingContainers(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{
		Roles: []workloadv1alpha1.Role{{Name: "prefill", EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a", Image: "i:2"}, {Name: "b", Image: "j:2"}}},
		}}},
	}}}
	pod := func(names ...string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			workloadv1alpha1.RoleLabelKey: "prefill", workloadv1alpha1.EntryLabelKey: "true",
		}}}
		for _, name := range names {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: name})
		}
		return p
	}
	tests := []struct {
		name string
		pod  *corev1.Pod
		want []string
	}{
		{name: "all containers exist", pod: pod("a", "b")},
		{name: "extra injected container is fine", pod: pod("a", "b", "sidecar")},
		{name: "renamed container", pod: pod("a", "renamed"), want: []string{"b"}},
		{name: "all missing", pod: pod("x"), want: []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MissingContainers(ms, tt.pod))
		})
	}
}

func TestImagesToPatch(t *testing.T) {
	pod := func(images map[string]string, state *State) *corev1.Pod {
		p := podWithState(t, state, true, corev1.ContainerStatus{})
		p.Spec.Containers = nil
		for _, name := range []string{"main", "sidecar"} {
			if image, ok := images[name]; ok {
				p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: name, Image: image})
			}
		}
		return p
	}
	target := map[string]string{"main": "app:v2", "sidecar": "side:v1"}
	source := map[string]string{"main": "app:v1", "sidecar": "side:v1"}

	tests := []struct {
		name       string
		pod        *corev1.Pod
		target     map[string]string
		source     map[string]string
		wantImages map[string]string
		wantDrift  []string
	}{
		{name: "changed container on the source image", pod: pod(source, nil), target: target, source: source, wantImages: map[string]string{"main": "app:v2"}},
		{name: "unchanged container rewritten by a webhook is kept", pod: pod(map[string]string{"main": "app:v1", "sidecar": "side:injected"}, nil), target: target, source: source, wantImages: map[string]string{"main": "app:v2"}},
		{name: "changed container rewritten by a webhook is drift", pod: pod(map[string]string{"main": "app:injected", "sidecar": "side:v1"}, nil), target: target, source: source, wantImages: map[string]string{}, wantDrift: []string{"main"}},
		{
			name: "container patched by an unfinished update is patched again", target: map[string]string{"main": "app:v3"}, source: source,
			pod:        pod(map[string]string{"main": "app:v2"}, &State{Revision: "v2", Patched: map[string]PatchedContainer{"main": {Image: "app:v2"}}}),
			wantImages: map[string]string{"main": "app:v3"},
		},
		{
			name: "earlier patched container edited externally is drift", target: map[string]string{"main": "app:v3"}, source: source,
			pod:        pod(map[string]string{"main": "app:hotfix"}, &State{Revision: "v2", Patched: map[string]PatchedContainer{"main": {Image: "app:v2"}}}),
			wantImages: map[string]string{}, wantDrift: []string{"main"},
		},
		{
			name: "container already at the target without an update of ours is drift", target: target, source: source,
			pod:        pod(map[string]string{"main": "app:v2", "sidecar": "side:v1"}, nil),
			wantImages: map[string]string{}, wantDrift: []string{"main"},
		},
		{name: "no source patches only earlier patched containers", pod: pod(source, nil), target: target, wantImages: map[string]string{}, wantDrift: []string{"main"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			images, drift := ImagesToPatch(tt.pod, tt.target, tt.source)
			assert.Equal(t, tt.wantImages, images)
			assert.Equal(t, tt.wantDrift, drift)
		})
	}
}

func TestOnTargetIgnoresCrashes(t *testing.T) {
	state := &State{
		Revision: "v2",
		Patched:  map[string]PatchedContainer{"main": {Image: "registry/app:v2", PrevContainerID: "c1", PrevImageID: "id1"}},
		Expected: map[string]int32{"main": 2},
	}
	pod := podWithState(t, state, true, corev1.ContainerStatus{RestartCount: 3, ContainerID: "c3", Image: "registry/app:v2", ImageID: "id2"})
	assert.True(t, OnTarget(pod))
	assert.False(t, Completed(pod))
}
