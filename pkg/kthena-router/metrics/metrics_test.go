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

func TestPrefixCacheHitMiss(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-prefix-hitmiss"

	hitsBefore := counterVal(t, &m.PrefixCacheHitsTotal, model)
	missBefore := counterVal(t, &m.PrefixCacheMissesTotal, model)

	m.RecordPrefixCacheHit(model)
	m.RecordPrefixCacheHit(model)
	m.RecordPrefixCacheMiss(model)

	if got := counterVal(t, &m.PrefixCacheHitsTotal, model) - hitsBefore; got != 2 {
		t.Errorf("prefix hits delta = %v, want 2", got)
	}
	if got := counterVal(t, &m.PrefixCacheMissesTotal, model) - missBefore; got != 1 {
		t.Errorf("prefix misses delta = %v, want 1", got)
	}
}

func TestPrefixCacheBlocksMatchedAndEviction(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-prefix-blocks"

	before := histCount(t, &m.PrefixCacheBlocksMatched, model)
	m.RecordPrefixCacheBlocksMatched(model, 5)
	if got := histCount(t, &m.PrefixCacheBlocksMatched, model) - before; got != 1 {
		t.Errorf("blocks_matched sample count delta = %d, want 1", got)
	}

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

func TestKVCacheHitMissAndError(t *testing.T) {
	m := DefaultMetrics
	const model = "metricstest-kv-hitmiss"

	hitsBefore := counterVal(t, &m.KVCacheHitsTotal, model)
	missBefore := counterVal(t, &m.KVCacheMissesTotal, model)
	redisErrBefore := counterVal(t, &m.KVCacheErrorsTotal, model, StageRedis)

	m.RecordKVCacheHit(model)
	m.RecordKVCacheMiss(model)
	m.RecordKVCacheError(model, StageRedis)

	if got := counterVal(t, &m.KVCacheHitsTotal, model) - hitsBefore; got != 1 {
		t.Errorf("kvcache hits delta = %v, want 1", got)
	}
	if got := counterVal(t, &m.KVCacheMissesTotal, model) - missBefore; got != 1 {
		t.Errorf("kvcache misses delta = %v, want 1", got)
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

	hitsBefore := counterVal(t, &m.PrefixCacheHitsTotal, model)
	kvHitsBefore := counterVal(t, &m.KVCacheHitsTotal, model)
	blocksBefore := histCount(t, &m.KVCacheBlocksMatched, model)

	r.RecordPrefixCacheHit()
	r.RecordKVCacheHit()
	r.RecordKVCacheBlocksMatched(9)

	if got := counterVal(t, &m.PrefixCacheHitsTotal, model) - hitsBefore; got != 1 {
		t.Errorf("recorder prefix hit delta = %v, want 1", got)
	}
	if got := counterVal(t, &m.KVCacheHitsTotal, model) - kvHitsBefore; got != 1 {
		t.Errorf("recorder kvcache hit delta = %v, want 1", got)
	}
	if got := histCount(t, &m.KVCacheBlocksMatched, model) - blocksBefore; got != 1 {
		t.Errorf("recorder kvcache blocks_matched delta = %d, want 1", got)
	}
}
