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

package utils

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
)

func TestGetMetricValueAggregatesMatchingDestinationSeries(t *testing.T) {
	tests := []struct {
		name       string
		metricName string
		metrics    []*dto.Metric
		getValue   func(map[string]*dto.MetricFamily, string, map[string]string) float64
		labels     map[string]string
		want       float64
	}{
		{
			name:       "Counter",
			metricName: "kthena_router_requests_total",
			metrics: []*dto.Metric{
				metricWithCounter(2, "model", "shared-model", "backend_type", "model_server", "backend_name", "default/ms"),
				metricWithCounter(3, "model", "shared-model", "backend_type", "external_provider", "backend_name", "default/provider"),
				metricWithCounter(7, "model", "other-model", "backend_type", "model_server", "backend_name", "default/other"),
			},
			getValue: GetCounterValue,
			labels:   map[string]string{"model": "shared-model"},
			want:     5,
		},
		{
			name:       "Histogram",
			metricName: "kthena_router_request_duration_seconds",
			metrics: []*dto.Metric{
				metricWithHistogram(4, "model", "shared-model", "backend_type", "model_server", "backend_name", "default/ms"),
				metricWithHistogram(6, "model", "shared-model", "backend_type", "inference_pool", "backend_name", "default/pool"),
				metricWithHistogram(9, "model", "other-model", "backend_type", "model_server", "backend_name", "default/other"),
			},
			getValue: func(metrics map[string]*dto.MetricFamily, metricName string, labels map[string]string) float64 {
				return float64(GetHistogramCount(metrics, metricName, labels))
			},
			labels: map[string]string{"model": "shared-model"},
			want:   10,
		},
		{
			name:       "Gauge",
			metricName: "kthena_router_active_upstream_requests",
			metrics: []*dto.Metric{
				metricWithGauge(1, "backend_type", "external_provider", "backend_name", "default/provider-a"),
				metricWithGauge(2, "backend_type", "external_provider", "backend_name", "default/provider-b"),
				metricWithGauge(8, "backend_type", "model_server", "backend_name", "default/model-server"),
			},
			getValue: GetGaugeValue,
			labels:   map[string]string{"backend_type": "external_provider"},
			want:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := map[string]*dto.MetricFamily{
				tt.metricName: {Metric: tt.metrics},
			}
			assert.Equal(t, tt.want, tt.getValue(metrics, tt.metricName, tt.labels))
		})
	}
}

func metricWithCounter(value float64, labels ...string) *dto.Metric {
	return &dto.Metric{
		Label:   metricLabels(labels...),
		Counter: &dto.Counter{Value: &value},
	}
}

func metricWithHistogram(count uint64, labels ...string) *dto.Metric {
	return &dto.Metric{
		Label:     metricLabels(labels...),
		Histogram: &dto.Histogram{SampleCount: &count},
	}
}

func metricWithGauge(value float64, labels ...string) *dto.Metric {
	return &dto.Metric{
		Label: metricLabels(labels...),
		Gauge: &dto.Gauge{Value: &value},
	}
}

func metricLabels(labels ...string) []*dto.LabelPair {
	pairs := make([]*dto.LabelPair, 0, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		name, value := labels[i], labels[i+1]
		pairs = append(pairs, &dto.LabelPair{Name: &name, Value: &value})
	}
	return pairs
}
