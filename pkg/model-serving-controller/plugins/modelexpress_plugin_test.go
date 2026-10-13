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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/bootstrap"
)

var podIPEnv = corev1.EnvVar{
	Name: ModelExpressWorkerHostEnv,
	ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"},
	},
}

func mxModelServing(mx *workloadv1alpha1.ModelExpressConfig, roles ...string) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{}
	ms.Name = "ms"
	ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
		Provider:     workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress,
		Roles:        roles,
		ModelExpress: mx,
	}
	return ms
}

func mxPod() *corev1.Pod {
	pod := &corev1.Pod{}
	pod.Name = "pod"
	pod.Spec.InitContainers = []corev1.Container{{Name: "engine-init"}}
	pod.Spec.Containers = []corev1.Container{
		{Name: "engine", Env: []corev1.EnvVar{{Name: "OTHER", Value: "x"}}},
		{Name: "sidecar"},
	}
	return pod
}

func TestModelExpressPluginOnPodCreate(t *testing.T) {
	fullConfig := &workloadv1alpha1.ModelExpressConfig{
		EngineContainers: []string{"engine"},
		ServerAddress:    "mx:8001",
		ReadyURL:         "http://localhost:8000/health",
	}
	tests := []struct {
		name       string
		ms         *workloadv1alpha1.ModelServing
		mutatePod  func(pod *corev1.Pod)
		wantEnv    []corev1.EnvVar
		wantErr    bool
		wantNoHash bool
	}{
		{
			name: "inject all variables",
			ms:   mxModelServing(fullConfig),
			wantEnv: []corev1.EnvVar{
				{Name: "OTHER", Value: "x"},
				{Name: ModelExpressServerAddressEnv, Value: "mx:8001"},
				{Name: ModelExpressReadyURLEnv, Value: "http://localhost:8000/health"},
				podIPEnv,
			},
		},
		{
			name: "existing variables win",
			ms:   mxModelServing(fullConfig),
			mutatePod: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = []corev1.EnvVar{
					{Name: ModelExpressServerAddressEnv, Value: "user:1"},
					{Name: ModelExpressReadyURLEnv, Value: "http://user/ready"},
					{Name: ModelExpressWorkerHostEnv, Value: "10.0.0.1"},
				}
			},
			wantEnv: []corev1.EnvVar{
				{Name: ModelExpressServerAddressEnv, Value: "user:1"},
				{Name: ModelExpressReadyURLEnv, Value: "http://user/ready"},
				{Name: ModelExpressWorkerHostEnv, Value: "10.0.0.1"},
			},
		},
		{
			name: "empty fields are not injected",
			ms:   mxModelServing(&workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}),
			wantEnv: []corev1.EnvVar{
				{Name: "OTHER", Value: "x"},
				podIPEnv,
			},
		},
		{
			name:    "modelExpress omitted only annotates",
			ms:      mxModelServing(nil),
			wantEnv: []corev1.EnvVar{{Name: "OTHER", Value: "x"}},
		},
		{
			name:    "missing engine container",
			ms:      mxModelServing(&workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"vllm"}}),
			wantErr: true,
		},
		{
			name:       "strategy disabled",
			ms:         &workloadv1alpha1.ModelServing{},
			wantEnv:    []corev1.EnvVar{{Name: "OTHER", Value: "x"}},
			wantNoHash: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := mxPod()
			if tt.mutatePod != nil {
				tt.mutatePod(pod)
			}
			req := &HookRequest{ModelServing: tt.ms, RoleName: "prefill", IsEntry: true, Pod: pod}
			err := (&ModelExpressPlugin{}).OnPodCreate(context.Background(), req)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantEnv, pod.Spec.Containers[0].Env)
			assert.Empty(t, pod.Spec.Containers[1].Env, "sidecar must not change")
			assert.Empty(t, pod.Spec.InitContainers[0].Env, "init container must not change")
			for _, container := range pod.Spec.Containers {
				for _, env := range container.Env {
					assert.NotEqual(t, "MODEL_EXPRESS_URL", env.Name)
				}
			}
			if tt.wantNoHash {
				assert.NotContains(t, pod.Annotations, workloadv1alpha1.BootstrapConfigHashAnnotationKey)
				return
			}
			want, err := bootstrap.ConfigHash(tt.ms, "prefill")
			require.NoError(t, err)
			assert.Equal(t, want, pod.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey])
		})
	}
}

func TestModelExpressPluginReadyURLKeepsHash(t *testing.T) {
	config := &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}, ReadyURL: "http://a/ready"}
	first := mxPod()
	require.NoError(t, (&ModelExpressPlugin{}).OnPodCreate(context.Background(),
		&HookRequest{ModelServing: mxModelServing(config), RoleName: "r", Pod: first}))
	config = config.DeepCopy()
	config.ReadyURL = "http://b/ready"
	second := mxPod()
	require.NoError(t, (&ModelExpressPlugin{}).OnPodCreate(context.Background(),
		&HookRequest{ModelServing: mxModelServing(config), RoleName: "r", Pod: second}))
	assert.Equal(t, first.Annotations, second.Annotations)
}

func TestNewChainForModelServing(t *testing.T) {
	registry := NewRegistry()
	registry.Register("set-address", func(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
		return &setEnvPlugin{name: spec.Name}, nil
	})
	config := &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}, ServerAddress: "mx:8001"}

	t.Run("managed plugin runs after user plugins", func(t *testing.T) {
		ms := mxModelServing(config)
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "set-address", Type: workloadv1alpha1.PluginTypeBuiltIn}}
		chain, err := NewChainForModelServing(registry, ms)
		require.NoError(t, err)
		require.Len(t, chain.entries, 2)
		assert.Equal(t, ModelExpressPluginName, chain.entries[1].plugin.Name())

		pod := mxPod()
		require.NoError(t, chain.OnPodCreate(context.Background(), &HookRequest{ModelServing: ms, RoleName: "r", IsEntry: false, Pod: pod}))
		assert.Contains(t, pod.Spec.Containers[0].Env, corev1.EnvVar{Name: ModelExpressServerAddressEnv, Value: "from-plugin:1"})
		assert.NotContains(t, pod.Spec.Containers[0].Env, corev1.EnvVar{Name: ModelExpressServerAddressEnv, Value: "mx:8001"})
		assert.Contains(t, pod.Annotations, workloadv1alpha1.BootstrapConfigHashAnnotationKey)
	})

	t.Run("scoped to strategy roles", func(t *testing.T) {
		ms := mxModelServing(config, "decode")
		chain, err := NewChainForModelServing(registry, ms)
		require.NoError(t, err)
		pod := mxPod()
		require.NoError(t, chain.OnPodCreate(context.Background(), &HookRequest{ModelServing: ms, RoleName: "prefill", IsEntry: true, Pod: pod}))
		assert.Empty(t, pod.Annotations)
		pod = mxPod()
		require.NoError(t, chain.OnPodCreate(context.Background(), &HookRequest{ModelServing: ms, RoleName: "decode", IsEntry: false, Pod: pod}))
		assert.Contains(t, pod.Annotations, workloadv1alpha1.BootstrapConfigHashAnnotationKey)
	})

	t.Run("disabled strategy adds nothing", func(t *testing.T) {
		chain, err := NewChainForModelServing(registry, &workloadv1alpha1.ModelServing{})
		require.NoError(t, err)
		assert.Empty(t, chain.entries)
	})

	t.Run("managed plugin is not registered", func(t *testing.T) {
		ms := &workloadv1alpha1.ModelServing{}
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: ModelExpressPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn}}
		_, err := NewChainForModelServing(DefaultRegistry, ms)
		assert.Error(t, err)
	})
}

type setEnvPlugin struct{ name string }

func (p *setEnvPlugin) Name() string { return p.name }

func (p *setEnvPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	req.Pod.Spec.Containers[0].Env = append(req.Pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: ModelExpressServerAddressEnv, Value: "from-plugin:1"})
	return nil
}

func (p *setEnvPlugin) OnPodReady(_ context.Context, _ *HookRequest) error { return nil }
