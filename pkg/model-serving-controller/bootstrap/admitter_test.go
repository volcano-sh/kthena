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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func testRole(name string, replicas int32) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:     name,
		Replicas: ptr.To(replicas),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "engine:v1"}}},
		},
	}
}

func testModelServing(replicas int32, strategy *workloadv1alpha1.BootstrapAccelerateStrategy, roles ...workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{}
	ms.Name = "ms"
	ms.Namespace = "default"
	ms.Spec.Replicas = ptr.To(replicas)
	ms.Spec.Template.Roles = roles
	ms.Spec.BootstrapAccelerateStrategy = strategy
	return ms
}

func TestEnabledAndInScope(t *testing.T) {
	tests := []struct {
		name        string
		strategy    *workloadv1alpha1.BootstrapAccelerateStrategy
		wantEnabled bool
		wantInScope map[string]bool
	}{
		{
			name:        "no strategy",
			wantInScope: map[string]bool{"prefill": false},
		},
		{
			name:        "empty provider defaults to ModelExpress and all roles",
			strategy:    &workloadv1alpha1.BootstrapAccelerateStrategy{},
			wantEnabled: true,
			wantInScope: map[string]bool{"prefill": true, "decode": true},
		},
		{
			name: "role subset",
			strategy: &workloadv1alpha1.BootstrapAccelerateStrategy{
				Provider: workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress,
				Roles:    []string{"decode"},
			},
			wantEnabled: true,
			wantInScope: map[string]bool{"prefill": false, "decode": true},
		},
		{
			name:        "unknown provider",
			strategy:    &workloadv1alpha1.BootstrapAccelerateStrategy{Provider: "Other"},
			wantInScope: map[string]bool{"prefill": false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := testModelServing(1, tt.strategy)
			assert.Equal(t, tt.wantEnabled, Enabled(ms))
			for role, want := range tt.wantInScope {
				assert.Equal(t, want, InScope(ms, role), role)
			}
		})
	}
}

func TestConfigHash(t *testing.T) {
	base := func() *workloadv1alpha1.ModelServing {
		ms := testModelServing(1, &workloadv1alpha1.BootstrapAccelerateStrategy{
			Provider: workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress,
			Roles:    []string{"prefill"},
			ModelExpress: &workloadv1alpha1.ModelExpressConfig{
				EngineContainers: []string{"engine", "aux"},
				ServerAddress:    "mx:8001",
				ReadyURL:         "http://localhost:8000/health",
			},
		}, testRole("prefill", 1))
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
			Name:   "p",
			Type:   workloadv1alpha1.PluginTypeBuiltIn,
			Config: &apiextensionsv1.JSON{Raw: []byte(`{"a":1,"b":2}`)},
		}}
		return ms
	}
	baseHash, err := ConfigHash(base(), "prefill")
	require.NoError(t, err)

	tests := []struct {
		name     string
		mutate   func(ms *workloadv1alpha1.ModelServing)
		role     string
		wantSame bool
	}{
		{
			name:     "plugin JSON key order",
			mutate:   func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Plugins[0].Config.Raw = []byte(`{ "b": 2, "a": 1 }`) },
			wantSame: true,
		},
		{
			name:     "plugin type default",
			mutate:   func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Plugins[0].Type = "" },
			wantSame: true,
		},
		{
			name: "engine container order",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"aux", "engine"}
			},
			wantSame: true,
		},
		{
			name: "readyURL",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = ""
			},
			wantSame: true,
		},
		{
			name: "seed and fan-out",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy.SeedReplicas = ptr.To(intstr.FromString("50%"))
				ms.Spec.BootstrapAccelerateStrategy.SourceFanOut = ptr.To[int32](3)
			},
			wantSame: true,
		},
		{
			name:     "replicas",
			mutate:   func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](5) },
			wantSame: true,
		},
		{
			name: "server address",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ServerAddress = "mx2:8001"
			},
		},
		{
			name: "engine containers",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"engine"}
			},
		},
		{
			name:   "modelExpress omitted",
			mutate: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.BootstrapAccelerateStrategy.ModelExpress = nil },
		},
		{
			name: "scope",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy.Roles = []string{"decode"}
			},
		},
		{
			name:   "plugin config",
			mutate: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Plugins[0].Config.Raw = []byte(`{"a":2,"b":2}`) },
		},
		{
			name: "plugin scope",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetEntry}
			},
		},
		{
			name: "plugin order",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins = append([]workloadv1alpha1.PluginSpec{{Name: "q", Type: workloadv1alpha1.PluginTypeBuiltIn}}, ms.Spec.Plugins...)
			},
		},
		{
			name: "role name",
			role: "decode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := base()
			if tt.mutate != nil {
				tt.mutate(ms)
			}
			role := tt.role
			if role == "" {
				role = "prefill"
			}
			hash, err := ConfigHash(ms, role)
			require.NoError(t, err)
			if tt.wantSame {
				assert.Equal(t, baseHash, hash)
			} else {
				assert.NotEqual(t, baseHash, hash)
			}
		})
	}

	t.Run("invalid plugin config", func(t *testing.T) {
		ms := base()
		ms.Spec.Plugins[0].Config.Raw = []byte(`{`)
		_, err := ConfigHash(ms, "prefill")
		assert.Error(t, err)
	})
}

func TestNewAdmitterDisabled(t *testing.T) {
	a, err := NewAdmitter(testModelServing(2, nil, testRole("r", 1)), false, nil)
	require.NoError(t, err)
	assert.Nil(t, a)
	assert.True(t, a.Admit([]UnitEntry{{Role: testRole("r", 1), Count: 5}}))
	assert.False(t, a.InScope("r"))
	assert.Nil(t, a.Pools())
}

func TestAdmitterBudget(t *testing.T) {
	tests := []struct {
		name        string
		replicas    int32
		roleReps    int32
		seed        *intstr.IntOrString
		fanOut      *int32
		gang        bool
		minRole     map[string]int32
		running     int
		starting    int
		wantBudget  int
		wantAdmit   int
		otherConfig bool
	}{
		{name: "default seed", replicas: 8, roleReps: 1, wantBudget: 1, wantAdmit: 1},
		{name: "integer seed", replicas: 8, roleReps: 1, seed: ptr.To(intstr.FromInt32(3)), wantBudget: 3, wantAdmit: 3},
		{name: "seed capped by desired", replicas: 2, roleReps: 1, seed: ptr.To(intstr.FromInt32(5)), wantBudget: 2, wantAdmit: 2},
		{name: "percentage seed rounds up", replicas: 3, roleReps: 1, seed: ptr.To(intstr.FromString("10%")), wantBudget: 1, wantAdmit: 1},
		{name: "percentage seed of all replicas", replicas: 4, roleReps: 2, seed: ptr.To(intstr.FromString("50%")), wantBudget: 4, wantAdmit: 4},
		{name: "zero desired", replicas: 0, roleReps: 1, wantBudget: 0, wantAdmit: 0},
		{name: "gang floor on seed", replicas: 4, roleReps: 4, gang: true, wantBudget: 4, wantAdmit: 4},
		{name: "gang floor uses minRoleReplicas", replicas: 4, roleReps: 4, gang: true, minRole: map[string]int32{"r": 2}, wantBudget: 2, wantAdmit: 2},
		{name: "minRoleReplicas zero floors at one", replicas: 4, roleReps: 4, gang: true, minRole: map[string]int32{"r": 0}, wantBudget: 1, wantAdmit: 1},
		{name: "no gang without PodGroup", replicas: 4, roleReps: 4, minRole: map[string]int32{"r": 2}, wantBudget: 1, wantAdmit: 1},
		{name: "fan-out", replicas: 8, roleReps: 1, running: 2, fanOut: ptr.To[int32](2), wantBudget: 4, wantAdmit: 4},
		{name: "fan-out minus starting", replicas: 8, roleReps: 1, running: 2, starting: 1, wantBudget: 2, wantAdmit: 1},
		{name: "starting above budget", replicas: 8, roleReps: 1, running: 1, starting: 3, wantBudget: 1, wantAdmit: 0},
		{name: "gang floor on fan-out", replicas: 4, roleReps: 4, gang: true, running: 1, wantBudget: 4, wantAdmit: 4},
		{name: "other pool is not a source", replicas: 8, roleReps: 1, running: 4, otherConfig: true, wantBudget: 1, wantAdmit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := testRole("r", tt.roleReps)
			ms := testModelServing(tt.replicas, &workloadv1alpha1.BootstrapAccelerateStrategy{
				SeedReplicas: tt.seed,
				SourceFanOut: tt.fanOut,
			}, role)
			if tt.minRole != nil {
				ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: tt.minRole}
			}
			pool := PoolKey{Role: "r", RoleTemplateHash: "x", ConfigHash: "y"}
			probe, err := NewAdmitter(ms, tt.gang, nil)
			require.NoError(t, err)
			if !tt.otherConfig {
				pool = probe.PoolFor(role)
			}
			var replicas []Replica
			for i := 0; i < tt.running; i++ {
				replicas = append(replicas, Replica{Pool: pool, Running: true})
			}
			for i := 0; i < tt.starting; i++ {
				replicas = append(replicas, Replica{Pool: pool})
			}
			a, err := NewAdmitter(ms, tt.gang, replicas)
			require.NoError(t, err)

			admitted := 0
			for i := 0; i < 16; i++ {
				if a.Admit([]UnitEntry{{Role: role, Count: 1}}) {
					admitted++
				}
			}
			assert.Equal(t, tt.wantAdmit, admitted)
			pools := a.Pools()
			if tt.wantAdmit+tt.starting+tt.running > 0 || tt.replicas > 0 {
				require.Len(t, pools, 1)
				assert.Equal(t, tt.wantBudget, pools[0].Budget)
				assert.Equal(t, tt.wantAdmit, pools[0].Admitted)
				assert.Equal(t, 16-tt.wantAdmit, pools[0].Deferred)
			}
		})
	}
}

func TestAdmitterUnits(t *testing.T) {
	prefill := testRole("prefill", 1)
	decode := testRole("decode", 1)
	other := testRole("other", 1)
	ms := testModelServing(8, &workloadv1alpha1.BootstrapAccelerateStrategy{
		Roles:        []string{"prefill", "decode"},
		SeedReplicas: ptr.To(intstr.FromInt32(2)),
	}, prefill, decode, other)

	t.Run("unit must fit every pool", func(t *testing.T) {
		a, err := NewAdmitter(ms, false, nil)
		require.NoError(t, err)
		assert.True(t, a.Admit([]UnitEntry{{Role: prefill, Count: 1}}))
		assert.False(t, a.Admit([]UnitEntry{{Role: prefill, Count: 1}, {Role: decode, Count: 3}}))
		// The denied unit blocks both pools, even for units that would fit.
		assert.False(t, a.Admit([]UnitEntry{{Role: prefill, Count: 1}}))
		assert.False(t, a.Admit([]UnitEntry{{Role: decode, Count: 1}}))
	})

	t.Run("out-of-scope roles are not gated", func(t *testing.T) {
		a, err := NewAdmitter(ms, false, nil)
		require.NoError(t, err)
		for i := 0; i < 10; i++ {
			assert.True(t, a.Admit([]UnitEntry{{Role: other, Count: 1}}))
		}
		assert.True(t, a.Admit([]UnitEntry{{Role: other, Count: 4}, {Role: prefill, Count: 2}}))
		assert.False(t, a.Admit([]UnitEntry{{Role: other, Count: 1}, {Role: prefill, Count: 1}}))
		assert.Empty(t, filterPool(a.Pools(), "other"))
	})

	t.Run("empty unit is admitted", func(t *testing.T) {
		a, err := NewAdmitter(ms, false, nil)
		require.NoError(t, err)
		assert.True(t, a.Admit(nil))
		assert.True(t, a.Admit([]UnitEntry{{Role: prefill, Count: 0}}))
	})

	t.Run("changed template is a new pool", func(t *testing.T) {
		a, err := NewAdmitter(ms, false, nil)
		require.NoError(t, err)
		newPrefill := *prefill.DeepCopy()
		newPrefill.EntryTemplate.Spec.Containers[0].Image = "engine:v2"
		oldPool := a.PoolFor(prefill)
		a, err = NewAdmitter(ms, false, []Replica{{Pool: oldPool, Running: true}, {Pool: oldPool, Running: true}, {Pool: oldPool, Running: true}})
		require.NoError(t, err)
		assert.True(t, a.Admit([]UnitEntry{{Role: newPrefill, Count: 2}}))
		assert.False(t, a.Admit([]UnitEntry{{Role: newPrefill, Count: 1}}))
		assert.True(t, a.Admit([]UnitEntry{{Role: prefill, Count: 3}}))
	})

	t.Run("created counts", func(t *testing.T) {
		a, err := NewAdmitter(ms, false, nil)
		require.NoError(t, err)
		unit := []UnitEntry{{Role: prefill, Count: 1}, {Role: other, Count: 1}}
		require.True(t, a.Admit(unit))
		a.Created(unit)
		pools := filterPool(a.Pools(), "prefill")
		require.Len(t, pools, 1)
		assert.Equal(t, 1, pools[0].Created)
	})
}

func filterPool(pools []PoolStatus, role string) []PoolStatus {
	var out []PoolStatus
	for _, pool := range pools {
		if pool.Pool.Role == role {
			out = append(out, pool)
		}
	}
	return out
}

// TestAdmitterBatchSequences reproduces the proposal's examples. Each batch
// admits units in ordinal order; admitted replicas become Running before the next batch.
func TestAdmitterBatchSequences(t *testing.T) {
	tests := []struct {
		name     string
		replicas int32
		existing int
		seed     int32
		fanOut   int32
		want     []int
	}{
		{name: "fan-out 1", replicas: 8, seed: 1, fanOut: 1, want: []int{1, 2, 4, 8}},
		{name: "fan-out 2", replicas: 8, seed: 1, fanOut: 2, want: []int{1, 3, 8}},
		{name: "fan-out 3", replicas: 8, seed: 1, fanOut: 3, want: []int{1, 4, 8}},
		{name: "scale 8 to 16", replicas: 16, existing: 8, seed: 1, fanOut: 1, want: []int{16}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := testRole("r", 1)
			ms := testModelServing(tt.replicas, &workloadv1alpha1.BootstrapAccelerateStrategy{
				SeedReplicas: ptr.To(intstr.FromInt32(tt.seed)),
				SourceFanOut: ptr.To(tt.fanOut),
			}, role)
			probe, err := NewAdmitter(ms, false, nil)
			require.NoError(t, err)
			pool := probe.PoolFor(role)

			created := tt.existing
			var got []int
			for created < int(tt.replicas) {
				replicas := make([]Replica, created)
				for i := range replicas {
					replicas[i] = Replica{Pool: pool, Running: true}
				}
				a, err := NewAdmitter(ms, false, replicas)
				require.NoError(t, err)
				for i := created; i < int(tt.replicas); i++ {
					if a.Admit([]UnitEntry{{Role: role, Count: 1}}) {
						created++
					}
				}
				got = append(got, created)
				require.LessOrEqual(t, len(got), 10)
			}
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("prefill decode with gang", func(t *testing.T) {
		prefill := testRole("prefill", 2)
		decode := testRole("decode", 4)
		ms := testModelServing(4, &workloadv1alpha1.BootstrapAccelerateStrategy{}, prefill, decode)
		ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1, "decode": 2}}
		probe, err := NewAdmitter(ms, true, nil)
		require.NoError(t, err)
		require.Equal(t, 1, probe.GangMin(prefill))
		require.Equal(t, 2, probe.GangMin(decode))
		prefillPool, decodePool := probe.PoolFor(prefill), probe.PoolFor(decode)

		cores, extraP, extraD := 0, 0, 0
		type batch struct{ cores, extraP, extraD int }
		var got []batch
		for len(got) < 6 && (cores < 4 || extraP < 4 || extraD < 8) {
			var replicas []Replica
			for i := 0; i < cores+extraP; i++ {
				replicas = append(replicas, Replica{Pool: prefillPool, Running: true})
			}
			for i := 0; i < 2*cores+extraD; i++ {
				replicas = append(replicas, Replica{Pool: decodePool, Running: true})
			}
			a, err := NewAdmitter(ms, true, replicas)
			require.NoError(t, err)
			newCores := 0
			for i := cores; i < 4; i++ {
				if a.Admit([]UnitEntry{{Role: prefill, Count: 1}, {Role: decode, Count: 2}}) {
					newCores++
				}
			}
			newP, newD := 0, 0
			for i := 0; i < cores-extraP; i++ {
				if a.Admit([]UnitEntry{{Role: prefill, Count: 1}}) {
					newP++
				}
			}
			for i := 0; i < 2*cores-extraD; i++ {
				if a.Admit([]UnitEntry{{Role: decode, Count: 1}}) {
					newD++
				}
			}
			cores += newCores
			extraP += newP
			extraD += newD
			got = append(got, batch{cores, extraP, extraD})
		}
		assert.Equal(t, []batch{{1, 0, 0}, {2, 0, 0}, {4, 0, 0}, {4, 4, 8}}, got)
	})
}
