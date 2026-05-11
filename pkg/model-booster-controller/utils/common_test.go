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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func makeTestModelBooster(name, uid string) *workloadv1alpha1.ModelBooster {
	return &workloadv1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(uid),
		},
	}
}

func TestTryGetField(t *testing.T) {
	tests := []struct {
		name     string
		config   []byte
		key      string
		expected interface{}
		wantErr  bool
	}{
		{
			name:     "ExistingStringField",
			config:   []byte(`{"model": "deepseek-r1"}`),
			key:      "model",
			expected: "deepseek-r1",
		},
		{
			name:     "MissingField",
			config:   []byte(`{"model": "deepseek-r1"}`),
			key:      "missing",
			expected: nil,
		},
		{
			name:     "ExistingNumberField",
			config:   []byte(`{"port": 8080}`),
			key:      "port",
			expected: float64(8080),
		},
		{
			name:    "InvalidJSON",
			config:  []byte(`invalid`),
			key:     "model",
			wantErr: true,
		},
		{
			name:     "ExistingBoolField",
			config:   []byte(`{"enabled": true}`),
			key:      "enabled",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := TryGetField(tt.config, tt.key)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestGetDeviceNum(t *testing.T) {
	tests := []struct {
		name     string
		worker   *workloadv1alpha1.ModelWorker
		expected int64
	}{
		{
			name: "SingleNvidiaGPU",
			worker: &workloadv1alpha1.ModelWorker{
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						"nvidia.com/gpu": resource.MustParse("2"),
					},
				},
			},
			expected: 2,
		},
		{
			name: "SingleAscendNPU",
			worker: &workloadv1alpha1.ModelWorker{
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						"huawei.com/ascend-1980": resource.MustParse("4"),
					},
				},
			},
			expected: 4,
		},
		{
			name: "MultipleXPUs",
			worker: &workloadv1alpha1.ModelWorker{
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						"nvidia.com/gpu":         resource.MustParse("2"),
						"huawei.com/ascend-1980": resource.MustParse("2"),
					},
				},
			},
			expected: 4,
		},
		{
			name: "NoLimits",
			worker: &workloadv1alpha1.ModelWorker{
				Resources: corev1.ResourceRequirements{},
			},
			expected: 0,
		},
		{
			name: "NoXPUResources",
			worker: &workloadv1alpha1.ModelWorker{
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						"cpu":    resource.MustParse("4"),
						"memory": resource.MustParse("8Gi"),
					},
				},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetDeviceNum(tt.worker)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNewModelOwnerRef(t *testing.T) {
	// Verifies that NewModelOwnerRef returns an OwnerReference with the correct fields

	model := makeTestModelBooster("test-model", "test-uid-123")
	ref := NewModelOwnerRef(model)

	assert.Equal(t, "test-model", ref.Name)
	assert.Equal(t, types.UID("test-uid-123"), ref.UID)
	assert.Equal(t, workloadv1alpha1.ModelKind.Kind, ref.Kind)
	assert.True(t, *ref.Controller)
	assert.True(t, *ref.BlockOwnerDeletion)
}
