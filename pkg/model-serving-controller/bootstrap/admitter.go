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
	"cmp"
	"slices"

	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// PoolKey identifies replicas that can serve as sources for each other.
type PoolKey struct {
	Role             string
	RoleTemplateHash string
	ConfigHash       string
}

// Replica is an existing role replica that is not being deleted.
type Replica struct {
	Pool    PoolKey
	Running bool
}

// UnitEntry is the number of replicas of one role in a unit.
type UnitEntry struct {
	// Role is the template the replicas are rendered from.
	Role  workloadv1alpha1.Role
	Count int
}

// PoolStatus summarizes the admission state of one pool.
type PoolStatus struct {
	Pool     PoolKey
	Running  int
	Starting int
	Budget   int
	Admitted int
	Created  int
	Deferred int
}

type poolState struct {
	PoolStatus
	blocked bool
}

// Admitter decides which units of missing role replicas may be created in one
// reconcile. It is not safe for concurrent use.
type Admitter struct {
	ms          *workloadv1alpha1.ModelServing
	gangEnabled bool
	plugins     []workloadv1alpha1.PluginSpec
	configHash  map[string]string
	observed    map[PoolKey]PoolStatus
	pools       map[PoolKey]*poolState
}

// NewAdmitter returns an Admitter for ms, or nil when acceleration is
// disabled. A nil Admitter admits every unit. gangEnabled reports whether
// the ServingGroups of ms are gang scheduled through a PodGroup.
func NewAdmitter(ms *workloadv1alpha1.ModelServing, gangEnabled bool, replicas []Replica) (*Admitter, error) {
	if !Enabled(ms) {
		return nil, nil
	}
	plugins, err := utils.NormalizePluginSpecs(ms.Spec.Plugins)
	if err != nil {
		return nil, err
	}
	a := &Admitter{
		ms:          ms,
		gangEnabled: gangEnabled,
		plugins:     plugins,
		configHash:  make(map[string]string),
		observed:    make(map[PoolKey]PoolStatus),
		pools:       make(map[PoolKey]*poolState),
	}
	for _, replica := range replicas {
		status := a.observed[replica.Pool]
		if replica.Running {
			status.Running++
		} else {
			status.Starting++
		}
		a.observed[replica.Pool] = status
	}
	return a, nil
}

// InScope reports whether replicas of the role named roleName are gated.
func (a *Admitter) InScope(roleName string) bool {
	return a != nil && InScope(a.ms, roleName)
}

// GangMin returns the number of replicas of role that one ServingGroup starts together.
func (a *Admitter) GangMin(role workloadv1alpha1.Role) int {
	replicas := roleReplicas(role)
	if !a.gangEnabled {
		return min(replicas, 1)
	}
	minReplicas := replicas
	if policy := a.ms.Spec.Template.GangPolicy; policy != nil {
		if value, ok := policy.MinRoleReplicas[role.Name]; ok {
			minReplicas = int(value)
		}
	}
	return min(replicas, max(1, minReplicas))
}

// PoolFor returns the pool of replicas rendered from role.
func (a *Admitter) PoolFor(role workloadv1alpha1.Role) PoolKey {
	hash, ok := a.configHash[role.Name]
	if !ok {
		hash = configHash(a.ms, role.Name, a.plugins)
		a.configHash[role.Name] = hash
	}
	return PoolKey{
		Role:             role.Name,
		RoleTemplateHash: utils.CalRoleTemplateHash(role),
		ConfigHash:       hash,
	}
}

// Admit reports whether every in-scope pool touched by the unit has room for
// it. An admitted unit counts as starting; a denied unit blocks its pools for
// the rest of the reconcile so that smaller units never overtake it.
func (a *Admitter) Admit(entries []UnitEntry) bool {
	if a == nil {
		return true
	}
	need := make(map[PoolKey]int)
	states := make(map[PoolKey]*poolState)
	for _, entry := range entries {
		if entry.Count <= 0 || !a.InScope(entry.Role.Name) {
			continue
		}
		key := a.PoolFor(entry.Role)
		need[key] += entry.Count
		states[key] = a.pool(key, entry.Role)
	}

	admit := true
	for key, count := range need {
		state := states[key]
		if state.blocked || count > state.Budget-state.Starting {
			admit = false
		}
	}
	for key, count := range need {
		state := states[key]
		if admit {
			state.Starting += count
			state.Admitted += count
		} else {
			state.blocked = true
			state.Deferred += count
		}
	}
	return admit
}

// Created records replicas of an admitted unit that were created.
func (a *Admitter) Created(entries []UnitEntry) {
	if a == nil {
		return
	}
	for _, entry := range entries {
		if entry.Count <= 0 || !a.InScope(entry.Role.Name) {
			continue
		}
		key := a.PoolFor(entry.Role)
		a.pool(key, entry.Role).Created += entry.Count
	}
}

// Pools returns the status of the pools touched in this reconcile.
func (a *Admitter) Pools() []PoolStatus {
	if a == nil {
		return nil
	}
	pools := make([]PoolStatus, 0, len(a.pools))
	for _, state := range a.pools {
		pools = append(pools, state.PoolStatus)
	}
	slices.SortFunc(pools, func(x, y PoolStatus) int {
		return cmp.Or(
			cmp.Compare(x.Pool.Role, y.Pool.Role),
			cmp.Compare(x.Pool.RoleTemplateHash, y.Pool.RoleTemplateHash),
			cmp.Compare(x.Pool.ConfigHash, y.Pool.ConfigHash),
		)
	})
	return pools
}

func (a *Admitter) pool(key PoolKey, role workloadv1alpha1.Role) *poolState {
	if state, ok := a.pools[key]; ok {
		return state
	}
	observed := a.observed[key]
	state := &poolState{PoolStatus: PoolStatus{
		Pool:     key,
		Running:  observed.Running,
		Starting: observed.Starting,
	}}
	gang := a.GangMin(role)
	if observed.Running > 0 {
		state.Budget = max(gang, a.sourceFanOut()*observed.Running)
	} else {
		desired := modelServingReplicas(a.ms) * roleReplicas(role)
		state.Budget = min(desired, max(gang, a.seedReplicas(desired)))
	}
	a.pools[key] = state
	return state
}

func (a *Admitter) seedReplicas(desired int) int {
	seed := a.ms.Spec.BootstrapAccelerateStrategy.SeedReplicas
	if seed == nil {
		return 1
	}
	value, err := intstr.GetScaledValueFromIntOrPercent(seed, desired, true)
	if err != nil {
		// The API server validates seedReplicas, so fall back to its default.
		return 1
	}
	return value
}

func (a *Admitter) sourceFanOut() int {
	fanOut := a.ms.Spec.BootstrapAccelerateStrategy.SourceFanOut
	if fanOut == nil || *fanOut < 1 {
		return 1
	}
	return int(*fanOut)
}

func modelServingReplicas(ms *workloadv1alpha1.ModelServing) int {
	if ms.Spec.Replicas == nil {
		return 0
	}
	return int(*ms.Spec.Replicas)
}

func roleReplicas(role workloadv1alpha1.Role) int {
	if role.Replicas == nil {
		return 1
	}
	return int(*role.Replicas)
}
