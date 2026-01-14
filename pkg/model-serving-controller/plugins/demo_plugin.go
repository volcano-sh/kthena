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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const DemoPluginName = "demo-pod-tweaks"

// DemoConfig is a simple config used by the demo plugin to mutate pods.
type DemoConfig struct {
	RuntimeClassName string            `json:"runtimeClassName,omitempty"`
	Annotations      map[string]string `json:"annotations,omitempty"`
	Env              []corev1.EnvVar   `json:"env,omitempty"`
}

// DemoPlugin is a sample built-in plugin that tweaks runtimeClass, annotations, and env vars.
type DemoPlugin struct {
	name string
	cfg  DemoConfig
}

func init() {
	DefaultRegistry.Register(DemoPluginName, NewDemoPlugin)
}

// NewDemoPlugin constructs the demo plugin from PluginSpec config.
func NewDemoPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	cfg := DemoConfig{}
	if err := DecodeJSON(spec.Config, &cfg); err != nil {
		return nil, err
	}
	return &DemoPlugin{name: spec.Name, cfg: cfg}, nil
}

func (p *DemoPlugin) Name() string { return p.name }

// OnPodCreate mutates the pod in-place based on the provided config.
func (p *DemoPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.Pod == nil {
		return nil
	}
	if p.cfg.RuntimeClassName != "" {
		req.Pod.Spec.RuntimeClassName = ptr.To(p.cfg.RuntimeClassName)
	}
	if len(p.cfg.Annotations) > 0 {
		if req.Pod.Annotations == nil {
			req.Pod.Annotations = map[string]string{}
		}
		for k, v := range p.cfg.Annotations {
			req.Pod.Annotations[k] = v
		}
	}
	if len(p.cfg.Env) > 0 {
		for i := range req.Pod.Spec.Containers {
			req.Pod.Spec.Containers[i].Env = append(req.Pod.Spec.Containers[i].Env, p.cfg.Env...)
		}
		for i := range req.Pod.Spec.InitContainers {
			req.Pod.Spec.InitContainers[i].Env = append(req.Pod.Spec.InitContainers[i].Env, p.cfg.Env...)
		}
	}
	return nil
}

// OnPodReady is a no-op for the demo plugin.
func (p *DemoPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}
