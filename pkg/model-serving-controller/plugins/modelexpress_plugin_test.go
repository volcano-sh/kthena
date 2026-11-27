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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func mxTestRole(name string) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:     name,
		Replicas: ptr.To[int32](1),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "engine", Image: name + ":v1"}},
		}},
	}
}

// mxTestModelServing enables bootstrap acceleration with optional role scoping.
func mxTestModelServing(scoped []string, roles ...workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ms"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](2),
			Template: workloadv1alpha1.ServingGroup{Roles: roles},
			BootstrapAccelerateStrategy: &workloadv1alpha1.BootstrapAccelerateStrategy{
				Roles: scoped,
			},
		},
	}
}

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

func engineEnv(container corev1.Container) map[string]corev1.EnvVar {
	result := make(map[string]corev1.EnvVar, len(container.Env))
	for _, env := range container.Env {
		result[env.Name] = env
	}
	return result
}

func pluginRequest(ms *workloadv1alpha1.ModelServing, role string, pod *corev1.Pod) *HookRequest {
	return &HookRequest{ModelServing: ms, ServingGroup: "ms-0", RoleName: role, RoleID: role + "-0", Pod: pod, IsEntry: true}
}

func workerRequest(ms *workloadv1alpha1.ModelServing, role string, pod *corev1.Pod) *HookRequest {
	req := pluginRequest(ms, role, pod)
	req.IsEntry = false
	return req
}

func TestNewModelExpressPlugin(t *testing.T) {
	plain := mxTestModelServing(nil, mxTestRole("prefill"))
	plain.Spec.BootstrapAccelerateStrategy = nil
	assert.Nil(t, NewModelExpressPlugin(plain, ""), "no strategy, no managed plugin")

	allRoles := mxTestModelServing(nil, mxTestRole("prefill"), mxTestRole("decode"))
	require.NotNil(t, NewModelExpressPlugin(allRoles, ""), "provider defaults to ModelExpress")
	assert.Equal(t, &workloadv1alpha1.PluginScope{Roles: nil, Target: workloadv1alpha1.PluginTargetAll}, ModelExpressPluginScope(allRoles))

	ms := mxTestModelServing([]string{"decode", "prefill"}, mxTestRole("prefill"), mxTestRole("decode"))
	require.NotNil(t, NewModelExpressPlugin(ms, ""), "provider defaults to ModelExpress")
	assert.Equal(t, &workloadv1alpha1.PluginScope{Roles: []string{"decode", "prefill"}, Target: workloadv1alpha1.PluginTargetAll}, ModelExpressPluginScope(ms))

	ms.Spec.BootstrapAccelerateStrategy.Provider = "Other"
	assert.Nil(t, NewModelExpressPlugin(ms, ""), "only the ModelExpress provider has a managed plugin")
}

func TestModelExpressPluginInjectsEngineEnv(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"), mxTestRole("decode"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{
		EngineContainers: []string{"engine"},
		ReadyURL:         "http://127.0.0.1:8000/health",
	}
	plugin := NewModelExpressPlugin(ms, "mx.modelexpress:8001")
	require.NotNil(t, plugin)
	assert.Equal(t, ModelExpressPluginName, plugin.Name())
	_, inPlace := plugin.(InPlaceUpdatePlugin)
	assert.False(t, inPlace, "MX_MODEL_REVISION cannot be patched in place")

	pod := enginePod("ms-0-prefill-0-0")
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	assert.Empty(t, pod.Annotations, "source identity lives in MX_MODEL_REVISION, not in Pod metadata")

	engine := engineEnv(pod.Spec.Containers[0])
	assert.Equal(t, "default/ms/tmpl-hash-0", engine[EnvModelRevision].Value, "the entry is position 0 of the replica")
	assert.Equal(t, "mx.modelexpress:8001", engine[EnvServerAddress].Value)
	assert.Equal(t, "http://127.0.0.1:8000/health", engine[EnvArtifactReadyURL].Value)
	assert.Equal(t, "1", engine[EnvP2PMetadata].Value)
	assert.Equal(t, "5555", engine[EnvMetadataPort].Value)
	assert.Equal(t, "6555", engine[EnvWorkerGRPCPort].Value)
	assert.Equal(t, "LIBFABRIC", engine[EnvNIXLBackend].Value, "an existing variable wins")
	assert.Equal(t, "0", engine[EnvInstantTensor].Value)
	assert.Equal(t, "1", engine[EnvArtifactTransfer].Value)
	assert.Equal(t, "1", engine[EnvPoolRegistration].Value)
	assert.Equal(t, "1", engine[EnvVMMArena].Value)
	assert.Equal(t, "INFO", engine[EnvModelExpressLog].Value)
	assert.Equal(t, "INFO", engine[EnvNIXLLog].Value)
	assert.Equal(t, "WARN", engine[EnvUCXLog].Value)
	assert.Equal(t, "auto", engine[EnvRDMANICPin].Value)
	for name, fieldPath := range map[string]string{
		EnvWorkerHost:   "status.podIP",
		EnvPodName:      "metadata.name",
		EnvPodUID:       "metadata.uid",
		EnvPodNamespace: "metadata.namespace",
	} {
		require.NotNil(t, engine[name].ValueFrom, name)
		assert.Equal(t, fieldPath, engine[name].ValueFrom.FieldRef.FieldPath, name)
	}
	_, deprecated := engine["MODEL_EXPRESS_URL"]
	assert.False(t, deprecated, "the deprecated endpoint variable is never injected")
	assert.Len(t, engine, 19)

	assert.Empty(t, pod.Spec.Containers[1].Env, "unselected containers are untouched")
	assert.Empty(t, pod.Spec.InitContainers[0].Env, "init containers are untouched")

	// Idempotent: a second run does not duplicate variables.
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	assert.Len(t, pod.Spec.Containers[0].Env, 19)
}

func TestModelExpressPluginDefaultAllRolesScope(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"), mxTestRole("decode"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	plugin := NewModelExpressPlugin(ms, "mx:8001")
	chain := (*Chain)(nil).WithManagedPlugin(plugin, ModelExpressPluginScope(ms))

	for _, role := range []string{"prefill", "decode"} {
		pod := enginePod("ms-0-" + role + "-0-0")
		require.NoError(t, chain.OnPodCreate(context.Background(), pluginRequest(ms, role, pod)))
		assert.Equal(t, "mx:8001", engineEnv(pod.Spec.Containers[0])[EnvServerAddress].Value, "role %s should receive MX env when roles is omitted", role)
	}
}

func TestModelExpressPluginModelRevisionPerPosition(t *testing.T) {
	ms := mxTestModelServing([]string{"prefill"}, mxTestRole("prefill"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	plugin := NewModelExpressPlugin(ms, "")

	worker := enginePod("ms-0-prefill-0-2")
	require.NoError(t, plugin.OnPodCreate(context.Background(), workerRequest(ms, "prefill", worker)))
	assert.Equal(t, "default/ms/tmpl-hash-2", engineEnv(worker.Spec.Containers[0])[EnvModelRevision].Value, "workers use their index from the Pod name")

	indexed := enginePod("ms-0-prefill-0-2")
	indexed.Spec.Containers[0].Env = append(indexed.Spec.Containers[0].Env, corev1.EnvVar{Name: workloadv1alpha1.WorkerIndexEnv, Value: "3"})
	require.NoError(t, plugin.OnPodCreate(context.Background(), workerRequest(ms, "prefill", indexed)))
	assert.Equal(t, "default/ms/tmpl-hash-3", engineEnv(indexed.Spec.Containers[0])[EnvModelRevision].Value, "the WORKER_INDEX the engine observes wins")

	entry := enginePod("ms-0-prefill-0-0")
	entry.Spec.Containers[0].Env = append(entry.Spec.Containers[0].Env, corev1.EnvVar{Name: workloadv1alpha1.WorkerIndexEnv, Value: "0"})
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", entry)))
	assert.Equal(t, "default/ms/tmpl-hash-0", engineEnv(entry.Spec.Containers[0])[EnvModelRevision].Value)

	unnamed := enginePod("nodash")
	err := plugin.OnPodCreate(context.Background(), workerRequest(ms, "prefill", unnamed))
	require.ErrorContains(t, err, "cannot derive worker-index")
}

// Identical role templates of other ModelServings can mount different weights,
// so their Pods must never match as sources of each other.
func TestModelExpressPluginModelRevisionPerModelServing(t *testing.T) {
	revision := func(namespace, name string) string {
		ms := mxTestModelServing(nil, mxTestRole("prefill"))
		ms.Namespace, ms.Name = namespace, name
		ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
		pod := enginePod(name + "-0-prefill-0-0")
		require.NoError(t, NewModelExpressPlugin(ms, "").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
		return engineEnv(pod.Spec.Containers[0])[EnvModelRevision].Value
	}
	assert.Equal(t, "team-a/llm/tmpl-hash-0", revision("team-a", "llm"))
	assert.Equal(t, revision("team-a", "llm"), revision("team-a", "llm"), "replicas of one ModelServing share sources")
	assert.NotEqual(t, revision("team-a", "llm"), revision("team-b", "llm"), "same template in another namespace")
	assert.NotEqual(t, revision("team-a", "llm"), revision("team-a", "other"), "same template in another ModelServing")
}

func TestModelExpressPluginKeepsUserNICSelection(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	plugin := NewModelExpressPlugin(ms, "")

	devices := enginePod("ms-0-prefill-0-0")
	devices.Spec.Containers[0].Env = append(devices.Spec.Containers[0].Env, corev1.EnvVar{Name: EnvUCXNetDevices, Value: "mlx5_0:1"})
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", devices)))
	engine := engineEnv(devices.Spec.Containers[0])
	_, pinned := engine[EnvRDMANICPin]
	assert.False(t, pinned, "the auto probe would overwrite the user's UCX_NET_DEVICES")
	assert.Equal(t, "mlx5_0:1", engine[EnvUCXNetDevices].Value)

	explicit := enginePod("ms-0-prefill-0-0")
	explicit.Spec.Containers[0].Env = append(explicit.Spec.Containers[0].Env,
		corev1.EnvVar{Name: EnvRDMANICPin, Value: "mlx5_0,mlx5_1"},
		corev1.EnvVar{Name: EnvPodName, Value: "custom"})
	require.NoError(t, plugin.OnPodCreate(context.Background(), pluginRequest(ms, "prefill", explicit)))
	engine = engineEnv(explicit.Spec.Containers[0])
	assert.Equal(t, "mlx5_0,mlx5_1", engine[EnvRDMANICPin].Value, "an explicit NIC list wins")
	assert.Equal(t, "custom", engine[EnvPodName].Value, "an existing variable wins")
	assert.Nil(t, engine[EnvPodName].ValueFrom)
}

func TestModelExpressPluginOptionalValues(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	pod := enginePod("ms-0-prefill-0-0")
	delete(pod.Labels, workloadv1alpha1.RoleTemplateHashLabelKey)
	require.NoError(t, NewModelExpressPlugin(ms, "").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	engine := engineEnv(pod.Spec.Containers[0])
	for _, name := range []string{EnvModelRevision, EnvServerAddress, EnvArtifactReadyURL} {
		_, present := engine[name]
		assert.False(t, present, "%s is injected only when its value is known", name)
	}
	assert.Equal(t, "1", engine[EnvP2PMetadata].Value)
}

func TestModelExpressPluginWithoutModelExpressConfig(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"))
	pod := enginePod("ms-0-prefill-0-0")
	require.NoError(t, NewModelExpressPlugin(ms, "mx:8001").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod)))
	assert.Empty(t, pod.Annotations)
	assert.Len(t, pod.Spec.Containers[0].Env, 1, "no environment is injected without modelExpress")
}

func TestModelExpressPluginSkipsOutOfScopeRole(t *testing.T) {
	ms := mxTestModelServing([]string{"prefill"}, mxTestRole("prefill"), mxTestRole("decode"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"engine"}}
	plugin := NewModelExpressPlugin(ms, "mx:8001")
	// Scope is enforced by the chain the controller builds, not by the plugin.
	chain := (*Chain)(nil).WithManagedPlugin(plugin, ModelExpressPluginScope(ms))
	pod := enginePod("ms-0-decode-0-0")
	require.NoError(t, chain.OnPodCreate(context.Background(), pluginRequest(ms, "decode", pod)))
	assert.Empty(t, pod.Annotations)
	assert.Len(t, pod.Spec.Containers[0].Env, 1)
	require.NoError(t, plugin.OnPodCreate(context.Background(), nil))
}

func TestModelExpressPluginRequiresEngineContainer(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"))
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"vllm"}}
	pod := enginePod("ms-0-prefill-0-0")
	err := NewModelExpressPlugin(ms, "mx:8001").OnPodCreate(context.Background(), pluginRequest(ms, "prefill", pod))
	require.ErrorContains(t, err, "none of the ModelExpress engine containers vllm")
}

func TestModelExpressPluginNoopHooks(t *testing.T) {
	ms := mxTestModelServing(nil, mxTestRole("prefill"))
	plugin := NewModelExpressPlugin(ms, "")
	require.NoError(t, plugin.OnPodReady(context.Background(), nil))
	require.NoError(t, plugin.OnRoleSync(context.Background(), nil))
	require.NoError(t, plugin.OnRoleDelete(context.Background(), nil))
}
