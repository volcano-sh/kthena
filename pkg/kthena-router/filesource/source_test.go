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

package filesource

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type fakePodRuntimeInspector struct{}

func (fakePodRuntimeInspector) GetPodMetrics(_ string, _ *corev1.Pod, _ uint32, _ map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	return nil, nil
}

func (fakePodRuntimeInspector) GetPodModels(_ string, _ *corev1.Pod, _ uint32) ([]string, error) {
	return nil, nil
}

const modelRouteManifest = `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: demo
spec:
  modelName: demo-model
  rules:
    - name: default
      targetModels:
        - modelServerName: demo-server
`

const modelServerManifest = `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
    - name: vllm-1
      address: 10.0.0.2
      port: 8001
`

func newTestSource(t *testing.T, dir string) (*Source, datastore.Store) {
	t.Helper()
	store := datastore.New(datastore.WithPodRuntimeInspector(fakePodRuntimeInspector{}))
	source, err := NewSource(dir, time.Second, store, nil)
	require.NoError(t, err)
	return source, store
}

func writeManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

func TestSourceLoadsResources(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "resources.yaml", modelRouteManifest+"---\n"+modelServerManifest)
	// Files that are not manifests must be ignored.
	writeManifest(t, dir, "notes.txt", "not a manifest")

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	require.NotNil(t, store.GetModelRoute("default/demo"))
	assert.Equal(t, []string{"demo-model"}, store.GetModelNames())

	msName := types.NamespacedName{Namespace: "default", Name: "demo-server"}
	ms := store.GetModelServer(msName)
	require.NotNil(t, ms)
	assert.Len(t, ms.Spec.Endpoints, 2)

	pods, err := store.GetPodsByModelServer(msName)
	require.NoError(t, err)
	assert.Len(t, pods, 2)

	podInfo := store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "demo-server:vllm-1"})
	require.NotNil(t, podInfo)
	assert.Equal(t, "10.0.0.2", podInfo.GetPod().Status.PodIP)
}

func TestSourceSkipsUnchangedDirectory(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "route.yaml", modelRouteManifest)

	source, _ := newTestSource(t, dir)
	require.NoError(t, source.sync())
	digest := source.digest

	require.NoError(t, source.sync())
	assert.Equal(t, digest, source.digest)
}

func TestSourceAppliesUpdatesAndDeletions(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "route.yaml", modelRouteManifest)
	writeManifest(t, dir, "server.yaml", modelServerManifest)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	require.NoError(t, os.Remove(filepath.Join(dir, "route.yaml")))
	writeManifest(t, dir, "server.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.9
`)
	require.NoError(t, source.sync())

	assert.Nil(t, store.GetModelRoute("default/demo"))
	assert.Empty(t, store.GetModelNames())

	msName := types.NamespacedName{Namespace: "default", Name: "demo-server"}
	pods, err := store.GetPodsByModelServer(msName)
	require.NoError(t, err)
	require.Len(t, pods, 1)
	assert.Equal(t, "10.0.0.9", pods[0].GetPod().Status.PodIP)

	require.NoError(t, os.Remove(filepath.Join(dir, "server.yaml")))
	require.NoError(t, source.sync())
	assert.Nil(t, store.GetModelServer(msName))
	assert.Nil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "demo-server:vllm-0"}))
}

func TestSourceKeepsLastGoodSnapshotOnParseError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "route.yaml", modelRouteManifest)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	writeManifest(t, dir, "broken.yaml", "apiVersion: networking.serving.volcano.sh/v1alpha1\nkind: ModelRoute\nmetadata:\n  name: broken\nspec:\n  rules: [\n")
	require.Error(t, source.sync())
	assert.NotNil(t, store.GetModelRoute("default/demo"))
}

func TestSourceRejectsResourceWithoutName(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "route.yaml", "apiVersion: networking.serving.volcano.sh/v1alpha1\nkind: ModelRoute\nspec:\n  modelName: demo\n")

	source, _ := newTestSource(t, dir)
	assert.ErrorContains(t, source.sync(), "metadata.name")
}

func TestSourceRejectsInvalidResources(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "route.yaml", modelRouteManifest)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	// Endpoints and workloadSelector.matchLabels are mutually exclusive; the
	// webhook would reject this ModelServer, so file mode must reject it too.
	writeManifest(t, dir, "server.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadSelector:
    matchLabels:
      app: vllm
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
      port: 8000
`)
	require.ErrorContains(t, source.sync(), "mutually exclusive")

	// The last good snapshot keeps serving and the invalid server is not stored.
	assert.NotNil(t, store.GetModelRoute("default/demo"))
	assert.Nil(t, store.GetModelServer(types.NamespacedName{Namespace: "default", Name: "demo-server"}))
}

func TestSourceAppliesCRDDefaults(t *testing.T) {
	dir := t.TempDir()
	// None of the manifests set the fields carrying `+kubebuilder:default`
	// markers, so loading from file must fill in the same values the API
	// server would apply through CRD structural defaulting.
	writeManifest(t, dir, "resources.yaml", modelRouteManifest+`  rateLimit:
    inputTokensPerUnit: 100
---
`+modelServerManifest+`  kvConnector: {}
  trafficPolicy:
    retry:
      attempts: 2
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: provider
spec:
  baseURL: https://api.example.com
`)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	route := store.GetModelRoute("default/demo")
	require.NotNil(t, route)
	require.NotNil(t, route.Spec.Rules[0].TargetModels[0].Weight)
	assert.Equal(t, uint32(100), *route.Spec.Rules[0].TargetModels[0].Weight)
	assert.Equal(t, aiv1alpha1.Second, route.Spec.RateLimit.Unit)

	ms := store.GetModelServer(types.NamespacedName{Namespace: "default", Name: "demo-server"})
	require.NotNil(t, ms)
	assert.Equal(t, "http", ms.Spec.WorkloadPort.Protocol)
	assert.Equal(t, aiv1alpha1.ConnectorTypeHTTP, ms.Spec.KVConnector.Type)
	require.NotNil(t, ms.Spec.TrafficPolicy.Retry.RetryInterval)
	assert.Equal(t, 100*time.Millisecond, ms.Spec.TrafficPolicy.Retry.RetryInterval.Duration)

	provider := store.GetExternalModelProvider(types.NamespacedName{Namespace: "default", Name: "provider"})
	require.NotNil(t, provider)
	assert.Equal(t, aiv1alpha1.OpenAI, provider.Spec.ProviderType)
}

func TestSourceConvertsSecretStringData(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "secret.yaml", `apiVersion: v1
kind: Secret
metadata:
  name: provider-auth
data:
  keep: a2VlcA==
  overridden: b2xk
stringData:
  token: my-token
  overridden: new
`)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	secret := store.GetSecret(types.NamespacedName{Namespace: "default", Name: "provider-auth"})
	require.NotNil(t, secret)
	assert.Empty(t, secret.StringData)
	assert.Equal(t, []byte("keep"), secret.Data["keep"])
	assert.Equal(t, []byte("my-token"), secret.Data["token"])
	// stringData wins over data on conflicting keys, like on the API server.
	assert.Equal(t, []byte("new"), secret.Data["overridden"])
}

func TestNewRejectsMissingDirectory(t *testing.T) {
	store := datastore.New()
	_, err := NewSource(filepath.Join(t.TempDir(), "missing"), time.Second, store, nil)
	assert.Error(t, err)
}

func TestRunSyncsAndSignalsReadiness(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "route.yaml", modelRouteManifest)

	source, store := newTestSource(t, dir)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- source.Run(stop) }()

	assert.Eventually(t, source.HasSynced, 5*time.Second, 10*time.Millisecond)
	assert.NotNil(t, store.GetModelRoute("default/demo"))

	close(stop)
	require.NoError(t, <-done)
}

const schedulerProfileManifest = `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: SchedulerProfile
metadata:
  name: fast
spec:
  plugins:
    score:
      enabled:
        - name: prefix-cache
          weight: 2
`

func TestSourceLoadsSchedulerProfile(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "profile.yaml", schedulerProfileManifest)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	key := types.NamespacedName{Namespace: "default", Name: "fast"}
	profile := store.GetSchedulerProfile(key)
	require.NotNil(t, profile)
	require.Len(t, profile.Spec.Plugins.Score.Enabled, 1)
	assert.Equal(t, "prefix-cache", profile.Spec.Plugins.Score.Enabled[0].Name)
	assert.EqualValues(t, 2, profile.Spec.Plugins.Score.Enabled[0].Weight)

	// Removing the file removes it from the store.
	require.NoError(t, os.Remove(filepath.Join(dir, "profile.yaml")))
	require.NoError(t, source.sync())
	assert.Nil(t, store.GetSchedulerProfile(key))
}

func TestSourceRejectsInvalidSchedulerProfile(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "profile.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: SchedulerProfile
metadata:
  name: bad
spec:
  plugins:
    filter:
      enabled: [no-such-plugin]
`)

	source, _ := newTestSource(t, dir)
	err := source.sync()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid SchedulerProfile default/bad")
}

func TestSourceRejectsMissingSchedulerProfileReference(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "resources.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
  trafficPolicy:
    loadBalancer:
      schedulerProfile: missing
`)

	source, store := newTestSource(t, dir)
	err := source.sync()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `references SchedulerProfile "missing" which is not loaded`)
	assert.Nil(t, store.GetModelServer(types.NamespacedName{Namespace: "default", Name: "demo-server"}))
}

func TestSourceAcceptsLoadedSchedulerProfileReference(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "resources.yaml", schedulerProfileManifest+`---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
  trafficPolicy:
    loadBalancer:
      schedulerProfile: fast
`)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())
	assert.NotNil(t, store.GetSchedulerProfile(types.NamespacedName{Namespace: "default", Name: "fast"}))
	assert.NotNil(t, store.GetModelServer(types.NamespacedName{Namespace: "default", Name: "demo-server"}))
}

func TestSourceUpdatesSchedulerProfile(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "profile.yaml", schedulerProfileManifest)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())
	key := types.NamespacedName{Namespace: "default", Name: "fast"}
	require.NotNil(t, store.GetSchedulerProfile(key))
	assert.EqualValues(t, 2, store.GetSchedulerProfile(key).Spec.Plugins.Score.Enabled[0].Weight)

	writeManifest(t, dir, "profile.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: SchedulerProfile
metadata:
  name: fast
spec:
  plugins:
    score:
      enabled:
        - name: prefix-cache
          weight: 7
`)
	require.NoError(t, source.sync())
	require.NotNil(t, store.GetSchedulerProfile(key))
	assert.EqualValues(t, 7, store.GetSchedulerProfile(key).Spec.Plugins.Score.Enabled[0].Weight)
}

func TestSourceRejectsCrossNamespaceProfileReference(t *testing.T) {
	dir := t.TempDir()
	// The profile lives in the default namespace; the ModelServer referencing
	// it lives in another one, and references are namespace-scoped.
	writeManifest(t, dir, "resources.yaml", schedulerProfileManifest+`---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
  namespace: other
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
  trafficPolicy:
    loadBalancer:
      schedulerProfile: fast
`)

	source, store := newTestSource(t, dir)
	err := source.sync()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `references SchedulerProfile "fast" which is not loaded`)
	assert.Nil(t, store.GetSchedulerProfile(types.NamespacedName{Namespace: "default", Name: "fast"}))
}

func TestSourceKeepsLastGoodSnapshotOnInvalidProfileReference(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "resources.yaml", schedulerProfileManifest+`---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
  trafficPolicy:
    loadBalancer:
      schedulerProfile: fast
`)

	source, store := newTestSource(t, dir)
	require.NoError(t, source.sync())

	// Break the reference: the snapshot is rejected and the previous one keeps
	// serving.
	writeManifest(t, dir, "resources.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: demo-server
spec:
  inferenceEngine: vLLM
  workloadPort:
    port: 8000
  endpoints:
    - name: vllm-0
      address: 10.0.0.1
  trafficPolicy:
    loadBalancer:
      schedulerProfile: fast
`)
	require.Error(t, source.sync())

	assert.NotNil(t, store.GetSchedulerProfile(types.NamespacedName{Namespace: "default", Name: "fast"}),
		"the last good snapshot must keep serving")
	assert.NotNil(t, store.GetModelServer(types.NamespacedName{Namespace: "default", Name: "demo-server"}))
}

func TestSourceRejectsInvalidProfileWeight(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "profile.yaml", `apiVersion: networking.serving.volcano.sh/v1alpha1
kind: SchedulerProfile
metadata:
  name: zero-weight
spec:
  plugins:
    score:
      enabled:
        - name: prefix-cache
          weight: 0
`)

	source, _ := newTestSource(t, dir)
	err := source.sync()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid SchedulerProfile default/zero-weight")
}
