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

package vllm

import (
	"fmt"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

var (
	GPUCacheUsage     = "vllm:gpu_cache_usage_perc"
	RequestWaitingNum = "vllm:num_requests_waiting"
	RequestRunningNum = "vllm:num_requests_running"
	TPOT              = "vllm:time_per_output_token_seconds"
	TTFT              = "vllm:time_to_first_token_seconds"
)

const defaultMetricPort uint32 = 8000

var (
	CounterAndGaugeMetrics = []string{
		GPUCacheUsage,
		RequestWaitingNum,
		RequestRunningNum,
	}

	HistogramMetrics = []string{
		TPOT,
		TTFT,
	}

	mapOfMetricsName = map[string]string{
		GPUCacheUsage:     utils.GPUCacheUsage,
		RequestWaitingNum: utils.RequestWaitingNum,
		RequestRunningNum: utils.RequestRunningNum,
		TPOT:              utils.TPOT,
		TTFT:              utils.TTFT,
	}
)

type vllmEngine struct {
	// The address of vllm's query metrics is http://{model server}:MetricPort/metrics
	// Default is 8000
	MetricPort uint32
}

func NewVllmEngine(metricPort ...uint32) *vllmEngine {
	port := defaultMetricPort
	if len(metricPort) > 0 && metricPort[0] > 0 {
		port = metricPort[0]
	}

	return &vllmEngine{
		MetricPort: port,
	}
}

func (engine *vllmEngine) GetPodMetrics(pod *corev1.Pod) (map[string]*dto.MetricFamily, error) {
	url := fmt.Sprintf("http://%s:%d/metrics", pod.Status.PodIP, engine.MetricPort)
	allMetrics, err := metrics.ParseMetricsURL(url)
	if err != nil {
		return nil, err
	}

	return allMetrics, nil
}

func (engine *vllmEngine) GetCountMetricsInfo(allMetrics map[string]*dto.MetricFamily) map[string]float64 {
	wantMetrics := make(map[string]float64)
	for _, metricName := range CounterAndGaugeMetrics {
		metricInfo, exist := allMetrics[metricName]
		if !exist {
			continue
		}
		for _, metric := range metricInfo.Metric {
			metricValue := metric.GetGauge().GetValue()
			wantMetrics[mapOfMetricsName[metricName]] = metricValue
		}
	}

	return wantMetrics
}

func (engine *vllmEngine) GetHistogramPodMetrics(allMetrics map[string]*dto.MetricFamily, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	wantMetrics := make(map[string]float64)
	histogramMetrics := make(map[string]*dto.Histogram)
	for _, metricName := range HistogramMetrics {
		metricInfo, exist := allMetrics[metricName]
		if !exist {
			continue
		}
		for _, metric := range metricInfo.Metric {
			metricValue := metric.GetHistogram()
			histogramMetrics[mapOfMetricsName[metricName]] = metricValue
			previousMetric := previousHistogram[mapOfMetricsName[metricName]]
			if previousMetric == nil {
				// Ignore the effects of history and give each pod a fair chance at the initial.
				wantMetrics[mapOfMetricsName[metricName]] = float64(0.0)
			} else {
				wantMetrics[mapOfMetricsName[metricName]] = metrics.LastPeriodAvg(previousMetric, metricValue)
			}
		}
	}

	return wantMetrics, histogramMetrics
}
