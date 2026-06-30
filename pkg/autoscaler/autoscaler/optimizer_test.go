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

// ---------------------------------------------------------------------------
// isRatioMetric
// ---------------------------------------------------------------------------

func TestIsRatioMetric(t *testing.T) {
	ratio := []string{
		"gpu_utilization",
		"cpu_utilization",
		"memory_utilization",
		"cache_usage",
		"disk_usage",
		"fill_ratio",
		"load_percent",
		"queue_saturation",
		// Mixed-case and uppercase Prometheus metric names must also be detected.
		"GPU_utilization",
		"CPU_UTILIZATION",
		"Cache_Usage",
	}
	for _, name := range ratio {
		assert.True(t, isRatioMetric(name), "expected %q to be a ratio metric", name)
	}

	additive := []string{
		"queue_length",
		"request_count",
		"token_rate",
		"request_rate",
		"throughput_rate",
		"pending_requests",
		"gpu_utilization_rate", // "rate" suffix wins over inner "_utilization"
	}
	for _, name := range additive {
		assert.False(t, isRatioMetric(name), "expected %q to be an additive metric", name)
	}
}

// ---------------------------------------------------------------------------
// aggregateExternalSamples
// ---------------------------------------------------------------------------

func TestAggregateExternalSamples_Additive(t *testing.T) {
	samples := []backendExternalSample{
		{value: 100, replicas: 3},
		{value: 200, replicas: 1},
	}
	got := aggregateExternalSamples("queue_length", samples)
	assert.Equal(t, 300.0, got, "additive metrics must be summed")
}

func TestAggregateExternalSamples_AdditiveRateNotAverage(t *testing.T) {
	// request_rate ends with "_rate" and must be summed, not averaged.
	samples := []backendExternalSample{
		{value: 50, replicas: 2},
		{value: 70, replicas: 2},
	}
	got := aggregateExternalSamples("request_rate", samples)
	assert.Equal(t, 120.0, got)
}

func TestAggregateExternalSamples_RatioWeightedAverage(t *testing.T) {
	// Backend A: gpu_utilization=60%, 3 replicas
	// Backend B: gpu_utilization=70%, 1 replica
	// Expected: (60*3 + 70*1) / (3+1) = (180+70)/4 = 250/4 = 62.5
	samples := []backendExternalSample{
		{value: 60, replicas: 3},
		{value: 70, replicas: 1},
	}
	got := aggregateExternalSamples("gpu_utilization", samples)
	assert.InDelta(t, 62.5, got, 1e-9)
}

func TestAggregateExternalSamples_RatioEqualReplicas(t *testing.T) {
	// Equal replica counts reduce to a simple average.
	// Backend A: 60%, 2 replicas; Backend B: 70%, 2 replicas → (60+70)/2 = 65
	samples := []backendExternalSample{
		{value: 60, replicas: 2},
		{value: 70, replicas: 2},
	}
	got := aggregateExternalSamples("memory_utilization", samples)
	assert.InDelta(t, 65.0, got, 1e-9)
}

func TestAggregateExternalSamples_RatioHeterogeneousWeights(t *testing.T) {
	// Reproduces the issue example: 60%+70% must NOT equal 130%.
	// Backend A: 60%, 4 replicas; Backend B: 70%, 6 replicas
	// Expected: (60*4 + 70*6) / (4+6) = (240+420)/10 = 66
	samples := []backendExternalSample{
		{value: 60, replicas: 4},
		{value: 70, replicas: 6},
	}
	got := aggregateExternalSamples("gpu_utilization", samples)
	assert.InDelta(t, 66.0, got, 1e-9)
	assert.Less(t, got, 100.0, "ratio metric must never exceed 100%%")
}

func TestAggregateExternalSamples_RatioZeroReplicasFallback(t *testing.T) {
	// When all backends report zero replicas the function falls back to the
	// unweighted average so that the metric is not silently dropped.
	samples := []backendExternalSample{
		{value: 60, replicas: 0},
		{value: 80, replicas: 0},
	}
	got := aggregateExternalSamples("cache_usage", samples)
	assert.InDelta(t, 70.0, got, 1e-9)
}

func TestAggregateExternalSamples_RatioOneBackendZeroReplicas(t *testing.T) {
	// One backend has replicas, one has zero.
	// Only the backend with replicas contributes weight.
	// (60*0 + 80*5) / (0+5) = 80
	samples := []backendExternalSample{
		{value: 60, replicas: 0},
		{value: 80, replicas: 5},
	}
	got := aggregateExternalSamples("gpu_utilization", samples)
	assert.InDelta(t, 80.0, got, 1e-9)
}

func TestAggregateExternalSamples_SingleBackendAdditive(t *testing.T) {
	samples := []backendExternalSample{{value: 42, replicas: 3}}
	got := aggregateExternalSamples("pending_requests", samples)
	assert.Equal(t, 42.0, got)
}

func TestAggregateExternalSamples_SingleBackendRatio(t *testing.T) {
	samples := []backendExternalSample{{value: 75, replicas: 2}}
	got := aggregateExternalSamples("gpu_utilization", samples)
	assert.InDelta(t, 75.0, got, 1e-9)
}

func TestAggregateExternalSamples_BackwardCompatAdditive(t *testing.T) {
	// Existing additive metrics must continue to be summed regardless of
	// replica counts, preserving pre-fix behaviour for queue-length style
	// metrics.
	samples := []backendExternalSample{
		{value: 10, replicas: 1},
		{value: 20, replicas: 4},
		{value: 30, replicas: 2},
	}
	got := aggregateExternalSamples("queue_length", samples)
	assert.Equal(t, 60.0, got)
}

func makePolicy(uid string, namespace string, costExpansionRate int32, params []workload.HeterogeneousTargetParam) *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Namespace: namespace,
		},
		Spec: workload.AutoscalingPolicySpec{
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
		policy := &workload.AutoscalingPolicy{
			Spec: workload.AutoscalingPolicySpec{},
		}
		got := NewOptimizerMeta(policy)
		assert.Nil(t, got)
	})

	t.Run("min/max replica aggregation across backends", func(t *testing.T) {
		policy := makePolicy("uid-1", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 2, 6),
			makeParam("a100", 5, 1, 4),
		})
		meta := NewOptimizerMeta(policy)
		require.NotNil(t, meta)
		assert.Equal(t, int32(3), meta.MinReplicas)
		assert.Equal(t, int32(10), meta.MaxReplicas)
	})

	t.Run("scope (namespace + UID) inherited from policy", func(t *testing.T) {
		policy := makePolicy("test-uid", "test-ns", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 3),
		})
		meta := NewOptimizerMeta(policy)
		require.NotNil(t, meta)
		assert.Equal(t, "test-ns", meta.Scope.Namespace)
		assert.Equal(t, types.UID("test-uid"), meta.Scope.OwnedPolicyId)
	})

	t.Run("zero-delta backends produce no blocks", func(t *testing.T) {
		policy := makePolicy("uid-2", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 5, 5),
		})
		meta := NewOptimizerMeta(policy)
		require.NotNil(t, meta)
		assert.Len(t, meta.ScalingOrder, 0)
	})

	t.Run("CostExpansionRatePercent == 100 — single block per backend", func(t *testing.T) {
		policy := makePolicy("uid-3", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 4),
			makeParam("a100", 5, 1, 3),
		})
		meta := NewOptimizerMeta(policy)
		require.NotNil(t, meta)
		assert.Len(t, meta.ScalingOrder, 2)
	})

	t.Run("scaling order sorted cheapest-first", func(t *testing.T) {
		policy := makePolicy("uid-4", "ns-1", 100, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 4),
			makeParam("a100", 5, 1, 3),
		})
		meta := NewOptimizerMeta(policy)
		require.NotNil(t, meta)
		require.GreaterOrEqual(t, len(meta.ScalingOrder), 2)
		assert.LessOrEqual(t, meta.ScalingOrder[0].cost, meta.ScalingOrder[1].cost)
	})

	t.Run("CostExpansionRatePercent != 100 — geometric multi-block splitting", func(t *testing.T) {
		policy := makePolicy("uid-5", "ns-1", 50, []workload.HeterogeneousTargetParam{
			makeParam("h100", 10, 1, 5),
		})
		meta := NewOptimizerMeta(policy)
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
			policy := makePolicy("uid", "ns", tt.rate, tt.params)
			meta := NewOptimizerMeta(policy)
			require.NotNil(t, meta)
			got := meta.RestoreReplicasOfEachBackend(tt.replicas)
			for backend, wantCount := range tt.want {
				assert.Equal(t, wantCount, got[backend], "backend %q", backend)
			}
		})
	}
}
