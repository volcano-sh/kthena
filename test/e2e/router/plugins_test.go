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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	plugincontext "github.com/volcano-sh/kthena/test/e2e/router/router-plugins/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSchedulerPluginKVCacheAware verifies cache hits prefer the owning replica and misses do not pin to one replica.
func TestSchedulerPluginKVCacheAware(t *testing.T) {
	ctx := context.Background()
	redisCleanup := ensureRedis(t, testCtx.KubeClient, kthenaNamespace)
	t.Cleanup(redisCleanup)

	pods := setupKVCacheFixture(t, testCtx.KubeClient, testNamespace, kthenaNamespace)
	_ = utils.CreateModelServerFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelServer-plugins-kvcache.yaml")

	chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerKVCacheAware, kvCacheFixtureDeploymentName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-kvcache.yaml")
	prompt := "kthena kvcache routing test"
	warmedPod := pods[0]
	waitForKVCacheOwnership(t, warmedPod, kthenaNamespace, route.Spec.ModelName, prompt)

	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, prompt, warmedPod, pods)
	assertKVCacheMissRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, pods)

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
}

// activeRouterPods returns ready router pods that are not terminating.
func activeRouterPods(t *testing.T) []corev1.Pod {
	t.Helper()
	var active []corev1.Pod
	for _, pod := range utils.GetReadyRouterPods(t, testCtx.KubeClient, kthenaNamespace) {
		if pod.DeletionTimestamp == nil {
			active = append(active, pod)
		}
	}
	require.NotEmpty(t, active, "no active router pods")
	return active
}

// setupKVCacheMemoryScenario deploys the memory-mode fixture, applies the memory scheduler config and
// waits until every sidecar has streamed to the current router pods.
func setupKVCacheMemoryScenario(t *testing.T, gated bool) (pods []corev1.Pod, route *networkingv1alpha1.ModelRoute, chatURL, metricsURL string) {
	t.Helper()
	ctx := context.Background()
	since := metav1.NewTime(time.Now())

	// Memory mode must not depend on Redis, so make sure none is deployed.
	_, err := testCtx.KubeClient.CoreV1().Services(kthenaNamespace).Get(ctx, "redis-server", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "memory mode must run without Redis, got err=%v", err)

	pods = setupKVCacheMemoryFixture(t, testCtx.KubeClient, testNamespace, gated)
	_ = utils.CreateModelServerFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelServer-plugins-kvcache.yaml")

	var restoreCfg func()
	chatURL, metricsURL, restoreCfg = utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerKVCacheAwareMemory, kvCacheFixtureDeploymentName, plugincontext.ModelName)
	t.Cleanup(restoreCfg)

	route = utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-kvcache.yaml")
	waitForRouterRegistration(t, testCtx.KubeClient, pods, activeRouterPods(t), since)
	return pods, route, chatURL, metricsURL
}

// TestSchedulerPluginKVCacheAwareMemory verifies memory index mode routes cache hits to the owning
// replica and does not pin misses, without Redis.
func TestSchedulerPluginKVCacheAwareMemory(t *testing.T) {
	pods, route, chatURL, metricsURL := setupKVCacheMemoryScenario(t, false)
	prompt := "kthena kvcache memory routing test"
	warmedPod := pods[0]
	warmKVCacheMemory(t, testCtx.KubeClient, warmedPod, route.Spec.ModelName, prompt)

	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, prompt, warmedPod, pods)
	assertKVCacheMissRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, pods)

	waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
}

// TestSchedulerPluginKVCacheAwareMemoryPodLifecycle verifies a deleted owner is no longer selected and
// that its replacement is picked up by the router and can own blocks again.
func TestSchedulerPluginKVCacheAwareMemoryPodLifecycle(t *testing.T) {
	ctx := context.Background()
	pods, route, chatURL, _ := setupKVCacheMemoryScenario(t, false)
	model := route.Spec.ModelName
	prompt := "kthena kvcache memory lifecycle test"
	ownerPod := pods[0]
	warmKVCacheMemory(t, testCtx.KubeClient, ownerPod, model, prompt)
	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, prompt, ownerPod, pods)

	deletedAt := metav1.NewTime(time.Now())
	require.NoError(t, testCtx.KubeClient.CoreV1().Pods(testNamespace).Delete(ctx, ownerPod.Name, metav1.DeleteOptions{}))
	var survivors []corev1.Pod
	var replacement corev1.Pod
	require.Eventually(t, func() bool {
		survivors, replacement = nil, corev1.Pod{}
		list, err := testCtx.KubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + kvCacheFixtureAppLabel})
		if err != nil {
			return false
		}
		for _, pod := range list.Items {
			if !utils.IsPodReady(pod) || pod.DeletionTimestamp != nil || pod.Name == ownerPod.Name {
				continue
			}
			survivors = append(survivors, pod)
			if pod.Name != pods[1].Name && pod.Name != pods[2].Name {
				replacement = pod
			}
		}
		return len(survivors) == pluginMockReplicaCount && replacement.Name != ""
	}, 5*time.Minute, 2*time.Second, "owner pod %s was not replaced", ownerPod.Name)
	waitForRouterRegistration(t, testCtx.KubeClient, []corev1.Pod{replacement}, activeRouterPods(t), deletedAt)

	messages := []utils.ChatMessage{utils.NewChatMessage("user", prompt)}
	selected := utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, messages, nil)
	require.NotEqual(t, ownerPod.Name, selected, "deleted owner must not stay preferred")

	// The simulated engine hashes only the first block, so lead with a token the old prompt does not share.
	newPrompt := "replacement kvcache memory test"
	warmKVCacheMemory(t, testCtx.KubeClient, replacement, model, newPrompt)
	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, newPrompt, replacement, survivors)
}

// TestSchedulerPluginKVCacheAwareMemoryRouterRestart verifies a restarted router rebuilds its index from
// the sidecar snapshots, so cache-hit routing resumes without warming again.
func TestSchedulerPluginKVCacheAwareMemoryRouterRestart(t *testing.T) {
	ctx := context.Background()
	pods, route, chatURL, _ := setupKVCacheMemoryScenario(t, false)
	model := route.Spec.ModelName
	prompt := "kthena kvcache memory restart test"
	warmedPod := pods[0]
	warmKVCacheMemory(t, testCtx.KubeClient, warmedPod, model, prompt)
	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, prompt, warmedPod, pods)

	restartedAt := metav1.NewTime(time.Now())
	require.NoError(t, utils.RolloutRestartDeployment(ctx, testCtx.KubeClient, kthenaNamespace, utils.RouterDeploymentName, utils.RouterRolloutTimeout))
	newChatURL, _, closePF := utils.SetupRouterPortForwardAfterRestart(t, kthenaNamespace)
	t.Cleanup(closePF)
	waitForRouterRegistration(t, testCtx.KubeClient, pods, activeRouterPods(t), restartedAt)

	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, newChatURL, model, prompt, warmedPod, pods)
}

// TestSchedulerPluginKVCacheAwareMemoryNotReady verifies a pod that turns NotReady is no longer routed to,
// its stream ends, and once Ready again the router reconnects and the snapshot restores cache-hit routing.
func TestSchedulerPluginKVCacheAwareMemoryNotReady(t *testing.T) {
	ctx := context.Background()
	pods, route, chatURL, _ := setupKVCacheMemoryScenario(t, true)
	model := route.Spec.ModelName
	ownedPrompt := "kthena kvcache memory notready test"
	// The second prompt is routed while the owner is NotReady, which warms another pod, so keep it apart from the first.
	routedPrompt := "unready kvcache memory probe test"
	owner := pods[0]
	warmKVCacheMemory(t, testCtx.KubeClient, owner, model, ownedPrompt)
	warmKVCacheMemory(t, testCtx.KubeClient, owner, model, routedPrompt)
	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, ownedPrompt, owner, pods)

	notReadyAt := metav1.NewTime(time.Now())
	require.NoError(t, setKVCacheReadinessGate(ctx, testCtx.KubeClient, owner, false))
	require.Eventually(t, func() bool {
		pod, err := testCtx.KubeClient.CoreV1().Pods(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		return err == nil && !utils.IsPodReady(*pod)
	}, time.Minute, time.Second, "owner pod %s did not become NotReady", owner.Name)
	require.Eventuallyf(t, func() bool {
		return strings.Contains(runtimeLogsSince(t, testCtx.KubeClient, owner, notReadyAt), "Router stream ended: id=")
	}, kvCacheStreamWaitTimeout, time.Second, "router did not end the stream of NotReady pod %s", owner.Name)

	messages := []utils.ChatMessage{utils.NewChatMessage("user", routedPrompt)}
	selected := utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, messages, nil)
	require.NotEqual(t, owner.Name, selected, "NotReady owner must not be selected")

	readyAt := metav1.NewTime(time.Now())
	require.NoError(t, setKVCacheReadinessGate(ctx, testCtx.KubeClient, owner, true))
	require.Eventually(t, func() bool {
		pod, err := testCtx.KubeClient.CoreV1().Pods(owner.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		return err == nil && utils.IsPodReady(*pod)
	}, time.Minute, time.Second, "owner pod %s did not become Ready again", owner.Name)
	waitForRouterRegistration(t, testCtx.KubeClient, []corev1.Pod{owner}, activeRouterPods(t), readyAt)

	assertKVCacheHitRouting(t, testCtx.KubeClient, kthenaNamespace, chatURL, model, ownedPrompt, owner, pods)
}

// TestSchedulerPluginKVCacheAwareMemoryMultiRouter verifies every router replica builds its own index from
// the sidecar streams and routes cache hits to the owning replica.
func TestSchedulerPluginKVCacheAwareMemoryMultiRouter(t *testing.T) {
	// Registrations of the original router happen during setup, so look back to the start of the test.
	testStart := metav1.NewTime(time.Now())
	pods, route, _, _ := setupKVCacheMemoryScenario(t, false)
	model := route.Spec.ModelName
	prompt := "kthena kvcache memory multirouter test"
	t.Cleanup(utils.ScaleDeploymentReplicas(t, testCtx.KubeClient, kthenaNamespace, utils.RouterDeploymentName, 2))

	routerPods := activeRouterPods(t)
	require.Len(t, routerPods, 2, "expected two router replicas")
	waitForRouterRegistration(t, testCtx.KubeClient, pods, routerPods, testStart)
	warmedPod := pods[0]
	warmKVCacheMemory(t, testCtx.KubeClient, warmedPod, model, prompt)

	for _, routerPod := range routerPods {
		localPort := utils.AllocateLocalPort(t)
		pf, err := utils.SetupPortForwardToPod(routerPod.Namespace, routerPod.Name, localPort, routerContainerPort)
		require.NoError(t, err, "port-forward to router %s", routerPod.Name)
		t.Cleanup(pf.Close)

		since := metav1.NewTime(time.Now())
		utils.SendRouterChatRequests(t, fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort), model, prompt, kvCacheRoutingRequestCount/2)
		time.Sleep(2 * time.Second)

		hits := countSelectedPodInRouterPodLogs(t, testCtx.KubeClient, routerPod, warmedPod.Name, since)
		total := 0
		for _, pod := range pods {
			total += countSelectedPodInRouterPodLogs(t, testCtx.KubeClient, routerPod, pod.Name, since)
		}
		t.Logf("router %s: warmed pod %s selected %d/%d", routerPod.Name, warmedPod.Name, hits, total)
		require.GreaterOrEqual(t, total, kvCacheRoutingRequestCount/4, "expected access logs on router %s", routerPod.Name)
		require.GreaterOrEqual(t, float64(hits)/float64(total), 0.9, "router %s must route hits to the owning pod", routerPod.Name)
	}
}

// TestSchedulerPluginMixedWeights verifies plugin weights decide the winner when score plugins disagree.
func TestSchedulerPluginMixedWeights(t *testing.T) {
	ctx := context.Background()
	redisCleanup := ensureRedis(t, testCtx.KubeClient, kthenaNamespace)
	t.Cleanup(redisCleanup)

	pods := setupKVCacheFixture(t, testCtx.KubeClient, testNamespace, kthenaNamespace)
	_ = utils.CreateModelServerFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelServer-plugins-kvcache.yaml")
	route := utils.CreateModelRouteFromFile(t, ctx, testCtx.KthenaClient, plugincontext.TestDataDir, testNamespace, "ModelRoute-plugins-kvcache.yaml")
	prompt := "kthena kvcache routing test"
	warmedPod := pods[0]
	otherHotPod := pods[1]
	coldPod := pods[2]
	hotPods := []corev1.Pod{warmedPod, otherHotPod}
	coldPods := []corev1.Pod{coldPod}
	waitForKVCacheOwnership(t, warmedPod, kthenaNamespace, route.Spec.ModelName, prompt)
	stopWarmedLoad := utils.StartSustainedLongRequestsToPod(t, warmedPod, route.Spec.ModelName, prompt, gpuCacheUsageLoadConcurrency, gpuCacheUsageLoadMaxTokens)
	t.Cleanup(stopWarmedLoad)
	stopOtherLoad := utils.StartSustainedLongRequestsToPod(t, otherHotPod, route.Spec.ModelName, "unrelated gpu load prompt", gpuCacheUsageLoadConcurrency, gpuCacheUsageLoadMaxTokens)
	t.Cleanup(stopOtherLoad)
	messages := []utils.ChatMessage{utils.NewChatMessage("user", prompt)}

	t.Run("kvcache weight wins", func(t *testing.T) {
		schedulerConfig := schedulerKVCacheAndGPUUsage(mixedPluginDominantWeight, 1)
		chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerConfig, kvCacheFixtureDeploymentName, plugincontext.ModelName)
		t.Cleanup(restoreCfg)
		waitForRouterGPUCacheSeparation(t, testCtx.KubeClient, kthenaNamespace, hotPods, coldPods)

		selectedPod := utils.SelectedPodAfterChatURLHeaders(t, testCtx.KubeClient, kthenaNamespace, chatURL, route.Spec.ModelName, messages, nil)
		require.Equal(t, warmedPod.Name, selectedPod)
		waitForSchedulerPluginInMetrics(t, metricsURL, plugins.KVCacheAwarePluginName, "score")
		waitForSchedulerPluginInMetrics(t, metricsURL, plugins.GPUCacheUsagePluginName, "score")
	})

	t.Run("gpu usage weight wins", func(t *testing.T) {
		schedulerConfig := schedulerKVCacheAndGPUUsage(1, mixedPluginDominantWeight)
		chatURL, metricsURL, restoreCfg := utils.ApplySchedulerConfig(t, testCtx.KubeClient, testCtx.KthenaClient, kthenaNamespace, testNamespace, schedulerConfig, kvCacheFixtureDeploymentName, plugincontext.ModelName)
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
