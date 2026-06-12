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
