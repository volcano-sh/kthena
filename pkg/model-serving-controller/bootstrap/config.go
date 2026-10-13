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

// Package bootstrap implements source-aware admission of role replicas for
// ModelServing bootstrap acceleration.
package bootstrap

import (
	"encoding/json"
	"hash/fnv"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/util/rand"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// configHashVersion must change whenever the hash input changes meaning.
const configHashVersion = "v1"

// Enabled reports whether bootstrap acceleration applies to ms.
func Enabled(ms *workloadv1alpha1.ModelServing) bool {
	if ms == nil || ms.Spec.BootstrapAccelerateStrategy == nil {
		return false
	}
	provider := ms.Spec.BootstrapAccelerateStrategy.Provider
	return provider == "" || provider == workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress
}

// InScope reports whether the role named roleName is accelerated.
func InScope(ms *workloadv1alpha1.ModelServing, roleName string) bool {
	if !Enabled(ms) {
		return false
	}
	roles := ms.Spec.BootstrapAccelerateStrategy.Roles
	return len(roles) == 0 || slices.Contains(roles, roleName)
}

type configHashInput struct {
	Version          string                        `json:"version"`
	Provider         string                        `json:"provider"`
	InScope          bool                          `json:"inScope"`
	ServerAddress    string                        `json:"serverAddress,omitempty"`
	EngineContainers []string                      `json:"engineContainers,omitempty"`
	Plugins          []workloadv1alpha1.PluginSpec `json:"plugins"`
}

// ConfigHash returns the bootstrap-configuration hash of the role named
// roleName. Replicas whose Pods carry different hashes never share sources.
func ConfigHash(ms *workloadv1alpha1.ModelServing, roleName string) (string, error) {
	plugins, err := utils.NormalizePluginSpecs(ms.Spec.Plugins)
	if err != nil {
		return "", err
	}
	return configHash(ms, roleName, plugins), nil
}

// configHash expects plugins normalized by utils.NormalizePluginSpecs.
func configHash(ms *workloadv1alpha1.ModelServing, roleName string, plugins []workloadv1alpha1.PluginSpec) string {
	input := configHashInput{
		Version: configHashVersion,
		InScope: InScope(ms, roleName),
		Plugins: plugins,
	}
	if strategy := ms.Spec.BootstrapAccelerateStrategy; strategy != nil {
		input.Provider = string(strategy.Provider)
		if input.Provider == "" {
			input.Provider = string(workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress)
		}
		if mx := strategy.ModelExpress; mx != nil {
			input.ServerAddress = mx.ServerAddress
			input.EngineContainers = slices.Clone(mx.EngineContainers)
			slices.Sort(input.EngineContainers)
			input.EngineContainers = slices.Compact(input.EngineContainers)
		}
	}
	// Marshaling plain strings, bools, and normalized plugin specs cannot fail.
	data, _ := json.Marshal(input)
	hasher := fnv.New64a()
	_, _ = hasher.Write(data)
	return rand.SafeEncodeString(strconv.FormatUint(hasher.Sum64(), 10))
}
