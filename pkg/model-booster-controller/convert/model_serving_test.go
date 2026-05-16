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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/env"
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
			name:     "normal case1",
			input:    "pvc://my-cache-path////",
			expected: "/my-cache-path",
		},
		{
			name:     "normal case2",
			input:    "pvc:///my-cache-path",
			expected: "/my-cache-path",
		},
		{
			name:     "normal case3",
			input:    "pvc:////my-cache-path",
			expected: "/my-cache-path",
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
			expected: "/path/with/multiple/separators",
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

func TestGetPVCClaimName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal pvc URI",
			input:    "pvc://my-pvc",
			expected: "my-pvc",
		},
		{
			name:     "extra leading slashes",
			input:    "pvc:///my-pvc",
			expected: "my-pvc",
		},
		{
			name:     "trailing slash",
			input:    "pvc://my-pvc/",
			expected: "my-pvc",
		},
		{
			name:     "multiple surrounding slashes",
			input:    "pvc:////my-pvc////",
			expected: "my-pvc",
		},
		{
			name:     "empty",
			input:    "",
			expected: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetPVCClaimName(tt.input); got != tt.expected {
				t.Errorf("GetPVCClaimName() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCreateModelServingResources(t *testing.T) {
	tests := []struct {
		name         string
		input        *workload.ModelBooster
		expected     *workload.ModelServing
		checkFn      func(*testing.T, *workload.ModelServing)
		expectErrMsg string
	}{
		{
			name:     "CacheVolume_HuggingFace_HostPath",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/model-serving.yaml"),
		},
		{
			name:     "PD disaggregation NPU",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-npu.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/disaggregated-model-serving.yaml"),
		},
		{
			name:     "PD disaggregation Mooncake",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-mooncake.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/disaggregated-model-serving-mooncake.yaml"),
		},
		{
			name:     "SGLang aggregated",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/sglang-aggregated-model.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/sglang-aggregated-model-serving.yaml"),
		},
		{
			name:     "SGLang disaggregated",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/sglang-disaggregated-model.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/sglang-disaggregated-model-serving.yaml"),
		},
		{
			name:  "vLLM with runtimeClassName",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/model-with-runtimeclass.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					assert.Equal(t, ptr.To("nvidia"), role.EntryTemplate.Spec.RuntimeClassName,
						"role %s entryTemplate should have runtimeClassName", role.Name)
					if role.WorkerReplicas > 0 {
						assert.Equal(t, ptr.To("nvidia"), role.WorkerTemplate.Spec.RuntimeClassName,
							"role %s workerTemplate should have runtimeClassName", role.Name)
					}
				}
			},
		},
		{
			name:  "PD disaggregated with runtimeClassName",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-with-runtimeclass.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					assert.Equal(t, ptr.To("nvidia"), role.EntryTemplate.Spec.RuntimeClassName,
						"role %s entryTemplate should have runtimeClassName", role.Name)
				}
			},
		},
		{
			name:  "vLLM without runtimeClassName is nil",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					assert.Nil(t, role.EntryTemplate.Spec.RuntimeClassName,
						"role %s entryTemplate should have nil runtimeClassName", role.Name)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildModelServing(tt.input)
			if tt.expectErrMsg != "" {
				assert.Contains(t, err.Error(), tt.expectErrMsg)
				return
			}
			assert.NoError(t, err)
			if tt.checkFn != nil {
				tt.checkFn(t, got)
				return
			}
			diff := cmp.Diff(tt.expected, got)
			if diff != "" {
				t.Errorf("ModelServing mismatch (-expected +actual):\n%s", diff)
			}
		})
	}
}

func TestBuildModelServingSkipEngineDependencyInstall(t *testing.T) {
	tests := []struct {
		name              string
		mutateModel       func(*workload.ModelBooster)
		connectorFragment string
	}{
		{
			name:              "MooncakeConnector",
			mutateModel:       func(*workload.ModelBooster) {},
			connectorFragment: "mooncake-transfer-engine",
		},
		{
			name: "NixlConnector",
			mutateModel: func(model *workload.ModelBooster) {
				for i := range model.Spec.Backend.Workers {
					model.Spec.Backend.Workers[i].Config.Raw = []byte(strings.ReplaceAll(
						string(model.Spec.Backend.Workers[i].Config.Raw),
						"MooncakeConnector",
						"NixlConnector",
					))
				}
			},
			connectorFragment: "nixl &&",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-mooncake.yaml")
			model.Spec.Backend.Env = append(model.Spec.Backend.Env, corev1.EnvVar{
				Name:  env.SkipEngineDependencyInstall,
				Value: "true",
			})
			tt.mutateModel(model)

			serving, err := BuildModelServing(model)
			assert.NoError(t, err)

			for _, role := range serving.Spec.Template.Roles {
				for _, container := range role.EntryTemplate.Spec.Containers {
					if container.Name != "vllm" {
						continue
					}
					command := strings.Join(container.Command, " ")
					assert.NotContains(t, command, "pip install", "role %s should not install engine dependencies at startup", role.Name)
					assert.NotContains(t, command, tt.connectorFragment, "role %s should use the prebuilt engine image dependencies", role.Name)
				}
			}
		})
	}
}

func TestBuildSGLangModelServing(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/sglang-aggregated-model.yaml")
	serving, err := BuildModelServing(model)
	assert.NoError(t, err)
	assert.NotNil(t, serving)
	assert.Len(t, serving.Spec.Template.Roles, 1, "aggregated SGLang produces a single 'leader' role")

	role := serving.Spec.Template.Roles[0]
	assert.Equal(t, "leader", role.Name)
	assert.Equal(t, int32(0), role.WorkerReplicas)

	containers := role.EntryTemplate.Spec.Containers
	assert.Len(t, containers, 2, "expected runtime sidecar + engine container")

	var engine *corev1.Container
	for i := range containers {
		if containers[i].Name == "engine" {
			engine = &containers[i]
			break
		}
	}
	assert.NotNil(t, engine, "engine container must be present")
	if engine == nil {
		return
	}
	command := strings.Join(engine.Command, " ")
	assert.Contains(t, command, "sglang.launch_server")
	// Bind 0.0.0.0 so the runtime sidecar and preStop drain can reach the
	// engine on localhost:30000.
	assert.Contains(t, command, "--host 0.0.0.0")
	assert.NotContains(t, command, "$(POD_IP)")
	assert.Contains(t, command, "--port 30000")
	assert.Contains(t, command, "--enable-metrics")
	assert.Contains(t, command, "--mem-fraction-static 0.85")
	assert.NotContains(t, command, "VLLM_USE_V1")

	for _, e := range engine.Env {
		assert.NotEqual(t, "POD_IP", e.Name, "POD_IP env is unused once SGLang binds 0.0.0.0")
		assert.NotEqual(t, "VLLM_USE_V1", e.Name)
	}
}

func TestBuildSGLangDisaggregatedModelServing(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/sglang-disaggregated-model.yaml")
	serving, err := BuildModelServing(model)
	assert.NoError(t, err)
	assert.NotNil(t, serving)
	assert.Len(t, serving.Spec.Template.Roles, 2)

	roleByName := map[string]*workload.Role{}
	for i, r := range serving.Spec.Template.Roles {
		roleByName[r.Name] = &serving.Spec.Template.Roles[i]
	}
	for _, expectedRole := range []string{"prefill", "decode"} {
		r, ok := roleByName[expectedRole]
		assert.True(t, ok, "role %q not found", expectedRole)
		if !ok {
			continue
		}
		var engine *corev1.Container
		for i := range r.EntryTemplate.Spec.Containers {
			if r.EntryTemplate.Spec.Containers[i].Name == "sglang" {
				engine = &r.EntryTemplate.Spec.Containers[i]
				break
			}
		}
		assert.NotNil(t, engine, "role %q must have an sglang engine container", expectedRole)
		if engine == nil {
			continue
		}
		command := strings.Join(engine.Command, " ")
		assert.Contains(t, command, "--disaggregation-mode "+expectedRole)
		assert.Contains(t, command, "--disaggregation-transfer-backend mooncake")
		assert.Contains(t, command, "--host 0.0.0.0")
		assert.NotContains(t, command, "$(POD_IP)")

		for _, e := range engine.Env {
			assert.NotEqual(t, "POD_IP", e.Name, "role %q: POD_IP env is unused once SGLang binds 0.0.0.0", expectedRole)
		}
	}
}

func TestBuildSGLangModelServerRouting(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/sglang-aggregated-model.yaml")
	servers, err := BuildModelServer(model)
	assert.NoError(t, err)
	assert.Len(t, servers, 1)
	spec := servers[0].Spec
	assert.Equal(t, "SGLang", string(spec.InferenceEngine))
	assert.Equal(t, int32(30000), spec.WorkloadPort.Port)
	assert.Nil(t, spec.KVConnector)
	assert.Nil(t, spec.WorkloadSelector.PDGroup)
}

func TestBuildSGLangDisaggregatedModelServerRouting(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/sglang-disaggregated-model.yaml")
	servers, err := BuildModelServer(model)
	assert.NoError(t, err)
	assert.Len(t, servers, 1)
	spec := servers[0].Spec
	assert.Equal(t, "SGLang", string(spec.InferenceEngine))
	assert.Equal(t, int32(30000), spec.WorkloadPort.Port)
	assert.Nil(t, spec.KVConnector)
	assert.NotNil(t, spec.WorkloadSelector.PDGroup)
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
			name: "PVC URI with extra slashes",
			input: &workload.ModelBackend{
				Name:     "test-backend",
				CacheURI: "pvc:///test-pvc",
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
						Path: "/test/path",
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
