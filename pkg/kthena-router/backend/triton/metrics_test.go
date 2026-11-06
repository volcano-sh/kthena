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
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"

	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

func gaugeMetricFamily(value float64) *dto.MetricFamily {
	return &dto.MetricFamily{
		Metric: []*dto.Metric{
			{Gauge: &dto.Gauge{Value: &value}},
		},
	}
}

func TestGetCountMetricsInfoPrefersVLLMBackend(t *testing.T) {
	engine := NewTritonEngine()
	got := engine.GetCountMetricsInfo(map[string]*dto.MetricFamily{
		VLLMKVCacheUsage:      gaugeMetricFamily(0.4),
		VLLMRequestWaitingNum: gaugeMetricFamily(2),
		VLLMRequestRunningNum: gaugeMetricFamily(3),
		PendingRequestCount:   gaugeMetricFamily(99),
	})
	assert.Equal(t, map[string]float64{
		utils.KVCacheUsage:      0.4,
		utils.RequestWaitingNum: 2,
		utils.RequestRunningNum: 3,
	}, got)
}

func TestGetCountMetricsInfoTritonNativeFallback(t *testing.T) {
	engine := NewTritonEngine()
	got := engine.GetCountMetricsInfo(map[string]*dto.MetricFamily{
		PendingRequestCount: gaugeMetricFamily(7),
	})
	assert.Equal(t, map[string]float64{
		utils.RequestWaitingNum: 7,
	}, got)
}

func TestGetCountMetricsInfoEmpty(t *testing.T) {
	engine := NewTritonEngine()
	got := engine.GetCountMetricsInfo(map[string]*dto.MetricFamily{})
	assert.Empty(t, got)
}
