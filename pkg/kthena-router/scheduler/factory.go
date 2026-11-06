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
	"fmt"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

type ScorePluginBuilder = func(store datastore.Store, arg runtime.RawExtension) (framework.ScorePlugin, error)
type FilterPluginBuilder = func(arg runtime.RawExtension) (framework.FilterPlugin, error)

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

// registerDefaultPlugins registers all scheduler plugins to the given registry,
// derived from plugins.SchedulerPlugins — the single registration point.
func registerDefaultPlugins(registry *PluginRegistry) {
	for name, plugin := range plugins.SchedulerPlugins {
		if plugin.NewScore != nil {
			registry.registerScorePlugin(name, plugin.NewScore)
		}
		if plugin.NewFilter != nil {
			registry.registerFilterPlugin(name, plugin.NewFilter)
		}
	}
}

func getFilterPlugins(registry *PluginRegistry, filterPlugins []string, pluginsArgMap map[string]runtime.RawExtension) ([]framework.FilterPlugin, error) {
	var list []framework.FilterPlugin
	// TODO: enable lora affinity when models from metrics are available.
	for _, pluginName := range filterPlugins {
		builderFunc, exist := registry.getFilterPlugin(pluginName)
		if !exist {
			klog.Errorf("Failed to get plugin %s.", pluginName)
			continue
		}
		plugin, err := builderFunc(pluginsArgMap[pluginName])
		if err != nil {
			return nil, fmt.Errorf("failed to build filter plugin %s: %w", pluginName, err)
		}
		if plugin != nil {
			list = append(list, plugin)
		}
	}
	return list, nil
}

func getScorePlugins(registry *PluginRegistry, store datastore.Store, scorePluginMap map[string]int, pluginsArgMap map[string]runtime.RawExtension) ([]*scorePlugin, error) {
	var list []*scorePlugin
	for pluginName, weight := range scorePluginMap {
		if weight < 0 {
			klog.Errorf("Weight for plugin '%s' is invalid, value is %d. Setting to 0", pluginName, weight)
			weight = 0
		}

		builderFunc, exist := registry.getScorePlugin(pluginName)
		if !exist {
			// Legacy lenient behavior for the global config: log and skip
			// (see getFilterPlugins).
			klog.Errorf("Failed to get plugin %s.", pluginName)
			continue
		}
		plugin, err := builderFunc(store, pluginsArgMap[pluginName])
		if err != nil {
			return nil, fmt.Errorf("failed to build score plugin %s: %w", pluginName, err)
		}
		if plugin != nil {
			list = append(list, &scorePlugin{
				plugin: plugin,
				weight: weight,
			})
		}
	}
	return list, nil
}

func getPostScheduleHooks(scorePlugins []*scorePlugin) []framework.PostScheduleHook {
	var hooks []framework.PostScheduleHook
	for _, scorePlugin := range scorePlugins {
		if hook, ok := scorePlugin.plugin.(framework.PostScheduleHook); ok {
			hooks = append(hooks, hook)
		}
	}
	return hooks
}
