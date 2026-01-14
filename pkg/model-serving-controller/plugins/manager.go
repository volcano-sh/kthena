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
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Factory constructs a Plugin from a PluginSpec.
type Factory func(spec workloadv1alpha1.PluginSpec) (Plugin, error)

// Registry keeps a mapping from plugin name to factory.
type Registry struct {
	factories map[string]Factory
}

var DefaultRegistry = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

func (r *Registry) Register(name string, factory Factory) {
	r.factories[name] = factory
}

// entry couples the instantiated plugin with its spec for scope evaluation.
type entry struct {
	plugin Plugin
	spec   workloadv1alpha1.PluginSpec
}

// Chain represents an ordered list of plugins built from a ModelServing spec.
type Chain struct {
	entries []entry
}

// NewChain builds a Chain from plugin specs and the registry.
func NewChain(registry *Registry, specs []workloadv1alpha1.PluginSpec) (*Chain, error) {
	if registry == nil {
		return &Chain{}, nil
	}
	var entries []entry
	for _, spec := range specs {
		if spec.Type != workloadv1alpha1.PluginTypeBuiltIn {
			return nil, fmt.Errorf("plugin %s has unsupported type %s", spec.Name, spec.Type)
		}
		factory, ok := registry.factories[spec.Name]
		if !ok {
			return nil, fmt.Errorf("plugin %s not registered", spec.Name)
		}
		p, err := factory(spec)
		if err != nil {
			return nil, fmt.Errorf("build plugin %s: %w", spec.Name, err)
		}
		entries = append(entries, entry{plugin: p, spec: spec})
	}
	return &Chain{entries: entries}, nil
}

// OnPodCreate executes plugins in order. Mutations are applied to req.Pod.
func (c *Chain) OnPodCreate(ctx context.Context, req *HookRequest) error {
	if c == nil {
		return nil
	}
	for _, entry := range c.entries {
		if !shouldRun(entry.spec, req) {
			continue
		}
		if err := entry.plugin.OnPodCreate(ctx, req); err != nil {
			return fmt.Errorf("plugin %s OnPodCreate: %w", entry.plugin.Name(), err)
		}
	}
	return nil
}

// OnPodReady executes plugins' ready hooks in order.
func (c *Chain) OnPodReady(ctx context.Context, req *HookRequest) error {
	if c == nil {
		return nil
	}
	for _, entry := range c.entries {
		if !shouldRun(entry.spec, req) {
			continue
		}
		if err := entry.plugin.OnPodReady(ctx, req); err != nil {
			return fmt.Errorf("plugin %s OnPodReady: %w", entry.plugin.Name(), err)
		}
	}
	return nil
}

func containsTarget(targets []workloadv1alpha1.PluginTarget, needle workloadv1alpha1.PluginTarget) bool {
	for _, t := range targets {
		if t == needle || t == workloadv1alpha1.PluginTargetAll {
			return true
		}
	}
	return false
}

func containsRole(roles []string, role string) bool {
	return slices.Contains(roles, role)
}

func shouldRun(spec workloadv1alpha1.PluginSpec, req *HookRequest) bool {
	if req == nil {
		return false
	}
	if spec.Scope == nil {
		return true
	}
	if len(spec.Scope.Roles) > 0 && !containsRole(spec.Scope.Roles, req.RoleName) {
		return false
	}
	if len(spec.Scope.Targets) == 0 {
		return true
	}
	if req.IsEntry {
		return containsTarget(spec.Scope.Targets, workloadv1alpha1.PluginTargetEntry)
	}
	return containsTarget(spec.Scope.Targets, workloadv1alpha1.PluginTargetWorker)
}

// DecodeJSON decodes a plugin config into the provided out struct. It is a helper for built-in plugins.
func DecodeJSON(cfg *apiextensionsv1.JSON, out any) error {
	if cfg == nil || len(cfg.Raw) == 0 {
		return nil
	}
	return json.Unmarshal(cfg.Raw, out)
}
