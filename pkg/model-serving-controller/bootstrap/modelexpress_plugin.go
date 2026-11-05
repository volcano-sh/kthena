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
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// ModelExpressPluginName is the managed plugin appended to the chain when
// bootstrapAccelerateStrategy is set. It cannot be listed in spec.plugins.
const ModelExpressPluginName = "modelexpress"

// ModelExpress client environment variables.
// https://github.com/ai-dynamo/modelexpress/blob/main/docs/DEPLOYMENT.md#p2p-environment-variables
const (
	EnvModelRevision    = "MX_MODEL_REVISION"
	EnvServerAddress    = "MX_SERVER_ADDRESS"
	EnvP2PMetadata      = "MX_P2P_METADATA"
	EnvMetadataPort     = "MX_METADATA_PORT"
	EnvWorkerGRPCPort   = "MX_WORKER_GRPC_PORT"
	EnvNIXLBackend      = "MX_NIXL_BACKEND"
	EnvInstantTensor    = "MX_INSTANT_TENSOR"
	EnvArtifactTransfer = "MX_ARTIFACT_TRANSFER"
	EnvArtifactReadyURL = "MX_ARTIFACT_READY_URL"
	EnvModelExpressLog  = "MODEL_EXPRESS_LOG_LEVEL"
	EnvNIXLLog          = "NIXL_LOG_LEVEL"
	EnvUCXLog           = "UCX_LOG_LEVEL"
	EnvWorkerHost       = "MX_WORKER_HOST"
)

// defaultEngineEnv holds the fixed central-server defaults. A variable already
// present in the selected container always wins.
var defaultEngineEnv = []corev1.EnvVar{
	{Name: EnvP2PMetadata, Value: "1"},
	{Name: EnvMetadataPort, Value: "5555"},
	{Name: EnvWorkerGRPCPort, Value: "6555"},
	{Name: EnvNIXLBackend, Value: "UCX"},
	{Name: EnvInstantTensor, Value: "0"},
	{Name: EnvArtifactTransfer, Value: "1"},
	{Name: EnvModelExpressLog, Value: "INFO"},
	{Name: EnvNIXLLog, Value: "INFO"},
	{Name: EnvUCXLog, Value: "WARN"},
	{Name: EnvWorkerHost, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
}

// modelExpressPlugin writes the bootstrap configuration hash on every in-scope
// Pod and, when modelExpress is configured, injects the MX client environment
// into the selected inference-engine containers.
//
// It deliberately does not implement plugins.InPlaceUpdatePlugin: MX_MODEL_REVISION
// is bound to the role template hash at creation and cannot be patched in place,
// so in-place updates fall back to recreating the outdated replicas.
type modelExpressPlugin struct {
	serverAddress string
}

var _ plugins.Plugin = (*modelExpressPlugin)(nil)

// NewModelExpressPlugin returns the managed plugin for ms, or nil when ms does
// not use the ModelExpress provider.
func NewModelExpressPlugin(ms *workloadv1alpha1.ModelServing, serverAddress string) plugins.Plugin {
	if Provider(ms) != workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress {
		return nil
	}
	return &modelExpressPlugin{serverAddress: serverAddress}
}

// PluginScope limits the managed plugin to the strategy's roles on both entry
// and worker Pods.
func PluginScope(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.PluginScope {
	return &workloadv1alpha1.PluginScope{Roles: ScopedRoleNames(ms), Target: workloadv1alpha1.PluginTargetAll}
}

func (p *modelExpressPlugin) Name() string { return ModelExpressPluginName }

func (p *modelExpressPlugin) OnPodCreate(_ context.Context, req *plugins.HookRequest) error {
	if req == nil || req.Pod == nil || req.ModelServing == nil {
		return nil
	}
	if _, ok := RoleConfig(req.ModelServing, req.RoleName); !ok {
		return nil
	}
	hash, err := ConfigHash(req.ModelServing, req.RoleName, p.serverAddress)
	if err != nil {
		return err
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
		p.injectEngineEnv(container, utils.ObjectRoleTemplateHash(req.Pod), mx.ReadyURL)
	}
	if selected == 0 {
		return fmt.Errorf("pod %s has none of the ModelExpress engine containers %s", req.Pod.Name, strings.Join(mx.EngineContainers, ", "))
	}
	return nil
}

func (p *modelExpressPlugin) injectEngineEnv(container *corev1.Container, roleTemplateHash, readyURL string) {
	if roleTemplateHash != "" {
		setEnvIfAbsent(container, corev1.EnvVar{Name: EnvModelRevision, Value: roleTemplateHash})
	}
	if p.serverAddress != "" {
		setEnvIfAbsent(container, corev1.EnvVar{Name: EnvServerAddress, Value: p.serverAddress})
	}
	if readyURL != "" {
		setEnvIfAbsent(container, corev1.EnvVar{Name: EnvArtifactReadyURL, Value: readyURL})
	}
	for _, env := range defaultEngineEnv {
		setEnvIfAbsent(container, *env.DeepCopy())
	}
}

func setEnvIfAbsent(container *corev1.Container, env corev1.EnvVar) {
	for _, existing := range container.Env {
		if existing.Name == env.Name {
			return
		}
	}
	container.Env = append(container.Env, env)
}

func (p *modelExpressPlugin) OnPodReady(_ context.Context, _ *plugins.HookRequest) error { return nil }

func (p *modelExpressPlugin) OnRoleSync(_ context.Context, _ *plugins.HookRequest) error { return nil }

func (p *modelExpressPlugin) OnRoleDelete(_ context.Context, _ *plugins.HookRequest) error {
	return nil
}
