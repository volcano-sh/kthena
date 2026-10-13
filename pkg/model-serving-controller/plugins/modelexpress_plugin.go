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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/bootstrap"
)

const (
	// ModelExpressPluginName is the managed plugin enabled by bootstrapAccelerateStrategy.
	// It cannot be registered in spec.plugins.
	ModelExpressPluginName = "modelexpress"

	ModelExpressServerAddressEnv = "MX_SERVER_ADDRESS"
	ModelExpressReadyURLEnv      = "MX_ARTIFACT_READY_URL"
	ModelExpressWorkerHostEnv    = "MX_WORKER_HOST"
)

// ModelExpressPlugin annotates in-scope Pods with their bootstrap-configuration
// hash and injects the ModelExpress client environment into engine containers.
type ModelExpressPlugin struct{}

func (p *ModelExpressPlugin) Name() string { return ModelExpressPluginName }

func (p *ModelExpressPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.Pod == nil || req.ModelServing == nil || !bootstrap.Enabled(req.ModelServing) {
		return nil
	}
	hash, err := bootstrap.ConfigHash(req.ModelServing, req.RoleName)
	if err != nil {
		return fmt.Errorf("calculate bootstrap configuration hash: %w", err)
	}
	if req.Pod.Annotations == nil {
		req.Pod.Annotations = map[string]string{}
	}
	req.Pod.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey] = hash

	mx := req.ModelServing.Spec.BootstrapAccelerateStrategy.ModelExpress
	if mx == nil {
		return nil
	}
	selected := 0
	for i := range req.Pod.Spec.Containers {
		container := &req.Pod.Spec.Containers[i]
		if !slices.Contains(mx.EngineContainers, container.Name) {
			continue
		}
		selected++
		if mx.ServerAddress != "" {
			addEnvIfAbsent(container, corev1.EnvVar{Name: ModelExpressServerAddressEnv, Value: mx.ServerAddress})
		}
		if mx.ReadyURL != "" {
			addEnvIfAbsent(container, corev1.EnvVar{Name: ModelExpressReadyURLEnv, Value: mx.ReadyURL})
		}
		addEnvIfAbsent(container, corev1.EnvVar{
			Name: ModelExpressWorkerHostEnv,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"},
			},
		})
	}
	if selected == 0 {
		return fmt.Errorf("pod %s has none of the engine containers %v", req.Pod.Name, mx.EngineContainers)
	}
	return nil
}

func (p *ModelExpressPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

func addEnvIfAbsent(container *corev1.Container, env corev1.EnvVar) {
	if slices.ContainsFunc(container.Env, func(existing corev1.EnvVar) bool { return existing.Name == env.Name }) {
		return
	}
	container.Env = append(container.Env, env)
}

// NewChainForModelServing builds the chain of ms's user plugins followed by
// the managed plugins enabled by its spec.
func NewChainForModelServing(registry *Registry, ms *workloadv1alpha1.ModelServing) (*Chain, error) {
	chain, err := NewChain(registry, ms.Spec.Plugins)
	if err != nil {
		return nil, err
	}
	if bootstrap.Enabled(ms) {
		chain.entries = append(chain.entries, entry{
			plugin: &ModelExpressPlugin{},
			spec: workloadv1alpha1.PluginSpec{
				Name: ModelExpressPluginName,
				Type: workloadv1alpha1.PluginTypeBuiltIn,
				Scope: &workloadv1alpha1.PluginScope{
					Roles:  ms.Spec.BootstrapAccelerateStrategy.Roles,
					Target: workloadv1alpha1.PluginTargetAll,
				},
			},
		})
	}
	return chain, nil
}
