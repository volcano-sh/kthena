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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func decodePatch(t *testing.T, data []byte) []jsonPatchOp {
	t.Helper()
	var ops []jsonPatchOp
	require.NoError(t, json.Unmarshal(data, &ops))
	return ops
}

func TestBuildPatch(t *testing.T) {
	state := newTestState(PhasePatched)
	stateValue, err := state.Encode()
	require.NoError(t, err)
	stateKey := "/metadata/annotations/modelserving.volcano.sh~1inplace-update-state"

	tests := []struct {
		name    string
		pod     func() *corev1.Pod
		opts    PatchOptions
		want    []jsonPatchOp
		wantErr string
	}{
		{
			name: "changed image is guarded and replaced, unchanged image is left alone",
			pod: func() *corev1.Pod {
				pod := newTestPod()
				pod.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
				pod.Annotations = map[string]string{}
				return pod
			},
			opts: PatchOptions{
				Images: map[string]string{"main": "example.com/engine:v2", "sidecar": "example.com/sidecar:v1"},
				Labels: map[string]string{workloadv1alpha1.RevisionLabelKey: "rev-2"},
				State:  state,
			},
			want: []jsonPatchOp{
				{Op: "test", Path: "/metadata/uid", Value: string(testPodUID)},
				{Op: "test", Path: "/metadata/resourceVersion", Value: "1"},
				{Op: "test", Path: "/spec/restartPolicy", Value: "Always"},
				{Op: "test", Path: "/spec/containers/0/name", Value: "main"},
				{Op: "test", Path: "/spec/containers/0/image", Value: "example.com/engine:v1"},
				{Op: "test", Path: "/spec/containers/0/imagePullPolicy", Value: "IfNotPresent"},
				{Op: "replace", Path: "/spec/containers/0/image", Value: "example.com/engine:v2"},
				{Op: "add", Path: "/metadata/labels/modelserving.volcano.sh~1revision", Value: "rev-2"},
				{Op: "add", Path: stateKey, Value: stateValue},
			},
		},
		{
			name: "state only patch adds a missing annotations map as a whole",
			pod:  newTestPod,
			opts: PatchOptions{State: state},
			want: []jsonPatchOp{
				{Op: "test", Path: "/metadata/uid", Value: string(testPodUID)},
				{Op: "test", Path: "/metadata/resourceVersion", Value: "1"},
				{Op: "add", Path: "/metadata/annotations", Value: map[string]interface{}{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: stateValue}},
			},
		},
		{
			name: "images that already match need no restart policy guard",
			pod: func() *corev1.Pod {
				pod := newTestPod()
				pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
				pod.Annotations = map[string]string{}
				return pod
			},
			opts: PatchOptions{Images: map[string]string{"main": "example.com/engine:v1"}, State: state},
			want: []jsonPatchOp{
				{Op: "test", Path: "/metadata/uid", Value: string(testPodUID)},
				{Op: "test", Path: "/metadata/resourceVersion", Value: "1"},
				{Op: "add", Path: stateKey, Value: stateValue},
			},
		},
		{
			name: "restart policy other than Always is rejected",
			pod: func() *corev1.Pod {
				pod := newTestPod()
				pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
				return pod
			},
			opts:    PatchOptions{Images: map[string]string{"main": "example.com/engine:v2"}, State: state},
			wantErr: `has restartPolicy "OnFailure"`,
		},
		{
			name:    "unknown container is rejected",
			pod:     newTestPod,
			opts:    PatchOptions{Images: map[string]string{"missing": "example.com/engine:v2"}, State: state},
			wantErr: `has no regular container "missing"`,
		},
		{
			name:    "state is required",
			pod:     newTestPod,
			opts:    PatchOptions{},
			wantErr: "state is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := BuildPatch(tt.pod(), tt.opts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			want, err := json.Marshal(tt.want)
			require.NoError(t, err)
			assert.Equal(t, decodePatch(t, want), decodePatch(t, data))
		})
	}
}
