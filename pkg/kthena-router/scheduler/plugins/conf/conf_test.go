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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestParseRouterConfig(t *testing.T) {
	testCases := []struct {
		name       string
		configFile string
		expectErrs string
	}{
		{
			name:       "ParseRouterConfig success",
			configFile: "../../../utils/testdata/configmap-jwt.yaml",
			expectErrs: "",
		},
		{
			name:       "missing config file",
			configFile: "non-existent-file.yaml",
			expectErrs: "failed to read config file",
		},
		{
			name:       "invalid YAML syntax",
			configFile: "../../../utils/testdata/configmap-malformed.yaml",
			expectErrs: "failed to Unmarshal routerConfiguration",
		},
		{
			name:       "plugin entry is not a mapping",
			configFile: "../../../utils/testdata/configmap-invalid.yaml",
			expectErrs: "plugin configuration must be a mapping",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRouterConfig(tc.configFile)
			if tc.expectErrs == "" {
				if err != nil {
					t.Errorf("expected no error, got %q", err.Error())
				}
				return
			}
			if err == nil {
				t.Errorf("expected error %q, got none", tc.expectErrs)
				return
			}
			if !strings.Contains(err.Error(), tc.expectErrs) {
				t.Errorf("expected error %q, got %q", tc.expectErrs, err.Error())
			}
		})
	}
}

func TestParseRouterConfigRejectsNonScalarPluginName(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "routerConfiguration.yaml")
	config := `scheduler:
  pluginConfig:
    - name:
        nested: least-latency
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write router configuration: %v", err)
	}

	_, err := ParseRouterConfig(configPath)
	if err == nil {
		t.Fatal("expected a non-scalar plugin name to be rejected")
	}
	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("expected a plugin name type error, got %v", err)
	}
}

// TestLoadSchedulerConfigAssemblesMaps checks the pure config-to-maps
// assembly; validation lives with the chain builders now.
func TestLoadSchedulerConfigAssemblesMaps(t *testing.T) {
	schedulerConfig := &SchedulerConfiguration{
		PluginConfig: []PluginConfig{
			{Name: "least-request", Args: runtime.RawExtension{Raw: []byte(`{"maxWaitingRequests": 10}`)}},
		},
		Plugins: Plugins{
			Filter: Filter{Enabled: []string{"least-request"}},
			Score:  Score{Enabled: []PluginWithWeight{{Name: "least-request", Weight: 3}}},
		},
	}

	scorePluginMap, filterPlugins, pluginsArgMap, err := LoadSchedulerConfig(schedulerConfig)
	if err != nil {
		t.Fatalf("load scheduler configuration: %v", err)
	}
	if got := scorePluginMap["least-request"]; got != 3 {
		t.Errorf("expected weight 3, got %d", got)
	}
	if len(filterPlugins) != 1 || filterPlugins[0] != "least-request" {
		t.Errorf("unexpected filter plugins %v", filterPlugins)
	}
	if _, ok := pluginsArgMap["least-request"]; !ok {
		t.Errorf("expected least-request args to be carried through")
	}
}
