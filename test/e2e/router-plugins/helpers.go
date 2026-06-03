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
	"net"
	"net/http"
	"path/filepath"
	"strings"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	routerConfigMapName  = "kthena-router-config"
	routerConfigKey      = "routerConfiguration"
	routerDeploymentName = "kthena-router"
	routerRolloutTimeout = 3 * time.Minute
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

func waitForRouterModelRouteWebhook(t *testing.T, kthena *clientset.Clientset, namespace, modelServerName, modelName string) {
	t.Helper()

	weight100 := uint32(100)
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := &networkingv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "webhook-ready-probe-" + utils.RandomString(5),
			},
			Spec: networkingv1alpha1.ModelRouteSpec{
				ModelName: modelName,
				Rules: []*networkingv1alpha1.Rule{
					{
						Name: "default",
						TargetModels: []*networkingv1alpha1.TargetModel{
							{ModelServerName: modelServerName, Weight: &weight100},
						},
					},
				},
			},
		}
		_, err := kthena.NetworkingV1alpha1().ModelRoutes(namespace).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "failed calling webhook") ||
				strings.Contains(errStr, "connect: connection refused") ||
				strings.Contains(errStr, "i/o timeout") ||
				strings.Contains(errStr, "context deadline exceeded") ||
				strings.Contains(errStr, "Client.Timeout exceeded") ||
				strings.Contains(errStr, "awaiting headers") ||
				strings.Contains(errStr, "EOF") ||
				strings.Contains(errStr, "connection reset by peer") ||
				strings.Contains(errStr, "no endpoints available") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	require.NoError(t, err, "kthena-router ModelRoute validating webhook did not become ready in time")
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
	_ = kube.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	require.Eventually(t, func() bool {
		_, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 2*time.Second, "slow mock deployment should be deleted before recreate")

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
	pods := utils.ListPodsByLabel(t, kube, namespace, "app="+appLabel)
	var ready []corev1.Pod
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			ready = append(ready, pod)
		}
	}
	return ready
}

func listReadyMockPods(t *testing.T, kube kubernetes.Interface, namespace string) []corev1.Pod {
	t.Helper()
	ready := listReadyPodsByApp(t, kube, namespace, plugincontext.AppLabel)
	require.NotEmpty(t, ready, "no ready mock pods")
	return ready
}

func scaleDeploymentReplicas(t *testing.T, kube kubernetes.Interface, namespace, name string, replicas int32) func() {
	t.Helper()
	ctx := context.Background()

	deploy, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	original := int32(1)
	if deploy.Spec.Replicas != nil {
		original = *deploy.Spec.Replicas
	}

	deploy.Spec.Replicas = &replicas
	_, err = kube.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForDeploymentReady(t, ctx, kube, namespace, name, replicas, 5*time.Minute)

	return func() {
		latest, err := kube.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return
		}
		latest.Spec.Replicas = &original
		if _, err := kube.AppsV1().Deployments(namespace).Update(context.Background(), latest, metav1.UpdateOptions{}); err != nil {
			return
		}
		_ = utils.WaitForDeploymentReadyE(context.Background(), kube, namespace, name, 5*time.Minute)
	}
}

func allocateLocalPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := fmt.Sprintf("%d", listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())
	return port
}

// setupRouterPortForwardAfterRestart mirrors router/shared.go: use a dynamic local port so
// rollout restart does not break the framework TestMain port-forward on :8080.
func setupRouterPortForwardAfterRestart(t *testing.T, kthenaNamespace string) (chatURL, metricsURL string, closePF func()) {
	t.Helper()
	localPort := allocateLocalPort(t)
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

	require.NoError(t, rolloutRestartDeployment(ctx, kube, kthenaNamespace, routerDeploymentName, routerRolloutTimeout))
	// Router pod can become Ready before webhook is fully initialised; wait to avoid flaky creates.
	waitForRouterModelRouteWebhook(t, kthena, testNamespace, plugincontext.ModelServerName, plugincontext.ModelName)
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
		_ = rolloutRestartDeployment(cleanupCtx, kube, kthenaNamespace, routerDeploymentName, routerRolloutTimeout)
	}
	return chatURL, metricsURL, restore
}

func rolloutRestartDeployment(ctx context.Context, kube kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	deploy, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	deploy.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	updated, err := kube.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	targetGen := updated.Generation
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		d, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if d.Status.ObservedGeneration < targetGen {
			return false, nil
		}
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		return d.Status.UpdatedReplicas == desired &&
			d.Status.ReadyReplicas == desired &&
			d.Status.AvailableReplicas == desired &&
			d.Status.Replicas == desired, nil
	})
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

func loadLoraOnPod(t *testing.T, pod corev1.Pod, loraName, loraPath string) {
	t.Helper()
	localPort := podLocalPort(pod.Name)
	pf, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err)
	defer pf.Close()
	utils.LoadLoRAAdapter(t, fmt.Sprintf("http://127.0.0.1:%s", localPort), loraName, loraPath)
}

func waitForLoRAAdapterRoutable(t *testing.T, routerChatURL, loraName string) {
	t.Helper()
	messages := []utils.ChatMessage{utils.NewChatMessage("user", "ready")}
	utils.WaitForChatModelReady(t, routerChatURL, loraName, messages, 90*time.Second)
}

func podLocalPort(podName string) string {
	h := 0
	for _, c := range podName {
		h = (h + int(c)) % 50
	}
	return fmt.Sprintf("%d", 28100+h)
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
	localPort := podLocalPort(pod.Name)
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

// startLongRequestsToPod fires concurrent long requests directly at one pod so the engine
// reports a waiting queue (used by least-request Filter, which keys off scraped waiting).
func startLongRequestsToPod(t *testing.T, pod corev1.Pod, model, prompt string, concurrency int, maxTokens int) func() {
	t.Helper()
	localPort := podLocalPort(pod.Name + "-load")
	pf, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err)

	url := fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	body, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     false,
	})
	client := &http.Client{Timeout: 2 * time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < concurrency; i++ {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	return func() {
		cancel()
		pf.Close()
	}
}

// startLongRequestsViaRouter fires concurrent long chat requests through the router so
// scheduler plugins observe onFlight and engine waiting on the selected pods.
func startLongRequestsViaRouter(t *testing.T, chatURL, model, prompt string, concurrency int, maxTokens int) func() {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     false,
	})
	client := &http.Client{Timeout: 2 * time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < concurrency; i++ {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	return cancel
}

func countSelectedPodInLogs(t *testing.T, kube kubernetes.Interface, kthenaNamespace, podName string, since metav1.Time) int {
	t.Helper()
	routerPod := utils.GetRouterPod(t, kube, kthenaNamespace)
	opts := &corev1.PodLogOptions{SinceTime: &since}
	logs, err := kube.CoreV1().Pods(kthenaNamespace).GetLogs(routerPod.Name, opts).Do(context.Background()).Raw()
	require.NoError(t, err)
	return strings.Count(string(logs), "selected_pod="+podName)
}

func countSelectedPodsInLogs(t *testing.T, kube kubernetes.Interface, kthenaNamespace string, since metav1.Time, pods []corev1.Pod) int {
	t.Helper()
	total := 0
	for _, pod := range pods {
		total += countSelectedPodInLogs(t, kube, kthenaNamespace, pod.Name, since)
	}
	return total
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
