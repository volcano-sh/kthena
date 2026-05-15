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
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"

	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
)

func TestConfigureEngineRegistryUsesConfiguredPorts(t *testing.T) {
	ConfigureEngineRegistry(31000, 18000)
	t.Cleanup(func() {
		ConfigureEngineRegistry(0, 0)
	})

	var requestedURLs []string
	patch := gomonkey.ApplyFunc(backendmetrics.ParseMetricsURL, func(url string) (map[string]*dto.MetricFamily, error) {
		requestedURLs = append(requestedURLs, url)
		return map[string]*dto.MetricFamily{}, nil
	})
	defer patch.Reset()

	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}

	GetPodMetrics("SGLang", pod, nil)
	GetPodMetrics("vLLM", pod, nil)

	if len(requestedURLs) != 2 {
		t.Fatalf("expected 2 metrics requests, got %d", len(requestedURLs))
	}
	if requestedURLs[0] != "http://10.0.0.1:31000/metrics" {
		t.Fatalf("expected sglang metrics URL to use port 31000, got %s", requestedURLs[0])
	}
	if requestedURLs[1] != "http://10.0.0.1:18000/metrics" {
		t.Fatalf("expected vllm metrics URL to use port 18000, got %s", requestedURLs[1])
	}
}
