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

// PoolKey identifies the role replicas that can serve each other as weight
// sources. It mirrors MX_MODEL_REVISION, which the managed plugin derives from
// the ModelServing and the role template hash, so the controller and
// ModelExpress agree on compatibility.
type PoolKey struct {
	Role         string
	TemplateHash string
}

func (k PoolKey) String() string {
	return fmt.Sprintf("%s/%s", k.Role, k.TemplateHash)
}

// GroupObservation summarizes one live (non-deleting) ServingGroup for
// bootstrap admission.
type GroupObservation struct {
	// Pools lists the in-scope role pools associated with this ServingGroup.
	// Duplicates within one group are counted once toward in-flight seed groups.
	Pools []PoolKey
	// ReadyReplicas lists one PoolKey per RoleRunning replica in this group.
	ReadyReplicas []PoolKey
}

type poolState struct {
	ready          int
	inFlightGroups int
	admittedGroups int
}

// PoolSummary reports one pool's ServingGroup admissions in a reconcile.
type PoolSummary struct {
	Key            PoolKey
	Ready          int
	AdmittedGroups int
}

// Admitter decides, for one reconcile, which missing ServingGroups may be
// created now. When every in-scope role pool of a candidate ServingGroup has a
// Ready source replica, the group is admitted immediately. When any touched
// pool has no Ready source, at most SeedBudget() whole ServingGroups may be in
// flight for that pool. It is not safe for concurrent use.
type Admitter struct {
	ms    *workloadv1alpha1.ModelServing
	pools map[PoolKey]*poolState
}

// NewAdmitter builds the ServingGroup admission state from observed live
// ServingGroups. It returns nil when ms does not use bootstrap acceleration.
func NewAdmitter(ms *workloadv1alpha1.ModelServing, groups []GroupObservation) *Admitter {
	if !Enabled(ms) {
		return nil
	}
	a := &Admitter{
		ms:    ms,
		pools: make(map[PoolKey]*poolState),
	}
	for _, group := range groups {
		seenPools := make(map[PoolKey]struct{}, len(group.Pools))
		for _, key := range group.Pools {
			if key.Role == "" || key.TemplateHash == "" {
				continue
			}
			if _, dup := seenPools[key]; dup {
				continue
			}
			seenPools[key] = struct{}{}
			a.pool(key).inFlightGroups++
		}
		for _, key := range group.ReadyReplicas {
			if key.Role == "" || key.TemplateHash == "" {
				continue
			}
			a.pool(key).ready++
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
	return InScope(a.ms, roleName)
}

// PoolFor returns the pool new replicas of roleName in workload join.
func PoolFor(workload *workloadv1alpha1.ModelServing, roleName string) PoolKey {
	if workload == nil {
		return PoolKey{Role: roleName}
	}
	hash, err := utils.RoleRevisionHash(workload, roleName)
	if err != nil {
		for _, role := range workload.Spec.Template.Roles {
			if role.Name == roleName {
				return PoolKey{Role: roleName, TemplateHash: utils.CalRoleTemplateHash(role)}
			}
		}
		return PoolKey{Role: roleName}
	}
	return PoolKey{Role: roleName, TemplateHash: hash}
}

// SeedBudget is the maximum number of ServingGroups that may be in flight for a
// cold pool (R == 0): min(D, max(1, initialSeedReplicas)) when D > 0, else 0.
func (a *Admitter) SeedBudget() int {
	if a == nil {
		return 0
	}
	desired := desiredGroups(a.ms)
	if desired <= 0 {
		return 0
	}
	seed := 1
	if configured := a.ms.Spec.BootstrapAccelerateStrategy.InitialSeedReplicas; configured != nil {
		if value, err := intstr.GetScaledValueFromIntOrPercent(configured, desired, true); err == nil {
			seed = value
		}
	}
	return min(desired, max(1, seed))
}

// AdmitGroup decides whether a whole ServingGroup for workload may be created
// now. All touched pools are checked atomically before reserving capacity.
func (a *Admitter) AdmitGroup(workload *workloadv1alpha1.ModelServing) bool {
	if a == nil || workload == nil {
		return true
	}
	touched := a.touchedPools(workload)
	if len(touched) == 0 {
		return true
	}
	budget := a.SeedBudget()
	for _, key := range touched {
		pool := a.pool(key)
		if pool.ready > 0 {
			continue
		}
		if pool.inFlightGroups+1 > budget {
			klog.V(4).InfoS("Deferring bootstrap ServingGroup until a source is ready",
				"modelServing", klog.KObj(a.ms), "pool", key,
				"inFlightGroups", pool.inFlightGroups, "seedBudget", budget)
			return false
		}
	}
	for _, key := range touched {
		pool := a.pool(key)
		pool.inFlightGroups++
		pool.admittedGroups++
	}
	return true
}

func (a *Admitter) touchedPools(workload *workloadv1alpha1.ModelServing) []PoolKey {
	var touched []PoolKey
	seen := make(map[PoolKey]struct{}, len(workload.Spec.Template.Roles))
	for _, role := range workload.Spec.Template.Roles {
		if !a.InScope(role.Name) || roleReplicas(role) <= 0 {
			continue
		}
		key := PoolFor(workload, role.Name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		touched = append(touched, key)
	}
	return touched
}

// Summary lists pools touched by ServingGroups admitted in this reconcile.
func (a *Admitter) Summary() []PoolSummary {
	if a == nil {
		return nil
	}
	var result []PoolSummary
	for key, pool := range a.pools {
		if pool.admittedGroups > 0 {
			result = append(result, PoolSummary{
				Key:            key,
				Ready:          pool.ready,
				AdmittedGroups: pool.admittedGroups,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key.String() < result[j].Key.String() })
	return result
}

func desiredGroups(ms *workloadv1alpha1.ModelServing) int {
	if ms == nil {
		return 0
	}
	if ms.Spec.Replicas == nil {
		return 1
	}
	return int(*ms.Spec.Replicas)
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
