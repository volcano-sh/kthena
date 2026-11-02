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
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/tokenization"
	plugincontext "github.com/volcano-sh/kthena/test/e2e/router/router-plugins/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	pluginMockReplicaCount         = 3
	leastRequestMaxWaitingRequests = 1
	leastRequestLoadWaitTimeout    = 60 * time.Second
	gpuCacheUsageBusyMin           = 0.1
	gpuCacheUsageIdleMax           = 0.05
	gpuCacheUsageLoadWaitTimeout   = 90 * time.Second
	gpuCacheUsageLoadConcurrency   = 2
	gpuCacheUsageLoadMaxTokens     = 256
	kvCacheFixtureDeploymentName   = "router-plugin-kvcache-mock"
	kvCacheFixtureAppLabel         = kvCacheFixtureDeploymentName
	kvCacheRuntimeContainer        = "kthena-runtime"
	routerContainerPort            = "8080" // kthenaRouter.port in the Helm chart; the Service exposes it as 80
	kvCacheSyncModeEnv             = "KV_EVENT_SYNC_MODE"
	kvCacheStreamWaitTimeout       = 90 * time.Second
	kvCacheOwnershipWaitTimeout    = 90 * time.Second
	kvCacheMappingPrefix           = "vllm:kv:block"
	kvCacheOwnershipPrefix         = "matrix:kv:block"
	kvCacheBlockSize               = 8
	kvCacheRoutingRequestCount     = 200
	mixedPluginDominantWeight      = 101 // one more than either plugin's maximum score
)

// kvCacheReadinessGate is a pod readiness gate the memory-mode tests flip to make a pod NotReady.
const kvCacheReadinessGate corev1.PodConditionType = "kthena.volcano.sh/e2e-ready"

func listReadyMockPods(t *testing.T, kube kubernetes.Interface, namespace string) []corev1.Pod {
	t.Helper()
	ready := utils.ListReadyPodsByLabel(t, kube, namespace, "app="+plugincontext.DeploymentName)
	require.NotEmpty(t, ready, "no ready mock pods")
	return ready
}

func setupKVCacheFixture(t *testing.T, kube kubernetes.Interface, namespace, redisNamespace string) []corev1.Pod {
	t.Helper()
	return createKVCacheFixture(t, kube, namespace, false, func(container *corev1.Container) {
		redisHostSet := false
		for j := range container.Env {
			if container.Env[j].Name == "REDIS_HOST" {
				container.Env[j].Value = fmt.Sprintf("redis-server.%s.svc.cluster.local", redisNamespace)
				redisHostSet = true
			}
		}
		require.True(t, redisHostSet, "KV-cache fixture Runtime needs REDIS_HOST")
	})
}

// setupKVCacheMemoryFixture deploys the KV-cache fixture with the runtime sidecar streaming
// KV events to routers (KV_EVENT_SYNC_MODE=memory) and without any Redis settings.
func setupKVCacheMemoryFixture(t *testing.T, kube kubernetes.Interface, namespace string, gated bool) []corev1.Pod {
	t.Helper()
	return createKVCacheFixture(t, kube, namespace, gated, func(container *corev1.Container) {
		env := make([]corev1.EnvVar, 0, len(container.Env)+1)
		for _, e := range container.Env {
			if e.Name != "REDIS_HOST" && e.Name != "REDIS_PORT" {
				env = append(env, e)
			}
		}
		container.Env = append(env, corev1.EnvVar{Name: kvCacheSyncModeEnv, Value: "memory"})
	})
}

// createKVCacheFixture deploys the KV-cache mock fixture. With gated set, pods also need the
// kvCacheReadinessGate condition, which the test flips to make a pod NotReady without restarting it.
func createKVCacheFixture(t *testing.T, kube kubernetes.Interface, namespace string, gated bool, configureRuntime func(*corev1.Container)) []corev1.Pod {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(plugincontext.TestDataDir, "LLM-Mock-plugins-kvcache.yaml"))
	deployment.Namespace = namespace
	runtimeFound := false
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name != kvCacheRuntimeContainer {
			continue
		}
		configureRuntime(container)
		runtimeFound = true
	}
	require.True(t, runtimeFound, "KV-cache fixture needs a %s container", kvCacheRuntimeContainer)

	if gated {
		deployment.Spec.Template.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: kvCacheReadinessGate}}
	}

	_, err := kube.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err, "create KV-cache fixture")
	t.Cleanup(func() {
		utils.DeleteDeploymentAndWait(t, kube, namespace, kvCacheFixtureDeploymentName, 2*time.Minute)
	})
	if gated {
		go openKVCacheReadinessGate(ctx, kube, namespace)
	}

	utils.WaitForDeploymentReady(t, ctx, kube, namespace, kvCacheFixtureDeploymentName, pluginMockReplicaCount, 5*time.Minute)
	pods := utils.ListReadyPodsByLabel(t, kube, namespace, "app="+kvCacheFixtureAppLabel)
	require.Len(t, pods, pluginMockReplicaCount, "KV-cache fixture needs %d mock pods", pluginMockReplicaCount)
	return pods
}

func waitForKVCacheOwnership(t *testing.T, pod corev1.Pod, redisNamespace, model, prompt string) {
	t.Helper()
	tokenizerPort := utils.AllocateLocalPort(t)
	tokenizerPF, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, tokenizerPort, "8000")
	require.NoError(t, err, "port-forward to tokenizer")
	defer tokenizerPF.Close()

	tokenizer, err := tokenization.NewRemoteTokenizer(tokenization.RemoteTokenizerConfig{
		Engine:   tokenization.EngineVLLM,
		Endpoint: "http://127.0.0.1:" + tokenizerPort,
		Model:    model,
	})
	require.NoError(t, err, "create tokenizer")
	extendedTokenizer, ok := tokenizer.(tokenization.ExtendedTokenizer)
	require.True(t, ok, "vLLM tokenizer must support chat templates")

	tokenizeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result, err := extendedTokenizer.TokenizeWithOptions(tokenizeCtx, tokenization.TokenizeInput{
		Type:                tokenization.ChatInput,
		Messages:            []common.Message{{Role: "user", Content: prompt}},
		AddGenerationPrompt: true,
	})
	cancel()
	require.NoError(t, err, "tokenize KV-cache prompt")
	require.GreaterOrEqual(t, len(result.Tokens), kvCacheBlockSize, "KV-cache prompt must contain a complete block")

	tokenBytes := make([]byte, kvCacheBlockSize*4)
	for i, tokenID := range result.Tokens[:kvCacheBlockSize] {
		binary.BigEndian.PutUint32(tokenBytes[i*4:], uint32(tokenID))
	}
	sum := sha256.Sum256(tokenBytes)
	expectedHash := fmt.Sprintf("%d", binary.BigEndian.Uint64(sum[:8])&0x7FFFFFFFFFFFFFFF)

	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForward(redisNamespace, "redis-server", localPort, "6379")
	require.NoError(t, err, "port-forward to Redis")
	defer pf.Close()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + localPort})
	defer client.Close()

	owner := pod.Name + "." + pod.Namespace
	mappingPattern := fmt.Sprintf("%s:%s@*", kvCacheMappingPrefix, owner)
	var observedMappings map[string]string
	var lastRedisErr error
	deadline := time.Now().Add(kvCacheOwnershipWaitTimeout)
	for time.Now().Before(deadline) {
		// Re-send while polling because ZMQ may drop an event before the subscriber is ready.
		utils.DirectChatToPod(t, pod, model, prompt, 1)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		observedMappings = make(map[string]string)
		iter := client.Scan(ctx, 0, mappingPattern, 0).Iterator()
		for iter.Next(ctx) {
			mappingKey := iter.Val()
			standardHash, err := client.Get(ctx, mappingKey).Result()
			if err != nil {
				lastRedisErr = err
				continue
			}
			observedMappings[mappingKey] = standardHash
			if standardHash != expectedHash {
				continue
			}

			ownershipKey := fmt.Sprintf("%s:%s@%s", kvCacheOwnershipPrefix, model, standardHash)
			exists, err := client.HExists(ctx, ownershipKey, owner).Result()
			if err == nil && exists {
				cancel()
				return
			} else if err != nil {
				lastRedisErr = err
			}
		}
		if err := iter.Err(); err != nil {
			lastRedisErr = err
		}
		cancel()
		time.Sleep(time.Second)
	}
	require.FailNowf(t, "KV-cache ownership not found",
		"owner=%s expected standardized hash=%s observed engine mappings=%v last Redis error=%v",
		owner, expectedHash, observedMappings, lastRedisErr)
}

// setKVCacheReadinessGate sets the readiness gate condition of a pod, making it Ready or NotReady.
func setKVCacheReadinessGate(ctx context.Context, kube kubernetes.Interface, pod corev1.Pod, ready bool) error {
	latest, err := kube.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	condition := corev1.PodCondition{Type: kvCacheReadinessGate, Status: status, LastTransitionTime: metav1.Now()}
	found := false
	for i := range latest.Status.Conditions {
		if latest.Status.Conditions[i].Type == kvCacheReadinessGate {
			latest.Status.Conditions[i] = condition
			found = true
		}
	}
	if !found {
		latest.Status.Conditions = append(latest.Status.Conditions, condition)
	}
	_, err = kube.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, latest, metav1.UpdateOptions{})
	return err
}

// openKVCacheReadinessGate marks every new fixture pod as gate-ready until the context ends.
func openKVCacheReadinessGate(ctx context.Context, kube kubernetes.Interface, namespace string) {
	for ctx.Err() == nil {
		pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + kvCacheFixtureAppLabel})
		if err == nil {
			for _, pod := range pods.Items {
				hasGate := false
				for _, c := range pod.Status.Conditions {
					if c.Type == kvCacheReadinessGate {
						hasGate = true
					}
				}
				if !hasGate && pod.DeletionTimestamp == nil {
					_ = setKVCacheReadinessGate(ctx, kube, pod, true)
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// countSelectedPodInRouterPodLogs counts access log lines of one router pod that selected the given pod.
func countSelectedPodInRouterPodLogs(t *testing.T, kube kubernetes.Interface, routerPod corev1.Pod, podName string, since metav1.Time) int {
	t.Helper()
	raw, err := kube.CoreV1().Pods(routerPod.Namespace).GetLogs(routerPod.Name, &corev1.PodLogOptions{SinceTime: &since}).Do(context.Background()).Raw()
	require.NoError(t, err)
	return strings.Count(string(raw), "selected_pod="+podName)
}

// runtimeLogsSince returns the kthena-runtime sidecar logs of a pod since the given time.
func runtimeLogsSince(t *testing.T, kube kubernetes.Interface, pod corev1.Pod, since metav1.Time) string {
	t.Helper()
	raw, err := kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: kvCacheRuntimeContainer,
		SinceTime: &since,
	}).Do(context.Background()).Raw()
	if err != nil {
		return ""
	}
	return string(raw)
}

// waitForRouterRegistration waits until every sidecar logs a stream registration from each router pod.
// Each stream starts with the sidecar's full block index, so a registered router has current state.
func waitForRouterRegistration(t *testing.T, kube kubernetes.Interface, pods []corev1.Pod, routerPods []corev1.Pod, since metav1.Time) {
	t.Helper()
	require.NotEmpty(t, routerPods)
	for _, pod := range pods {
		for _, routerPod := range routerPods {
			routerID := routerPod.Namespace + "/" + routerPod.Name
			require.Eventuallyf(t, func() bool {
				return strings.Contains(runtimeLogsSince(t, kube, pod, since), "Router registered: id="+routerID)
			}, kvCacheStreamWaitTimeout, time.Second,
				"sidecar of pod %s did not log a registration from router %s", pod.Name, routerID)
		}
	}
}

// warmKVCacheMemory sends the prompt directly to the pod and waits until its sidecar has streamed the
// resulting blocks to the registered routers. Observing the sidecar log avoids probing through the
// router, which would warm other pods on a miss.
func warmKVCacheMemory(t *testing.T, kube kubernetes.Interface, pod corev1.Pod, model, prompt string) {
	t.Helper()
	since := metav1.NewTime(time.Now())
	utils.DirectChatToPod(t, pod, model, prompt, 1)
	streamed := fmt.Sprintf("Runtime memory stream - Model: %s, Pod: %s.%s", model, pod.Name, pod.Namespace)
	require.Eventuallyf(t, func() bool {
		return strings.Contains(runtimeLogsSince(t, kube, pod, since), streamed)
	}, kvCacheStreamWaitTimeout, time.Second,
		"sidecar of pod %s did not stream KV blocks to routers", pod.Name)
}

// assertKVCacheHitRouting sends the warmed prompt through the router and expects most requests on the owning pod.
func assertKVCacheHitRouting(t *testing.T, kube kubernetes.Interface, kthenaNamespace, chatURL, model, prompt string, warmedPod corev1.Pod, pods []corev1.Pod) {
	t.Helper()
	hitSince := metav1.NewTime(time.Now())
	utils.SendRouterChatRequests(t, chatURL, model, prompt, kvCacheRoutingRequestCount)
	time.Sleep(2 * time.Second)

	warmedCount := utils.CountSelectedPodInRouterLogs(t, kube, kthenaNamespace, warmedPod.Name, hitSince)
	hitRouted := utils.CountSelectedPodsInRouterLogs(t, kube, kthenaNamespace, hitSince, pods)
	t.Logf("kvcache-aware hit: warmed pod %s selected %d/%d (of %d log lines)", warmedPod.Name, warmedCount, kvCacheRoutingRequestCount, hitRouted)
	require.GreaterOrEqual(t, hitRouted, kvCacheRoutingRequestCount/2, "expected access logs for cache-hit requests")
	require.GreaterOrEqual(t, float64(warmedCount)/float64(hitRouted), 0.9)
}

// assertKVCacheMissRouting sends unique prompts and expects no single pod to take most of them.
func assertKVCacheMissRouting(t *testing.T, kube kubernetes.Interface, kthenaNamespace, chatURL, model string, pods []corev1.Pod) {
	t.Helper()
	missSince := metav1.NewTime(time.Now())
	for i := 0; i < kvCacheRoutingRequestCount; i++ {
		// Keep each miss prompt unique so routed requests do not warm later probes.
		missPrompt := fmt.Sprintf("miss-%03d kvcache probe", i)
		utils.SendRouterChatRequests(t, chatURL, model, missPrompt, 1)
	}
	time.Sleep(2 * time.Second)

	maxMissCount := 0
	missRouted := 0
	for _, pod := range pods {
		selectedCount := utils.CountSelectedPodInRouterLogs(t, kube, kthenaNamespace, pod.Name, missSince)
		missRouted += selectedCount
		if selectedCount > maxMissCount {
			maxMissCount = selectedCount
		}
	}
	t.Logf("kvcache-aware miss: dominant pod selected %d/%d (of %d log lines)", maxMissCount, kvCacheRoutingRequestCount, missRouted)
	require.GreaterOrEqual(t, missRouted, kvCacheRoutingRequestCount/2, "expected access logs for cache-miss requests")
	require.Less(t, float64(maxMissCount)/float64(missRouted), 0.9)
}

func waitForSchedulerPluginInMetrics(t *testing.T, metricsURL, pluginName, pluginType string) {
	t.Helper()
	require.Eventually(t, func() bool {
		metricsData, err := backendmetrics.ParseMetricsURL(metricsURL)
		if err != nil {
			return false
		}
		return utils.GetHistogramCount(metricsData, "kthena_router_scheduler_plugin_duration_seconds", map[string]string{
			"plugin": pluginName,
			"type":   pluginType,
		}) > 0
	}, 30*time.Second, time.Second)
}

type routerPodMetricsSnapshot struct {
	RequestWaitingNum float64
	RequestRunningNum float64
	GPUCacheUsage     float64
	TPOT              float64
	TTFT              float64
}

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
			GPUCacheUsage     float64 `json:"gpuCacheUsage"`
			TPOT              float64 `json:"tpot"`
			TTFT              float64 `json:"ttft"`
		} `json:"metrics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || parsed.Metrics == nil {
		return routerPodMetricsSnapshot{}, false
	}
	return routerPodMetricsSnapshot{
		RequestWaitingNum: parsed.Metrics.RequestWaitingNum,
		RequestRunningNum: parsed.Metrics.RequestRunningNum,
		GPUCacheUsage:     parsed.Metrics.GPUCacheUsage,
		TPOT:              parsed.Metrics.TPOT,
		TTFT:              parsed.Metrics.TTFT,
	}, true
}

func waitForLeastLatencyMetricsSeparation(t *testing.T, kube kubernetes.Interface, kthenaNamespace string, fastPods, slowPods []corev1.Pod) {
	t.Helper()
	require.NotEmpty(t, fastPods)
	require.NotEmpty(t, slowPods)

	routerPod := utils.GetRouterPod(t, kube, kthenaNamespace)
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(routerPod.Namespace, routerPod.Name, localPort, utils.RouterDebugPort)
	require.NoError(t, err, "port-forward to router debug API")
	defer pf.Close()

	debugBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)
	require.Eventually(t, func() bool {
		maxFastTTFT := 0.0
		maxFastTPOT := 0.0
		for _, fastPod := range fastPods {
			fastMetrics, ok := fetchRouterPodMetricsViaDebug(t, debugBaseURL, fastPod)
			if !ok || fastMetrics.TTFT <= 0 || fastMetrics.TPOT <= 0 {
				return false
			}
			if fastMetrics.TTFT > maxFastTTFT {
				maxFastTTFT = fastMetrics.TTFT
			}
			if fastMetrics.TPOT > maxFastTPOT {
				maxFastTPOT = fastMetrics.TPOT
			}
		}

		allSlowMeasuredAndSlower := true
		for _, slowPod := range slowPods {
			slowMetrics, ok := fetchRouterPodMetricsViaDebug(t, debugBaseURL, slowPod)
			if !ok || slowMetrics.TTFT <= 0 || slowMetrics.TPOT <= 0 || slowMetrics.TTFT <= maxFastTTFT || slowMetrics.TPOT <= maxFastTPOT {
				allSlowMeasuredAndSlower = false
				break
			}
		}
		if allSlowMeasuredAndSlower {
			t.Logf("least-latency metrics ready: max fast ttft=%.4f tpot=%.4f", maxFastTTFT, maxFastTPOT)
		}
		return allSlowMeasuredAndSlower
	}, 45*time.Second, time.Second,
		"router should observe non-zero latency metrics where slow pods are slower than all fast pods")
}

func waitForRouterGPUCacheSeparation(t *testing.T, kube kubernetes.Interface, kthenaNamespace string, hotPods, coldPods []corev1.Pod) {
	t.Helper()
	require.NotEmpty(t, hotPods)
	require.NotEmpty(t, coldPods)

	routerPod := utils.GetRouterPod(t, kube, kthenaNamespace)
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(routerPod.Namespace, routerPod.Name, localPort, utils.RouterDebugPort)
	require.NoError(t, err, "port-forward to router debug API")
	defer pf.Close()

	debugBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)

	require.Eventually(t, func() bool {
		for _, pod := range hotPods {
			metrics, ok := fetchRouterPodMetricsViaDebug(t, debugBaseURL, pod)
			if !ok || metrics.GPUCacheUsage < gpuCacheUsageBusyMin {
				return false
			}
		}
		for _, pod := range coldPods {
			metrics, ok := fetchRouterPodMetricsViaDebug(t, debugBaseURL, pod)
			if !ok || metrics.GPUCacheUsage >= gpuCacheUsageIdleMax {
				return false
			}
		}
		return true
	}, gpuCacheUsageLoadWaitTimeout, 2*time.Second,
		"router should observe hot pods with gpu cache usage >= %.2f and cold pods with usage < %.2f",
		gpuCacheUsageBusyMin, gpuCacheUsageIdleMax)
}

func waitForLeastRequestLoadSeparation(t *testing.T, kube kubernetes.Interface, kthenaNamespace string, busyPods []corev1.Pod, idlePod corev1.Pod, maxWaitingRequests int) {
	t.Helper()
	require.NotEmpty(t, busyPods)
	require.Greater(t, maxWaitingRequests, 0)
	threshold := float64(maxWaitingRequests)

	routerPod := utils.GetRouterPod(t, kube, kthenaNamespace)
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(routerPod.Namespace, routerPod.Name, localPort, utils.RouterDebugPort)
	require.NoError(t, err, "port-forward to router debug API")
	defer pf.Close()

	debugBaseURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)
	require.Eventually(t, func() bool {
		allBusySaturated := true
		for _, busyPod := range busyPods {
			busy, okBusy := fetchRouterPodMetricsViaDebug(t, debugBaseURL, busyPod)
			if !okBusy || busy.RequestWaitingNum < threshold {
				allBusySaturated = false
				break
			}
		}
		idle, okIdle := fetchRouterPodMetricsViaDebug(t, debugBaseURL, idlePod)
		if !okIdle {
			return false
		}
		idleFree := idle.RequestWaitingNum < threshold
		if allBusySaturated && idleFree {
			for _, busyPod := range busyPods {
				busy, _ := fetchRouterPodMetricsViaDebug(t, debugBaseURL, busyPod)
				t.Logf("least-request load ready: busy %s waiting=%.0f running=%.0f",
					busyPod.Name, busy.RequestWaitingNum, busy.RequestRunningNum)
			}
			t.Logf("least-request load ready: idle %s waiting=%.0f running=%.0f",
				idlePod.Name, idle.RequestWaitingNum, idle.RequestRunningNum)
		}
		return allBusySaturated && idleFree
	}, leastRequestLoadWaitTimeout, 2*time.Second,
		"all busy pods should have request_waiting >= %d and idle pod %s should have request_waiting < %d",
		maxWaitingRequests, idlePod.Name, maxWaitingRequests)
}

const (
	schedulerKVCacheAwareMemory = `scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      indexMode: memory
      blockSizeToHash: 8
      maxBlocksToMatch: 128
  - name: least-request
    args:
      maxWaitingRequests: 10
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1
        - name: least-request
          weight: 1`

	schedulerKVCacheAware = `scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 8
      maxBlocksToMatch: 128
  - name: least-request
    args:
      maxWaitingRequests: 10
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1
        - name: least-request
          weight: 1`

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

	schedulerOnlyGPUCacheUsage = `scheduler:
  pluginConfig: []
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: gpu-usage
          weight: 1`
)

func schedulerKVCacheAndGPUUsage(kvCacheWeight, gpuUsageWeight int) string {
	return fmt.Sprintf(`scheduler:
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
          weight: %d
        - name: gpu-usage
          weight: %d`, kvCacheWeight, gpuUsageWeight)
}
