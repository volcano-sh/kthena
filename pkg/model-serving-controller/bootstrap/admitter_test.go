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

package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func testRole(name string, replicas int32, image string) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:     name,
		Replicas: ptr.To(replicas),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "engine", Image: image}},
			},
		},
	}
}

func testModelServing(groups int32, strategy *workloadv1alpha1.BootstrapAccelerateStrategy, roles ...workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:                    ptr.To(groups),
			BootstrapAccelerateStrategy: strategy,
			Template:                    workloadv1alpha1.ServingGroup{Roles: roles},
		},
	}
}

func TestSeedBudget(t *testing.T) {
	role := testRole("prefill", 4, "vllm:v1")
	tests := []struct {
		name     string
		groups   *int32
		seed     *intstr.IntOrString
		expected int
	}{
		{
			name:     "default seed is 1",
			groups:   ptr.To(int32(8)),
			expected: 1,
		},
		{
			name:     "nil replicas defaults to 1",
			groups:   nil,
			seed:     ptr.To(intstr.FromInt32(3)),
			expected: 1,
		},
		{
			name:     "zero desired groups yields zero budget",
			groups:   ptr.To(int32(0)),
			seed:     ptr.To(intstr.FromInt32(2)),
			expected: 0,
		},
		{
			name:     "integer seed clamped to desired groups",
			groups:   ptr.To(int32(3)),
			seed:     ptr.To(intstr.FromInt32(10)),
			expected: 3,
		},
		{
			name:     "percentage rounded up against spec.replicas",
			groups:   ptr.To(int32(3)),
			seed:     ptr.To(intstr.FromString("10%")),
			expected: 1,
		},
		{
			name:     "34 percent of 3 groups rounds up to 2",
			groups:   ptr.To(int32(3)),
			seed:     ptr.To(intstr.FromString("34%")),
			expected: 2,
		},
		{
			name:     "25 percent of 8 groups is 2",
			groups:   ptr.To(int32(8)),
			seed:     ptr.To(intstr.FromString("25%")),
			expected: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo"},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: tc.groups,
					BootstrapAccelerateStrategy: &workloadv1alpha1.BootstrapAccelerateStrategy{
						InitialSeedReplicas: tc.seed,
					},
					Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{role}},
				},
			}
			admitter := NewAdmitter(ms, nil)
			require.NotNil(t, admitter)
			assert.Equal(t, tc.expected, admitter.SeedBudget())
		})
	}
}

func TestAdmitGroupColdThenWarmBatches(t *testing.T) {
	tests := []struct {
		name      string
		groups    int32
		seed      *intstr.IntOrString
		wantCold  int
		wantTotal int
	}{
		{
			name:      "default seed 1 admits 1 then 8",
			groups:    8,
			wantCold:  1,
			wantTotal: 8,
		},
		{
			name:      "seed 2 admits 2 then 8",
			groups:    8,
			seed:      ptr.To(intstr.FromInt32(2)),
			wantCold:  2,
			wantTotal: 8,
		},
		{
			name:      "25 percent of 8 admits 2 then 8",
			groups:    8,
			seed:      ptr.To(intstr.FromString("25%")),
			wantCold:  2,
			wantTotal: 8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ms := testModelServing(tc.groups, &workloadv1alpha1.BootstrapAccelerateStrategy{
				InitialSeedReplicas: tc.seed,
			}, testRole("prefill", 2, "vllm:v1"))
			pool := PoolFor(ms, "prefill")

			cold := NewAdmitter(ms, nil)
			admitted := 0
			for range tc.groups {
				if cold.AdmitGroup(ms) {
					admitted++
				}
			}
			assert.Equal(t, tc.wantCold, admitted)
			assert.Equal(t, []PoolSummary{{Key: pool, Ready: 0, AdmittedGroups: tc.wantCold}}, cold.Summary())

			// While all seed groups remain unready, no further groups are admitted.
			stillColdObs := make([]GroupObservation, tc.wantCold)
			for i := range stillColdObs {
				stillColdObs[i] = GroupObservation{Pools: []PoolKey{pool}}
			}
			stillCold := NewAdmitter(ms, stillColdObs)
			assert.False(t, stillCold.AdmitGroup(ms))

			// Once one replica in a seed group becomes Ready, all remaining groups are admitted.
			warmObs := append([]GroupObservation(nil), stillColdObs...)
			warmObs[0].ReadyReplicas = []PoolKey{pool}
			warm := NewAdmitter(ms, warmObs)
			remaining := int(tc.groups) - tc.wantCold
			warmAdmitted := 0
			for range remaining {
				if warm.AdmitGroup(ms) {
					warmAdmitted++
				}
			}
			assert.Equal(t, remaining, warmAdmitted)
			assert.Equal(t, tc.wantTotal, tc.wantCold+warmAdmitted)
			assert.Equal(t, []PoolSummary{{Key: pool, Ready: 1, AdmittedGroups: remaining}}, warm.Summary())
		})
	}
}

func TestAdmitGroupDisaggregatedPDWaitsForEveryInScopePool(t *testing.T) {
	ms := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
	}, testRole("prefill", 2, "vllm:v1"), testRole("decode", 4, "vllm:v1"))

	prefillPool := PoolFor(ms, "prefill")
	decodePool := PoolFor(ms, "decode")

	// Batch 1: cold start admits only ServingGroup 0.
	batch1 := NewAdmitter(ms, nil)
	assert.True(t, batch1.AdmitGroup(ms))
	assert.False(t, batch1.AdmitGroup(ms))

	// Duplicate PoolKey entries from multiple role replicas inside one group still count as 1 in-flight group.
	prefillReadyOnly := NewAdmitter(ms, []GroupObservation{{
		Pools:         []PoolKey{prefillPool, prefillPool, decodePool, decodePool, decodePool, decodePool},
		ReadyReplicas: []PoolKey{prefillPool},
	}})
	assert.False(t, prefillReadyOnly.AdmitGroup(ms), "groups 1-3 must wait until decode also has a ready source")

	// Once both prefill and decode have at least one Ready replica, groups 1-3 are all admitted.
	bothReady := NewAdmitter(ms, []GroupObservation{{
		Pools:         []PoolKey{prefillPool, decodePool},
		ReadyReplicas: []PoolKey{prefillPool, decodePool},
	}})
	for i := 1; i < 4; i++ {
		assert.True(t, bothReady.AdmitGroup(ms), "group %d should be admitted once both pools are warm", i)
	}
}

func TestAdmitGroupOptionalRolesScopingAndZeroReplicas(t *testing.T) {
	ms := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
		Roles:               []string{"prefill"},
	}, testRole("prefill", 2, "vllm:v1"), testRole("router", 1, "router:v1"))

	prefillPool := PoolFor(ms, "prefill")
	routerPool := PoolFor(ms, "router")

	// Router has no ready replicas, but is out of scope; once prefill is ready, remaining groups are admitted.
	admitter := NewAdmitter(ms, []GroupObservation{{
		Pools:         []PoolKey{prefillPool, routerPool},
		ReadyReplicas: []PoolKey{prefillPool},
	}})
	assert.True(t, admitter.AdmitGroup(ms))
	assert.True(t, admitter.AdmitGroup(ms))
	assert.True(t, admitter.AdmitGroup(ms))

	// A workload whose in-scope role has zero replicas imposes no pool constraint.
	zeroRoleMS := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
		Roles:               []string{"prefill"},
	}, testRole("prefill", 0, "vllm:v1"), testRole("router", 1, "router:v1"))
	zeroAdmitter := NewAdmitter(zeroRoleMS, nil)
	assert.True(t, zeroAdmitter.AdmitGroup(zeroRoleMS))
	assert.True(t, zeroAdmitter.AdmitGroup(zeroRoleMS))
	assert.Empty(t, zeroAdmitter.Summary())
}

func TestAdmitGroupLoweredSeedBudgetDoesNotOvershoot(t *testing.T) {
	ms := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
	}, testRole("prefill", 1, "vllm:v1"))
	pool := PoolFor(ms, "prefill")

	// Two seed groups were already in flight before initialSeedReplicas was lowered from 2 to 1.
	admitter := NewAdmitter(ms, []GroupObservation{
		{Pools: []PoolKey{pool}},
		{Pools: []PoolKey{pool}},
	})
	assert.False(t, admitter.AdmitGroup(ms))
}

func TestAdmitGroupPoolIsolationAndEmptyHashIgnored(t *testing.T) {
	v1 := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
	}, testRole("prefill", 1, "vllm:v1"))
	v2 := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
	}, testRole("prefill", 1, "vllm:v2"))

	v1Pool := PoolFor(v1, "prefill")
	v2Pool := PoolFor(v2, "prefill")
	require.NotEqual(t, v1Pool, v2Pool)

	// Running v1 replicas and hashless replicas do not warm the v2 pool.
	admitter := NewAdmitter(v2, []GroupObservation{
		{Pools: []PoolKey{v1Pool}, ReadyReplicas: []PoolKey{v1Pool, {Role: "prefill", TemplateHash: ""}}},
	})
	// Partition-protected v1 group is warm and unrestricted; v2 is cold and capped at 1 seed group.
	assert.True(t, admitter.AdmitGroup(v1))
	assert.True(t, admitter.AdmitGroup(v1))
	assert.True(t, admitter.AdmitGroup(v2))
	assert.False(t, admitter.AdmitGroup(v2))
}

func TestAdmitterNilAndContextHelpers(t *testing.T) {
	var nilAdmitter *Admitter
	assert.False(t, nilAdmitter.InScope("prefill"))
	assert.Equal(t, 0, nilAdmitter.SeedBudget())
	assert.True(t, nilAdmitter.AdmitGroup(nil))
	assert.Nil(t, nilAdmitter.Summary())

	disabled := testModelServing(2, nil, testRole("prefill", 1, "vllm:v1"))
	assert.Nil(t, NewAdmitter(disabled, nil))

	ctx := context.Background()
	assert.Nil(t, AdmitterFrom(ctx))
	assert.Equal(t, ctx, WithAdmitter(ctx, nil))

	ms := testModelServing(2, &workloadv1alpha1.BootstrapAccelerateStrategy{}, testRole("prefill", 1, "vllm:v1"))
	a := NewAdmitter(ms, nil)
	ctx = WithAdmitter(ctx, a)
	assert.Same(t, a, AdmitterFrom(ctx))
}
