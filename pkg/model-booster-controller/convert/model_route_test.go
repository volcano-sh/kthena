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
)

func TestBuildModelRoute(t *testing.T) {
	tests := []struct {
		name     string
		input    *registry.ModelBooster
		expected *networking.ModelRoute
	}{
		{
			name:     "simple backend",
			input:    loadYaml[registry.ModelBooster](t, "testdata/input/model.yaml"),
			expected: loadYaml[networking.ModelRoute](t, "testdata/expected/model-route.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildModelRoute(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}
