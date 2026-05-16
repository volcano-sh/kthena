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

package scheduler

import (
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

type ScorePluginBuilder = func(arg runtime.RawExtension) framework.ScorePlugin
type FilterPluginBuilder = func(arg runtime.RawExtension) framework.FilterPlugin

// PluginRegistry manages the registration and retrieval of scheduler plugins
type PluginRegistry struct {
	scorePluginBuilders  map[string]ScorePluginBuilder
	filterPluginBuilders map[string]FilterPluginBuilder
}

// NewPluginRegistry creates a new plugin registry
func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		scorePluginBuilders:  make(map[string]ScorePluginBuilder),
		filterPluginBuilders: make(map[string]FilterPluginBuilder),
	}
}

// registerScorePlugin registers a score plugin builder in this registry
func (r *PluginRegistry) registerScorePlugin(name string, sp ScorePluginBuilder) {
	r.scorePluginBuilders[name] = sp
}

// getScorePlugin retrieves a score plugin builder from this registry
func (r *PluginRegistry) getScorePlugin(name string) (ScorePluginBuilder, bool) {
	sp, exist := r.scorePluginBuilders[name]
	return sp, exist
}

// registerFilterPlugin registers a filter plugin builder in this registry
func (r *PluginRegistry) registerFilterPlugin(name string, fp FilterPluginBuilder) {
	r.filterPluginBuilders[name] = fp
}

// getFilterPlugin retrieves a filter plugin builder from this registry
func (r *PluginRegistry) getFilterPlugin(name string) (FilterPluginBuilder, bool) {
	fp, exist := r.filterPluginBuilders[name]
	return fp, exist
}

// registerDefaultPlugins registers all default plugins to the given registry
func registerDefaultPlugins(registry *PluginRegistry) {
	// scorePlugin
	registry.registerScorePlugin(plugins.GPUCacheUsagePluginName, func(args runtime.RawExtension) framework.ScorePlugin {
		return plugins.NewGPUCacheUsage()
	})
	registry.registerScorePlugin(plugins.LeastLatencyPluginName, func(args runtime.RawExtension) framework.ScorePlugin {
		return plugins.NewLeastLatency(args)
	})
	registry.registerScorePlugin(plugins.LeastRequestPluginName, func(args runtime.RawExtension) framework.ScorePlugin {
		return plugins.NewLeastRequest(args)
	})
	registry.registerScorePlugin(plugins.RandomPluginName, func(args runtime.RawExtension) framework.ScorePlugin {
		return plugins.NewRandom(args)
	})
	// PrefixCache requires two parameters and is instantiated during use
	registry.registerScorePlugin(plugins.PrefixCachePluginName, func(args runtime.RawExtension) framework.ScorePlugin {
		return &plugins.PrefixCache{}
	})

	registry.registerScorePlugin(plugins.KVCacheAwarePluginName, func(args runtime.RawExtension) framework.ScorePlugin {
		return plugins.NewKVCacheAware(args)
	})
	// filterPlugin
	registry.registerFilterPlugin(plugins.LeastRequestPluginName, func(args runtime.RawExtension) framework.FilterPlugin {
		return plugins.NewLeastRequest(args)
	})
	registry.registerFilterPlugin(plugins.LoraAffinityPluginName, func(args runtime.RawExtension) framework.FilterPlugin {
		return plugins.NewLoraAffinity()
	})
}

func getFilterPlugins(registry *PluginRegistry, filterPluginMap []string, pluginsArgMap map[string]runtime.RawExtension) []framework.FilterPlugin {
	var list []framework.FilterPlugin
	// TODO: enable lora affinity when models from metrics are available.
	for _, pluginName := range filterPluginMap {
		if builderFunc, exist := registry.getFilterPlugin(pluginName); !exist {
			klog.Errorf("Failed to get plugin %s.", pluginName)
			continue
		} else {
			plugin := builderFunc(pluginsArgMap[pluginName])
			if plugin != nil {
				list = append(list, plugin)
			}
		}
	}
	return list
}

func getScorePlugins(registry *PluginRegistry, prefixCache *plugins.PrefixCache, scorePluginMap map[string]int, pluginsArgMap map[string]runtime.RawExtension) []*scorePlugin {
	var list []*scorePlugin
	for pluginName, weight := range scorePluginMap {
		if weight < 0 {
			klog.Errorf("Weight for plugin '%s' is invalid, value is %d. Setting to 0", pluginName, weight)
			weight = 0
		}

		if pluginName == plugins.PrefixCachePluginName {
			list = append(list, &scorePlugin{
				plugin: prefixCache,
				weight: weight,
			})
			continue
		}

		if builderFunc, exist := registry.getScorePlugin(pluginName); !exist {
			klog.Errorf("Failed to get plugin %s.", pluginName)
		} else {
			plugin := builderFunc(pluginsArgMap[pluginName])
			if plugin != nil {
				list = append(list, &scorePlugin{
					plugin: plugin,
					weight: weight,
				})
			}
		}
	}
	return list
}
