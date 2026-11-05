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
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// PoolKey identifies the replicas that can serve each other as weight sources.
// It mirrors MX_MODEL_REVISION, which the managed plugin derives from the role
// template hash, so the controller and ModelExpress agree on compatibility.
type PoolKey struct {
	Role         string
	TemplateHash string
}

func (k PoolKey) String() string {
	return fmt.Sprintf("%s/%s", k.Role, k.TemplateHash)
}

// Replica is one observed role replica.
type Replica struct {
	Role         string
	TemplateHash string
	// Running is true when every Pod of the replica is Ready.
	Running bool
}

// Options configures NewAdmitter.
type Options struct {
	// GangScheduling is true when ServingGroups get a PodGroup, so a role's gang
	// minimum must be created together.
	GangScheduling bool
}

type poolState struct {
	ready    int
	inFlight int
	admitted int
}

// PoolSummary reports one pool's admissions in a reconcile.
type PoolSummary struct {
	Key      PoolKey
	Ready    int
	Admitted int
}

// Admitter decides, for one reconcile, which missing role replicas may be
// created now. A pool with a Ready source admits everything; a pool without one
// admits at most its seed budget. It is not safe for concurrent use.
type Admitter struct {
	ms      *workloadv1alpha1.ModelServing
	options Options

	roleSpecs map[string]workloadv1alpha1.Role
	pools     map[PoolKey]*poolState
}

// NewAdmitter builds the admission state from the observed in-scope replicas.
// It returns nil when ms does not use bootstrap acceleration.
func NewAdmitter(ms *workloadv1alpha1.ModelServing, replicas []Replica, options Options) *Admitter {
	if !Enabled(ms) {
		return nil
	}
	a := &Admitter{
		ms:        ms,
		options:   options,
		roleSpecs: make(map[string]workloadv1alpha1.Role, len(ms.Spec.Template.Roles)),
		pools:     make(map[PoolKey]*poolState),
	}
	for _, role := range ms.Spec.Template.Roles {
		a.roleSpecs[role.Name] = role
	}
	for _, replica := range replicas {
		pool := a.pool(PoolKey{Role: replica.Role, TemplateHash: replica.TemplateHash})
		if replica.Running {
			pool.ready++
		} else {
			pool.inFlight++
		}
	}
	return a
}

func (a *Admitter) pool(key PoolKey) *poolState {
	state, ok := a.pools[key]
	if !ok {
		state = &poolState{}
		a.pools[key] = state
	}
	return state
}

// InScope reports whether roleName participates in bootstrap acceleration.
func (a *Admitter) InScope(roleName string) bool {
	if a == nil {
		return false
	}
	_, ok := RoleConfig(a.ms, roleName)
	return ok
}

// PoolFor returns the pool new replicas of role join.
func PoolFor(role workloadv1alpha1.Role) PoolKey {
	return PoolKey{Role: role.Name, TemplateHash: utils.CalRoleTemplateHash(role)}
}

// GangMinimum is the number of replicas of role that one ServingGroup must
// create together for its PodGroup to become schedulable.
func (a *Admitter) GangMinimum(role workloadv1alpha1.Role) int {
	replicas := roleReplicas(role)
	if replicas <= 0 {
		return 0
	}
	if !a.options.GangScheduling {
		return 1
	}
	minimum := replicas
	if gang := a.ms.Spec.Template.GangPolicy; gang != nil {
		if configured, ok := gang.MinRoleReplicas[role.Name]; ok {
			minimum = min(int(configured), minimum)
		}
	}
	return max(1, minimum)
}

// SeedBudget is the number of replicas of roleName that may be in flight while
// its pool has no Ready source: min(D, max(g, initialSeedReplicas)).
func (a *Admitter) SeedBudget(roleName string) int {
	if a == nil {
		return 0
	}
	spec, ok := a.roleSpecs[roleName]
	if !ok {
		return 0
	}
	desired := 0
	if a.ms.Spec.Replicas != nil {
		desired = int(*a.ms.Spec.Replicas) * roleReplicas(spec)
	}
	if desired <= 0 {
		return 0
	}
	seed := 1
	if config, ok := RoleConfig(a.ms, roleName); ok && config.InitialSeedReplicas != nil {
		if value, err := intstr.GetScaledValueFromIntOrPercent(config.InitialSeedReplicas, desired, true); err == nil {
			seed = value
		}
	}
	return min(desired, max(a.GangMinimum(spec), max(1, seed)))
}

// AdmitGroup decides whether a new ServingGroup may be created from roles. On
// success it returns how many replicas of each role to create now: the gang
// minimum for cold in-scope roles and every replica otherwise. A nil map with ok
// means no role is gated and all replicas may be created.
func (a *Admitter) AdmitGroup(roles []workloadv1alpha1.Role) (map[string]int, bool) {
	if a == nil {
		return nil, true
	}
	minReplicaToCreate := make(map[PoolKey]int)
	replicasToCreate := make(map[string]int, len(roles))
	for _, role := range roles {
		if !a.InScope(role.Name) {
			replicasToCreate[role.Name] = roleReplicas(role)
			continue
		}
		key := PoolFor(role)
		count := roleReplicas(role)
		if a.pool(key).ready == 0 {
			count = a.GangMinimum(role)
		}
		replicasToCreate[role.Name] = count
		if count > 0 {
			minReplicaToCreate[key] += count
		}
	}
	if len(minReplicaToCreate) == 0 {
		return nil, true
	}
	if !a.admit(minReplicaToCreate) {
		return nil, false
	}
	return replicasToCreate, true
}

// AdmitRole admits one missing replica using the global pool budget.
func (a *Admitter) AdmitRole(role workloadv1alpha1.Role) bool {
	if a == nil || !a.InScope(role.Name) {
		return true
	}
	return a.admit(map[PoolKey]int{PoolFor(role): 1})
}

// admit reserves all pools or none so an unschedulable partial gang cannot consume seed budgets.
func (a *Admitter) admit(minReplicaToCreate map[PoolKey]int) bool {
	for key, count := range minReplicaToCreate {
		pool := a.pool(key)
		if pool.ready > 0 {
			continue
		}
		budget := a.SeedBudget(key.Role)
		if pool.inFlight+count > budget {
			klog.V(4).InfoS("Deferring bootstrap replicas until a source is ready",
				"modelServing", klog.KObj(a.ms), "pool", key, "requested", count,
				"inFlight", pool.inFlight, "seedBudget", budget)
			return false
		}
	}
	for key, count := range minReplicaToCreate {
		pool := a.pool(key)
		pool.inFlight += count
		pool.admitted += count
	}
	return true
}

// Summary lists pools that admitted replicas in this reconcile.
func (a *Admitter) Summary() []PoolSummary {
	if a == nil {
		return nil
	}
	var result []PoolSummary
	for key, pool := range a.pools {
		if pool.admitted > 0 {
			result = append(result, PoolSummary{Key: key, Ready: pool.ready, Admitted: pool.admitted})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key.String() < result[j].Key.String() })
	return result
}

func roleReplicas(role workloadv1alpha1.Role) int {
	if role.Replicas == nil {
		return 1
	}
	return int(*role.Replicas)
}

type admitterKey struct{}

// WithAdmitter attaches the reconcile's admitter to ctx. A nil admitter leaves
// creation ungated.
func WithAdmitter(ctx context.Context, admitter *Admitter) context.Context {
	if admitter == nil {
		return ctx
	}
	return context.WithValue(ctx, admitterKey{}, admitter)
}

// AdmitterFrom returns the admitter attached by WithAdmitter, or nil.
func AdmitterFrom(ctx context.Context) *Admitter {
	admitter, _ := ctx.Value(admitterKey{}).(*Admitter)
	return admitter
}
