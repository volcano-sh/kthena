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

package plugins

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

type ScorePluginBuilder = func(buildCtx BuildContext) (framework.ScorePlugin, error)
type FilterPluginBuilder = func(buildCtx BuildContext) (framework.FilterPlugin, error)

// SchedulerPlugin describes a scheduler plugin: how to build it for each
// role it serves, and how to validate its configured arguments.
type SchedulerPlugin struct {
	ValidateArgs func(runtime.RawExtension) error
	NewScore     ScorePluginBuilder
	NewFilter    FilterPluginBuilder
}

// BuildContext is the construction context of one plugin instance: its
// identity, dependencies, and configured arguments.
type BuildContext struct {
	// Profile is the chain the plugin instance serves; empty is the global
	// chain built from the router config.
	Profile types.NamespacedName
	// Store is the shared datastore the instance may read from.
	Store datastore.Store
	// Args is the pluginConfig entry assigned to this instance.
	Args runtime.RawExtension
}

// IsGlobal reports whether this context builds plugins for the global
// scheduler configuration rather than a specific SchedulerProfile.
func (buildCtx BuildContext) IsGlobal() bool {
	return buildCtx.Profile == types.NamespacedName{}
}

// OwnerName renders a chain owner for logs: "global" or "ns/name".
func OwnerName(profile types.NamespacedName) string {
	if profile == (types.NamespacedName{}) {
		return "global"
	}
	return profile.String()
}

// schedulerPlugins holds every scheduler plugin. Adding a plugin means
// adding one entry here; use RegisterPlugin/GetPlugin to access it.
var schedulerPlugins = map[string]SchedulerPlugin{
	GPUCacheUsagePluginName: {
		ValidateArgs: ValidateGPUCacheUsageArgs,
		NewScore: func(buildCtx BuildContext) (framework.ScorePlugin, error) {
			return NewGPUCacheUsage(buildCtx), nil
		},
	},
	LeastLatencyPluginName: {
		ValidateArgs: ValidateLeastLatencyArgs,
		NewScore: func(buildCtx BuildContext) (framework.ScorePlugin, error) {
			return NewLeastLatency(buildCtx), nil
		},
	},
	LeastRequestPluginName: {
		ValidateArgs: ValidateLeastRequestArgs,
		NewScore: func(buildCtx BuildContext) (framework.ScorePlugin, error) {
			return NewLeastRequest(buildCtx), nil
		},
		NewFilter: func(buildCtx BuildContext) (framework.FilterPlugin, error) {
			return NewLeastRequest(buildCtx), nil
		},
	},
	RandomPluginName: {
		ValidateArgs: ValidateRandomArgs,
		NewScore: func(buildCtx BuildContext) (framework.ScorePlugin, error) {
			return NewRandom(buildCtx), nil
		},
	},
	PrefixCachePluginName: {
		ValidateArgs: ValidatePrefixCacheArgs,
		NewScore: func(buildCtx BuildContext) (framework.ScorePlugin, error) {
			return NewPrefixCache(buildCtx), nil
		},
	},
	KVCacheAwarePluginName: {
		ValidateArgs: ValidateKVCacheAwareArgs,
		NewScore: func(buildCtx BuildContext) (framework.ScorePlugin, error) {
			return NewKVCacheAware(buildCtx)
		},
	},
	LoraAffinityPluginName: {
		ValidateArgs: ValidateLoraAffinityArgs,
		NewFilter: func(buildCtx BuildContext) (framework.FilterPlugin, error) {
			return NewLoraAffinity(buildCtx), nil
		},
	},
}

// RegisterPlugin adds or replaces the plugin registered under name.
func RegisterPlugin(name string, p SchedulerPlugin) {
	schedulerPlugins[name] = p
}

// GetPlugin returns the plugin registered under name.
func GetPlugin(name string) (SchedulerPlugin, bool) {
	p, ok := schedulerPlugins[name]
	return p, ok
}

// SupportsFilterRole reports whether the named plugin can serve as a filter.
// Unknown plugins return false.
func SupportsFilterRole(name string) bool {
	plugin, ok := GetPlugin(name)
	return ok && plugin.NewFilter != nil
}

// SupportsScoreRole reports whether the named plugin can serve as a score
// plugin. Unknown plugins return false.
func SupportsScoreRole(name string) bool {
	plugin, ok := GetPlugin(name)
	return ok && plugin.NewScore != nil
}

// ValidateScorePluginCombination checks semantic rules across the set of
// enabled score plugins: the random plugin must be the only score plugin,
// since mixing random scores with meaningful scores defeats the purpose of
// scheduling
func ValidateScorePluginCombination(names []string) error {
	hasRandom := slices.Contains(names, RandomPluginName)
	if hasRandom && len(names) > 1 {
		return fmt.Errorf("the random plugin must be the only score plugin")
	}
	return nil
}

// IsKnownPlugin reports whether name refers to a registered scheduler plugin.
func IsKnownPlugin(name string) bool {
	_, ok := GetPlugin(name)
	return ok
}

// ValidatePluginArgs validates the arguments of the named plugin. It returns
// an error for unknown plugins as well, so callers get a single entry point.
func ValidatePluginArgs(name string, pluginArg runtime.RawExtension) error {
	plugin, ok := GetPlugin(name)
	if !ok {
		return fmt.Errorf("unknown plugin %q, known plugins: %v", name, knownPluginNames())
	}
	if plugin.ValidateArgs == nil {
		return nil
	}
	return plugin.ValidateArgs(pluginArg)
}

// knownPluginNames returns the names of all registered scheduler plugins.
func knownPluginNames() []string {
	names := make([]string, 0, len(schedulerPlugins))
	for name := range schedulerPlugins {
		names = append(names, name)
	}
	return names
}
