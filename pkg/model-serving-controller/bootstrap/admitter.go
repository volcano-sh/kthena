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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// PoolKey identifies the replicas that can serve each other as weight sources.
type PoolKey struct {
	Role         string
	TemplateHash string
	ConfigHash   string
}

func (k PoolKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.Role, k.TemplateHash, k.ConfigHash)
}

// Replica is one observed role replica.
type Replica struct {
	Role         string
	TemplateHash string
	// ConfigHash is the bootstrap configuration hash shared by all of the
	// replica's Pods. It is empty when they disagree or carry no annotation.
	ConfigHash string
	// HashKnown is false when no Pod of the replica has been observed yet. Such a
	// replica is attributed to the current pool so admission stays conservative.
	HashKnown bool
	// Running is true when every Pod of the replica is Ready.
	Running bool
}

// Options configures NewAdmitter.
type Options struct {
	// ServerAddress is the controller's ModelExpress server address, part of the
	// configuration hash.
	ServerAddress string
	// GangScheduling is true when ServingGroups get a PodGroup, so a role's gang
	// minimum must be created together.
	GangScheduling bool
}

type poolState struct {
	ready    int
	inFlight int
	admitted int
	blocked  bool
}

type coreDecision struct {
	admitted bool
	roles    sets.Set[string]
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

	roleSpecs   map[string]workloadv1alpha1.Role
	configHash  map[string]string
	pools       map[PoolKey]*poolState
	coreByGroup map[string]coreDecision
}

// NewAdmitter builds the admission state from the observed replicas. It returns
// nil when ms does not use bootstrap acceleration.
func NewAdmitter(ms *workloadv1alpha1.ModelServing, replicas []Replica, options Options) (*Admitter, error) {
	if !Enabled(ms) {
		return nil, nil
	}
	a := &Admitter{
		ms:          ms,
		options:     options,
		roleSpecs:   make(map[string]workloadv1alpha1.Role, len(ms.Spec.Template.Roles)),
		configHash:  make(map[string]string),
		pools:       make(map[PoolKey]*poolState),
		coreByGroup: make(map[string]coreDecision),
	}
	for _, role := range ms.Spec.Template.Roles {
		a.roleSpecs[role.Name] = role
		if _, ok := RoleConfig(ms, role.Name); !ok {
			continue
		}
		hash, err := ConfigHash(ms, role.Name, options.ServerAddress)
		if err != nil {
			return nil, err
		}
		a.configHash[role.Name] = hash
	}
	for _, replica := range replicas {
		current, inScope := a.configHash[replica.Role]
		if !inScope {
			continue
		}
		hash := replica.ConfigHash
		if !replica.HashKnown {
			if replica.Running {
				// Readiness is observed from Pods, so this cannot happen; never
				// invent a source from it.
				continue
			}
			hash = current
		}
		pool := a.pool(PoolKey{Role: replica.Role, TemplateHash: replica.TemplateHash, ConfigHash: hash})
		if replica.Running {
			pool.ready++
		} else {
			pool.inFlight++
		}
	}
	return a, nil
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
	_, ok := a.configHash[roleName]
	return ok
}

// PoolFor returns the pool new replicas of role join.
func (a *Admitter) PoolFor(role workloadv1alpha1.Role) PoolKey {
	key := PoolKey{Role: role.Name, TemplateHash: utils.CalRoleTemplateHash(role)}
	if a != nil {
		key.ConfigHash = a.configHash[role.Name]
	}
	return key
}

// GangMinimum is the number of replicas of role that one ServingGroup must
// create together for its PodGroup to become schedulable.
func (a *Admitter) GangMinimum(role workloadv1alpha1.Role) int {
	replicas := roleReplicas(role)
	if replicas <= 0 {
		return 0
	}
	if a == nil || !a.options.GangScheduling {
		return 1
	}
	minimum := replicas
	if gang := a.ms.Spec.Template.GangPolicy; gang != nil {
		if configured, ok := gang.MinRoleReplicas[role.Name]; ok {
			minimum = min(int(configured), replicas)
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
// minimum for in-scope roles and every replica otherwise. A nil map with ok
// means no role is gated and all replicas may be created.
func (a *Admitter) AdmitGroup(roles []workloadv1alpha1.Role) (map[string]int, bool) {
	if a == nil {
		return nil, true
	}
	unit := make(map[PoolKey]int)
	limits := make(map[string]int, len(roles))
	for _, role := range roles {
		if !a.InScope(role.Name) {
			limits[role.Name] = roleReplicas(role)
			continue
		}
		core := a.GangMinimum(role)
		limits[role.Name] = core
		if core > 0 {
			unit[a.PoolFor(role)] += core
		}
	}
	if len(unit) == 0 {
		return nil, true
	}
	if !a.admit(unit, true) {
		return nil, false
	}
	return limits, true
}

// PlanGroupCore admits the missing gang-minimum replicas of an existing group
// as one unit so a repaired core becomes schedulable together. present holds
// the ordinals each role already has. The decision is replayed by AdmitReplica.
// Repairs ignore pools blocked by earlier denials in this reconcile: an existing
// group's incomplete core must not be starved by a deferred new group.
func (a *Admitter) PlanGroupCore(group string, roles []workloadv1alpha1.Role, present map[string]sets.Set[int]) {
	if a == nil {
		return
	}
	unit := make(map[PoolKey]int)
	planned := sets.New[string]()
	for _, role := range roles {
		if !a.InScope(role.Name) {
			continue
		}
		missing := 0
		for ordinal := 0; ordinal < a.GangMinimum(role); ordinal++ {
			if !present[role.Name].Has(ordinal) {
				missing++
			}
		}
		if missing == 0 {
			continue
		}
		unit[a.PoolFor(role)] += missing
		planned.Insert(role.Name)
	}
	if len(unit) == 0 {
		return
	}
	a.coreByGroup[group] = coreDecision{admitted: a.admit(unit, false), roles: planned}
}

// AdmitReplica decides whether one missing replica of role at ordinal may be
// created in group now.
func (a *Admitter) AdmitReplica(group string, role workloadv1alpha1.Role, ordinal int) bool {
	if a == nil || !a.InScope(role.Name) {
		return true
	}
	if ordinal < a.GangMinimum(role) {
		if decision, ok := a.coreByGroup[group]; ok && decision.roles.Has(role.Name) {
			return decision.admitted
		}
	}
	return a.admit(map[PoolKey]int{a.PoolFor(role): 1}, true)
}

// admit reserves unit in every pool it touches, or denies it as a whole.
func (a *Admitter) admit(unit map[PoolKey]int, respectBlocks bool) bool {
	for key, count := range unit {
		pool := a.pool(key)
		if pool.ready > 0 {
			continue
		}
		budget := a.SeedBudget(key.Role)
		if (respectBlocks && pool.blocked) || pool.inFlight+count > budget {
			for blockedKey := range unit {
				if blocked := a.pool(blockedKey); blocked.ready == 0 {
					blocked.blocked = true
				}
			}
			klog.V(4).InfoS("Deferring bootstrap replicas until a source is ready",
				"modelServing", klog.KObj(a.ms), "pool", key, "requested", count,
				"inFlight", pool.inFlight, "seedBudget", budget, "blocked", pool.blocked)
			return false
		}
	}
	for key, count := range unit {
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
