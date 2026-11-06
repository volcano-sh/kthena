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

package triton

import (
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/vllm"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// Default OpenAI-compatible frontend port used by Triton's python/openai server.
const DefaultOpenAIPort uint32 = 9000

// Metrics scraped from Triton's OpenAI frontend /metrics. When the model uses
// the vLLM backend, vllm:* series may also appear and are preferred.
var (
	PendingRequestCount = "nv_inference_pending_request_count"

	VLLMKVCacheUsage      = "vllm:kv_cache_usage_perc"
	VLLMRequestWaitingNum = "vllm:num_requests_waiting"
	VLLMRequestRunningNum = "vllm:num_requests_running"
	VLLMITL               = "vllm:inter_token_latency_seconds"
	VLLMTTFT              = "vllm:time_to_first_token_seconds"
)

type tritonEngine struct{}

func NewTritonEngine() *tritonEngine {
	return &tritonEngine{}
}

func (engine *tritonEngine) GetPodMetrics(pod *corev1.Pod, port uint32) (map[string]*dto.MetricFamily, error) {
	if port == 0 {
		port = DefaultOpenAIPort
	}
	url := metrics.PodEndpointURL(pod.Status.PodIP, port, "/metrics")
	return metrics.ParseMetricsURL(url)
}

func (engine *tritonEngine) GetCountMetricsInfo(allMetrics map[string]*dto.MetricFamily) map[string]float64 {
	wantMetrics := make(map[string]float64)

	setGauge(wantMetrics, allMetrics, VLLMKVCacheUsage, utils.KVCacheUsage)
	setGauge(wantMetrics, allMetrics, VLLMRequestWaitingNum, utils.RequestWaitingNum)
	setGauge(wantMetrics, allMetrics, VLLMRequestRunningNum, utils.RequestRunningNum)

	if _, ok := wantMetrics[utils.RequestWaitingNum]; !ok {
		setGauge(wantMetrics, allMetrics, PendingRequestCount, utils.RequestWaitingNum)
	}

	return wantMetrics
}

func (engine *tritonEngine) GetHistogramPodMetrics(allMetrics map[string]*dto.MetricFamily, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	wantMetrics := make(map[string]float64)
	histogramMetrics := make(map[string]*dto.Histogram)

	for metricName, canonical := range map[string]string{
		VLLMITL:  utils.TPOT,
		VLLMTTFT: utils.TTFT,
	} {
		metricInfo, exist := allMetrics[metricName]
		if !exist {
			continue
		}
		for _, metric := range metricInfo.Metric {
			metricValue := metric.GetHistogram()
			histogramMetrics[canonical] = metricValue
			previousMetric := previousHistogram[canonical]
			if previousMetric == nil {
				wantMetrics[canonical] = float64(0.0)
			} else {
				wantMetrics[canonical] = metrics.LastPeriodAvg(previousMetric, metricValue)
			}
		}
	}

	return wantMetrics, histogramMetrics
}

func (engine *tritonEngine) GetPodModels(pod *corev1.Pod, port uint32) ([]string, error) {
	if port == 0 {
		port = DefaultOpenAIPort
	}
	return vllm.FetchPodModels(pod.Status.PodIP, port)
}

func setGauge(dst map[string]float64, all map[string]*dto.MetricFamily, srcName, canonical string) {
	metricInfo, exist := all[srcName]
	if !exist {
		return
	}
	for _, metric := range metricInfo.Metric {
		dst[canonical] = metric.GetGauge().GetValue()
		return
	}
}
