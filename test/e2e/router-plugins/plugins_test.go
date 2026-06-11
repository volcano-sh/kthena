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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	plugincontext "github.com/volcano-sh/kthena/test/e2e/router-plugins/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSchedulerPluginPrefixCache verifies repeated prompts stick to one pod after warmup.
func TestSchedulerPluginPrefixCache(t *testing.T) {
	ctx := context.Background()
	chatURL, metricsURL, restoreCfg := applySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyPrefixCache)
	t.Cleanup(restoreCfg)

	route := deployModelRouteFromFile(t, ctx, testCtx.KthenaClient, testNamespace, "ModelRoute-plugins.yaml")
	prompt := "kthena-router-plugin-e2e-fixed-prompt-prefix-cache"

	sendRouterChatRequests(t, chatURL, route.Spec.ModelName, prompt, 30)
	time.Sleep(2 * time.Second)

	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	since := metav1.NewTime(time.Now())
	sendRouterChatRequests(t, chatURL, route.Spec.ModelName, prompt, 200)
	time.Sleep(2 * time.Second)

	maxCount := 0
	routed := 0
	for _, pod := range pods {
		c := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, pod.Name, since)
		t.Logf("prefix-cache: pod %s selected %d/%d", pod.Name, c, 200)
		routed += c
		if c > maxCount {
			maxCount = c
		}
	}
	t.Logf("prefix-cache: dominant pod %d/%d (of %d log lines)", maxCount, 200, routed)
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")
	require.GreaterOrEqual(t, float64(maxCount)/float64(routed), 0.9)

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.PrefixCachePluginName, "score")
}

// TestSchedulerPluginLeastRequest verifies least-request Filter avoids a saturated replica.
// Two identical fast backends: sustain load via direct port-forward on one pod (raises engine
// waiting, does not touch router access logs), then router probe traffic should land on the
// idle replica only.
func TestSchedulerPluginLeastRequest(t *testing.T) {
	ctx := context.Background()
	restoreReplicas := utils.ScaleDeploymentReplicas(t, testCtx.KubeClient, testNamespace, plugincontext.DeploymentName, 2)
	t.Cleanup(restoreReplicas)

	chatURL, metricsURL, restoreCfg := applySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyLeastRequest)
	t.Cleanup(restoreCfg)

	route := deployModelRouteFromFile(t, ctx, testCtx.KthenaClient, testNamespace, "ModelRoute-plugins.yaml")
	model := route.Spec.ModelName

	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, 2, "least-request test needs exactly 2 identical mock pods")
	busyPod, idlePod := pods[0], pods[1]

	stopLoad := startSustainedLongRequestsToPod(t, busyPod, model, "kthena-router-plugin-e2e-fixed-prompt-least-request-busy-load", 20, 128)
	t.Cleanup(stopLoad)
	waitForLeastRequestLoadSeparation(t, testCtx.KubeClient, kthenaNamespace, busyPod, idlePod, leastRequestMaxWaitingRequests)

	since := metav1.NewTime(time.Now())
	sendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-least-request-route", 200)
	time.Sleep(2 * time.Second)

	busyCount := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, busyPod.Name, since)
	idleCount := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, idlePod.Name, since)
	routed := busyCount + idleCount
	t.Logf("least-request: busy pod %s %d, idle pod %s %d (of %d log lines)", busyPod.Name, busyCount, idlePod.Name, idleCount, routed)
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")
	require.Greater(t, idleCount, busyCount, "least-request should prefer the idle pod over the saturated pod")
	require.GreaterOrEqual(t, float64(idleCount)/float64(routed), 0.9,
		"least-request should route at least 90%% to the idle pod")
	require.LessOrEqual(t, float64(busyCount)/float64(routed), 0.1,
		"least-request should route at most 10%% to the saturated pod")

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.LeastRequestPluginName, "score")
	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.LeastRequestPluginName, "filter")
}

// TestSchedulerPluginLeastLatency verifies least-latency prefers the intrinsically faster
// backend when both pools are idle and scored by observed TTFT/TPOT only.
func TestSchedulerPluginLeastLatency(t *testing.T) {
	ctx := context.Background()
	restoreFastReplicas := utils.ScaleDeploymentReplicas(t, testCtx.KubeClient, testNamespace, plugincontext.DeploymentName, 1)
	t.Cleanup(restoreFastReplicas)

	deploySlowLatencyMockStack(t, testCtx.KubeClient, testNamespace)
	_ = deployModelServerFromFile(t, ctx, testCtx.KthenaClient, testNamespace, "ModelServer-plugins-mixed.yaml")

	chatURL, metricsURL, restoreCfg := applySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyLeastLatency)
	t.Cleanup(restoreCfg)

	route := deployModelRouteFromFile(t, ctx, testCtx.KthenaClient, testNamespace, "ModelRoute-plugins-latency.yaml")
	model := route.Spec.ModelName

	fastPods := listReadyPodsByApp(t, testCtx.KubeClient, testNamespace, plugincontext.AppLabel)
	slowPods := listReadyPodsByApp(t, testCtx.KubeClient, testNamespace, plugincontext.SlowMockAppLabel)
	require.Len(t, fastPods, 1, "fast mock pool")
	require.Len(t, slowPods, 1, "slow mock pool")

	// Prime both backends while idle so the router records TTFT/TPOT; do not saturate either pool.
	const primeRequests = 40
	directChatToPod(t, fastPods[0], model, "kthena-router-plugin-e2e-fixed-prompt-latency-fast-prime", primeRequests)
	directChatToPod(t, slowPods[0], model, "kthena-router-plugin-e2e-fixed-prompt-latency-slow-prime", primeRequests)
	time.Sleep(3 * time.Second)

	since := metav1.NewTime(time.Now())
	sendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-latency-route", 200)
	time.Sleep(2 * time.Second)

	fastCount := utils.CountSelectedPodsInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, since, fastPods)
	slowCount := utils.CountSelectedPodsInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, since, slowPods)
	routed := fastCount + slowCount
	t.Logf("least-latency: fast pool %d, slow pool %d (of %d log lines)", fastCount, slowCount, routed)
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")
	require.Greater(t, fastCount, slowCount, "least-latency should prefer the faster backend when both pools are idle")
	require.GreaterOrEqual(t, float64(fastCount)/float64(routed), 0.9,
		"least-latency should route at least 90%% to the fast pool")
	require.LessOrEqual(t, float64(slowCount)/float64(routed), 0.1,
		"least-latency should route at most 10%% to the slow pool")

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.LeastLatencyPluginName, "score")
}

// TestSchedulerPluginLoraAffinity verifies lora-affinity filters to pods that list the adapter in /v1/models.
func TestSchedulerPluginLoraAffinity(t *testing.T) {
	ctx := context.Background()
	restoreReplicas := utils.ScaleDeploymentReplicas(t, testCtx.KubeClient, testNamespace, plugincontext.DeploymentName, 2)
	t.Cleanup(restoreReplicas)

	chatURL, metricsURL, restoreCfg := applySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyLoraAffinity)
	t.Cleanup(restoreCfg)

	_ = deployModelRouteFromFile(t, ctx, testCtx.KthenaClient, testNamespace, "ModelRoute-plugins-lora.yaml")
	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, 2, "lora test needs exactly 2 mock pods")

	loadedPod := pods[0]
	utils.LoadLoRAAdapterOnPod(t, loadedPod, "lora-A", "/models/lora-A")
	utils.WaitForChatModelReady(t, chatURL, "lora-A", []utils.ChatMessage{utils.NewChatMessage("user", "ready")}, 90*time.Second)
	time.Sleep(3 * time.Second)

	since := metav1.NewTime(time.Now())
	sendRouterChatRequests(t, chatURL, "lora-A", "kthena-router-plugin-e2e-fixed-prompt-lora-affinity", 200)
	time.Sleep(2 * time.Second)

	loadedCount := 0
	otherCount := 0
	for _, pod := range pods {
		c := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, pod.Name, since)
		t.Logf("lora-affinity: pod %s selected %d/%d", pod.Name, c, 200)
		if pod.Name == loadedPod.Name {
			loadedCount = c
		} else {
			otherCount += c
		}
	}
	routed := loadedCount + otherCount
	t.Logf("lora-affinity: loaded pod %s %d, other pods %d (of %d log lines)", loadedPod.Name, loadedCount, otherCount, routed)
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")
	require.Equal(t, 0, otherCount, "lora-affinity filter should not route to pods without the adapter")
	require.GreaterOrEqual(t, float64(loadedCount)/float64(routed), 0.9,
		"lora-affinity should route at least 90%% to the pod that loaded the adapter")

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.LoraAffinityPluginName, "filter")
}

// TestSchedulerPluginRandom verifies random score plugin is active.
func TestSchedulerPluginRandom(t *testing.T) {
	ctx := context.Background()
	chatURL, metricsURL, restoreCfg := applySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyRandom)
	t.Cleanup(restoreCfg)

	route := deployModelRouteFromFile(t, ctx, testCtx.KthenaClient, testNamespace, "ModelRoute-plugins.yaml")
	model := route.Spec.ModelName
	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, 3, "random test needs 3 mock pods")

	since := metav1.NewTime(time.Now())
	sendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-random", 200)
	time.Sleep(2 * time.Second)

	counts := make([]int, len(pods))
	routed := 0
	for i, pod := range pods {
		c := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, pod.Name, since)
		counts[i] = c
		routed += c
		t.Logf("random: pod %s selected %d/%d", pod.Name, c, 200)
	}
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")

	// Each pod should receive roughly 1/3 of traffic (±10% absolute ratio).
	const randomMaxRatioDeviation = 0.10
	expectedRatio := 1.0 / float64(len(pods))
	for i, c := range counts {
		require.Greater(t, c, 0, "random should route some traffic to pod %s", pods[i].Name)
		ratio := float64(c) / float64(routed)
		require.GreaterOrEqual(t, ratio, expectedRatio-randomMaxRatioDeviation,
			"random pod %s ratio %.1f%% below uniform %.1f%% - counts=%v", pods[i].Name, ratio*100, expectedRatio*100, counts)
		require.LessOrEqual(t, ratio, expectedRatio+randomMaxRatioDeviation,
			"random pod %s ratio %.1f%% above uniform %.1f%% - counts=%v", pods[i].Name, ratio*100, expectedRatio*100, counts)
	}

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.RandomPluginName, "score")
}
