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
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{
			name:  "vLLM with affinity",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/model-with-affinity.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					affinity := role.EntryTemplate.Spec.Affinity
					assert.NotNil(t, affinity.NodeAffinity, "role %s entryTemplate should have nodeAffinity", role.Name)
					req := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
					assert.NotNil(t, req, "role %s entryTemplate should have requiredDuringSchedulingIgnoredDuringExecution", role.Name)
					assert.Len(t, req.NodeSelectorTerms, 1)
					assert.Equal(t, "gpu-type", req.NodeSelectorTerms[0].MatchExpressions[0].Key)
					if role.WorkerReplicas > 0 {
						workerAffinity := role.WorkerTemplate.Spec.Affinity
						assert.NotNil(t, workerAffinity.NodeAffinity, "role %s workerTemplate should have nodeAffinity", role.Name)
					}
				}
			},
		},
		{
			name:  "PD disaggregated with different affinities per role",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-with-affinity.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				require.Len(t, got.Spec.Template.Roles, 2)
				prefillRole := got.Spec.Template.Roles[0]
				decodeRole := got.Spec.Template.Roles[1]

				// prefill: only nodeAffinity
				prefillAffinity := prefillRole.EntryTemplate.Spec.Affinity
				assert.NotNil(t, prefillAffinity.NodeAffinity, "prefill should have nodeAffinity")
				assert.Nil(t, prefillAffinity.PodAntiAffinity, "prefill should not have podAntiAffinity")
				prefillReq := prefillAffinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
				assert.Equal(t, "a100", prefillReq.NodeSelectorTerms[0].MatchExpressions[0].Values[0])

				// decode: nodeAffinity + podAntiAffinity
				decodeAffinity := decodeRole.EntryTemplate.Spec.Affinity
				assert.NotNil(t, decodeAffinity.NodeAffinity, "decode should have nodeAffinity")
				assert.NotNil(t, decodeAffinity.PodAntiAffinity, "decode should have podAntiAffinity")
				decodeReq := decodeAffinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
				assert.Equal(t, "h100", decodeReq.NodeSelectorTerms[0].MatchExpressions[0].Values[0])
				assert.Len(t, decodeAffinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, 1)
			},
		},
		{
			name:  "vLLM without affinity renders empty affinity",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					affinity := role.EntryTemplate.Spec.Affinity
					assert.NotNil(t, affinity, "role %s entryTemplate should have non-nil (empty) affinity when unset", role.Name)
					assert.Nil(t, affinity.NodeAffinity, "role %s entryTemplate should have nil NodeAffinity", role.Name)
					assert.Nil(t, affinity.PodAffinity, "role %s entryTemplate should have nil PodAffinity", role.Name)
					assert.Nil(t, affinity.PodAntiAffinity, "role %s entryTemplate should have nil PodAntiAffinity", role.Name)
					if role.WorkerReplicas > 0 {
						workerAffinity := role.WorkerTemplate.Spec.Affinity
						assert.NotNil(t, workerAffinity, "role %s workerTemplate should have non-nil (empty) affinity when unset", role.Name)
						assert.Nil(t, workerAffinity.NodeAffinity, "role %s workerTemplate should have nil NodeAffinity", role.Name)
					}
				}
			},
		},
		{
			name:  "vLLM with tolerations",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/model-with-nodeselector-tolerations.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					assert.Len(t, role.EntryTemplate.Spec.Tolerations, 2,
						"role %s entryTemplate should have 2 tolerations", role.Name)
					assert.Equal(t, "nvidia.com/gpu", role.EntryTemplate.Spec.Tolerations[0].Key)
					assert.Equal(t, corev1.TolerationOpExists, role.EntryTemplate.Spec.Tolerations[0].Operator)
					assert.Equal(t, corev1.TaintEffectNoSchedule, role.EntryTemplate.Spec.Tolerations[0].Effect)
					assert.Equal(t, "dedicated", role.EntryTemplate.Spec.Tolerations[1].Key)
					assert.Equal(t, "inference", role.EntryTemplate.Spec.Tolerations[1].Value)
					assert.Equal(t, corev1.TaintEffectNoSchedule, role.EntryTemplate.Spec.Tolerations[1].Effect)
					if role.WorkerReplicas > 0 {
						assert.Len(t, role.WorkerTemplate.Spec.Tolerations, 2,
							"role %s workerTemplate should have 2 tolerations", role.Name)
						assert.Equal(t, "nvidia.com/gpu", role.WorkerTemplate.Spec.Tolerations[0].Key)
						assert.Equal(t, corev1.TolerationOpExists, role.WorkerTemplate.Spec.Tolerations[0].Operator)
						assert.Equal(t, corev1.TaintEffectNoSchedule, role.WorkerTemplate.Spec.Tolerations[0].Effect)
						assert.Equal(t, "dedicated", role.WorkerTemplate.Spec.Tolerations[1].Key)
						assert.Equal(t, "inference", role.WorkerTemplate.Spec.Tolerations[1].Value)
						assert.Equal(t, corev1.TaintEffectNoSchedule, role.WorkerTemplate.Spec.Tolerations[1].Effect)
					}
				}
			},
		},
		{
			name:  "vLLM without tolerations is nil",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				for _, role := range got.Spec.Template.Roles {
					assert.Nil(t, role.EntryTemplate.Spec.Tolerations,
						"role %s entryTemplate should have nil tolerations", role.Name)
					assert.Nil(t, role.WorkerTemplate.Spec.Tolerations,
						"role %s workerTemplate should have nil tolerations", role.Name)
				}
			},
		},
		{
			name:  "PD disaggregated with tolerations",
			input: loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-with-nodeselector-tolerations.yaml"),
			checkFn: func(t *testing.T, got *workload.ModelServing) {
				rolesByName := map[string]workload.Role{}
				for _, role := range got.Spec.Template.Roles {
					rolesByName[role.Name] = role
				}
				// prefill role
				prefill, ok := rolesByName["prefill"]
				assert.True(t, ok, "prefill role should exist")
				assert.Len(t, prefill.EntryTemplate.Spec.Tolerations, 1)
				assert.Equal(t, "nvidia.com/gpu", prefill.EntryTemplate.Spec.Tolerations[0].Key)
				if prefill.WorkerReplicas > 0 {
					assert.Len(t, prefill.WorkerTemplate.Spec.Tolerations, 1)
					assert.Equal(t, "nvidia.com/gpu", prefill.WorkerTemplate.Spec.Tolerations[0].Key)
				}
				// decode role
				decode, ok := rolesByName["decode"]
				assert.True(t, ok, "decode role should exist")
				assert.Len(t, decode.EntryTemplate.Spec.Tolerations, 2)
				assert.Equal(t, "dedicated", decode.EntryTemplate.Spec.Tolerations[1].Key)
				assert.Equal(t, "inference", decode.EntryTemplate.Spec.Tolerations[1].Value)
				assert.Equal(t, corev1.TaintEffectPreferNoSchedule, decode.EntryTemplate.Spec.Tolerations[1].Effect)
				if decode.WorkerReplicas > 0 {
					assert.Len(t, decode.WorkerTemplate.Spec.Tolerations, 2)
					assert.Equal(t, "nvidia.com/gpu", decode.WorkerTemplate.Spec.Tolerations[0].Key)
					assert.Equal(t, "dedicated", decode.WorkerTemplate.Spec.Tolerations[1].Key)
					assert.Equal(t, "inference", decode.WorkerTemplate.Spec.Tolerations[1].Value)
					assert.Equal(t, corev1.TaintEffectPreferNoSchedule, decode.WorkerTemplate.Spec.Tolerations[1].Effect)
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

func TestBuildModelServingVLLMPreStopHandlesUnavailableMetrics(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml")
	serving, err := BuildModelServing(model)
	require.NoError(t, err)
	require.NotEmpty(t, serving.Spec.Template.Roles)

	var engine *corev1.Container
	for roleIndex := range serving.Spec.Template.Roles {
		for containerIndex := range serving.Spec.Template.Roles[roleIndex].EntryTemplate.Spec.Containers {
			container := &serving.Spec.Template.Roles[roleIndex].EntryTemplate.Spec.Containers[containerIndex]
			if container.Name == "engine" {
				engine = container
				break
			}
		}
		if engine != nil {
			break
		}
	}
	require.NotNil(t, engine)
	require.NotNil(t, engine.Lifecycle)
	require.NotNil(t, engine.Lifecycle.PreStop)
	require.NotNil(t, engine.Lifecycle.PreStop.Exec)
	require.Len(t, engine.Lifecycle.PreStop.Exec.Command, 3)

	script := engine.Lifecycle.PreStop.Exec.Command[2]
	assert.Contains(t, script, "curl -s --max-time 2 http://localhost:8000/metrics")
	assert.Contains(t, script, `if [ -z "$RUNNING" ]; then`)
	assert.Contains(t, script, "Metrics endpoint unavailable, exiting preStop")
	assert.NotContains(t, script, "$ERR")
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

func TestBuildModelServingSchedulerNameSerialization(t *testing.T) {
	t.Run("omits empty schedulerName", func(t *testing.T) {
		model := loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml")
		serving, err := BuildModelServing(model)
		require.NoError(t, err)

		data, err := json.Marshal(serving)
		require.NoError(t, err)
		assert.NotContains(t, string(data), `"schedulerName"`)
	})

	t.Run("keeps explicit schedulerName", func(t *testing.T) {
		model := loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml")
		model.Spec.Backend.SchedulerName = "volcano"

		serving, err := BuildModelServing(model)
		require.NoError(t, err)

		data, err := json.Marshal(serving)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"schedulerName":"volcano"`)
	})
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
