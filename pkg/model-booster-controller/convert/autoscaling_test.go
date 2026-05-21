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
	"os"
	"testing"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/yaml"
)

func TestBuildScalingPolicyBinding(t *testing.T) {
	tests := []struct {
		name     string
		input    *workload.ModelBooster
		expected *workload.AutoscalingPolicyBinding
	}{
		{
			name:     "simple backend",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			expected: loadYaml[workload.AutoscalingPolicyBinding](t, "testdata/expected/scaling-asp-binding.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := tt.input.Spec.Backend
			got := BuildScalingPolicyBinding(tt.input, &backend, utils.GetBackendResourceName(tt.input.Name, backend.Name))
			tt.expected.Labels = got.Labels
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildAutoscalingPolicy(t *testing.T) {
	tests := []struct {
		name     string
		input    *workload.ModelBooster
		expected *workload.AutoscalingPolicy
	}{
		{
			name:     "simple-backend",
			input:    loadYaml[workload.ModelBooster](t, "testdata/input/model.yaml"),
			expected: loadYaml[workload.AutoscalingPolicy](t, "testdata/expected/scaling-asp.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.input.Spec.AutoscalingPolicy != nil {
				got := BuildAutoscalingPolicy(tt.input.Spec.AutoscalingPolicy, tt.input, "")
				tt.expected.Labels = got.Labels
				assert.Equal(t, tt.expected, got)
			}
		})
	}
}

// loadYaml transfer yaml data into a struct of type T.
// Used for test.
func loadYaml[T any](t *testing.T, path string) *T {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read YAML: %v", err)
	}
	var expected T
	if err := yaml.Unmarshal(data, &expected); err != nil {
		t.Fatalf("Failed to unmarshal YAML: %v", err)
	}
	return &expected
}
