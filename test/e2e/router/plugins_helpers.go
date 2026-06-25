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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/vllm"
	routerutils "github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	plugincontext "github.com/volcano-sh/kthena/test/e2e/router/router-plugins/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
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

	kvCacheRedisWaitTimeout = 90 * time.Second
	kvCacheWarmupRequests   = 30
	kvCacheE2EMaxTokens     = 8
	redisServerAppLabel     = "app.kubernetes.io/component=redis-server"
	kvCacheMatrixKeyPrefix  = "matrix:kv:block:"
)

func listReadyMockPods(t *testing.T, kube kubernetes.Interface, namespace string) []corev1.Pod {
	t.Helper()
	ready := utils.ListReadyPodsByLabel(t, kube, namespace, "app="+plugincontext.DeploymentName)
	require.NotEmpty(t, ready, "no ready mock pods")
	return ready
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

type mockPodMetricsPortForward struct {
	pod     corev1.Pod
	metrics string
	close   func()
}

func setupMockPodMetricsPortForward(t *testing.T, pod corev1.Pod) mockPodMetricsPortForward {
	t.Helper()
	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err, "port-forward to mock pod %s for /metrics", pod.Name)
	return mockPodMetricsPortForward{
		pod:     pod,
		metrics: fmt.Sprintf("http://127.0.0.1:%s/metrics", localPort),
		close:   func() { pf.Close() },
	}
}

func fetchMockPodKVCacheUsage(metricsURL string) (float64, bool) {
	allMetrics, err := backendmetrics.ParseMetricsURL(metricsURL)
	if err != nil {
		return 0, false
	}
	countMetrics := vllm.NewVllmEngine().GetCountMetricsInfo(allMetrics)
	kvUsage, ok := countMetrics[routerutils.KVCacheUsage]
	return kvUsage, ok
}

func waitForMockPodKVCacheSeparation(t *testing.T, busyPods []corev1.Pod, idlePod corev1.Pod) {
	t.Helper()
	require.NotEmpty(t, busyPods)

	forwards := make([]mockPodMetricsPortForward, 0, len(busyPods)+1)
	for _, pod := range busyPods {
		forwards = append(forwards, setupMockPodMetricsPortForward(t, pod))
	}
	forwards = append(forwards, setupMockPodMetricsPortForward(t, idlePod))
	defer func() {
		for _, forward := range forwards {
			forward.close()
		}
	}()

	require.Eventually(t, func() bool {
		allBusyHot := true
		for _, forward := range forwards[:len(busyPods)] {
			kvUsage, ok := fetchMockPodKVCacheUsage(forward.metrics)
			if !ok || kvUsage < gpuCacheUsageBusyMin {
				allBusyHot = false
				break
			}
		}
		idleForward := forwards[len(forwards)-1]
		idleUsage, okIdle := fetchMockPodKVCacheUsage(idleForward.metrics)
		if !okIdle {
			return false
		}
		idleCool := idleUsage < gpuCacheUsageIdleMax
		if allBusyHot && idleCool {
			for _, forward := range forwards[:len(busyPods)] {
				kvUsage, _ := fetchMockPodKVCacheUsage(forward.metrics)
				t.Logf("gpu-usage load ready: busy %s kv_cache=%.3f (mock /metrics)", forward.pod.Name, kvUsage)
			}
			t.Logf("gpu-usage load ready: idle %s kv_cache=%.3f (mock /metrics)", idleForward.pod.Name, idleUsage)
		}
		return allBusyHot && idleCool
	}, gpuCacheUsageLoadWaitTimeout, 2*time.Second,
		"all busy pods should report kv_cache_usage_perc >= %.2f and idle pod %s should report < %.2f on mock /metrics",
		gpuCacheUsageBusyMin, idlePod.Name, gpuCacheUsageIdleMax)
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

	schedulerOnlyKVCacheAware = `scheduler:
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
)

func setupRedisClient(t *testing.T, kube kubernetes.Interface, namespace string) (*redis.Client, func()) {
	t.Helper()
	pods := utils.ListReadyPodsByLabel(t, kube, namespace, redisServerAppLabel)
	require.NotEmpty(t, pods, "no ready redis pods in namespace %s", namespace)

	localPort := utils.AllocateLocalPort(t)
	pf, err := utils.SetupPortForwardToPod(namespace, pods[0].Name, localPort, "6379")
	require.NoError(t, err, "port-forward to redis pod %s", pods[0].Name)

	addr := fmt.Sprintf("127.0.0.1:%s", localPort)
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err(), "redis ping via port-forward")

	return client, func() {
		_ = client.Close()
		pf.Close()
	}
}

func logMockPodContainerTail(t *testing.T, kube kubernetes.Interface, pod corev1.Pod, container string, tailLines int64) {
	t.Helper()
	raw, err := kube.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tailLines,
	}).Do(context.Background()).Raw()
	if err != nil {
		t.Logf("kvcache-aware: failed to read %s logs from pod %s: %v", container, pod.Name, err)
		return
	}
	t.Logf("kvcache-aware: pod %s container %s (tail %d lines):\n%s", pod.Name, container, tailLines, string(raw))
}

func waitForKVCachePodInRedis(t *testing.T, kube kubernetes.Interface, redisNamespace string, pod corev1.Pod, modelName string) {
	t.Helper()
	podIdentifier := fmt.Sprintf("%s.%s", pod.Name, pod.Namespace)
	keyPattern := fmt.Sprintf("%s%s@*", kvCacheMatrixKeyPrefix, modelName)

	deadline := time.Now().Add(kvCacheRedisWaitTimeout)
	poll := 0
	for time.Now().Before(deadline) {
		poll++
		client, closeRedis := setupRedisClient(t, kube, redisNamespace)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		keys, err := client.Keys(ctx, keyPattern).Result()
		cancel()
		closeRedis()

		if err != nil {
			if poll%5 == 0 {
				t.Logf("kvcache-aware: redis poll #%d keys lookup failed: %v", poll, err)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if len(keys) == 0 {
			if poll%5 == 0 {
				t.Logf("kvcache-aware: redis poll #%d no keys matching %q", poll, keyPattern)
			}
			time.Sleep(2 * time.Second)
			continue
		}

		for _, key := range keys {
			client, closeRedis := setupRedisClient(t, kube, redisNamespace)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			fields, err := client.HKeys(ctx, key).Result()
			cancel()
			closeRedis()
			if err != nil {
				continue
			}
			for _, field := range fields {
				if field == podIdentifier {
					t.Logf("kvcache-aware redis ready: key=%s pod=%s (poll #%d, %d keys)", key, podIdentifier, poll, len(keys))
					logMockPodContainerTail(t, kube, pod, "zmq-bridge", 30)
					logMockPodContainerTail(t, kube, pod, "runtime", 60)
					return
				}
			}
		}

		if poll%5 == 0 {
			t.Logf("kvcache-aware: redis poll #%d found %d keys but pod %s not listed yet", poll, len(keys), podIdentifier)
		}
		time.Sleep(2 * time.Second)
	}

	logMockPodContainerTail(t, kube, pod, "zmq-bridge", 60)
	logMockPodContainerTail(t, kube, pod, "runtime", 120)
	t.Fatalf("redis did not contain kv block mappings for pod %s model %q (pattern %q)", podIdentifier, modelName, keyPattern)
}
