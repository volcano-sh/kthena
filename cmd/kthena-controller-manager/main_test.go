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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/volcano-sh/kthena/pkg/controller"
)

func TestParseControllers(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected map[string]bool
	}{
		{
			name:  "wildcard_only",
			input: []string{"*"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
		{
			name:  "wildcard_with_other_controllers",
			input: []string{"*", "modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
		{
			name:  "single_controller_modelserving",
			input: []string{"modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "single_controller_modelbooster",
			input: []string{"modelbooster"},
			expected: map[string]bool{
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "single_controller_autoscaler",
			input: []string{"autoscaler"},
			expected: map[string]bool{
				controller.AutoscalerController: true,
			},
		},
		{
			name:  "multiple_controllers",
			input: []string{"modelserving", "modelbooster"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "all_controllers_explicit",
			input: []string{"modelserving", "modelbooster", "autoscaler"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
		{
			name:  "controllers_with_spaces",
			input: []string{" modelserving ", " modelbooster "},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "invalid_controller_name",
			input: []string{"invalid"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
		{
			name:  "mixed_valid_and_invalid_controllers",
			input: []string{"modelserving", "invalid"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "only_commas",
			input: []string{",,"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
		{
			name:  "invalid_with_no_valid_controllers",
			input: []string{"invalid1", "invalid2"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
		{
			name:  "duplicate_valid_controllers",
			input: []string{"modelserving", "modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "invalid_controller",
			input: []string{"*modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
				controller.AutoscalerController:   true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseControllers(tt.input)
			assert.Equal(t, tt.expected, result, "parseControllers(%q) result mismatch", tt.input)
		})
	}
}
