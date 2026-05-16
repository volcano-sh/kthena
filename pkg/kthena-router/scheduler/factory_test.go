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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
)

func TestNewPluginRegistry(t *testing.T) {
	registry := NewPluginRegistry()

	assert.NotNil(t, registry)
	assert.NotNil(t, registry.scorePluginBuilders)
	assert.NotNil(t, registry.filterPluginBuilders)
	assert.Equal(t, 0, len(registry.scorePluginBuilders))
	assert.Equal(t, 0, len(registry.filterPluginBuilders))
}

func TestRegisterDefaultPlugins(t *testing.T) {
	registry := NewPluginRegistry()

	// Before registration
	assert.Equal(t, 0, len(registry.scorePluginBuilders))
	assert.Equal(t, 0, len(registry.filterPluginBuilders))

	registerDefaultPlugins(registry)

	// Test that all expected score plugins are registered
	expectedScorePlugins := []string{
		plugins.GPUCacheUsagePluginName,
		plugins.LeastLatencyPluginName,
		plugins.LeastRequestPluginName,
		plugins.RandomPluginName,
		plugins.PrefixCachePluginName,
		plugins.KVCacheAwarePluginName,
	}

	for _, pluginName := range expectedScorePlugins {
		builder, exists := registry.getScorePlugin(pluginName)
		assert.True(t, exists, "Score plugin %s should be registered", pluginName)
		assert.NotNil(t, builder, "Score plugin builder for %s should not be nil", pluginName)

		// Test that the builder actually creates a plugin
		plugin := builder(runtime.RawExtension{})
		assert.NotNil(t, plugin, "Plugin %s should be created successfully", pluginName)

		// PrefixCache plugin from registry is not properly initialized (empty struct)
		// It needs special initialization with datastore in actual usage
		if pluginName != plugins.PrefixCachePluginName {
			assert.Equal(t, pluginName, plugin.Name(), "Plugin name should match")
		}
	}

	// Test that all expected filter plugins are registered
	expectedFilterPlugins := []string{
		plugins.LeastRequestPluginName,
		plugins.LoraAffinityPluginName,
	}

	for _, pluginName := range expectedFilterPlugins {
		builder, exists := registry.getFilterPlugin(pluginName)
		assert.True(t, exists, "Filter plugin %s should be registered", pluginName)
		assert.NotNil(t, builder, "Filter plugin builder for %s should not be nil", pluginName)

		// Test that the builder actually creates a plugin
		plugin := builder(runtime.RawExtension{})
		assert.NotNil(t, plugin, "Plugin %s should be created successfully", pluginName)
		assert.Equal(t, pluginName, plugin.Name(), "Plugin name should match")
	}
}

func TestGetFilterPlugins(t *testing.T) {
	registry := NewPluginRegistry()
	registerDefaultPlugins(registry)

	tests := []struct {
		name            string
		filterPluginMap []string
		pluginsArgMap   map[string]runtime.RawExtension
		expectedCount   int
		expectedNames   []string
	}{
		{
			name:            "empty filter plugin map",
			filterPluginMap: []string{},
			pluginsArgMap:   map[string]runtime.RawExtension{},
			expectedCount:   0,
			expectedNames:   []string{},
		},
		{
			name:            "single valid plugin",
			filterPluginMap: []string{plugins.LeastRequestPluginName},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName: {Raw: []byte(`{"maxWaitingRequests": 10}`)},
			},
			expectedCount: 1,
			expectedNames: []string{plugins.LeastRequestPluginName},
		},
		{
			name:            "multiple valid plugins",
			filterPluginMap: []string{plugins.LeastRequestPluginName, plugins.LoraAffinityPluginName},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName: {Raw: []byte(`{"maxWaitingRequests": 10}`)},
				plugins.LoraAffinityPluginName: {Raw: []byte(`{}`)},
			},
			expectedCount: 2,
			expectedNames: []string{plugins.LeastRequestPluginName, plugins.LoraAffinityPluginName},
		},
		{
			name:            "non-existent plugin should be skipped",
			filterPluginMap: []string{plugins.LeastRequestPluginName, "non-existent-plugin"},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName: {Raw: []byte(`{"maxWaitingRequests": 10}`)},
				"non-existent-plugin":          {Raw: []byte(`{}`)},
			},
			expectedCount: 1,
			expectedNames: []string{plugins.LeastRequestPluginName},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filterPlugins := getFilterPlugins(registry, tt.filterPluginMap, tt.pluginsArgMap)

			assert.Equal(t, tt.expectedCount, len(filterPlugins))

			for i, expectedName := range tt.expectedNames {
				if i < len(filterPlugins) {
					assert.Equal(t, expectedName, filterPlugins[i].Name())
				}
			}
		})
	}
}

func TestGetScorePlugins(t *testing.T) {
	registry := NewPluginRegistry()
	registerDefaultPlugins(registry)

	// Create a mock prefix cache for testing
	mockStore := datastore.New()
	prefixCache := plugins.NewPrefixCache(mockStore, runtime.RawExtension{Raw: []byte(`{"blockSizeToHash": 64}`)})

	tests := []struct {
		name            string
		scorePluginMap  map[string]int
		pluginsArgMap   map[string]runtime.RawExtension
		expectedCount   int
		expectedWeights map[string]int
	}{
		{
			name:            "empty score plugin map",
			scorePluginMap:  map[string]int{},
			pluginsArgMap:   map[string]runtime.RawExtension{},
			expectedCount:   0,
			expectedWeights: map[string]int{},
		},
		{
			name: "single valid plugin",
			scorePluginMap: map[string]int{
				plugins.LeastRequestPluginName: 5,
			},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName: {Raw: []byte(`{"maxWaitingRequests": 10}`)},
			},
			expectedCount: 1,
			expectedWeights: map[string]int{
				plugins.LeastRequestPluginName: 5,
			},
		},
		{
			name: "multiple valid plugins with different weights",
			scorePluginMap: map[string]int{
				plugins.LeastRequestPluginName:  3,
				plugins.GPUCacheUsagePluginName: 7,
			},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName:  {Raw: []byte(`{"maxWaitingRequests": 10}`)},
				plugins.GPUCacheUsagePluginName: {Raw: []byte(`{}`)},
			},
			expectedCount: 2,
			expectedWeights: map[string]int{
				plugins.LeastRequestPluginName:  3,
				plugins.GPUCacheUsagePluginName: 7,
			},
		},
		{
			name: "prefix cache plugin special handling",
			scorePluginMap: map[string]int{
				plugins.PrefixCachePluginName: 10,
			},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.PrefixCachePluginName: {Raw: []byte(`{"blockSizeToHash": 64}`)},
			},
			expectedCount: 1,
			expectedWeights: map[string]int{
				plugins.PrefixCachePluginName: 10,
			},
		},
		{
			name: "negative weight should be set to 0",
			scorePluginMap: map[string]int{
				plugins.LeastRequestPluginName: -5,
			},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName: {Raw: []byte(`{"maxWaitingRequests": 10}`)},
			},
			expectedCount: 1,
			expectedWeights: map[string]int{
				plugins.LeastRequestPluginName: 0,
			},
		},
		{
			name: "non-existent plugin should be skipped",
			scorePluginMap: map[string]int{
				plugins.LeastRequestPluginName: 3,
				"non-existent-plugin":          5,
			},
			pluginsArgMap: map[string]runtime.RawExtension{
				plugins.LeastRequestPluginName: {Raw: []byte(`{"maxWaitingRequests": 10}`)},
				"non-existent-plugin":          {Raw: []byte(`{}`)},
			},
			expectedCount: 1,
			expectedWeights: map[string]int{
				plugins.LeastRequestPluginName: 3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scorePlugins := getScorePlugins(registry, prefixCache, tt.scorePluginMap, tt.pluginsArgMap)

			assert.Equal(t, tt.expectedCount, len(scorePlugins))

			// Verify weights and plugin names
			for _, scorePlugin := range scorePlugins {
				pluginName := scorePlugin.plugin.Name()
				expectedWeight, exists := tt.expectedWeights[pluginName]

				assert.True(t, exists, "Plugin %s should have expected weight", pluginName)
				assert.Equal(t, expectedWeight, scorePlugin.weight, "Weight for plugin %s should match", pluginName)
			}
		})
	}
}
