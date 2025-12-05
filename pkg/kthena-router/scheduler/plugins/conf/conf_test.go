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

package conf

import (
	"strings"
	"testing"
)

func TestLoadSchedulerConfig(t *testing.T) {
	testCases := []struct {
		name       string
		configFile string
		expectErrs string
	}{
		{
			name:       "LoadSchedulerConfig success",
			configFile: "../../../utils/testdata/configmap-jwt.yaml",
			expectErrs: "",
		},
		{
			name:       "empty plugins config",
			configFile: "non-existent-file.yaml",
			expectErrs: "failed to read config file",
		},
		{
			name:       "invalid YAML syntax",
			configFile: "../../../utils/testdata/configmap-invalid.yaml",
			expectErrs: "failed to Unmarshal schedulerConfiguration",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			routerConf, err := ParseRouterConfig(tc.configFile)
			if err != nil {
				if !strings.Contains(err.Error(), tc.expectErrs) {
					t.Errorf("expected error %q, got %q", tc.expectErrs, err.Error())
				}
			} else {
				_, _, _, err = LoadSchedulerConfig(&routerConf.Scheduler)
				if err != nil {
					if !strings.Contains(err.Error(), tc.expectErrs) {
						t.Errorf("expected error %q, got %q", tc.expectErrs, err.Error())
					}
				}
			}
		})
	}
}

func TestHandleRandomPluginConflicts(t *testing.T) {
	tests := []struct {
		name            string
		scorePluginMap  map[string]int
		expectedPlugins map[string]int
		expectConflict  bool
	}{
		{
			name: "random plugin with other score plugins - should remove random",
			scorePluginMap: map[string]int{
				"random":        10,
				"least-latency": 20,
				"gpu-usage":     30,
			},
			expectedPlugins: map[string]int{
				"least-latency": 20,
				"gpu-usage":     30,
			},
			expectConflict: true,
		},
		{
			name: "only random plugin - should keep random",
			scorePluginMap: map[string]int{
				"random": 10,
			},
			expectedPlugins: map[string]int{
				"random": 10,
			},
			expectConflict: false,
		},
		{
			name: "no random plugin - should keep all",
			scorePluginMap: map[string]int{
				"least-latency": 20,
				"gpu-usage":     30,
			},
			expectedPlugins: map[string]int{
				"least-latency": 20,
				"gpu-usage":     30,
			},
			expectConflict: false,
		},
		{
			name:            "empty plugin map - should remain empty",
			scorePluginMap:  map[string]int{},
			expectedPlugins: map[string]int{},
			expectConflict:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := handleRandomPluginConflicts(tt.scorePluginMap)

			// Check the number of plugins
			if len(result) != len(tt.expectedPlugins) {
				t.Errorf("Expected %d plugins, got %d", len(tt.expectedPlugins), len(result))
			}

			// Check each expected plugin exists with correct weight
			for expectedName, expectedWeight := range tt.expectedPlugins {
				if actualWeight, exists := result[expectedName]; !exists {
					t.Errorf("Expected plugin %s to exist", expectedName)
				} else if actualWeight != expectedWeight {
					t.Errorf("Expected weight %d for plugin %s, got %d", expectedWeight, expectedName, actualWeight)
				}
			}

			// Verify random plugin is removed when there's a conflict
			if tt.expectConflict {
				if _, exists := result["random"]; exists {
					t.Errorf("Random plugin should be removed when configured with other score plugins")
				}
			}
		})
	}
}
