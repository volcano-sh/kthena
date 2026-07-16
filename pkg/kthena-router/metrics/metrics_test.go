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

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func histCount(t *testing.T, vec *prometheus.HistogramVec, lvs ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func histSum(t *testing.T, vec *prometheus.HistogramVec, lvs ...string) float64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}

// histBucket returns the cumulative count of the bucket whose upper bound equals le.
func histBucket(t *testing.T, vec *prometheus.HistogramVec, le float64, lvs ...string) uint64 {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, b := range m.GetHistogram().GetBucket() {
		if b.GetUpperBound() == le {
			return b.GetCumulativeCount()
		}
	}
	t.Fatalf("bucket le=%v not found", le)
	return 0
}

func counterVal(t *testing.T, vec *prometheus.CounterVec, lvs ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func gaugeVal(t *testing.T, vec *prometheus.GaugeVec, lvs ...string) float64 {
	t.Helper()
	g, err := vec.GetMetricWithLabelValues(lvs...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestPrefixCacheMatchRatio(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-prefix-matchratio"

	countBefore := histCount(t, &m.PrefixCacheMatchRatio, model)
	sumBefore := histSum(t, &m.PrefixCacheMatchRatio, model)
	missBefore := histBucket(t, &m.PrefixCacheMatchRatio, 0, model)

	m.RecordPrefixCacheMatchRatio(model, 0.5)
	m.RecordPrefixCacheMatchRatio(model, 0) // miss

	if got := histCount(t, &m.PrefixCacheMatchRatio, model) - countBefore; got != 2 {
		t.Errorf("match_ratio sample count delta = %d, want 2", got)
	}
	if got := histSum(t, &m.PrefixCacheMatchRatio, model) - sumBefore; got != 0.5 {
		t.Errorf("match_ratio sample sum delta = %v, want 0.5", got)
	}
	// The le=0 bucket counts misses, so hit/miss is derivable from the histogram.
	if got := histBucket(t, &m.PrefixCacheMatchRatio, 0, model) - missBefore; got != 1 {
		t.Errorf("match_ratio le=0 (miss) delta = %d, want 1", got)
	}
}

func TestPrefixCacheEviction(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-prefix-eviction"

	evBefore := counterVal(t, &m.PrefixCacheEvictionsTotal, model)
	m.RecordPrefixCacheEviction(model)
	if got := counterVal(t, &m.PrefixCacheEvictionsTotal, model) - evBefore; got != 1 {
		t.Errorf("evictions delta = %v, want 1", got)
	}
}

func TestPrefixCacheEntriesProvider(t *testing.T) {
	m := DefaultMetrics
	m.SetPrefixCacheEntriesProvider(func() float64 { return 42 })

	dm := &dto.Metric{}
	if err := m.PrefixCacheEntries.Write(dm); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := dm.GetGauge().GetValue(); got != 42 {
		t.Errorf("entries gauge = %v, want 42", got)
	}
}

func TestKVCacheMatchRatioAndError(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-kv-matchratio"

	countBefore := histCount(t, &m.KVCacheMatchRatio, model)
	missBefore := histBucket(t, &m.KVCacheMatchRatio, 0, model)
	redisErrBefore := counterVal(t, &m.KVCacheErrorsTotal, model, StageRedis)

	m.RecordKVCacheMatchRatio(model, 1.0)
	m.RecordKVCacheMatchRatio(model, 0) // miss
	m.RecordKVCacheError(model, StageRedis)

	if got := histCount(t, &m.KVCacheMatchRatio, model) - countBefore; got != 2 {
		t.Errorf("kvcache match_ratio sample count delta = %d, want 2", got)
	}
	if got := histBucket(t, &m.KVCacheMatchRatio, 0, model) - missBefore; got != 1 {
		t.Errorf("kvcache match_ratio le=0 (miss) delta = %d, want 1", got)
	}
	if got := counterVal(t, &m.KVCacheErrorsTotal, model, StageRedis) - redisErrBefore; got != 1 {
		t.Errorf("kvcache redis errors delta = %v, want 1", got)
	}
}

func TestKVCacheDurations(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-kv-durations"

	redisBefore := histCount(t, &m.KVCacheRedisDuration, model)
	tokBefore := histCount(t, &m.KVCacheTokenizeDuration, model)

	m.RecordKVCacheRedisDuration(model, 3*time.Millisecond)
	m.RecordKVCacheTokenizeDuration(model, 7*time.Millisecond)

	if got := histCount(t, &m.KVCacheRedisDuration, model) - redisBefore; got != 1 {
		t.Errorf("redis duration sample count delta = %d, want 1", got)
	}
	if got := histCount(t, &m.KVCacheTokenizeDuration, model) - tokBefore; got != 1 {
		t.Errorf("tokenize duration sample count delta = %d, want 1", got)
	}
}

func TestRequestRecorderDelegation(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-recorder"
	r := NewRequestMetricsRecorder(m, model, "/v1/chat/completions")

	prefixBefore := histCount(t, &m.PrefixCacheMatchRatio, model)
	kvBefore := histCount(t, &m.KVCacheMatchRatio, model)

	r.RecordPrefixCacheMatchRatio(0.25)
	r.RecordKVCacheMatchRatio(0.75)

	if got := histCount(t, &m.PrefixCacheMatchRatio, model) - prefixBefore; got != 1 {
		t.Errorf("recorder prefix match_ratio delta = %d, want 1", got)
	}
	if got := histCount(t, &m.KVCacheMatchRatio, model) - kvBefore; got != 1 {
		t.Errorf("recorder kvcache match_ratio delta = %d, want 1", got)
	}
}

func TestRequestRecorderBindsDestinationLabels(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-recorder-destination"
	const path = "/v1/chat/completions"
	const modelRoute = "default/mr-external"
	const backendName = "default/openai-provider"
	const upstreamModel = "gpt-4o-mini"
	r := NewRequestMetricsRecorder(m, model, path)

	inputBefore := counterVal(t, &m.TokensTotal, model, path, TokenTypeInput, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	outputBefore := counterVal(t, &m.TokensTotal, model, path, TokenTypeOutput, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	requestBefore := counterVal(t, &m.RequestsTotal, model, path, "200", "successful_request", modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	durationBefore := histCount(t, &m.RequestDuration, model, path, "200", modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)

	r.RecordInputTokens(5)
	if got := counterVal(t, &m.TokensTotal, model, path, TokenTypeInput, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - inputBefore; got != 0 {
		t.Errorf("input token delta before destination bind = %v, want 0", got)
	}

	r.BindDestination(modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	r.RecordOutputTokens(3)
	r.Finish("200", "successful_request")

	if got := counterVal(t, &m.TokensTotal, model, path, TokenTypeInput, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - inputBefore; got != 5 {
		t.Errorf("input token delta after destination bind = %v, want 5", got)
	}
	if got := counterVal(t, &m.TokensTotal, model, path, TokenTypeOutput, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - outputBefore; got != 3 {
		t.Errorf("output token delta = %v, want 3", got)
	}
	if got := counterVal(t, &m.RequestsTotal, model, path, "200", "successful_request", modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - requestBefore; got != 1 {
		t.Errorf("request delta = %v, want 1", got)
	}
	if got := histCount(t, &m.RequestDuration, model, path, "200", modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - durationBefore; got != 1 {
		t.Errorf("request duration sample count delta = %d, want 1", got)
	}
}

func TestRequestRecorderFlushesInputTokensAsUnresolved(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-recorder-unresolved"
	const path = "/v1/chat/completions"
	r := NewRequestMetricsRecorder(m, model, path)

	before := counterVal(t, &m.TokensTotal, model, path, TokenTypeInput, DestinationLabelValueNone, BackendTypeUnresolved, DestinationLabelValueNone, DestinationLabelValueNone)
	r.RecordInputTokens(7)
	r.Finish("404", "model_server_matching")

	if got := counterVal(t, &m.TokensTotal, model, path, TokenTypeInput, DestinationLabelValueNone, BackendTypeUnresolved, DestinationLabelValueNone, DestinationLabelValueNone) - before; got != 7 {
		t.Errorf("unresolved input token delta = %v, want 7", got)
	}
}

func TestActiveUpstreamRequestsKeepModelServerCompatibilityLabel(t *testing.T) {
	m := DefaultMetrics
	const modelRoute = "default/mr-external"
	const backendName = "default/openai-provider"
	const upstreamModel = "gpt-4o-mini"

	before := gaugeVal(t, &m.ActiveUpstreamRequests, DestinationLabelValueNone, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	m.IncActiveUpstreamRequestsForDestination(modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	if got := gaugeVal(t, &m.ActiveUpstreamRequests, DestinationLabelValueNone, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - before; got != 1 {
		t.Errorf("external active upstream delta = %v, want 1", got)
	}
	m.DecActiveUpstreamRequestsForDestination(modelRoute, BackendTypeExternalProvider, backendName, upstreamModel)
	if got := gaugeVal(t, &m.ActiveUpstreamRequests, DestinationLabelValueNone, modelRoute, BackendTypeExternalProvider, backendName, upstreamModel) - before; got != 0 {
		t.Errorf("external active upstream delta after decrement = %v, want 0", got)
	}
}
