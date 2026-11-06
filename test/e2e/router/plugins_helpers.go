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
	"io"
	"net/http"
	"path/filepath"
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
	kvCacheFixtureName             = "router-plugin-kvcache-mock"
	kvCacheFixtureAppLabel         = kvCacheFixtureName
	kvCacheOwnershipWaitTimeout    = 90 * time.Second
	kvCacheMappingPrefix           = "vllm:kv:block"
	kvCacheOwnershipPrefix         = "matrix:kv:block"
	kvCacheBlockSize               = 8
	kvCacheRoutingRequestCount     = 200
	mixedPluginDominantWeight      = 101 // one more than either plugin's maximum score
)

type kvCacheOwnership struct {
	mappingKey   string
	ownershipKey string
	owner        string
}

func listReadyMockPods(t *testing.T, kube kubernetes.Interface, namespace string) []corev1.Pod {
	t.Helper()
	ready := utils.ListReadyPodsByLabel(t, kube, namespace, "app="+plugincontext.DeploymentName)
	require.NotEmpty(t, ready, "no ready mock pods")
	return ready
}

func setupKVCacheFixture(t *testing.T, kube kubernetes.Interface, namespace, redisNamespace string) []corev1.Pod {
	t.Helper()
	ctx := context.Background()
	statefulSet := utils.LoadYAMLFromFile[appsv1.StatefulSet](filepath.Join(plugincontext.TestDataDir, "LLM-Mock-plugins-kvcache.yaml"))
	statefulSet.Namespace = namespace
	redisHostSet := false
	for i := range statefulSet.Spec.Template.Spec.Containers {
		container := &statefulSet.Spec.Template.Spec.Containers[i]
		if container.Name != "kthena-runtime" {
			continue
		}
		for j := range container.Env {
			if container.Env[j].Name == "REDIS_HOST" {
				container.Env[j].Value = fmt.Sprintf("redis-server.%s.svc.cluster.local", redisNamespace)
				redisHostSet = true
			}
		}
	}
	require.True(t, redisHostSet, "KV-cache fixture Runtime needs REDIS_HOST")

	_, err := kube.AppsV1().StatefulSets(namespace).Create(ctx, statefulSet, metav1.CreateOptions{})
	require.NoError(t, err, "create KV-cache fixture")
	t.Cleanup(func() {
		foreground := metav1.DeletePropagationForeground
		_ = kube.AppsV1().StatefulSets(namespace).Delete(context.Background(), kvCacheFixtureName, metav1.DeleteOptions{PropagationPolicy: &foreground})
		require.Eventually(t, func() bool {
			pods, err := kube.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + kvCacheFixtureAppLabel})
			return err == nil && len(pods.Items) == 0
		}, 2*time.Minute, time.Second, "KV-cache fixture pods should be deleted")
	})

	require.Eventually(t, func() bool {
		current, err := kube.AppsV1().StatefulSets(namespace).Get(ctx, kvCacheFixtureName, metav1.GetOptions{})
		return err == nil && current.Status.ReadyReplicas == pluginMockReplicaCount
	}, 5*time.Minute, time.Second, "KV-cache fixture should have %d ready pods", pluginMockReplicaCount)
	pods := utils.ListReadyPodsByLabel(t, kube, namespace, "app="+kvCacheFixtureAppLabel)
	require.Len(t, pods, pluginMockReplicaCount, "KV-cache fixture needs %d mock pods", pluginMockReplicaCount)
	return pods
}

func setupKVCacheRedisClient(t *testing.T, redisNamespace string) *redis.Client {
	t.Helper()
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForward(redisNamespace, "redis-server", localPort, "6379")
	require.NoError(t, err, "port-forward to Redis")

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + localPort})
	t.Cleanup(func() {
		_ = client.Close()
		pf.Close()
	})
	return client
}

func kvCachePromptHashes(t *testing.T, pod corev1.Pod, model, prompt string) []string {
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

	hashes := make([]string, 0, len(result.Tokens)/kvCacheBlockSize)
	for start := 0; start+kvCacheBlockSize <= len(result.Tokens); start += kvCacheBlockSize {
		tokenBytes := make([]byte, kvCacheBlockSize*4)
		for i, tokenID := range result.Tokens[start : start+kvCacheBlockSize] {
			binary.BigEndian.PutUint32(tokenBytes[i*4:], uint32(tokenID))
		}
		sum := sha256.Sum256(tokenBytes)
		hashes = append(hashes, fmt.Sprintf("%d", binary.BigEndian.Uint64(sum[:8])&0x7FFFFFFFFFFFFFFF))
	}
	return hashes
}

func waitForKVCacheOwnership(t *testing.T, pod corev1.Pod, client *redis.Client, model, prompt string) kvCacheOwnership {
	t.Helper()
	return waitForKVCacheHashOwnership(t, pod, client, model, prompt, kvCachePromptHashes(t, pod, model, prompt)[0])
}

func waitForKVCacheHashOwnership(t *testing.T, pod corev1.Pod, client *redis.Client, model, prompt, expectedHash string) kvCacheOwnership {
	t.Helper()
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
				return kvCacheOwnership{mappingKey: mappingKey, ownershipKey: ownershipKey, owner: owner}
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
	return kvCacheOwnership{}
}

func waitForKVCacheOwnershipRemoved(t *testing.T, client *redis.Client, ownership kvCacheOwnership) {
	t.Helper()
	var mappingExists int64
	var ownerExists bool
	var lastRedisErr error
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		mappingExists, lastRedisErr = client.Exists(ctx, ownership.mappingKey).Result()
		if lastRedisErr != nil {
			return false
		}
		ownerExists, lastRedisErr = client.HExists(ctx, ownership.ownershipKey, ownership.owner).Result()
		return lastRedisErr == nil && mappingExists == 0 && !ownerExists
	}, kvCacheOwnershipWaitTimeout, time.Second,
		"KV-cache ownership was not removed: mapping=%s exists=%d ownership=%s owner=%s exists=%t last Redis error=%v",
		ownership.mappingKey, mappingExists, ownership.ownershipKey, ownership.owner, ownerExists, lastRedisErr)
}

func scanKVCacheKeys(ctx context.Context, client *redis.Client, pattern string) ([]string, error) {
	var keys []string
	iter := client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

func waitForAllKVCacheOwnershipRemoved(t *testing.T, client *redis.Client, pod corev1.Pod, model string) {
	t.Helper()
	owner := pod.Name + "." + pod.Namespace
	mappingPattern := fmt.Sprintf("%s:%s@*", kvCacheMappingPrefix, owner)
	ownershipPattern := fmt.Sprintf("%s:%s@*", kvCacheOwnershipPrefix, model)
	var mappingKeys, ownedKeys []string
	var lastRedisErr error
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		mappingKeys, lastRedisErr = scanKVCacheKeys(ctx, client, mappingPattern)
		if lastRedisErr != nil {
			return false
		}
		ownedKeys = ownedKeys[:0]
		keys, err := scanKVCacheKeys(ctx, client, ownershipPattern)
		if err != nil {
			lastRedisErr = err
			return false
		}
		for _, key := range keys {
			exists, err := client.HExists(ctx, key, owner).Result()
			if err != nil {
				lastRedisErr = err
				return false
			}
			if exists {
				ownedKeys = append(ownedKeys, key)
			}
		}
		return len(mappingKeys) == 0 && len(ownedKeys) == 0
	}, kvCacheOwnershipWaitTimeout, time.Second,
		"KV-cache state was not cleared: owner=%s mappings=%v ownership=%v last Redis error=%v",
		owner, mappingKeys, ownedKeys, lastRedisErr)
}

func postKVCacheLifecycle(t *testing.T, pod corev1.Pod, path string) {
	t.Helper()
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err, "port-forward to KV-cache simulator")
	defer pf.Close()

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%s%s", localPort, path), nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
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
