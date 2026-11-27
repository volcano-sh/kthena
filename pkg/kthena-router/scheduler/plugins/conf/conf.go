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
)

type RouterConfiguration struct {
	Scheduler     SchedulerConfiguration `yaml:"scheduler"`
	Auth          AuthenticationConfig   `yaml:"auth"`
	SessionSticky *SessionStickyConfig   `yaml:"sessionSticky,omitempty"`
	AccessLog     AccessLogConfig        `yaml:"accessLog,omitempty"`
}

// AccessLogConfig holds access-log options that do not fit the ACCESS_LOG_*
// environment variables, which keep controlling enablement, format and output.
type AccessLogConfig struct {
	// IncludeUpstreamUsage records the input and cached-input token counts the
	// upstream reports in its usage as upstream_input_tokens and
	// cached_input_tokens.
	IncludeUpstreamUsage bool `yaml:"includeUpstreamUsage"`
}

// SessionStickyConfig selects the session affinity backing store for the router.
type SessionStickyConfig struct {
	// Backend is "memory" (default) or "redis".
	Backend string              `yaml:"backend"`
	Redis   *SessionStickyRedis `yaml:"redis,omitempty"`
}

// SessionStickyRedis holds Redis connection settings for shared session stickiness.
// TODO(FAUST-BENCHOU): Support Redis password via a Kubernetes Secret reference when auth is required.
type SessionStickyRedis struct {
	Address string `yaml:"address"`
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
	scorePluginMap, filterPlugins := unmarshalPlugins(schedulerConfig)
	pluginsArgMap := unmarshalPluginsConfig(schedulerConfig)
	return scorePluginMap, filterPlugins, pluginsArgMap, nil
}

func unmarshalPlugins(schedulerConfig *SchedulerConfiguration) (map[string]int, []string) {
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
	return scorePluginMap, filterPlugins
}

func unmarshalPluginsConfig(schedulerConfig *SchedulerConfiguration) map[string]runtime.RawExtension {
	pluginsArgMap := make(map[string]runtime.RawExtension)

	if len(schedulerConfig.PluginConfig) > 0 {
		for _, pluginArg := range schedulerConfig.PluginConfig {
			pluginsArgMap[pluginArg.Name] = pluginArg.Args
		}
	}
	return pluginsArgMap
}
