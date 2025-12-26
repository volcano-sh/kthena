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

	"github.com/stretchr/testify/assert"
	networking "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	registry "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildModelServer(t *testing.T) {
	tests := []struct {
		name         string
		input        *registry.ModelBooster
		expected     []*networking.ModelServer
		expectErrMsg string
	}{
		{
			name:     "normal case with VLLM backend",
			input:    loadYaml[registry.ModelBooster](t, "testdata/input/model.yaml"),
			expected: []*networking.ModelServer{loadYaml[networking.ModelServer](t, "testdata/expected/model-server.yaml")},
		},
		{
			name:     "PD disaggregation case",
			input:    loadYaml[registry.ModelBooster](t, "testdata/input/pd-disaggregated-model-npu.yaml"),
			expected: []*networking.ModelServer{loadYaml[networking.ModelServer](t, "testdata/expected/pd-model-server.yaml")},
		},
		{
			name: "invalid backend type",
			input: &registry.ModelBooster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-backend",
					Namespace: "default",
				},
				Spec: registry.ModelBoosterSpec{
					Backend: registry.ModelBackend{
						Name: "invalid",
						Type: "InvalidType",
					},
				},
			},
			expectErrMsg: "not support InvalidType backend yet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildModelServer(tt.input)
			if tt.expectErrMsg != "" {
				assert.Contains(t, err.Error(), tt.expectErrMsg)
				return
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expected, got)
		})
	}
}
