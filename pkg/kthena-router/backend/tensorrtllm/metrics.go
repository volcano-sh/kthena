/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tensorrtllm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/vllm"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// Synthetic gauge names used after converting trtllm-serve JSON /metrics
// into MetricFamily values for the shared MetricsProvider interface.
var (
	KVCacheUsage      = "trtllm:kv_cache_usage_perc"
	RequestWaitingNum = "trtllm:num_requests_waiting"
	RequestRunningNum = "trtllm:num_requests_running"
)

var (
	CounterAndGaugeMetrics = []string{
		KVCacheUsage,
		RequestWaitingNum,
		RequestRunningNum,
	}

	mapOfMetricsName = map[string]string{
		KVCacheUsage:      utils.KVCacheUsage,
		RequestWaitingNum: utils.RequestWaitingNum,
		RequestRunningNum: utils.RequestRunningNum,
	}
)

// iterStats mirrors the JSON objects returned by trtllm-serve GET /metrics.
// Fields are optional because the payload evolves across releases.
type iterStats struct {
	NumActiveRequests *float64      `json:"numActiveRequests"`
	NumQueuedRequests *float64      `json:"numQueuedRequests"`
	KVCacheStats      *kvCacheStats `json:"kvCacheStats"`
}

type kvCacheStats struct {
	FreeNumBlocks *float64 `json:"freeNumBlocks"`
	MaxNumBlocks  *float64 `json:"maxNumBlocks"`
	UsedNumBlocks *float64 `json:"usedNumBlocks"`
}

type tensorrtllmEngine struct{}

func NewTensorRTLLMEngine() *tensorrtllmEngine {
	return &tensorrtllmEngine{}
}

func (engine *tensorrtllmEngine) GetPodMetrics(pod *corev1.Pod, port uint32) (map[string]*dto.MetricFamily, error) {
	if port == 0 {
		port = 8000
	}
	url := metrics.PodEndpointURL(pod.Status.PodIP, port, "/metrics")
	return fetchJSONMetrics(url)
}

func fetchJSONMetrics(url string) (map[string]*dto.MetricFamily, error) {
	resp, err := metrics.HTTPClient().Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch metrics from %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch metrics from %s: HTTP %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseIterStatsJSON(body)
}

// parseIterStatsJSON converts a trtllm-serve /metrics JSON array into gauge
// MetricFamilies. An empty array (no new iterations since last poll) yields
// an empty map so the router can keep prior scores.
func parseIterStatsJSON(body []byte) (map[string]*dto.MetricFamily, error) {
	var samples []iterStats
	if err := json.Unmarshal(body, &samples); err != nil {
		return nil, fmt.Errorf("error parsing tensorrt-llm metrics JSON: %v", err)
	}
	if len(samples) == 0 {
		return map[string]*dto.MetricFamily{}, nil
	}

	// /metrics drains a queue; use the newest sample for current load.
	latest := samples[len(samples)-1]
	out := make(map[string]*dto.MetricFamily)

	if latest.NumActiveRequests != nil {
		out[RequestRunningNum] = gaugeFamily(*latest.NumActiveRequests)
	}
	if latest.NumQueuedRequests != nil {
		out[RequestWaitingNum] = gaugeFamily(*latest.NumQueuedRequests)
	}
	if usage, ok := kvUsageFraction(latest.KVCacheStats); ok {
		out[KVCacheUsage] = gaugeFamily(usage)
	}

	return out, nil
}

func kvUsageFraction(stats *kvCacheStats) (float64, bool) {
	if stats == nil || stats.MaxNumBlocks == nil || *stats.MaxNumBlocks <= 0 {
		return 0, false
	}
	used := 0.0
	if stats.UsedNumBlocks != nil {
		used = *stats.UsedNumBlocks
	} else if stats.FreeNumBlocks != nil {
		used = *stats.MaxNumBlocks - *stats.FreeNumBlocks
	} else {
		return 0, false
	}
	return used / *stats.MaxNumBlocks, true
}

func gaugeFamily(value float64) *dto.MetricFamily {
	v := value
	return &dto.MetricFamily{
		Metric: []*dto.Metric{
			{Gauge: &dto.Gauge{Value: &v}},
		},
	}
}

func (engine *tensorrtllmEngine) GetCountMetricsInfo(allMetrics map[string]*dto.MetricFamily) map[string]float64 {
	wantMetrics := make(map[string]float64)
	for _, metricName := range CounterAndGaugeMetrics {
		metricInfo, exist := allMetrics[metricName]
		if !exist {
			continue
		}
		for _, metric := range metricInfo.Metric {
			wantMetrics[mapOfMetricsName[metricName]] = metric.GetGauge().GetValue()
		}
	}
	return wantMetrics
}

func (engine *tensorrtllmEngine) GetHistogramPodMetrics(
	_ map[string]*dto.MetricFamily,
	_ map[string]*dto.Histogram,
) (map[string]float64, map[string]*dto.Histogram) {
	// trtllm-serve iteration JSON does not expose TTFT/TPOT histograms.
	return map[string]float64{}, map[string]*dto.Histogram{}
}

func (engine *tensorrtllmEngine) GetPodModels(pod *corev1.Pod, port uint32) ([]string, error) {
	if port == 0 {
		port = 8000
	}
	return vllm.FetchPodModels(pod.Status.PodIP, port)
}
