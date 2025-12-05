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

package convert

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestGetMountPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal case",
			input:    "models/llama-2-7b",
			expected: "/8590cc9fef9361779a5bd7862eb82b6d",
		},
		{
			name:     "empty modelURI",
			input:    "",
			expected: "/d41d8cd98f00b204e9800998ecf8427e",
		},
		{
			name:     "special characters",
			input:    "model_@#$",
			expected: "/1f8d57abec22d679835ba0c38f634b06",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetMountPath(tt.input); got != tt.expected {
				t.Errorf("GetMountPath() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestGetCachePath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal case",
			input:    "pvc://my-cache-path",
			expected: "my-cache-path",
		},
		{
			name:     "empty cache path",
			input:    "",
			expected: "",
		},
		{
			name:     "invalid cache path",
			input:    "invalidpath",
			expected: "",
		},
		{
			name:     "multiple separators",
			input:    "pvc://path/with/multiple/separators",
			expected: "path/with/multiple/separators",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetCachePath(tt.input); got != tt.expected {
				t.Errorf("GetCachePath() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCreateModelServingResources(t *testing.T) {
	tests := []struct {
		name         string
		input        *workload.ModelBooster
		expected     *workload.ModelServing
		expectErrMsg string
	}{
		{
			name:     "CacheVolume_HuggingFace_HostPath",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/model-serving.yaml"),
		},
		{
			name:     "PD disaggregation",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/disaggregated-model-serving.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildModelServing(tt.input)
			if tt.expectErrMsg != "" {
				assert.Contains(t, err.Error(), tt.expectErrMsg)
				return
			} else {
				assert.NoError(t, err)
			}
			diff := cmp.Diff(tt.expected, got)
			if diff != "" {
				t.Errorf("ModelServing mismatch (-expected +actual):\n%s", diff)
			}
		})
	}
}

func TestBuildCacheVolume(t *testing.T) {
	tests := []struct {
		name         string
		input        *workload.ModelBackend
		expected     *corev1.Volume
		expectErrMsg string
	}{
		{
			name: "empty cache URI",
			input: &workload.ModelBackend{
				Name:     "test-backend",
				CacheURI: "",
			},
			expected: &corev1.Volume{
				Name: "test-backend-weights",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		},
		{
			name: "PVC URI",
			input: &workload.ModelBackend{
				Name:     "test-backend",
				CacheURI: "pvc://test-pvc",
			},
			expected: &corev1.Volume{
				Name: "test-backend-weights",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "test-pvc",
					},
				},
			},
		},
		{
			name: "HostPath URI",
			input: &workload.ModelBackend{
				Name:     "test-backend",
				CacheURI: "hostpath://test/path",
			},
			expected: &corev1.Volume{
				Name: "test-backend-weights",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "test/path",
						Type: ptr.To(corev1.HostPathDirectoryOrCreate),
					},
				},
			},
		},
		{
			name: "invalid URI",
			input: &workload.ModelBackend{
				Name:     "test-backend",
				CacheURI: "hostPath://invalid/path",
			},
			expectErrMsg: "not support prefix in CacheURI: hostPath://invalid/path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildCacheVolume(tt.input)
			if len(tt.expectErrMsg) != 0 {
				assert.Contains(t, err.Error(), tt.expectErrMsg)
				return
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expected, got)
		})
	}
}
