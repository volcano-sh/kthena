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

package routerplugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	plugincontext "github.com/volcano-sh/kthena/test/e2e/router-plugins/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	routerConfigMapName  = "kthena-router-config"
	routerConfigKey      = "routerConfiguration"
	routerDeploymentName = "kthena-router"
	routerRolloutTimeout = 3 * time.Minute
	routerDebugPort      = "15000"

	leastRequestMaxWaitingRequests = 1
	leastRequestLoadWaitTimeout    = 60 * time.Second
)

func deployModelServerFromFile(t *testing.T, ctx context.Context, kthena *clientset.Clientset, namespace, file string) *networkingv1alpha1.ModelServer {
	t.Helper()
	ms := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(plugincontext.TestDataDir, file))
	ms.Namespace = namespace
	created, err := kthena.NetworkingV1alpha1().ModelServers(namespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kthena.NetworkingV1alpha1().ModelServers(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	})
	return created
}

func deployModelRouteFromFile(t *testing.T, ctx context.Context, kthena *clientset.Clientset, namespace, file string) *networkingv1alpha1.ModelRoute {
	t.Helper()
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute](filepath.Join(plugincontext.TestDataDir, file))
	modelRoute.Namespace = namespace
	created, err := kthena.NetworkingV1alpha1().ModelRoutes(namespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kthena.NetworkingV1alpha1().ModelRoutes(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	})
	return created
}

func deploySlowLatencyMockStack(t *testing.T, kube kubernetes.Interface, namespace string) {
	t.Helper()
	ctx := context.Background()
	name := plugincontext.SlowMockDeploymentName

	// Recreate so a prior test or rollout cannot leave two ready slow pods (different ReplicaSets).
	utils.DeleteDeploymentAndWait(t, kube, namespace, name, 2*time.Minute)

	deployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(plugincontext.TestDataDir, "LLM-Mock-plugins-slow.yaml"))
	deployment.Namespace = namespace
	_, err := kube.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kube.AppsV1().Deployments(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	utils.WaitForDeploymentReady(t, ctx, kube, namespace, name, 1, 5*time.Minute)
	require.Eventually(t, func() bool {
		return len(listReadyPodsByApp(t, kube, namespace, plugincontext.SlowMockAppLabel)) == 1
	}, 2*time.Minute, 2*time.Second, "expected exactly one ready slow mock pod")
}

func listReadyPodsByApp(t *testing.T, kube kubernetes.Interface, namespace, appLabel string) []corev1.Pod {
	t.Helper()
	return utils.ListReadyPodsByLabel(t, kube, namespace, "app="+appLabel)
}

func listReadyMockPods(t *testing.T, kube kubernetes.Interface, namespace string) []corev1.Pod {
	t.Helper()
	ready := listReadyPodsByApp(t, kube, namespace, plugincontext.AppLabel)
	require.NotEmpty(t, ready, "no ready mock pods")
	return ready
}

// setupRouterPortForwardAfterRestart mirrors router/shared.go: use a dynamic local port so
// rollout restart does not break the framework TestMain port-forward on :8080.
func setupRouterPortForwardAfterRestart(t *testing.T, kthenaNamespace string) (chatURL, metricsURL string, closePF func()) {
	t.Helper()
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForward(kthenaNamespace, routerDeploymentName, localPort, "80")
	require.NoError(t, err, "failed to setup port-forward to restarted router")
	chatURL = fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	metricsURL = fmt.Sprintf("http://127.0.0.1:%s/metrics", localPort)
	return chatURL, metricsURL, func() { pf.Close() }
}

func applySchedulerConfig(t *testing.T, kube kubernetes.Interface, kthena *clientset.Clientset, kthenaNamespace, testNamespace, schedulerYAML string) (chatURL, metricsURL string, restore func()) {
	t.Helper()
	ctx := context.Background()
	cm, err := kube.CoreV1().ConfigMaps(kthenaNamespace).Get(ctx, routerConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	original := cm.Data[routerConfigKey]

	updated := schedulerYAML
	cm.Data[routerConfigKey] = updated
	_, err = kube.CoreV1().ConfigMaps(kthenaNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, utils.RolloutRestartDeployment(ctx, kube, kthenaNamespace, routerDeploymentName, routerRolloutTimeout))
	// Router pod can become Ready before webhook is fully initialised; wait to avoid flaky creates.
	utils.WaitForRouterValidatingWebhook(t, ctx, kthena, testNamespace, plugincontext.ModelServerName, plugincontext.ModelName)
	chatURL, metricsURL, closePF := setupRouterPortForwardAfterRestart(t, kthenaNamespace)

	restore = func() {
		closePF()
		cleanupCtx := context.Background()
		latest, err := kube.CoreV1().ConfigMaps(kthenaNamespace).Get(cleanupCtx, routerConfigMapName, metav1.GetOptions{})
		if err != nil {
			return
		}
		latest.Data[routerConfigKey] = original
		if _, err := kube.CoreV1().ConfigMaps(kthenaNamespace).Update(cleanupCtx, latest, metav1.UpdateOptions{}); err != nil {
			return
		}
		_ = utils.RolloutRestartDeployment(cleanupCtx, kube, kthenaNamespace, routerDeploymentName, routerRolloutTimeout)
	}
	return chatURL, metricsURL, restore
}

func waitForSchedulerPluginInMetrics(t *testing.T, metricsURL, pluginName, pluginType string) {
	t.Helper()
	require.Eventually(t, func() bool {
		metricsData, err := backendmetrics.ParseMetricsURL(metricsURL)
		if err != nil {
			return false
		}
		return getHistogramCount(metricsData, "kthena_router_scheduler_plugin_duration_seconds", map[string]string{
			"plugin": pluginName,
			"type":   pluginType,
		}) > 0
	}, 30*time.Second, time.Second)
}

func getHistogramCount(metrics map[string]*dto.MetricFamily, name string, labels map[string]string) uint64 {
	mf, ok := metrics[name]
	if !ok {
		return 0
	}
	for _, m := range mf.GetMetric() {
		if metricMatches(m, labels) {
			return m.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

func metricMatches(m *dto.Metric, labels map[string]string) bool {
	labelMap := make(map[string]string)
	for _, lp := range m.GetLabel() {
		labelMap[lp.GetName()] = lp.GetValue()
	}
	for k, v := range labels {
		if labelMap[k] != v {
			return false
		}
	}
	return true
}

func directChatToPod(t *testing.T, pod corev1.Pod, model, prompt string, count int) {
	t.Helper()
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err)
	defer pf.Close()

	url := fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	body, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 32,
		"stream":     true,
	})
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 0; i < count; i++ {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

// startSustainedLongRequestsToPod keeps concurrent long requests on one pod via port-forward.
// Traffic bypasses the router (no access-log pollution) and raises engine waiting for Filter.
func startSustainedLongRequestsToPod(t *testing.T, pod corev1.Pod, model, prompt string, concurrency, maxTokens int) func() {
	t.Helper()
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err)

	url := fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	body, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     true,
	})
	require.NoError(t, err)

	client := &http.Client{Timeout: 2 * time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < concurrency; i++ {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if err != nil && ctx.Err() != nil {
					return
				}
			}
		}()
	}

	return func() {
		cancel()
		pf.Close()
	}
}

type routerPodMetricsSnapshot struct {
	RequestWaitingNum float64
	RequestRunningNum float64
}

// fetchRouterPodMetricsViaDebug reads scraped engine metrics for a pod from the router debug API.
func fetchRouterPodMetricsViaDebug(t *testing.T, debugBaseURL string, pod corev1.Pod) (routerPodMetricsSnapshot, bool) {
	t.Helper()
	url := fmt.Sprintf("%s/debug/config_dump/namespaces/%s/pods/%s", debugBaseURL, pod.Namespace, pod.Name)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return routerPodMetricsSnapshot{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return routerPodMetricsSnapshot{}, false
	}
	var parsed struct {
		Metrics *struct {
			RequestWaitingNum float64 `json:"requestWaitingNum"`
			RequestRunningNum float64 `json:"requestRunningNum"`
		} `json:"metrics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || parsed.Metrics == nil {
		return routerPodMetricsSnapshot{}, false
	}
	return routerPodMetricsSnapshot{
		RequestWaitingNum: parsed.Metrics.RequestWaitingNum,
		RequestRunningNum: parsed.Metrics.RequestRunningNum,
	}, true
}

// waitForLeastRequestLoadSeparation polls until busyPod is saturated (waiting >= maxWaitingRequests)
// and idlePod stays below that threshold, matching least-request Filter semantics.
func waitForLeastRequestLoadSeparation(t *testing.T, kube kubernetes.Interface, kthenaNamespace string, busyPod, idlePod corev1.Pod, maxWaitingRequests int) {
	t.Helper()
	require.Greater(t, maxWaitingRequests, 0)
	threshold := float64(maxWaitingRequests)

	routerPod := utils.GetRouterPod(t, kube, kthenaNamespace)
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(routerPod.Namespace, routerPod.Name, localPort, routerDebugPort)
	require.NoError(t, err, "port-forward to router debug API")
	defer pf.Close()

	debugBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)
	require.Eventually(t, func() bool {
		busy, okBusy := fetchRouterPodMetricsViaDebug(t, debugBaseURL, busyPod)
		idle, okIdle := fetchRouterPodMetricsViaDebug(t, debugBaseURL, idlePod)
		if !okBusy || !okIdle {
			return false
		}
		busySaturated := busy.RequestWaitingNum >= threshold
		idleFree := idle.RequestWaitingNum < threshold
		if busySaturated && idleFree {
			t.Logf("least-request load ready: busy %s waiting=%.0f running=%.0f; idle %s waiting=%.0f running=%.0f",
				busyPod.Name, busy.RequestWaitingNum, busy.RequestRunningNum,
				idlePod.Name, idle.RequestWaitingNum, idle.RequestRunningNum)
		}
		return busySaturated && idleFree
	}, leastRequestLoadWaitTimeout, 2*time.Second,
		"busy pod %s should have request_waiting >= %d and idle pod %s should have request_waiting < %d",
		busyPod.Name, maxWaitingRequests, idlePod.Name, maxWaitingRequests)
}

func sendRouterChatRequests(t *testing.T, routerChatURL, modelName, prompt string, count int) {
	t.Helper()
	messages := []utils.ChatMessage{utils.NewChatMessage("user", prompt)}
	for i := 0; i < count; i++ {
		resp := utils.SendChatRequestWithRetryQuiet(t, routerChatURL, modelName, messages, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

const (
	schedulerOnlyPrefixCache = `scheduler:
  pluginConfig:
  - name: prefix-cache
    args:
      blockSizeToHash: 64
      maxBlocksToMatch: 128
      maxHashCacheSize: 50000
      topKMatches: 5
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: prefix-cache
          weight: 1`

	schedulerOnlyLeastRequest = `scheduler:
  pluginConfig:
  - name: least-request
    args:
      maxWaitingRequests: 1
  plugins:
    Filter:
      enabled:
        - least-request
    Score:
      enabled:
        - name: least-request
          weight: 1`

	schedulerOnlyLeastLatency = `scheduler:
  pluginConfig:
  - name: least-latency
    args:
      TTFTTPOTWeightFactor: 0.5
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: least-latency
          weight: 1`

	schedulerOnlyLoraAffinity = `scheduler:
  pluginConfig: []
  plugins:
    Filter:
      enabled:
        - lora-affinity
    Score:
      enabled:
        - name: random
          weight: 1`

	schedulerOnlyRandom = `scheduler:
  pluginConfig: []
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: random
          weight: 1`
)
