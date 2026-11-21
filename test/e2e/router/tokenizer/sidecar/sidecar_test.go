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

package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// rendererHTTPClient is used for direct requests to renderer stub subprocesses.
var rendererHTTPClient = &http.Client{Timeout: 10 * time.Second}

// rendererPortForModel queries the tokenizer service's /models endpoint and returns
// the renderer subprocess's port for modelName, or 0 if the model is not yet ready.
func rendererPortForModel(t *testing.T, serviceBaseURL, modelName string) int {
	t.Helper()
	resp, err := rendererHTTPClient.Get(serviceBaseURL + "/models") //nolint:gosec
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

// rendererStatsAt fetches /stats from a renderer subprocess at the given base URL.
// Returns -1 on error.
func rendererStatsAt(t *testing.T, rendBaseURL string) int64 {
	t.Helper()
	resp, err := rendererHTTPClient.Get(rendBaseURL + "/stats") //nolint:gosec
	if err != nil {
		t.Logf("rendererStatsAt: GET %s/stats: %v", rendBaseURL, err)
		return -1
	}
	defer resp.Body.Close()
	var body struct {
		Requests int64 `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Logf("rendererStatsAt: decode: %v", err)
		return -1
	}
	return body.Requests
}

// tokenizeErrorCountURL fetches kthena_router_kvcache_aware_errors_total{stage=tokenize}
// from metricsURL. Returns -1 on error.
func tokenizeErrorCountURL(t *testing.T, metricsURL string) float64 {
	t.Helper()
	md, err := backendmetrics.ParseMetricsURL(metricsURL)
	if err != nil {
		t.Logf("tokenizeErrorCountURL: %v", err)
		return -1
	}
	return utils.GetCounterValue(md, "kthena_router_kvcache_aware_errors_total", map[string]string{"stage": "tokenize"})
}

// TestTokenizerSidecarMode verifies the tokenizer in sidecar deployment mode.
// The sidecar listens on port 8100 inside the router pod; this test port-forwards
// to the router pod directly rather than through a standalone tokenizer Service.
func TestTokenizerSidecarMode(t *testing.T) {
	t.Run("no_standalone_deployment", func(t *testing.T) {
		_, err := testCtx.KubeClient.AppsV1().Deployments(kthenaNamespace).Get(
			context.Background(), utils.TokenizerDeploymentName, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(err),
			"kthena-tokenizer Deployment should not exist in sidecar mode (got err: %v)", err)
	})

	t.Run("router_pod_has_tokenizer_sidecar", func(t *testing.T) {
		pods := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)
		hasSidecar := false
		for _, c := range pods.Spec.Containers {
			if c.Name == "tokenizer" || c.Name == "kthena-tokenizer" {
				hasSidecar = true
				break
			}
		}
		assert.True(t, hasSidecar, "router pod should have a tokenizer sidecar container")
	})

	t.Run("tokenizer_healthz_via_pod_portforward", func(t *testing.T) {
		routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)
		localPort := utils.AllocateLocalPort(t)
		pf, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, localPort, utils.TokenizerPort)
		require.NoError(t, err, "port-forward to router pod port 8100")
		defer pf.Close()

		sidecarBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)
		utils.WaitForTokenizerHealthy(t, sidecarBaseURL, 2*time.Minute)
	})

	t.Run("tokenize_via_sidecar", func(t *testing.T) {
		routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)
		localPort := utils.AllocateLocalPort(t)
		pf, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, localPort, utils.TokenizerPort)
		require.NoError(t, err, "port-forward to router pod port 8100")
		defer pf.Close()

		sidecarBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)

		require.Eventually(t, func() bool {
			_, resp, err := utils.SendTokenizeRequest(t, sidecarBaseURL, "tokenizer-test-model", "hello")
			return err == nil && resp.Count == 1
		}, 5*time.Minute, 5*time.Second, "sidecar tokenize request should eventually succeed")
	})

	// router_routes_via_sidecar_tokenizer proves that the router kvcache-aware plugin calls
	// the sidecar tokenizer (localhost:8100) and the tokenizer proxies to its renderer
	// subprocess. Verified by checking the renderer subprocess's /stats counter.
	t.Run("router_routes_via_sidecar_tokenizer", func(t *testing.T) {
		const sidecarSchedulerYAML = `scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 8
      maxBlocksToMatch: 128
      tokenizerService:
        enabled: true
        endpoint: "http://localhost:8100"
        fallbackToEngine: false
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1`

		chatURL, metricsURL, restore := utils.ApplySchedulerConfig(t,
			testCtx.KubeClient, testCtx.KthenaClient,
			kthenaNamespace, testCtx.Namespace,
			sidecarSchedulerYAML,
			tokenizercontext.ModelServerName, "tokenizer-test-model")
		t.Cleanup(restore)

		// After ApplySchedulerConfig the router pod (and its sidecar) restarted.
		routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)

		sidecarPort := utils.AllocateLocalPort(t)
		sidecarPF, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, sidecarPort, utils.TokenizerPort)
		require.NoError(t, err, "port-forward to sidecar tokenizer on restarted router pod")
		defer sidecarPF.Close()
		sidecarBaseURL := fmt.Sprintf("http://127.0.0.1:%s", sidecarPort)

		utils.WaitForTokenizerHealthy(t, sidecarBaseURL, 2*time.Minute)

		var rendererPort int
		require.Eventually(t, func() bool {
			p := rendererPortForModel(t, sidecarBaseURL, tokenizercontext.ModelServerName)
			if p != 0 {
				rendererPort = p
				return true
			}
			return false
		}, tokenizercontext.TokenizerReadyTimeout, 5*time.Second,
			"sidecar renderer should become ready and publish its port via /models")

		rendPort := utils.AllocateLocalPort(t)
		rendPF, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, rendPort, strconv.Itoa(rendererPort))
		require.NoError(t, err, "port-forward to sidecar renderer subprocess port %d", rendererPort)
		defer rendPF.Close()
		rendBaseURL := fmt.Sprintf("http://127.0.0.1:%s", rendPort)

		before := rendererStatsAt(t, rendBaseURL)
		require.GreaterOrEqual(t, before, int64(0), "sidecar renderer /stats should be reachable")

		utils.SendRouterChatRequests(t, chatURL, "tokenizer-test-model", "hello sidecar router", 3)

		require.Eventually(t, func() bool {
			return rendererStatsAt(t, rendBaseURL) > before
		}, 30*time.Second, 2*time.Second,
			"sidecar renderer /stats must grow: router → sidecar service → renderer subprocess")

		require.Equal(t, float64(0), tokenizeErrorCountURL(t, metricsURL),
			"no kvcache tokenize error should occur when sidecar tokenizer succeeds")
	})
}
