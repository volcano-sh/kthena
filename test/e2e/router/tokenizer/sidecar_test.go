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

package tokenizer

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
	"github.com/volcano-sh/kthena/test/e2e/framework"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestTokenizerSidecarMode reinstalls kthena with the tokenizer in sidecar mode
// (running as a container inside the router pod), performs health and readiness
// checks, then restores standalone mode.
//
// The sidecar listens on the same port (8100) but is only reachable from inside
// the router pod. This test port-forwards to the router pod's port 8100 directly.
func TestTokenizerSidecarMode(t *testing.T) {
	// Reinstall kthena in sidecar mode.
	sidecarConfig := framework.NewDefaultConfig()
	sidecarConfig.TokenizerEnabled = true
	sidecarConfig.TokenizerMode = "sidecar"
	sidecarConfig.TokenizerRenderCommand = "/usr/local/bin/kthena-tokenizer-stub"
	sidecarConfig.TokenizerModels = map[string]string{
		"tokenizer-test-model": "stub-source",
	}

	fmt.Println("Reinstalling kthena in sidecar mode for sidecar tests...")
	if err := framework.UninstallKthena(kthenaNamespace); err != nil {
		t.Fatalf("uninstall before sidecar test: %v", err)
	}
	if err := framework.InstallKthena(sidecarConfig); err != nil {
		t.Fatalf("install kthena in sidecar mode: %v", err)
	}
	t.Cleanup(func() {
		// Restore standalone mode so subsequent tests in this run are unaffected.
		fmt.Println("Restoring kthena to standalone tokenizer mode...")
		if err := framework.UninstallKthena(kthenaNamespace); err != nil {
			t.Errorf("failed to uninstall kthena during sidecar cleanup: %v", err)
		}
		standaloneCfg := framework.NewDefaultConfig()
		standaloneCfg.TokenizerEnabled = true
		standaloneCfg.TokenizerMode = "standalone"
		standaloneCfg.TokenizerRenderCommand = "/usr/local/bin/kthena-tokenizer-stub"
		standaloneCfg.TokenizerModels = map[string]string{
			"tokenizer-test-model": "stub-source",
		}
		if err := framework.InstallKthena(standaloneCfg); err != nil {
			t.Errorf("failed to restore standalone tokenizer mode after sidecar test: %v", err)
		}
	})

	t.Run("no_standalone_deployment", func(t *testing.T) {
		// In sidecar mode the kthena-tokenizer Deployment must not exist.
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
		// WaitForTokenizerHealthy polls /healthz with a 10 s HTTP timeout until it returns 200.
		utils.WaitForTokenizerHealthy(t, sidecarBaseURL, 2*time.Minute)
	})

	t.Run("tokenize_via_sidecar", func(t *testing.T) {
		routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)
		localPort := utils.AllocateLocalPort(t)
		pf, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, localPort, utils.TokenizerPort)
		require.NoError(t, err, "port-forward to router pod port 8100")
		defer pf.Close()

		sidecarBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)

		// Wait for the stub renderer to become ready.
		require.Eventually(t, func() bool {
			_, resp, err := utils.SendTokenizeRequest(t, sidecarBaseURL, "tokenizer-test-model", "hello")
			return err == nil && resp.Count == 3
		}, 5*time.Minute, 5*time.Second, "sidecar tokenize request should eventually succeed")
	})

	// router_routes_via_sidecar_tokenizer proves that the router kvcache-aware plugin calls
	// the sidecar tokenizer (localhost:8100) and the tokenizer proxies to its renderer
	// subprocess. Verified by checking the renderer subprocess's /stats counter.
	//
	// Architecture in sidecar mode:
	//   router process → localhost:8100 (sidecar tokenizer service)
	//                    → 127.0.0.1:8200+ (renderer subprocess, kthena-tokenizer-stub)
	//
	// Both the sidecar service (port 8100) and its renderer subprocess (port 8200+) run
	// inside the router pod's containers. The /models endpoint of the sidecar service
	// publishes the renderer port; we port-forward to it to read /stats.
	t.Run("router_routes_via_sidecar_tokenizer", func(t *testing.T) {
		// fallbackToEngine=false: any tokenizer failure would surface as an error metric,
		// so a clean run proves the sidecar path worked end-to-end.
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
		// Get the new router pod.
		routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)

		// Port-forward to the sidecar's main tokenizer process (port 8100).
		sidecarPort := utils.AllocateLocalPort(t)
		sidecarPF, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, sidecarPort, utils.TokenizerPort)
		require.NoError(t, err, "port-forward to sidecar tokenizer on restarted router pod")
		defer sidecarPF.Close()
		sidecarBaseURL := fmt.Sprintf("http://127.0.0.1:%s", sidecarPort)

		// Wait for the sidecar's tokenizer service to be healthy.
		utils.WaitForTokenizerHealthy(t, sidecarBaseURL, 2*time.Minute)

		// Wait for the sidecar's renderer subprocess to become ready.
		// This polls /models until the model reports status=ready and publishes its port.
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

		// Port-forward to the renderer subprocess port inside the router pod.
		rendPort := utils.AllocateLocalPort(t)
		rendPF, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, rendPort, strconv.Itoa(rendererPort))
		require.NoError(t, err, "port-forward to sidecar renderer subprocess port %d", rendererPort)
		defer rendPF.Close()
		rendBaseURL := fmt.Sprintf("http://127.0.0.1:%s", rendPort)

		// Baseline: renderer just restarted with the pod, so /stats should be 0.
		before := rendererStatsAt(t, rendBaseURL)
		require.GreaterOrEqual(t, before, int64(0), "sidecar renderer /stats should be reachable")

		utils.SendRouterChatRequests(t, chatURL, "tokenizer-test-model", "hello sidecar router", 3)

		// The kvcache-aware plugin called the sidecar service, which proxied to the renderer.
		require.Eventually(t, func() bool {
			return rendererStatsAt(t, rendBaseURL) > before
		}, 30*time.Second, 2*time.Second,
			"sidecar renderer /stats must grow: router → sidecar service → renderer subprocess")

		// No error metric confirms the sidecar path succeeded end-to-end.
		require.Equal(t, float64(0), tokenizeErrorCountURL(t, metricsURL),
			"no kvcache tokenize error should occur when sidecar tokenizer succeeds")
	})
}

// rendererStatsAt fetches /stats from a renderer subprocess at the given base URL.
// Unlike rendererStubStats, this helper assumes the caller has already set up the
// port-forward (rendBaseURL points directly at the renderer port).
// Returns -1 on error.
// rendererHTTPClient is used for direct requests to renderer stub subprocesses.
var rendererHTTPClient = &http.Client{Timeout: 10 * time.Second}

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
