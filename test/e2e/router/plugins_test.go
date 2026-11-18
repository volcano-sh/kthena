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

package router

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	plugincontext "github.com/volcano-sh/kthena/test/e2e/router/router-plugins/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSchedulerPluginKVCacheAware verifies cache hits prefer the owning replica and misses do not pin to one replica.
func TestSchedulerPluginKVCacheAware(t *testing.T) {
	ctx := context.Background()
	redisCleanup := ensureRedis(t, testCtx.KubeClient, kthenaNamespace)
	t.Cleanup(redisCleanup)

	pods := setupKVCacheFixture(t, testCtx.KubeClient, testNamespace, kthenaNamespace)
	redisClient := setupKVCacheRedisClient(t, kthenaNamespace)
	_ = utils.CreateModelServerFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelServer-plugins-kvcache.yaml")

	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerKVCacheAware, kvCacheFixtureName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-kvcache.yaml")
	prompt := "kthena kvcache routing test"
	warmedPod := pods[0]
	waitForKVCacheOwnership(t, warmedPod, redisClient, route.Spec.ModelName, prompt)

	hitSince := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, route.Spec.ModelName, prompt, kvCacheRoutingRequestCount)
	time.Sleep(2 * time.Second)

	warmedCount := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, warmedPod.Name, hitSince)
	hitRouted := utils.CountSelectedPodsInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, hitSince, pods)
	t.Logf("kvcache-aware hit: warmed pod %s selected %d/%d (of %d log lines)", warmedPod.Name, warmedCount, kvCacheRoutingRequestCount, hitRouted)
	require.GreaterOrEqual(t, hitRouted, kvCacheRoutingRequestCount/2, "expected access logs for cache-hit requests")
	require.GreaterOrEqual(t, float64(warmedCount)/float64(hitRouted), 0.9)

	missSince := metav1.NewTime(time.Now())
	for i := 0; i < kvCacheRoutingRequestCount; i++ {
		// Keep each miss prompt unique so routed requests do not warm later probes.
		missPrompt := fmt.Sprintf("miss-%03d kvcache probe", i)
		utils.SendRouterChatRequests(t, chatURL, route.Spec.ModelName, missPrompt, 1)
	}
	time.Sleep(2 * time.Second)

	maxMissCount := 0
	missRouted := 0
	for _, pod := range pods {
		selectedCount := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, pod.Name, missSince)
		missRouted += selectedCount
		if selectedCount > maxMissCount {
			maxMissCount = selectedCount
		}
	}
	t.Logf("kvcache-aware miss: dominant pod selected %d/%d (of %d log lines)", maxMissCount, kvCacheRoutingRequestCount, missRouted)
	require.GreaterOrEqual(t, missRouted, kvCacheRoutingRequestCount/2, "expected access logs for cache-miss requests")
	require.Less(t, float64(maxMissCount)/float64(missRouted), 0.9)

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
}

// TestSchedulerPluginKVCacheAwarePrefixAndLifecycle verifies longest-prefix scoring, removal events, and stale ownership handling.
func TestSchedulerPluginKVCacheAwarePrefixAndLifecycle(t *testing.T) {
	ctx := context.Background()
	redisCleanup := ensureRedis(t, testCtx.KubeClient, kthenaNamespace)
	t.Cleanup(redisCleanup)

	pods := setupKVCacheFixture(t, testCtx.KubeClient, testNamespace, kthenaNamespace)
	redisClient := setupKVCacheRedisClient(t, kthenaNamespace)
	_ = utils.CreateModelServerFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelServer-plugins-kvcache.yaml")

	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerKVCacheAware, kvCacheFixtureName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-kvcache.yaml")
	shortPrompt := "phase four alpha beta"
	longPrompt := shortPrompt + " gamma delta epsilon zeta eta theta iota"
	shortHashes := kvCachePromptHashes(t, pods[0], route.Spec.ModelName, shortPrompt)
	longHashes := kvCachePromptHashes(t, pods[0], route.Spec.ModelName, longPrompt)
	sharedBlocks := 0
	for sharedBlocks < len(shortHashes) && sharedBlocks < len(longHashes) && shortHashes[sharedBlocks] == longHashes[sharedBlocks] {
		sharedBlocks++
	}
	require.GreaterOrEqual(t, sharedBlocks, 1, "prompts must share at least one complete block")
	require.Less(t, sharedBlocks, len(longHashes), "long prompt needs an additional complete block")

	shortPrefixPod := pods[0]
	longPrefixPod := pods[1]
	coldPod := pods[2]
	waitForKVCacheOwnership(t, shortPrefixPod, redisClient, route.Spec.ModelName, shortPrompt)
	longOwnership := waitForKVCacheHashOwnership(t, longPrefixPod, redisClient, route.Spec.ModelName, longPrompt, longHashes[sharedBlocks])
	messages := []utils.ChatMessage{utils.NewChatMessage("user", longPrompt)}
	selectedPod := utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
	require.Equal(t, longPrefixPod.Name, selectedPod)

	for i := 0; i < 12; i++ {
		evictionPrompt := fmt.Sprintf("phase four eviction %02d unique alpha beta gamma delta epsilon zeta", i)
		utils.DirectChatToPod(t, longPrefixPod, route.Spec.ModelName, evictionPrompt, 1)
	}
	waitForKVCacheOwnershipRemoved(t, redisClient, longOwnership)
	waitForKVCacheHashOwnership(t, coldPod, redisClient, route.Spec.ModelName, longPrompt, longHashes[sharedBlocks])
	selectedPod = utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
	require.Equal(t, coldPod.Name, selectedPod)

	postKVCacheLifecycle(t, longPrefixPod, "/sleep")
	waitForAllKVCacheOwnershipRemoved(t, redisClient, longPrefixPod, route.Spec.ModelName)
	postKVCacheLifecycle(t, longPrefixPod, "/wake_up")
	postKVCacheLifecycle(t, coldPod, "/sleep")
	waitForAllKVCacheOwnershipRemoved(t, redisClient, coldPod, route.Spec.ModelName)
	postKVCacheLifecycle(t, coldPod, "/wake_up")
	selectedPod = utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
	require.Equal(t, shortPrefixPod.Name, selectedPod)

	postKVCacheLifecycle(t, shortPrefixPod, "/sleep")
	waitForAllKVCacheOwnershipRemoved(t, redisClient, shortPrefixPod, route.Spec.ModelName)
	postKVCacheLifecycle(t, shortPrefixPod, "/wake_up")
	waitForKVCacheOwnership(t, shortPrefixPod, redisClient, route.Spec.ModelName, shortPrompt)

	staleOwnership := waitForKVCacheHashOwnership(t, coldPod, redisClient, route.Spec.ModelName, longPrompt, longHashes[sharedBlocks])
	var engineStartedAt metav1.Time
	for _, status := range coldPod.Status.ContainerStatuses {
		if status.Name == "llm-engine" && status.State.Running != nil {
			engineStartedAt = status.State.Running.StartedAt
		}
	}
	require.False(t, engineStartedAt.IsZero(), "warmed simulator container should be running")
	originalUID := coldPod.UID
	zero := int64(0)
	err := testCtx.KubeClient.CoreV1().Pods(coldPod.Namespace).Delete(ctx, coldPod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	require.NoError(t, err, "delete warmed simulator Pod")
	var replacementPod corev1.Pod
	var replacementLastContainerStartedAt metav1.Time
	require.Eventually(t, func() bool {
		pod, err := testCtx.KubeClient.CoreV1().Pods(coldPod.Namespace).Get(ctx, coldPod.Name, metav1.GetOptions{})
		if err != nil || pod.UID == originalUID || !utils.IsPodReady(*pod) {
			return false
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "llm-engine" && status.State.Running != nil && status.State.Running.StartedAt.After(engineStartedAt.Time) {
				replacementPod = *pod
				for _, replacementStatus := range pod.Status.ContainerStatuses {
					if replacementStatus.State.Running != nil && replacementStatus.State.Running.StartedAt.After(replacementLastContainerStartedAt.Time) {
						replacementLastContainerStartedAt = replacementStatus.State.Running.StartedAt
					}
				}
				return true
			}
		}
		return false
	}, 5*time.Minute, 2*time.Second, "KV-cache fixture did not restart %s with the same Pod name", coldPod.Name)

	redisCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	staleOwnerExists, err := redisClient.HExists(redisCtx, staleOwnership.ownershipKey, staleOwnership.owner).Result()
	cancel()
	require.NoError(t, err)
	require.True(t, staleOwnerExists, "abrupt Pod deletion should leave stale ownership for this check")

	candidatePrompt := "replacement candidate turquoise onyx magenta saffron"
	candidateHashes := kvCachePromptHashes(t, replacementPod, route.Spec.ModelName, candidatePrompt)
	for _, candidateHash := range candidateHashes {
		require.NotContains(t, longHashes, candidateHash, "candidate probe must not refresh the stale prompt")
	}
	require.Eventually(t, func() bool {
		return time.Now().Unix() > replacementLastContainerStartedAt.Unix()
	}, 2*time.Second, 50*time.Millisecond, "replacement ownership must be newer than its container start time")
	waitForKVCacheOwnership(t, replacementPod, redisClient, route.Spec.ModelName, candidatePrompt)
	candidateMessages := []utils.ChatMessage{utils.NewChatMessage("user", candidatePrompt)}
	selectedPod = utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, candidateMessages, nil)
	require.Equal(t, replacementPod.Name, selectedPod, "replacement Pod should remain a routing candidate")

	selectedPod = utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
	require.Equal(t, shortPrefixPod.Name, selectedPod, "stale ownership from before the same-name Pod restart must not affect selection")

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
}

// TestSchedulerPluginMixedWeights verifies plugin weights decide the winner when score plugins disagree.
func TestSchedulerPluginMixedWeights(t *testing.T) {
	ctx := context.Background()
	redisCleanup := ensureRedis(t, testCtx.KubeClient, kthenaNamespace)
	t.Cleanup(redisCleanup)

	pods := setupKVCacheFixture(t, testCtx.KubeClient, testNamespace, kthenaNamespace)
	redisClient := setupKVCacheRedisClient(t, kthenaNamespace)
	_ = utils.CreateModelServerFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelServer-plugins-kvcache.yaml")
	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-kvcache.yaml")
	prompt := "kthena kvcache routing test"
	warmedPod := pods[0]
	otherHotPod := pods[1]
	coldPod := pods[2]
	hotPods := []corev1.Pod{warmedPod, otherHotPod}
	coldPods := []corev1.Pod{coldPod}
	waitForKVCacheOwnership(t, warmedPod, redisClient, route.Spec.ModelName, prompt)
	stopWarmedLoad := utils.StartSustainedLongRequestsToPod(t, warmedPod, route.Spec.ModelName, prompt, gpuCacheUsageLoadConcurrency, gpuCacheUsageLoadMaxTokens)
	t.Cleanup(stopWarmedLoad)
	stopOtherLoad := utils.StartSustainedLongRequestsToPod(t, otherHotPod, route.Spec.ModelName, "unrelated gpu load prompt", gpuCacheUsageLoadConcurrency, gpuCacheUsageLoadMaxTokens)
	t.Cleanup(stopOtherLoad)
	messages := []utils.ChatMessage{utils.NewChatMessage("user", prompt)}

	t.Run("kvcache weight wins", func(t *testing.T) {
		schedulerConfig := schedulerKVCacheAndGPUUsage(mixedPluginDominantWeight, 1)
		chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerConfig, kvCacheFixtureName, plugincontext.ModelName)
		t.Cleanup(restoreCfg)
		waitForRouterGPUCacheSeparation(t, testCtx.KubeClient, kthenaNamespace, hotPods, coldPods)

		selectedPod := utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
		require.Equal(t, warmedPod.Name, selectedPod)
		waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
		waitForSchedulerPluginInMetrics(t, metricsURL, plugins.GPUCacheUsagePluginName, "score")
	})

	t.Run("gpu usage weight wins", func(t *testing.T) {
		schedulerConfig := schedulerKVCacheAndGPUUsage(1, mixedPluginDominantWeight)
		chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerConfig, kvCacheFixtureName, plugincontext.ModelName)
		t.Cleanup(restoreCfg)
		waitForRouterGPUCacheSeparation(t, testCtx.KubeClient, kthenaNamespace, hotPods, coldPods)

		selectedPod := utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
		require.Equal(t, coldPod.Name, selectedPod)
		waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
		waitForSchedulerPluginInMetrics(t, metricsURL, plugins.GPUCacheUsagePluginName, "score")
	})
}

// TestSchedulerPluginPrefixCache verifies repeated prompts stick to one pod after warmup.
func TestSchedulerPluginPrefixCache(t *testing.T) {
	ctx := context.Background()
	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyPrefixCache, plugincontext.ModelServerName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins.yaml")
	prompt := "kthena-router-plugin-e2e-fixed-prompt-prefix-cache"

	utils.SendRouterChatRequests(t, chatURL, route.Spec.ModelName, prompt, 30)
	time.Sleep(2 * time.Second)

	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	since := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, route.Spec.ModelName, prompt, 200)
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

// TestSchedulerPluginLeastRequest verifies least-request Filter avoids saturated replicas.
// Three fast backends: sustain load on two pods (raises engine waiting), leave one idle,
// then router probe traffic should land on the idle replica only.
func TestSchedulerPluginLeastRequest(t *testing.T) {
	ctx := context.Background()
	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyLeastRequest, plugincontext.ModelServerName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins.yaml")
	model := route.Spec.ModelName

	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, pluginMockReplicaCount, "least-request test needs %d mock pods", pluginMockReplicaCount)
	busyPods := pods[:pluginMockReplicaCount-1]
	idlePod := pods[pluginMockReplicaCount-1]

	for i, busyPod := range busyPods {
		stopLoad := utils.StartSustainedLongRequestsToPod(t, busyPod, model,
			fmt.Sprintf("kthena-router-plugin-e2e-fixed-prompt-least-request-busy-load-%d", i), 20, 128)
		t.Cleanup(stopLoad)
	}
	waitForLeastRequestLoadSeparation(t, testCtx.KubeClient, kthenaNamespace, busyPods, idlePod, leastRequestMaxWaitingRequests)

	since := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-least-request-route", 200)
	time.Sleep(2 * time.Second)

	busyCount := utils.CountSelectedPodsInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, since, busyPods)
	idleCount := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, idlePod.Name, since)
	routed := busyCount + idleCount
	t.Logf("least-request: busy pool %d, idle pod %s %d (of %d log lines)", busyCount, idlePod.Name, idleCount, routed)
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")
	require.Greater(t, idleCount, busyCount, "least-request should prefer the idle pod over saturated pods")
	require.GreaterOrEqual(t, float64(idleCount)/float64(routed), 0.9,
		"least-request should route at least 90%% to the idle pod")
	require.LessOrEqual(t, float64(busyCount)/float64(routed), 0.1,
		"least-request should route at most 10%% to saturated pods")

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.LeastRequestPluginName, "score")
	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.LeastRequestPluginName, "filter")
}

// TestSchedulerPluginGPUCacheUsage verifies gpu-usage prefers replicas with lower KV cache
// utilization. Mock backends run with enable-kvcache (see LLM-Mock-plugins.yaml); two
// replicas are kept hot with sustained long streaming load and probe traffic lands on idle.
func TestSchedulerPluginGPUCacheUsage(t *testing.T) {
	ctx := context.Background()

	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyGPUCacheUsage, plugincontext.ModelServerName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins.yaml")
	model := route.Spec.ModelName

	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, pluginMockReplicaCount, "gpu-usage test needs %d mock pods", pluginMockReplicaCount)
	busyPods := pods[:pluginMockReplicaCount-1]
	idlePod := pods[pluginMockReplicaCount-1]

	for i, busyPod := range busyPods {
		stopLoad := utils.StartSustainedLongRequestsToPod(t, busyPod, model,
			fmt.Sprintf("kthena-router-plugin-e2e-fixed-prompt-gpu-usage-busy-load-%d with many tokens to consume kv cache blocks for gpu usage plugin testing", i),
			gpuCacheUsageLoadConcurrency, gpuCacheUsageLoadMaxTokens)
		t.Cleanup(stopLoad)
	}
	waitForRouterGPUCacheSeparation(t, testCtx.KubeClient, kthenaNamespace, busyPods, []corev1.Pod{idlePod})

	since := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-gpu-usage-route", 200)
	time.Sleep(2 * time.Second)

	busyCount := utils.CountSelectedPodsInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, since, busyPods)
	idleCount := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, idlePod.Name, since)
	routed := busyCount + idleCount
	t.Logf("gpu-usage: busy pool %d, idle pod %s %d (of %d log lines)", busyCount, idlePod.Name, idleCount, routed)
	require.GreaterOrEqual(t, routed, 200/2, "expected access logs for routed requests")
	require.Greater(t, idleCount, busyCount, "gpu-usage should prefer the idle pod over kv-cache-hot pods")
	require.GreaterOrEqual(t, float64(idleCount)/float64(routed), 0.9,
		"gpu-usage should route at least 90%% to the idle pod")
	require.LessOrEqual(t, float64(busyCount)/float64(routed), 0.1,
		"gpu-usage should route at most 10%% to kv-cache-hot pods")

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.GPUCacheUsagePluginName, "score")
}

// TestSchedulerPluginLeastLatency verifies least-latency prefers the intrinsically faster
// backend when both pools are idle and scored by observed TTFT/TPOT only.
func TestSchedulerPluginLeastLatency(t *testing.T) {
	ctx := context.Background()

	fastPods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, fastPods, pluginMockReplicaCount, "fast mock pool")

	slowPods := utils.ListReadyPodsByLabel(t, testCtx.KubeClient, testNamespace, "app="+plugincontext.SlowMockAppLabel)
	require.Len(t, slowPods, 1, "slow mock pool")

	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyLeastLatency, plugincontext.ModelServerName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-latency.yaml")
	model := route.Spec.ModelName

	// Prime both pools after the scheduler-specific router restart. Histogram deltas need
	// an initial scrape baseline, so send a small baseline batch first, then the measured
	// batch that should make the fast pool strictly lower-latency than the slow pool.
	latencyPods := append(append([]corev1.Pod{}, fastPods...), slowPods...)
	for _, pod := range latencyPods {
		utils.DirectChatToPod(t, pod, model, "kthena-router-plugin-e2e-fixed-prompt-latency-baseline-prime", 2)
	}
	time.Sleep(2 * time.Second)
	for _, pod := range latencyPods {
		utils.DirectChatToPod(t, pod, model, "kthena-router-plugin-e2e-fixed-prompt-latency-measured-prime", 8)
	}
	waitForLeastLatencyMetricsSeparation(t, testCtx.KubeClient, kthenaNamespace, fastPods, slowPods)

	since := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-latency-route", 200)
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
	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyLoraAffinity, plugincontext.ModelServerName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	_ = utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-lora.yaml")
	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, pluginMockReplicaCount, "lora test needs %d mock pods", pluginMockReplicaCount)

	loadedPod := pods[0]
	utils.LoadLoRAAdapterOnPod(t, loadedPod, "lora-A", "/models/lora-A")
	utils.WaitForChatModelReady(t, chatURL, "lora-A", []utils.ChatMessage{utils.NewChatMessage("user", "ready")}, 90*time.Second)
	time.Sleep(3 * time.Second)

	since := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, "lora-A", "kthena-router-plugin-e2e-fixed-prompt-lora-affinity", 200)
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
	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerOnlyRandom, plugincontext.ModelServerName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins.yaml")
	model := route.Spec.ModelName
	pods := listReadyMockPods(t, testCtx.KubeClient, testNamespace)
	require.Len(t, pods, pluginMockReplicaCount, "random test needs %d mock pods", pluginMockReplicaCount)

	const (
		requestsPerBatch        = 200
		maxBatches              = 5
		randomMaxRatioDeviation = 0.10
	)
	expectedRatio := 1.0 / float64(len(pods))
	distributionOK := false
	var lastCounts []int
	since := metav1.Now()
	for batch := 1; batch <= maxBatches; batch++ {
		// Start with the original 200-request sample. If random variance puts that
		// sample outside the bounds, add more observations so a healthy scheduler
		// converges while a persistent bias remains visible.
		utils.SendRouterChatRequests(t, chatURL, model, "kthena-router-plugin-e2e-fixed-prompt-random", requestsPerBatch)
		time.Sleep(2 * time.Second)

		counts := make([]int, len(pods))
		routed := 0
		for i, pod := range pods {
			c := utils.CountSelectedPodInRouterLogs(t, testCtx.KubeClient, kthenaNamespace, pod.Name, since)
			counts[i] = c
			routed += c
			t.Logf("random batch %d/%d: pod %s selected %d cumulative requests", batch, maxBatches, pod.Name, c)
		}
		lastCounts = counts
		expectedRequests := batch * requestsPerBatch
		if routed < expectedRequests/2 {
			t.Logf("random distribution after batch %d/%d had insufficient access logs: routed=%d expected=%d counts=%v",
				batch, maxBatches, routed, expectedRequests, counts)
			continue
		}

		distributionWithinBounds := true
		for _, c := range counts {
			ratio := float64(c) / float64(routed)
			if c == 0 || ratio < expectedRatio-randomMaxRatioDeviation || ratio > expectedRatio+randomMaxRatioDeviation {
				distributionWithinBounds = false
				break
			}
		}
		if !distributionWithinBounds {
			t.Logf("random distribution after batch %d/%d outside uniform %.1f%% +/- %.1f%%: routed=%d counts=%v",
				batch, maxBatches, expectedRatio*100, randomMaxRatioDeviation*100, routed, counts)
			continue
		}

		t.Logf("random distribution verified after batch %d/%d: routed=%d counts=%v", batch, maxBatches, routed, counts)
		distributionOK = true
		break
	}
	if !distributionOK {
		t.Fatalf("random distribution failed after up to %d cumulative requests; last counts=%v", requestsPerBatch*maxBatches, lastCounts)
	}

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.RandomPluginName, "score")
}
