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

package backend

import (
	"fmt"
	"sync"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/sglang"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/vllm"
)

type MetricsProvider interface {
	GetPodMetrics(pod *corev1.Pod) (map[string]*dto.MetricFamily, error)
	GetPodModels(pod *corev1.Pod) ([]string, error)
	GetCountMetricsInfo(allMetrics map[string]*dto.MetricFamily) map[string]float64
	GetHistogramPodMetrics(allMetrics map[string]*dto.MetricFamily, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram)
}

var (
	engineRegistryMu sync.RWMutex
	engineRegistries = buildEngineRegistries(0, 0)
)

func buildEngineRegistries(sglangMetricPort, vllmMetricPort uint32) map[string]MetricsProvider {
	return map[string]MetricsProvider{
		"SGLang": sglang.NewSglangEngine(sglangMetricPort),
		"vLLM":   vllm.NewVllmEngine(vllmMetricPort),
	}
}

// ConfigureEngineRegistry rebuilds engine providers with configured ports.
// Zero-valued or invalid ports are handled by each engine constructor fallback logic.
func ConfigureEngineRegistry(sglangMetricPort, vllmMetricPort uint32) {
	engineRegistryMu.Lock()
	defer engineRegistryMu.Unlock()
	engineRegistries = buildEngineRegistries(sglangMetricPort, vllmMetricPort)
}

func GetPodMetrics(engine string, pod *corev1.Pod, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	provider, err := GetMetricsProvider(engine)
	if err != nil {
		klog.Errorf("Failed to get inference engine: %v", err)
		return nil, nil
	}

	allMetrics, err := provider.GetPodMetrics(pod)
	if err != nil {
		klog.V(4).Infof("failed to get metrics of pod: %s/%s: %v", pod.GetNamespace(), pod.GetName(), err)
		return nil, nil
	}

	countMetricsInfo := provider.GetCountMetricsInfo(allMetrics)
	histogramMetricsInfo, histogramMetrics := provider.GetHistogramPodMetrics(allMetrics, previousHistogram)

	for name, value := range histogramMetricsInfo {
		// Since the key in countMetricInfo must not be the same as the key in histogramMetricsInfo.
		// You don't have to worry about overriding the value
		countMetricsInfo[name] = value
	}

	return countMetricsInfo, histogramMetrics
}

func GetMetricsProvider(engine string) (MetricsProvider, error) {
	engineRegistryMu.RLock()
	defer engineRegistryMu.RUnlock()
	if provider, exists := engineRegistries[engine]; exists {
		return provider, nil
	}
	return nil, fmt.Errorf("unsupported engine: %s", engine)
}

func GetPodModels(engine string, pod *corev1.Pod) ([]string, error) {
	provider, err := GetMetricsProvider(engine)
	if err != nil {
		klog.Errorf("Failed to get inference engine: %v", err)
		return nil, nil
	}

	return provider.GetPodModels(pod)
}
