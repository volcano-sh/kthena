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

package framework

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKthenaDeployments(t *testing.T) {
	tests := []struct {
		name     string
		config   *KthenaConfig
		expected []string
	}{
		{
			name:     "no components enabled",
			config:   &KthenaConfig{},
			expected: []string{},
		},
		{
			name: "workload enabled",
			config: &KthenaConfig{
				WorkloadEnabled: true,
			},
			expected: []string{"kthena-controller-manager"},
		},
		{
			name: "networking enabled",
			config: &KthenaConfig{
				NetworkingEnabled: true,
			},
			expected: []string{"kthena-router"},
		},
		{
			name: "all components enabled",
			config: &KthenaConfig{
				WorkloadEnabled:   true,
				NetworkingEnabled: true,
			},
			expected: []string{"kthena-controller-manager", "kthena-router"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, kthenaDeployments(tt.config))
		})
	}
}
