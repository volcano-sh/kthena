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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func testRole(name string, replicas int32) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:     name,
		Replicas: ptr.To(replicas),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "engine", Image: name + ":v1"}},
		}},
	}
}

func testModelServing(replicas int32, roles ...workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ms"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To(replicas),
			SchedulerName: "volcano",
			Template:      workloadv1alpha1.ServingGroup{Roles: roles},
		},
	}
	return ms
}

func withStrategy(ms *workloadv1alpha1.ModelServing, roles ...workloadv1alpha1.BootstrapRole) *workloadv1alpha1.ModelServing {
	ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{Roles: roles}
	return ms
}

func seed(name string, value *intstr.IntOrString) workloadv1alpha1.BootstrapRole {
	return workloadv1alpha1.BootstrapRole{Name: name, InitialSeedReplicas: value}
}

// observed builds the current-pool replicas of role: ready Running sources and
// in-flight Creating replicas.
func observed(role workloadv1alpha1.Role, ready, inFlight int) []Replica {
	var replicas []Replica
	for i := 0; i < ready; i++ {
		replicas = append(replicas, Replica{Role: role.Name, TemplateHash: utils.CalRoleTemplateHash(role), Running: true})
	}
	for i := 0; i < inFlight; i++ {
		replicas = append(replicas, Replica{Role: role.Name, TemplateHash: utils.CalRoleTemplateHash(role)})
	}
	return replicas
}

func TestNewAdmitterDisabled(t *testing.T) {
	ms := testModelServing(2, testRole("prefill", 1))
	assert.Nil(t, NewAdmitter(ms, nil, Options{}))

	var nilAdmitter *Admitter
	replicasToCreate, ok := nilAdmitter.AdmitGroup(ms.Spec.Template.Roles)
	assert.True(t, ok)
	assert.Nil(t, replicasToCreate)
	assert.True(t, nilAdmitter.AdmitRole("ms-0", ms.Spec.Template.Roles[0]))
	assert.False(t, nilAdmitter.InScope("prefill"))
	assert.Nil(t, nilAdmitter.Summary())
	assert.Nil(t, AdmitterFrom(WithAdmitter(context.Background(), nil)))
}

func TestSeedBudget(t *testing.T) {
	tests := []struct {
		name            string
		replicas        int32
		roleReplicas    int32
		seed            *intstr.IntOrString
		minRoleReplicas map[string]int32
		gang            bool
		want            int
		wantGang        int
	}{
		{name: "default seed is one", replicas: 8, roleReplicas: 1, want: 1, wantGang: 1},
		{name: "integer seed", replicas: 8, roleReplicas: 1, seed: ptr.To(intstr.FromInt32(3)), want: 3, wantGang: 1},
		{name: "percentage rounds up", replicas: 3, roleReplicas: 1, seed: ptr.To(intstr.FromString("10%")), want: 1, wantGang: 1},
		{name: "percentage of desired", replicas: 4, roleReplicas: 2, seed: ptr.To(intstr.FromString("50%")), want: 4, wantGang: 1},
		{name: "seed capped at desired", replicas: 2, roleReplicas: 1, seed: ptr.To(intstr.FromInt32(10)), want: 2, wantGang: 1},
		{name: "gang floor without minRoleReplicas is the whole role", replicas: 4, roleReplicas: 4, gang: true, want: 4, wantGang: 4},
		{name: "gang floor from minRoleReplicas", replicas: 4, roleReplicas: 4, minRoleReplicas: map[string]int32{"prefill": 2}, gang: true, want: 2, wantGang: 2},
		{name: "seed above gang floor wins", replicas: 4, roleReplicas: 4, minRoleReplicas: map[string]int32{"prefill": 2}, seed: ptr.To(intstr.FromInt32(3)), gang: true, want: 3, wantGang: 2},
		{name: "minRoleReplicas zero still needs one", replicas: 4, roleReplicas: 4, minRoleReplicas: map[string]int32{"prefill": 0}, gang: true, want: 1, wantGang: 1},
		{name: "zero desired creates nothing", replicas: 0, roleReplicas: 2, gang: true, want: 0, wantGang: 2},
		{name: "zero role replicas", replicas: 3, roleReplicas: 0, gang: true, want: 0, wantGang: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := testRole("prefill", tt.roleReplicas)
			ms := withStrategy(testModelServing(tt.replicas, role), seed("prefill", tt.seed))
			if tt.minRoleReplicas != nil {
				ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: tt.minRoleReplicas}
			}
			admitter := NewAdmitter(ms, nil, Options{GangScheduling: tt.gang})
			require.NotNil(t, admitter)
			assert.Equal(t, tt.wantGang, admitter.GangMinimum(role), "gang minimum")
			assert.Equal(t, tt.want, admitter.SeedBudget("prefill"), "seed budget")
			assert.Equal(t, 0, admitter.SeedBudget("unknown"))
		})
	}
}

func TestAdmitGroupSeedThenAll(t *testing.T) {
	role := testRole("prefill", 1)
	ms := withStrategy(testModelServing(8, role), seed("prefill", nil))

	// R=0: only the seed fits; later groups are deferred and the pool is blocked.
	admitter := NewAdmitter(ms, nil, Options{GangScheduling: true})
	replicasToCreate, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
	require.True(t, ok)
	assert.Equal(t, map[string]int{"prefill": 1}, replicasToCreate)
	for i := 1; i < 8; i++ {
		_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
		assert.False(t, ok, "group %d must wait for the seed", i)
	}
	assert.False(t, admitter.AdmitRole("ms-3", role), "a denied pool stays blocked for the sync")
	summary := admitter.Summary()
	require.Len(t, summary, 1)
	assert.Equal(t, 1, summary[0].Admitted)
	assert.Equal(t, 0, summary[0].Ready)

	// R=0, I=1 from the previous sync: still nothing more.
	admitter = NewAdmitter(ms, observed(role, 0, 1), Options{GangScheduling: true})
	_, ok = admitter.AdmitGroup(ms.Spec.Template.Roles)
	assert.False(t, ok)
	assert.Empty(t, admitter.Summary())

	// R=1: everything else is admitted at once (1 -> 8).
	admitter = NewAdmitter(ms, observed(role, 1, 0), Options{GangScheduling: true})
	for i := 1; i < 8; i++ {
		_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
		assert.True(t, ok, "group %d must be admitted once a source is ready", i)
	}
	summary = admitter.Summary()
	require.Len(t, summary, 1)
	assert.Equal(t, 7, summary[0].Admitted)
	assert.Equal(t, 1, summary[0].Ready)
}

func TestAdmitGroupSeedOfTwo(t *testing.T) {
	role := testRole("prefill", 1)
	ms := withStrategy(testModelServing(8, role), seed("prefill", ptr.To(intstr.FromInt32(2))))
	admitter := NewAdmitter(ms, nil, Options{GangScheduling: true})
	admitted := 0
	for i := 0; i < 8; i++ {
		if _, ok := admitter.AdmitGroup(ms.Spec.Template.Roles); ok {
			admitted++
		}
	}
	assert.Equal(t, 2, admitted, "2 -> 8")
}

func TestAdmitGroupScaleUpWithReadySources(t *testing.T) {
	role := testRole("prefill", 1)
	ms := withStrategy(testModelServing(16, role), seed("prefill", nil))
	admitter := NewAdmitter(ms, observed(role, 4, 0), Options{GangScheduling: true})
	for i := 4; i < 16; i++ {
		_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
		assert.True(t, ok, "scale 8 -> 16 with ready sources creates all at once")
	}
}

func TestAdmitGroupOutOfScopeRolesAreNotGated(t *testing.T) {
	prefill, decode := testRole("prefill", 2), testRole("decode", 4)
	ms := withStrategy(testModelServing(2, prefill, decode), seed("prefill", nil))
	admitter := NewAdmitter(ms, nil, Options{GangScheduling: false})
	replicasToCreate, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
	require.True(t, ok)
	assert.Equal(t, map[string]int{"prefill": 1, "decode": 4}, replicasToCreate)
	assert.False(t, admitter.InScope("decode"))
	assert.True(t, admitter.AdmitRole("ms-0", decode))

	noScoped := testModelServing(2, prefill, decode)
	noScoped.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{Roles: []workloadv1alpha1.BootstrapRole{{Name: "absent"}}}
	admitter = NewAdmitter(noScoped, nil, Options{})
	replicasToCreate, ok = admitter.AdmitGroup(noScoped.Spec.Template.Roles)
	assert.True(t, ok)
	assert.Nil(t, replicasToCreate, "no in-scope role means the group is created whole")
}

// Prefill/decode with minRoleReplicas {prefill: 1, decode: 2}: the first group
// core is cold, and every other replica follows once both pools have a source.
func TestAdmitGroupMultiRoleGangCore(t *testing.T) {
	prefill, decode := testRole("prefill", 2), testRole("decode", 4)
	ms := withStrategy(testModelServing(4, prefill, decode), seed("prefill", nil), seed("decode", nil))
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1, "decode": 2}}

	admitter := NewAdmitter(ms, nil, Options{GangScheduling: true})
	assert.Equal(t, 1, admitter.SeedBudget("prefill"))
	assert.Equal(t, 2, admitter.SeedBudget("decode"))
	replicasToCreate, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
	require.True(t, ok)
	assert.Equal(t, map[string]int{"prefill": 1, "decode": 2}, replicasToCreate)
	_, ok = admitter.AdmitGroup(ms.Spec.Template.Roles)
	assert.False(t, ok, "group 1 core does not fit the prefill seed budget")
	assert.False(t, admitter.AdmitRole("ms-0", prefill), "non-core prefill waits")
	assert.False(t, admitter.AdmitRole("ms-0", decode), "non-core decode waits")

	replicas := append(observed(prefill, 1, 0), observed(decode, 2, 0)...)
	admitter = NewAdmitter(ms, replicas, Options{GangScheduling: true})
	for i := 1; i < 4; i++ {
		_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
		assert.True(t, ok, "group %d core", i)
	}
	assert.True(t, admitter.AdmitRole("ms-0", prefill))
	assert.True(t, admitter.AdmitRole("ms-0", decode))
}

func TestAdmitGroupNeedsEveryPool(t *testing.T) {
	prefill, decode := testRole("prefill", 1), testRole("decode", 1)
	ms := withStrategy(testModelServing(2, prefill, decode), seed("prefill", nil), seed("decode", nil))
	// Prefill has a source, decode is still cold with its seed in flight.
	replicas := append(observed(prefill, 1, 0), observed(decode, 0, 1)...)
	admitter := NewAdmitter(ms, replicas, Options{GangScheduling: true})
	_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
	assert.False(t, ok, "a unit must fit every pool without a source")
	assert.True(t, admitter.AdmitRole("ms-0", prefill), "the pool with a source is unlimited")
}

func TestAdmitReplicaStopsWhenSeedLoweredBelowInFlight(t *testing.T) {
	role := testRole("prefill", 1)
	ms := withStrategy(testModelServing(8, role), seed("prefill", ptr.To(intstr.FromInt32(1))))
	admitter := NewAdmitter(ms, observed(role, 0, 3), Options{})
	assert.False(t, admitter.AdmitRole("ms-3", role))
}

func TestPlanGroupCoreRepairsBypassBlocksButNotBudget(t *testing.T) {
	prefill, decode := testRole("prefill", 1), testRole("decode", 1)
	ms := withStrategy(testModelServing(2, prefill, decode), seed("prefill", nil), seed("decode", nil))
	// Group 0 created prefill but decode failed: prefill in flight, decode missing.
	admitter := NewAdmitter(ms, observed(prefill, 0, 1), Options{GangScheduling: true})

	// The new group 1 is denied and blocks both pools.
	_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
	require.False(t, ok)
	assert.False(t, admitter.AdmitRole("ms-1", decode), "units of one respect the block")

	// Repairing group 0's core still fits decode's budget and must proceed.
	admitter.PlanGroupCore("ms-0", ms.Spec.Template.Roles, map[string]int{"prefill": 1})
	assert.True(t, admitter.AdmitRole("ms-0", decode))
	assert.False(t, admitter.AdmitRole("ms-0", prefill), "prefill is present, not planned, and its pool is full")

	// A repair that exceeds the budget is denied as a whole.
	admitter = NewAdmitter(ms, observed(decode, 0, 1), Options{GangScheduling: true})
	admitter.PlanGroupCore("ms-1", ms.Spec.Template.Roles, map[string]int{})
	assert.False(t, admitter.AdmitRole("ms-1", prefill), "a multi-role core is denied when any pool is full")
	assert.False(t, admitter.AdmitRole("ms-1", decode))
}

func TestPlanGroupCoreUnitCountsOnce(t *testing.T) {
	role := testRole("prefill", 4)
	ms := withStrategy(testModelServing(1, role), seed("prefill", nil))
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 2}}
	admitter := NewAdmitter(ms, nil, Options{GangScheduling: true})
	admitter.PlanGroupCore("ms-0", ms.Spec.Template.Roles, map[string]int{})
	assert.True(t, admitter.AdmitRole("ms-0", role))
	assert.True(t, admitter.AdmitRole("ms-0", role))
	assert.False(t, admitter.AdmitRole("ms-0", role), "non-core replicas wait for the seed budget")
	summary := admitter.Summary()
	require.Len(t, summary, 1)
	assert.Equal(t, 2, summary[0].Admitted)
}

func TestPlanGroupCoreReservationMatchesTemplate(t *testing.T) {
	role := testRole("prefill", 4)
	ms := withStrategy(testModelServing(1, role), seed("prefill", nil))
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 2}}
	oldRole := *role.DeepCopy()
	oldRole.EntryTemplate.Spec.Containers[0].Image = "old-image"
	admitter := NewAdmitter(ms, observed(oldRole, 0, 2), Options{GangScheduling: true})
	admitter.PlanGroupCore("ms-0", ms.Spec.Template.Roles, nil)
	assert.False(t, admitter.AdmitRole("ms-0", oldRole), "another template cannot consume the core reservation")
	assert.True(t, admitter.AdmitRole("ms-0", role))
	assert.True(t, admitter.AdmitRole("ms-0", role))
	assert.False(t, admitter.AdmitRole("ms-0", role), "the core reservation is exhausted")
}

func TestReplicaAttribution(t *testing.T) {
	role := testRole("prefill", 1)
	ms := withStrategy(testModelServing(4, role), seed("prefill", nil))
	templateHash := utils.CalRoleTemplateHash(role)

	tests := []struct {
		name     string
		replicas []Replica
		admits   bool
	}{
		{name: "running replica of another template is not a source",
			replicas: []Replica{{Role: "prefill", TemplateHash: "old", Running: true}}, admits: true},
		{name: "running replica without a template hash is not a source",
			replicas: []Replica{{Role: "prefill", Running: true}}, admits: true},
		{name: "in-flight replica of the current template spends the budget",
			replicas: []Replica{{Role: "prefill", TemplateHash: templateHash}}, admits: false},
		{name: "in-flight replica of another template does not spend the budget",
			replicas: []Replica{{Role: "prefill", TemplateHash: "old"}}, admits: true},
		{name: "out-of-scope role is ignored",
			replicas: []Replica{{Role: "decode", TemplateHash: templateHash}}, admits: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admitter := NewAdmitter(ms, tt.replicas, Options{})
			_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
			assert.Equal(t, tt.admits, ok)
			if ok {
				// Only a matching source releases more than the seed.
				_, second := admitter.AdmitGroup(ms.Spec.Template.Roles)
				assert.False(t, second)
			}
		})
	}
}

func TestAdmitterContext(t *testing.T) {
	ms := withStrategy(testModelServing(1, testRole("prefill", 1)), seed("prefill", nil))
	admitter := NewAdmitter(ms, nil, Options{})
	ctx := WithAdmitter(context.Background(), admitter)
	assert.Same(t, admitter, AdmitterFrom(ctx))
	assert.Nil(t, AdmitterFrom(context.Background()))
}
