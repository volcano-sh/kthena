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

package standalone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const kvCacheTokenizeErrorMetric = "kthena_router_kvcache_aware_errors_total"

// fallbackHTTPClient is used for direct HTTP calls to renderer subprocesses and engine
// mocks. The 10-second timeout prevents a stalled port-forward from blocking indefinitely.
var fallbackHTTPClient = &http.Client{Timeout: 10 * time.Second}

// schedulerWithTokenizer returns a kvcache-aware scheduler YAML configured to call the
// standalone kthena-tokenizer service. fallbackToEngine controls whether the router falls
// back to calling the engine pod's /tokenize endpoint when the service fails.
func schedulerWithTokenizer(ns string, fallbackToEngine bool) string {
	return fmt.Sprintf(`scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 8
      maxBlocksToMatch: 128
      tokenizerService:
        enabled: true
        endpoint: "http://kthena-tokenizer.%s.svc.cluster.local:8100"
        fallbackToEngine: %v
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1`, ns, fallbackToEngine)
}

// schedulerWithoutTokenizer is a plain kvcache-aware config with no tokenizer service.
const schedulerWithoutTokenizer = `scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 8
      maxBlocksToMatch: 128
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1`

// schedulerWithBrokenTokenizerNoFallback points to a non-existent tokenizer service with
// fallbackToEngine=false. The router will fail to tokenize and increment the error metric,
// but Score() returns nil so chat completion still succeeds.
const schedulerWithBrokenTokenizerNoFallback = `scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 8
      maxBlocksToMatch: 128
      tokenizerService:
        enabled: true
        endpoint: "http://no-such-tokenizer-svc.invalid:8100"
        fallbackToEngine: false
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1`

// schedulerWithBrokenTokenizerWithFallback points to a non-existent tokenizer service but
// allows the router to fall back to calling the engine pod's /tokenize endpoint directly.
const schedulerWithBrokenTokenizerWithFallback = `scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 8
      maxBlocksToMatch: 128
      tokenizerService:
        enabled: true
        endpoint: "http://no-such-tokenizer-svc.invalid:8100"
        fallbackToEngine: true
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1`

// rendererPortForModel queries the tokenizer service's /models endpoint and returns
// the renderer subprocess's port for modelName, or 0 if the model is not yet ready.
func rendererPortForModel(t *testing.T, serviceBaseURL, modelName string) int {
	t.Helper()
	resp, err := fallbackHTTPClient.Get(serviceBaseURL + "/models") //nolint:gosec
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Model  string `json:"model"`
			Status string `json:"status"`
			Port   int    `json:"port"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0
	}
	for _, m := range out.Models {
		if m.Model == modelName && m.Status == "ready" {
			return m.Port
		}
	}
	return 0
}

// rendererStubStats discovers the renderer subprocess port via the tokenizer service's
// /models endpoint, opens a pod-level port-forward to that port, and returns the /stats
// request counter. Returns -1 on any error.
func rendererStubStats(t *testing.T, podNamespace, podName, serviceBaseURL, modelName string) int64 {
	t.Helper()
	port := rendererPortForModel(t, serviceBaseURL, modelName)
	if port == 0 {
		t.Logf("rendererStubStats: model %q not ready (port=0 from /models)", modelName)
		return -1
	}
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(podNamespace, podName, localPort, strconv.Itoa(port))
	if err != nil {
		t.Logf("rendererStubStats: port-forward %s/%s :%d → %v", podNamespace, podName, port, err)
		return -1
	}
	defer pf.Close()

	resp, err := fallbackHTTPClient.Get(fmt.Sprintf("http://127.0.0.1:%s/stats", localPort)) //nolint:gosec
	if err != nil {
		t.Logf("rendererStubStats: GET /stats: %v", err)
		return -1
	}
	defer resp.Body.Close()
	var body struct {
		Requests int64 `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Logf("rendererStubStats: decode /stats: %v", err)
		return -1
	}
	return body.Requests
}

// engineMockStats port-forwards to a ready engine mock pod (app=tokenizer-test-engine,
// port 8000) and returns the /tokenize-stats request counter. Returns -1 on any error.
func engineMockStats(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pods, err := testCtx.KubeClient.CoreV1().Pods(testCtx.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + tokenizercontext.EngineDeploymentName,
	})
	if err != nil || len(pods.Items) == 0 {
		t.Logf("engineMockStats: no pods with app=%s: %v", tokenizercontext.EngineDeploymentName, err)
		return -1
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if utils.IsPodReady(pods.Items[i]) {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil {
		t.Logf("engineMockStats: no ready pods with app=%s", tokenizercontext.EngineDeploymentName)
		return -1
	}
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(testCtx.Namespace, pod.Name, localPort, "8000")
	if err != nil {
		t.Logf("engineMockStats: port-forward %s/%s :8000: %v", testCtx.Namespace, pod.Name, err)
		return -1
	}
	defer pf.Close()

	resp, err := fallbackHTTPClient.Get(fmt.Sprintf("http://127.0.0.1:%s/tokenize-stats", localPort)) //nolint:gosec
	if err != nil {
		t.Logf("engineMockStats: GET /tokenize-stats: %v", err)
		return -1
	}
	defer resp.Body.Close()
	var body struct {
		Requests int64 `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Logf("engineMockStats: decode /tokenize-stats: %v", err)
		return -1
	}
	return body.Requests
}

// tokenizeErrorCountURL fetches kthena_router_kvcache_aware_errors_total{stage=tokenize}
// from metricsURL. Returns -1 on network/parse error.
func tokenizeErrorCountURL(t *testing.T, metricsURL string) float64 {
	t.Helper()
	md, err := backendmetrics.ParseMetricsURL(metricsURL)
	if err != nil {
		t.Logf("tokenizeErrorCountURL: parse metrics from %s: %v", metricsURL, err)
		return -1
	}
	return utils.GetCounterValue(md, kvCacheTokenizeErrorMetric, map[string]string{"stage": "tokenize"})
}

// TestRouterKVCacheAwareWithTokenizerService verifies router-level tokenizer integration
// through the kvcache-aware scheduling plugin.
//
// ApplySchedulerConfig restarts the router each time, which resets all Prometheus counters
// in the new process to zero. Tests exploit this clean-slate property: the baseline counter
// after a router restart is always 0. Engine mock stats persist across restarts because the
// engine pod is a separate process in testCtx.Namespace.
//
// Four sub-tests:
//
//   - tokenizer_service_invoked_success: working tokenizer, fallbackToEngine=false.
//     Renderer subprocess /stats grows (router → service → renderer), error metric stays 0.
//
//   - engine_fallback_when_tokenizer_unavailable: broken endpoint, fallbackToEngine=true.
//     Engine mock /tokenize-stats grows; error metric stays 0 (fallback succeeded).
//
//   - error_metric_increments_no_engine_fallback: broken endpoint, fallbackToEngine=false.
//     Error metric > 0; engine mock /tokenize-stats unchanged.
//
//   - routing_works_without_tokenizer_configured: plain kvcache-aware, no tokenizer service.
func TestRouterKVCacheAwareWithTokenizerService(t *testing.T) {
	utils.WaitForTokenizerHealthy(t, testCtx.TokenizerBaseURL, 2*time.Minute)
	tokenizerPod := utils.GetTokenizerPod(t, testCtx.KubeClient, kthenaNamespace)
	utils.WaitForTokenizerModelReady(t, testCtx.TokenizerBaseURL,
		tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)

	t.Run("tokenizer_service_invoked_success", func(t *testing.T) {
		rendBefore := rendererStubStats(t, kthenaNamespace, tokenizerPod.Name,
			testCtx.TokenizerBaseURL, tokenizercontext.ModelServerName)
		require.GreaterOrEqual(t, rendBefore, int64(0),
			"renderer must be reachable and report stats before configuring the router")

		chatURL, metricsURL, restore := utils.ApplySchedulerConfig(t,
			testCtx.KubeClient, testCtx.KthenaClient,
			kthenaNamespace, testCtx.Namespace,
			schedulerWithTokenizer(kthenaNamespace, false),
			tokenizercontext.ModelServerName, "tokenizer-test-model")
		t.Cleanup(restore)

		utils.SendRouterChatRequests(t, chatURL, "tokenizer-test-model", "hello tokenizer", 3)

		require.Eventually(t, func() bool {
			after := rendererStubStats(t, kthenaNamespace, tokenizerPod.Name,
				testCtx.TokenizerBaseURL, tokenizercontext.ModelServerName)
			return after > rendBefore
		}, 30*time.Second, 2*time.Second,
			"renderer stub /stats should grow: router → tokenizer service → renderer subprocess")

		require.Equal(t, float64(0), tokenizeErrorCountURL(t, metricsURL),
			"no kvcache tokenize error should occur when the tokenizer service succeeds")
	})

	t.Run("engine_fallback_when_tokenizer_unavailable", func(t *testing.T) {
		engineBefore := engineMockStats(t)
		require.GreaterOrEqual(t, engineBefore, int64(0), "engine mock must be reachable")

		chatURL, metricsURL, restore := utils.ApplySchedulerConfig(t,
			testCtx.KubeClient, testCtx.KthenaClient,
			kthenaNamespace, testCtx.Namespace,
			schedulerWithBrokenTokenizerWithFallback,
			tokenizercontext.ModelServerName, "tokenizer-test-model")
		t.Cleanup(restore)

		utils.SendRouterChatRequests(t, chatURL, "tokenizer-test-model", "hello engine fallback", 3)

		require.Eventually(t, func() bool {
			return engineMockStats(t) > engineBefore
		}, 30*time.Second, 2*time.Second,
			"engine mock /tokenize-stats must grow when router falls back to engine tokenization")

		require.Equal(t, float64(0), tokenizeErrorCountURL(t, metricsURL),
			"kvcache error metric must be 0 when fallback to engine succeeds")
	})

	t.Run("error_metric_increments_no_engine_fallback", func(t *testing.T) {
		chatURL, metricsURL, restore := utils.ApplySchedulerConfig(t,
			testCtx.KubeClient, testCtx.KthenaClient,
			kthenaNamespace, testCtx.Namespace,
			schedulerWithBrokenTokenizerNoFallback,
			tokenizercontext.ModelServerName, "tokenizer-test-model")
		t.Cleanup(restore)

		engineBefore := engineMockStats(t)
		require.GreaterOrEqual(t, engineBefore, int64(0), "engine mock must be reachable before asserting it was not used")

		utils.SendRouterChatRequests(t, chatURL, "tokenizer-test-model", "hello no fallback", 3)

		require.Eventually(t, func() bool {
			return tokenizeErrorCountURL(t, metricsURL) > 0
		}, 30*time.Second, 2*time.Second,
			"kthena_router_kvcache_aware_errors_total{stage=tokenize} must increment when tokenizer unreachable and fallbackToEngine=false")

		require.Equal(t, engineBefore, engineMockStats(t),
			"engine mock /tokenize-stats must NOT increase when fallbackToEngine=false")
	})

	t.Run("routing_works_without_tokenizer_configured", func(t *testing.T) {
		chatURL, _, restore := utils.ApplySchedulerConfig(t,
			testCtx.KubeClient, testCtx.KthenaClient,
			kthenaNamespace, testCtx.Namespace,
			schedulerWithoutTokenizer,
			tokenizercontext.ModelServerName, "tokenizer-test-model")
		t.Cleanup(restore)

		utils.SendRouterChatRequests(t, chatURL, "tokenizer-test-model", "hello no tokenizer", 3)
	})
}
