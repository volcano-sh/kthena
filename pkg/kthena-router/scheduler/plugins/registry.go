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

// schedulerPlugin describes a scheduler plugin: how to build it for each
// role it serves, and how to validate its configured arguments.
type schedulerPlugin struct {
	ValidateArgs func(runtime.RawExtension) error
	NewScore     func(BuildContext, datastore.Store, runtime.RawExtension) (framework.ScorePlugin, error)
	NewFilter    func(BuildContext, runtime.RawExtension) (framework.FilterPlugin, error)
}

type BuildContext struct {
	Profile types.NamespacedName
}

// IsGlobal reports whether this context builds plugins for the global
// scheduler configuration rather than a specific SchedulerProfile.
func (bc BuildContext) IsGlobal() bool {
	return bc.Profile == types.NamespacedName{}
}

// SchedulerPlugins registers every scheduler plugin with its builders and
// args validator. Adding a plugin means adding one entry here.
var SchedulerPlugins = map[string]schedulerPlugin{
	GPUCacheUsagePluginName: {
		ValidateArgs: ValidateGPUCacheUsageArgs,
		NewScore: func(bc BuildContext, _ datastore.Store, _ runtime.RawExtension) (framework.ScorePlugin, error) {
			return NewGPUCacheUsage(bc), nil
		},
	},
	LeastLatencyPluginName: {
		ValidateArgs: ValidateLeastLatencyArgs,
		NewScore: func(bc BuildContext, _ datastore.Store, args runtime.RawExtension) (framework.ScorePlugin, error) {
			return NewLeastLatency(bc, args), nil
		},
	},
	LeastRequestPluginName: {
		ValidateArgs: ValidateLeastRequestArgs,
		NewScore: func(bc BuildContext, _ datastore.Store, args runtime.RawExtension) (framework.ScorePlugin, error) {
			return NewLeastRequest(bc, args), nil
		},
		NewFilter: func(bc BuildContext, args runtime.RawExtension) (framework.FilterPlugin, error) {
			return NewLeastRequest(bc, args), nil
		},
	},
	RandomPluginName: {
		ValidateArgs: ValidateRandomArgs,
		NewScore: func(bc BuildContext, _ datastore.Store, args runtime.RawExtension) (framework.ScorePlugin, error) {
			return NewRandom(bc, args), nil
		},
	},
	PrefixCachePluginName: {
		ValidateArgs: ValidatePrefixCacheArgs,
		NewScore: func(bc BuildContext, store datastore.Store, args runtime.RawExtension) (framework.ScorePlugin, error) {
			return NewPrefixCache(bc, store, args), nil
		},
	},
	KVCacheAwarePluginName: {
		ValidateArgs: ValidateKVCacheAwareArgs,
		NewScore: func(bc BuildContext, store datastore.Store, args runtime.RawExtension) (framework.ScorePlugin, error) {
			return NewKVCacheAware(bc, store, args)
		},
	},
	LoraAffinityPluginName: {
		ValidateArgs: ValidateLoraAffinityArgs,
		NewFilter: func(bc BuildContext, args runtime.RawExtension) (framework.FilterPlugin, error) {
			return NewLoraAffinity(bc), nil
		},
	},
}

// SupportsFilterRole reports whether the named plugin can serve as a filter.
// Unknown plugins return false.
func SupportsFilterRole(name string) bool {
	plugin, ok := SchedulerPlugins[name]
	return ok && plugin.NewFilter != nil
}

// SupportsScoreRole reports whether the named plugin can serve as a score
// plugin. Unknown plugins return false.
func SupportsScoreRole(name string) bool {
	plugin, ok := SchedulerPlugins[name]
	return ok && plugin.NewScore != nil
}

// ValidateScorePluginCombination checks semantic rules across the set of
// enabled score plugins: the random plugin must be the only score plugin,
// since mixing random scores with meaningful scores defeats the purpose of
// scheduling (mirrors handleRandomPluginConflicts for the global config).
func ValidateScorePluginCombination(names []string) error {
	hasRandom := slices.Contains(names, RandomPluginName)
	if hasRandom && len(names) > 1 {
		return fmt.Errorf("the random plugin must be the only score plugin")
	}
	return nil
}

// IsKnownPlugin reports whether name refers to a registered scheduler plugin.
func IsKnownPlugin(name string) bool {
	_, ok := SchedulerPlugins[name]
	return ok
}

// ValidatePluginArgs validates the arguments of the named plugin. It returns
// an error for unknown plugins as well, so callers get a single entry point.
func ValidatePluginArgs(name string, pluginArg runtime.RawExtension) error {
	plugin, ok := SchedulerPlugins[name]
	if !ok {
		return fmt.Errorf("unknown plugin %q, known plugins: %v", name, knownPluginNames())
	}
	return plugin.ValidateArgs(pluginArg)
}

// knownPluginNames returns the names of all registered scheduler plugins.
func knownPluginNames() []string {
	names := make([]string, 0, len(SchedulerPlugins))
	for name := range SchedulerPlugins {
		names = append(names, name)
	}
	return names
}
