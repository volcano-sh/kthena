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
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// gaugeMetricFamily creates a mock MetricFamily with a single Gauge metric.
func gaugeMetricFamily(value float64) *dto.MetricFamily {
	return &dto.MetricFamily{
		Metric: []*dto.Metric{
			{Gauge: &dto.Gauge{Value: &value}},
		},
	}
}

// histogramMetricFamily creates a mock MetricFamily with a single Histogram metric.
func histogramMetricFamily(sum float64, count uint64) *dto.MetricFamily {
	return &dto.MetricFamily{
		Metric: []*dto.Metric{
			{Histogram: &dto.Histogram{SampleSum: &sum, SampleCount: &count}},
		},
	}
}

// makePreviousHistogram builds a *dto.Histogram for use as prior scrape data.
func makePreviousHistogram(sum float64, count uint64) *dto.Histogram {
	return &dto.Histogram{SampleSum: &sum, SampleCount: &count}
}

func TestGetCountMetricsInfo(t *testing.T) {
	engine := NewVllmEngine()

	tests := []struct {
		name       string
		allMetrics map[string]*dto.MetricFamily
		want       map[string]float64
	}{
		{
			name: "all known gauge metrics present",
			allMetrics: map[string]*dto.MetricFamily{
				KVCacheUsage:      gaugeMetricFamily(0.75),
				RequestWaitingNum: gaugeMetricFamily(3.0),
				RequestRunningNum: gaugeMetricFamily(5.0),
			},
			want: map[string]float64{
				utils.KVCacheUsage:      0.75,
				utils.RequestWaitingNum: 3.0,
				utils.RequestRunningNum: 5.0,
			},
		},
		{
			name:       "empty input returns empty map",
			allMetrics: map[string]*dto.MetricFamily{},
			want:       map[string]float64{},
		},
		{
			name: "unknown metrics are ignored",
			allMetrics: map[string]*dto.MetricFamily{
				"some:unknown:metric": gaugeMetricFamily(99.0),
			},
			want: map[string]float64{},
		},
		{
			name: "partial metrics — only kv cache present",
			allMetrics: map[string]*dto.MetricFamily{
				KVCacheUsage: gaugeMetricFamily(0.5),
			},
			want: map[string]float64{
				utils.KVCacheUsage: 0.5,
			},
		},
		{
			name: "zero values stored correctly",
			allMetrics: map[string]*dto.MetricFamily{
				KVCacheUsage:      gaugeMetricFamily(0.0),
				RequestWaitingNum: gaugeMetricFamily(0.0),
				RequestRunningNum: gaugeMetricFamily(0.0),
			},
			want: map[string]float64{
				utils.KVCacheUsage:      0.0,
				utils.RequestWaitingNum: 0.0,
				utils.RequestRunningNum: 0.0,
			},
		},
		{
			name: "vllm-internal name is mapped to utils constant",
			allMetrics: map[string]*dto.MetricFamily{
				RequestRunningNum: gaugeMetricFamily(7.0),
			},
			want: map[string]float64{
				utils.RequestRunningNum: 7.0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := engine.GetCountMetricsInfo(tt.allMetrics)
			require.Len(t, got, len(tt.want))
			for k, wantVal := range tt.want {
				gotVal, ok := got[k]
				require.True(t, ok, "missing key %q in result", k)
				assert.InDelta(t, wantVal, gotVal, 1e-9)
			}
		})
	}
}

func TestGetHistogramPodMetrics(t *testing.T) {
	engine := NewVllmEngine()

	tests := []struct {
		name              string
		allMetrics        map[string]*dto.MetricFamily
		previousHistogram map[string]*dto.Histogram
		wantMetrics       map[string]float64
		wantHistogramKeys []string
	}{
		{
			name: "no previous histogram returns zero latency for each metric",
			allMetrics: map[string]*dto.MetricFamily{
				ITL:  histogramMetricFamily(10.0, 5),
				TTFT: histogramMetricFamily(20.0, 8),
			},
			previousHistogram: map[string]*dto.Histogram{},
			wantMetrics: map[string]float64{
				utils.TPOT: 0.0,
				utils.TTFT: 0.0,
			},
			wantHistogramKeys: []string{utils.TPOT, utils.TTFT},
		},
		{
			name: "with previous histogram computes correct delta average",
			allMetrics: map[string]*dto.MetricFamily{
				// current: sum=20, count=10; previous: sum=10, count=5 -> delta avg = 10/5 = 2.0
				ITL: histogramMetricFamily(20.0, 10),
			},
			previousHistogram: map[string]*dto.Histogram{
				utils.TPOT: makePreviousHistogram(10.0, 5),
			},
			wantMetrics: map[string]float64{
				utils.TPOT: 2.0,
			},
			wantHistogramKeys: []string{utils.TPOT},
		},
		{
			name: "zero delta count returns zero to avoid division by zero",
			allMetrics: map[string]*dto.MetricFamily{
				// count unchanged between scrapes -> deltaCount=0 -> avg must be 0
				TTFT: histogramMetricFamily(15.0, 5),
			},
			previousHistogram: map[string]*dto.Histogram{
				utils.TTFT: makePreviousHistogram(10.0, 5),
			},
			wantMetrics: map[string]float64{
				utils.TTFT: 0.0,
			},
			wantHistogramKeys: []string{utils.TTFT},
		},
		{
			name:              "empty input returns empty maps",
			allMetrics:        map[string]*dto.MetricFamily{},
			previousHistogram: map[string]*dto.Histogram{},
			wantMetrics:       map[string]float64{},
			wantHistogramKeys: []string{},
		},
		{
			name: "unknown metrics are ignored",
			allMetrics: map[string]*dto.MetricFamily{
				"vllm:unknown_histogram": histogramMetricFamily(5.0, 2),
			},
			previousHistogram: map[string]*dto.Histogram{},
			wantMetrics:       map[string]float64{},
			wantHistogramKeys: []string{},
		},
		{
			name: "stored histogram matches current scrape values",
			allMetrics: map[string]*dto.MetricFamily{
				TTFT: histogramMetricFamily(30.0, 15),
			},
			previousHistogram: map[string]*dto.Histogram{},
			wantMetrics: map[string]float64{
				utils.TTFT: 0.0,
			},
			wantHistogramKeys: []string{utils.TTFT},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMetrics, gotHistograms := engine.GetHistogramPodMetrics(tt.allMetrics, tt.previousHistogram)

			require.Len(t, gotMetrics, len(tt.wantMetrics))
			for k, wantVal := range tt.wantMetrics {
				gotVal, ok := gotMetrics[k]
				require.True(t, ok, "missing metric key %q", k)
				assert.InDelta(t, wantVal, gotVal, 1e-9)
			}

			require.Len(t, gotHistograms, len(tt.wantHistogramKeys))
			for _, k := range tt.wantHistogramKeys {
				h, ok := gotHistograms[k]
				require.True(t, ok, "missing histogram key %q", k)
				require.NotNil(t, h, "histogram %q must not be nil", k)
			}
		})
	}
}

func TestGetHistogramPodMetrics_StoredHistogramContent(t *testing.T) {
	engine := NewVllmEngine()
	allMetrics := map[string]*dto.MetricFamily{
		TTFT: histogramMetricFamily(30.0, 15),
	}

	_, gotHistograms := engine.GetHistogramPodMetrics(allMetrics, map[string]*dto.Histogram{})

	stored, ok := gotHistograms[utils.TTFT]
	require.True(t, ok)
	require.NotNil(t, stored)
	assert.InDelta(t, 30.0, stored.GetSampleSum(), 1e-9)
	assert.Equal(t, uint64(15), stored.GetSampleCount())
}
