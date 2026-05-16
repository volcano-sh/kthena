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
	"k8s.io/apimachinery/pkg/api/resource"
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
			name:     "PD disaggregation NPU",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-npu.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/disaggregated-model-serving.yaml"),
		},
		{
			name:     "PD disaggregation Mooncake",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/pd-disaggregated-model-mooncake.yaml"),
			expected: loadYaml[workload.ModelServing](t, "testdata/expected/disaggregated-model-serving-mooncake.yaml"),
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

func TestBuildVllmModelServingUsesServerWorkerConfig(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml")
	serverWorker := model.Spec.Backend.Workers[0]
	serverWorker.Image = "vllm-server:server"
	serverWorker.Replicas = 2
	serverWorker.Pods = 3
	serverWorker.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	serverWorker.Config.Raw = []byte(`{"served-model-name":"server-model","max-model-len":222}`)

	prefillWorker := serverWorker
	prefillWorker.Type = workload.ModelWorkerTypePrefill
	prefillWorker.Image = "vllm-server:prefill"
	prefillWorker.Replicas = 9
	prefillWorker.Pods = 7
	prefillWorker.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	}
	prefillWorker.Config.Raw = []byte(`{"served-model-name":"prefill-model","max-model-len":111}`)

	model.Spec.Backend.Workers = []workload.ModelWorker{prefillWorker, serverWorker}

	serving, err := BuildModelServing(model)
	assert.NoError(t, err)

	role := modelServingRole(t, serving, "leader")
	if role.Replicas == nil {
		t.Fatal("leader role replicas not set")
	}
	if serving.Spec.Template.GangPolicy == nil {
		t.Fatal("leader gang policy not set")
	}
	assert.Equal(t, int32(2), *role.Replicas)
	assert.Equal(t, int32(2), serving.Spec.Template.GangPolicy.MinRoleReplicas["leader"])
	assert.Equal(t, int32(2), role.WorkerReplicas)

	engine := entryContainer(t, role, "engine")
	assert.Equal(t, "vllm-server:server", engine.Image)
	assert.Equal(t, serverWorker.Resources, engine.Resources)

	command := strings.Join(engine.Command, " ")
	assert.Contains(t, command, "--served-model-name server-model")
	assert.Contains(t, command, "--max-model-len 222")
	assert.Contains(t, command, "--ray_cluster_size=3")
	assert.NotContains(t, command, "prefill-model")
	assert.NotContains(t, command, "--max-model-len 111")

	worker := workerContainer(t, role)
	assert.Equal(t, "vllm-server:server", worker.Image)
	assert.Equal(t, serverWorker.Resources, worker.Resources)
}

func TestBuildVllmModelServingRequiresServerWorker(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml")
	prefillWorker := model.Spec.Backend.Workers[0]
	prefillWorker.Type = workload.ModelWorkerTypePrefill
	model.Spec.Backend.Workers = []workload.ModelWorker{prefillWorker}

	serving, err := BuildModelServing(model)

	assert.Nil(t, serving)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "server worker not found")
	}
}

func modelServingRole(t *testing.T, serving *workload.ModelServing, name string) workload.Role {
	t.Helper()

	for _, role := range serving.Spec.Template.Roles {
		if role.Name == name {
			return role
		}
	}
	t.Fatalf("role %q not found", name)
	return workload.Role{}
}

func entryContainer(t *testing.T, role workload.Role, name string) corev1.Container {
	t.Helper()

	for _, container := range role.EntryTemplate.Spec.Containers {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("entry container %q not found", name)
	return corev1.Container{}
}

func workerContainer(t *testing.T, role workload.Role) corev1.Container {
	t.Helper()

	if role.WorkerTemplate == nil {
		t.Fatal("worker template not found")
	}
	if len(role.WorkerTemplate.Spec.Containers) == 0 {
		t.Fatal("worker container not found")
	}
	return role.WorkerTemplate.Spec.Containers[0]
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
						ClaimName: "/test-pvc",
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
