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
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
)

type RouterConfiguration struct {
	Scheduler SchedulerConfiguration `yaml:"scheduler"`
	Auth      AuthenticationConfig   `yaml:"auth"`
}

type SchedulerConfiguration struct {
	PluginConfig []PluginConfig `yaml:"pluginConfig"`
	Plugins      Plugins        `yaml:"plugins"`
}

type Plugins struct {
	Filter Filter `yaml:"Filter"`
	Score  Score  `yaml:"Score"`
}

type Filter struct {
	Enabled  []string `yaml:"enabled"`
	Disabled []string `yaml:"disabled"`
}

type Score struct {
	Enabled  []PluginWithWeight `yaml:"enabled"`
	Disabled []PluginWithWeight `yaml:"disabled"`
}

type PluginWithWeight struct {
	Name   string `yaml:"name"`
	Weight int    `yaml:"weight"`
}

type PluginConfig struct {
	Name string               `yaml:"name"`
	Args runtime.RawExtension `yaml:"args,omitempty"`
}

// UnmarshalYAML preserves plugin arguments as YAML. Parsing the outer
// configuration through JSON rejects valid YAML scalar values such as .nan and
// .inf before the plugin has a chance to validate or default them.
func (p *PluginConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("plugin configuration must be a mapping")
	}

	p.Args = runtime.RawExtension{}
	for i := 0; i < len(value.Content); i += 2 {
		key := value.Content[i]
		field := value.Content[i+1]
		switch key.Value {
		case "name":
			if err := field.Decode(&p.Name); err != nil {
				return fmt.Errorf("decode plugin name: %w", err)
			}
		case "args":
			args, err := yaml.Marshal(field)
			if err != nil {
				return fmt.Errorf("marshal plugin arguments: %w", err)
			}
			p.Args.Raw = args
		}
	}
	return nil
}

type AuthenticationConfig struct {
	Issuer    string   `yaml:"issuer"`
	Audiences []string `yaml:"audiences"`
	JwksUri   string   `yaml:"jwksUri"`
}

func ParseRouterConfig(configMapPath string) (*RouterConfiguration, error) {
	data, err := os.ReadFile(configMapPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configMapPath, err)
	}
	var routerConfig RouterConfiguration
	if err := yaml.Unmarshal(data, &routerConfig); err != nil {
		klog.Errorf("failed to Unmarshal routerConfiguration: %v", err)
		return nil, fmt.Errorf("failed to Unmarshal routerConfiguration: %v", err)
	}
	return &routerConfig, nil
}

func LoadSchedulerConfig(schedulerConfig *SchedulerConfiguration) (map[string]int, []string, map[string]runtime.RawExtension, error) {
	if schedulerConfig == nil {
		return nil, nil, nil, fmt.Errorf("schedulerConfig is nil")
	}

	scorePluginMap, filterPlugins, err := unmarshalPlugins(schedulerConfig)
	if err != nil {
		klog.Errorf("failed to Unmarshal Plugins: %v", err)
		return nil, nil, nil, fmt.Errorf("failed to Unmarshal Plugins: %v", err)
	}

	// Check for random plugin conflicts and remove random plugin if needed
	scorePluginMap = handleRandomPluginConflicts(scorePluginMap)

	pluginsArgMap, err := unmarshalPluginsConfig(schedulerConfig)
	if err != nil {
		klog.Errorf("failed to Unmarshal PluginsConfig: %v", err)
		return nil, nil, nil, fmt.Errorf("failed to Unmarshal PluginsConfig: %v", err)
	}

	return scorePluginMap, filterPlugins, pluginsArgMap, nil
}

// handleRandomPluginConflicts checks if random plugin is configured with other score plugins
// and removes the random plugin while logging a warning if conflicts are detected
func handleRandomPluginConflicts(scorePluginMap map[string]int) map[string]int {
	const randomPluginName = "random"

	// Check if random plugin exists using direct map lookup
	_, hasRandomPlugin := scorePluginMap[randomPluginName]

	// Check if there are other plugins besides random
	hasOtherScorePlugins := len(scorePluginMap) > 1 && hasRandomPlugin

	// If both random and other score plugins are configured, remove random plugin and warn
	if hasRandomPlugin && hasOtherScorePlugins {
		klog.Warningf("Random plugin is configured along with other score plugins. Random plugin will be removed as it should be used independently. " +
			"Mixing random scores with meaningful scores defeats the purpose of intelligent scheduling.")

		delete(scorePluginMap, randomPluginName)
	}

	return scorePluginMap
}

func unmarshalPlugins(schedulerConfig *SchedulerConfiguration) (map[string]int, []string, error) {
	var filterPlugins []string
	scorePluginMap := make(map[string]int)
	if len(schedulerConfig.Plugins.Score.Enabled) > 0 {
		for _, plugin := range schedulerConfig.Plugins.Score.Enabled {
			scorePluginMap[plugin.Name] = plugin.Weight
		}
	}

	if len(schedulerConfig.Plugins.Filter.Enabled) > 0 {
		filterPlugins = schedulerConfig.Plugins.Filter.Enabled
	}
	return scorePluginMap, filterPlugins, nil
}

func unmarshalPluginsConfig(schedulerConfig *SchedulerConfiguration) (map[string]runtime.RawExtension, error) {
	pluginsArgMap := make(map[string]runtime.RawExtension)

	if len(schedulerConfig.PluginConfig) > 0 {
		for _, pluginArg := range schedulerConfig.PluginConfig {
			if pluginArg.Name == plugins.LeastLatencyPluginName {
				if err := plugins.ValidateLeastLatencyArgs(pluginArg.Args); err != nil {
					return nil, fmt.Errorf("invalid %s plugin configuration: %w", pluginArg.Name, err)
				}
			}
			pluginsArgMap[pluginArg.Name] = pluginArg.Args
		}
	}

	return pluginsArgMap, nil
}
