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

package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

func enginePod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{workloadv1alpha1.RoleTemplateHashLabelKey: "tmpl-hash"},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "downloader"}},
			Containers: []corev1.Container{
				{Name: "engine", Env: []corev1.EnvVar{{Name: EnvNIXLBackend, Value: "LIBFABRIC"}}},
				{Name: "sidecar"},
			},
		},
	}
}

func envMap(container corev1.Container) map[string]corev1.EnvVar {
	result := make(map[string]corev1.EnvVar, len(container.Env))
	for _, env := range container.Env {
		result[env.Name] = env
	}
	return result
}

func pluginRequest(ms *workloadv1alpha1.ModelServing, role string, pod *corev1.Pod) *plugins.HookRequest {
	return &plugins.HookRequest{ModelServing: ms, ServingGroup: "ms-0", RoleName: role, RoleID: role + "-0", Pod: pod, IsEntry: true}
}

func TestModelExpressPluginInjectsEngineEnv(t *testing.T) {
	ms := withStrategy(testModelServing(2, testRole("prefill", 1), testRole("decode", 1)), seed("prefill", nil))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{
		EngineContainers: []string{"engine"},
		ReadyURL:         "http://127.0.0.1:8000/health",
	}
	plugin := NewModelExpressPlugin(ms, "mx.modelexpress:8001")
	require.NotNil(t, plugin)
	assert.Equal(t, ModelExpressPluginName, plugin.Name())
	_, inPlace := plugin.(plugins.InPlaceUpdatePlugin)
	assert.False(t, inPlace, "MX_MODEL_REVISION cannot be patched in place")

	pod := enginePod("ms-0-prefill-0-0")
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))

	hash, err := ConfigHash(ms, "prefill", "mx.modelexpress:8001")
	require.NoError(t, err)
	assert.Equal(t, hash, pod.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey])

	engine := envMap(pod.Spec.Containers[0])
	assert.Equal(t, "tmpl-hash", engine[EnvModelRevision].Value)
	assert.Equal(t, "mx.modelexpress:8001", engine[EnvServerAddress].Value)
	assert.Equal(t, "http://127.0.0.1:8000/health", engine[EnvArtifactReadyURL].Value)
	assert.Equal(t, "1", engine[EnvP2PMetadata].Value)
	assert.Equal(t, "5555", engine[EnvMetadataPort].Value)
	assert.Equal(t, "6555", engine[EnvWorkerGRPCPort].Value)
	assert.Equal(t, "LIBFABRIC", engine[EnvNIXLBackend].Value, "an existing variable wins")
	assert.Equal(t, "0", engine[EnvInstantTensor].Value)
	assert.Equal(t, "1", engine[EnvArtifactTransfer].Value)
	assert.Equal(t, "INFO", engine[EnvModelExpressLog].Value)
	assert.Equal(t, "INFO", engine[EnvNIXLLog].Value)
	assert.Equal(t, "WARN", engine[EnvUCXLog].Value)
	require.NotNil(t, engine[EnvWorkerHost].ValueFrom)
	assert.Equal(t, "status.podIP", engine[EnvWorkerHost].ValueFrom.FieldRef.FieldPath)
	_, deprecated := engine["MODEL_EXPRESS_URL"]
	assert.False(t, deprecated, "the deprecated endpoint variable is never injected")
	assert.Len(t, engine, 13)

	assert.Empty(t, pod.Spec.Containers[1].Env, "unselected containers are untouched")
	assert.Empty(t, pod.Spec.InitContainers[0].Env, "init containers are untouched")

	// Idempotent: a second run does not duplicate variables.
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	assert.Len(t, pod.Spec.Containers[0].Env, 13)
}

func TestModelExpressPluginOptionalValues(t *testing.T) {
	ms := withStrategy(testModelServing(2, testRole("prefill", 1)), seed("prefill", nil))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	pod := enginePod("ms-0-prefill-0-0")
	delete(pod.Labels, workloadv1alpha1.RoleTemplateHashLabelKey)
	require.NoError(t, NewModelExpressPlugin(ms, "").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	engine := envMap(pod.Spec.Containers[0])
	for _, name := range []string{EnvModelRevision, EnvServerAddress, EnvArtifactReadyURL} {
		_, present := engine[name]
		assert.False(t, present, "%s is injected only when its value is known", name)
	}
	assert.Equal(t, "1", engine[EnvP2PMetadata].Value)
}

func TestModelExpressPluginWithoutModelExpressConfig(t *testing.T) {
	ms := withStrategy(testModelServing(2, testRole("prefill", 1)), seed("prefill", nil))
	pod := enginePod("ms-0-prefill-0-0")
	require.NoError(t, NewModelExpressPlugin(ms, "mx:8001").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	assert.NotEmpty(t, pod.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey], "source identity is recorded even without env injection")
	assert.Len(t, pod.Spec.Containers[0].Env, 1, "no environment is injected without modelExpress")
}

func TestModelExpressPluginSkipsOutOfScopeRole(t *testing.T) {
	ms := withStrategy(testModelServing(2, testRole("prefill", 1), testRole("decode", 1)), seed("prefill", nil))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	pod := enginePod("ms-0-decode-0-0")
	require.NoError(t, NewModelExpressPlugin(ms, "mx:8001").OnPodCreate(context.Background(), pluginRequest(ms, "decode", pod)))
	assert.Empty(t, pod.Annotations)
	assert.Len(t, pod.Spec.Containers[0].Env, 1)
	require.NoError(t, NewModelExpressPlugin(ms, "mx:8001").OnPodCreate(context.Background(), nil))
}

func TestModelExpressPluginRequiresEngineContainer(t *testing.T) {
	ms := withStrategy(testModelServing(2, testRole("prefill", 1)), seed("prefill", nil))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"vllm"}}
	pod := enginePod("ms-0-prefill-0-0")
	err := NewModelExpressPlugin(ms, "mx:8001").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod))
	require.ErrorContains(t, err, "none of the ModelExpress engine containers vllm")
}

func TestModelExpressPluginNoopHooks(t *testing.T) {
	ms := withStrategy(testModelServing(1, testRole("prefill", 1)), seed("prefill", nil))
	plugin := NewModelExpressPlugin(ms, "")
	require.NoError(t, plugin.OnPodReady(context.Background(), nil))
	require.NoError(t, plugin.OnRoleSync(context.Background(), nil))
	require.NoError(t, plugin.OnRoleDelete(context.Background(), nil))
}
