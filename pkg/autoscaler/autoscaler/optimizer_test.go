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

package autoscaler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func makeBinding(uid string, namespace string, costExpansionRate int32, params []workload.HeterogeneousTargetParam) *workload.AutoscalingPolicyBinding {
	return &workload.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Namespace: namespace,
		},
		Spec: workload.AutoscalingPolicyBindingSpec{
			HeterogeneousTarget: &workload.HeterogeneousTarget{
				CostExpansionRatePercent: costExpansionRate,
				Params:                   params,
			},
		},
	}
}

func makeParam(name string, cost, minReplicas, maxReplicas int32) workload.HeterogeneousTargetParam {
	return workload.HeterogeneousTargetParam{
		Target: workload.Target{
			TargetRef: corev1.ObjectReference{Name: name},
		},
		Cost:        cost,
		MinReplicas: minReplicas,
		MaxReplicas: maxReplicas,
	}
}

func TestNewOptimizerMeta(t *testing.T) {
	t.Run("nil when HeterogeneousTarget unset", func(t *testing.T) {
		binding := &workload.AutoscalingPolicyBinding{
			Spec: workload.AutoscalingPolicyBindingSpec{},
		}
		got := NewOptimizerMeta(binding)
		assert.Nil(t, got)
	})

	t.Run("min/max replica aggregation across backends", func(t *testing.T) {
		binding := makeBinding("uid-1", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 2, 6),
			makeParam("a100", 5, 1, 4),
		})
		meta := NewOptimizerMeta(binding)
		require.NotNil(t, meta)
		assert.Equal(t, int32(3), meta.MinReplicas)
		assert.Equal(t, int32(10), meta.MaxReplicas)
	})

	t.Run("scope (namespace + UID) inherited from binding", func(t *testing.T) {
		binding := makeBinding("test-uid", "test-ns", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 3),
		})
		meta := NewOptimizerMeta(binding)
		require.NotNil(t, meta)
		assert.Equal(t, "test-ns", meta.Scope.Namespace)
		assert.Equal(t, types.UID("test-uid"), meta.Scope.OwnedBindingId)
	})

	t.Run("zero-delta backends produce no blocks", func(t *testing.T) {
		binding := makeBinding("uid-2", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 5, 5),
		})
		meta := NewOptimizerMeta(binding)
		require.NotNil(t, meta)
		assert.Len(t, meta.ScalingOrder, 0)
	})

	t.Run("CostExpansionRatePercent == 100 — single block per backend", func(t *testing.T) {
		binding := makeBinding("uid-3", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 4),
			makeParam("a100", 5, 1, 3),
		})
		meta := NewOptimizerMeta(binding)
		require.NotNil(t, meta)
		assert.Len(t, meta.ScalingOrder, 2)
	})

	t.Run("scaling order sorted cheapest-first", func(t *testing.T) {
		binding := makeBinding("uid-4", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 4),
			makeParam("a100", 5, 1, 3),
		})
		meta := NewOptimizerMeta(binding)
		require.NotNil(t, meta)
		require.GreaterOrEqual(t, len(meta.ScalingOrder), 2)
		assert.LessOrEqual(t, meta.ScalingOrder[0].cost, meta.ScalingOrder[1].cost)
	})

	t.Run("CostExpansionRatePercent != 100 — geometric multi-block splitting", func(t *testing.T) {
		binding := makeBinding("uid-5", "ns-1", 50, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 5),
		})
		meta := NewOptimizerMeta(binding)
		require.NotNil(t, meta)
		assert.Greater(t, len(meta.ScalingOrder), 1)
	})
}

func TestRestoreReplicasOfEachBackend(t *testing.T) {
	tests := []struct {
		name     string
		params   []workload.HeterogeneousTargetParam
		rate     int32
		replicas int32
		want     map[string]int32
	}{
		{
			name: "cheaper backend fills first, spills to next",
			params: []workload.HeterogeneousTargetParam{
				makeParam("h100", 10, 1, 4),
				makeParam("a100", 5, 1, 3),
			},
			rate:     100,
			replicas: 5,
			want: map[string]int32{
				"a100": 3,
				"h100": 2,
			},
		},
		{
			name: "clamping below min",
			params: []workload.HeterogeneousTargetParam{
				makeParam("h100", 10, 2, 5),
			},
			rate:     100,
			replicas: 0,
			want: map[string]int32{
				"h100": 2,
			},
		},
		{
			name: "clamping above max",
			params: []workload.HeterogeneousTargetParam{
				makeParam("h100", 10, 1, 3),
			},
			rate:     100,
			replicas: 999,
			want: map[string]int32{
				"h100": 3,
			},
		},
		{
			name: "single-backend full range sweep",
			params: []workload.HeterogeneousTargetParam{
				makeParam("h100", 10, 1, 4),
			},
			rate:     100,
			replicas: 4,
			want: map[string]int32{
				"h100": 4,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding := makeBinding("uid", "ns", tt.rate, tt.params)
			meta := NewOptimizerMeta(binding)
			require.NotNil(t, meta)
			got := meta.RestoreReplicasOfEachBackend(tt.replicas)
			for backend, wantCount := range tt.want {
				assert.Equal(t, wantCount, got[backend], "backend %q", backend)
			}
		})
	}
}

func TestExternalMetricAggregation(t *testing.T) {
	tests := []struct {
		name    string
		samples []struct {
			metricName string
			value      float64
			replicas   int32
		}
		want map[string]float64
	}{
		{
			name: "additive metrics are summed across backends",
			samples: []struct {
				metricName string
				value      float64
				replicas   int32
			}{
				{metricName: "num_requests_waiting", value: 10, replicas: 2},
				{metricName: "num_requests_waiting", value: 15, replicas: 3},
			},
			want: map[string]float64{
				"num_requests_waiting": 25,
			},
		},
		{
			name: "ratio metrics use weighted sum by backend replica count",
			samples: []struct {
				metricName string
				value      float64
				replicas   int32
			}{
				{metricName: "gpu_utilization", value: 60, replicas: 2},
				{metricName: "gpu_utilization", value: 90, replicas: 1},
			},
			want: map[string]float64{
				"gpu_utilization": 210,
			},
		},
		{
			name: "cache usage percent suffix uses weighted sum",
			samples: []struct {
				metricName string
				value      float64
				replicas   int32
			}{
				{metricName: "gpu_cache_usage_perc", value: 50, replicas: 1},
				{metricName: "gpu_cache_usage_perc", value: 70, replicas: 1},
			},
			want: map[string]float64{
				"gpu_cache_usage_perc": 120,
			},
		},
		{
			name: "ratio metrics fall back to sample average when replicas are zero",
			samples: []struct {
				metricName string
				value      float64
				replicas   int32
			}{
				{metricName: "cache_usage", value: 40, replicas: 0},
				{metricName: "cache_usage", value: 80, replicas: 0},
			},
			want: map[string]float64{
				"cache_usage": 60,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aggregates := make(map[string]*externalMetricAggregate)
			for _, sample := range tt.samples {
				addExternalMetric(aggregates, sample.metricName, sample.value, sample.replicas)
			}
			got := finalizeExternalMetrics(aggregates)
			for metricName, want := range tt.want {
				assert.InDelta(t, want, got[metricName], 0.001)
			}
		})
	}
}

func TestAverageExternalMetric(t *testing.T) {
	tests := []struct {
		name       string
		metricName string
		want       bool
	}{
		{name: "utilization", metricName: "gpu_utilization", want: true},
		{name: "usage", metricName: "gpu_cache_usage", want: true},
		{name: "percent", metricName: "memory_percent", want: true},
		{name: "percentage", metricName: "kv_cache_percentage", want: true},
		{name: "ratio", metricName: "hit_ratio", want: true},
		{name: "rate", metricName: "token_rate", want: false},
		{name: "perc", metricName: "gpu_cache_usage_perc", want: true},
		{name: "bare suffix", metricName: "usage", want: true},
		{name: "suffix without separator", metricName: "upperperc", want: false},
		{name: "false rate suffix", metricName: "error_crate", want: false},
		{name: "count", metricName: "request_count", want: false},
		{name: "waiting", metricName: "num_requests_waiting", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, averageExternalMetric(tt.metricName))
		})
	}
}
