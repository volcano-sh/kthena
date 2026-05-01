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
	"context"
	"math"
	"sort"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type Optimizer struct {
	Meta       *OptimizerMeta
	Collectors map[string]*MetricCollector
	Status     *Status
	Generations
}

type OptimizerMeta struct {
	Config        *workload.HeterogeneousTarget
	MetricTargets map[string]float64
	ScalingOrder  []*ReplicaBlock
	MinReplicas   int32
	MaxReplicas   int32
	Scope         Scope
}

type ReplicaBlock struct {
	name     string
	index    int32
	replicas int32
	cost     int64
}

func (meta *OptimizerMeta) RestoreReplicasOfEachBackend(replicas int32) map[string]int32 {
	replicasMap := make(map[string]int32, len(meta.Config.Params))
	for _, param := range meta.Config.Params {
		replicasMap[param.Target.TargetRef.Name] = param.MinReplicas
	}
	replicas = min(max(replicas, meta.MinReplicas), meta.MaxReplicas)
	replicas -= meta.MinReplicas
	for _, block := range meta.ScalingOrder {
		slot := min(replicas, block.replicas)
		replicasMap[block.name] += slot
		replicas -= slot
		if replicas <= 0 {
			break
		}
	}
	return replicasMap
}

func NewOptimizerMeta(binding *workload.AutoscalingPolicyBinding) *OptimizerMeta {
	if binding.Spec.HeterogeneousTarget == nil {
		klog.Warningf("OptimizerConfig not configured in binding: %s", binding.Name)
		return nil
	}
	costExpansionRatePercent := binding.Spec.HeterogeneousTarget.CostExpansionRatePercent
	minReplicas := int32(0)
	maxReplicas := int32(0)
	var scalingOrder []*ReplicaBlock
	for index, param := range binding.Spec.HeterogeneousTarget.Params {
		minReplicas += param.MinReplicas
		maxReplicas += param.MaxReplicas
		replicas := param.MaxReplicas - param.MinReplicas
		if replicas <= 0 {
			continue
		}
		if costExpansionRatePercent == 100 {
			scalingOrder = append(scalingOrder, &ReplicaBlock{
				index:    int32(index),
				name:     param.Target.TargetRef.Name,
				replicas: replicas,
				cost:     int64(param.Cost),
			})
			continue
		}
		packageLen := 1.0
		for replicas > 0 {
			currentLen := min(replicas, max(int32(packageLen), 1))
			scalingOrder = append(scalingOrder, &ReplicaBlock{
				name:     param.Target.TargetRef.Name,
				index:    int32(index),
				replicas: currentLen,
				cost:     int64(param.Cost) * int64(currentLen),
			})
			replicas -= currentLen
			packageLen = packageLen * float64(costExpansionRatePercent) / 100
		}
	}
	sort.Slice(scalingOrder, func(i, j int) bool {
		if scalingOrder[i].cost != scalingOrder[j].cost {
			return scalingOrder[i].cost < scalingOrder[j].cost
		}
		return scalingOrder[i].index < scalingOrder[j].index
	})
	return &OptimizerMeta{
		Config:       binding.Spec.HeterogeneousTarget,
		MinReplicas:  minReplicas,
		MaxReplicas:  maxReplicas,
		ScalingOrder: scalingOrder,
		Scope: Scope{
			OwnedBindingId: binding.UID,
			Namespace:      binding.Namespace,
		},
	}
}

func NewOptimizer(autoscalePolicy *workload.AutoscalingPolicy, binding *workload.AutoscalingPolicyBinding) *Optimizer {
	metricTargets := GetMetricTargets(autoscalePolicy)
	collectors := make(map[string]*MetricCollector)
	for _, param := range binding.Spec.HeterogeneousTarget.Params {
		collectors[param.Target.TargetRef.Name] = NewMetricCollector(&param.Target, binding, metricTargets)
	}

	meta := NewOptimizerMeta(binding)
	meta.MetricTargets = metricTargets
	return &Optimizer{
		Meta:       meta,
		Collectors: collectors,
		Status:     NewStatus(&autoscalePolicy.Spec.Behavior),
		Generations: Generations{
			AutoscalePolicyGeneration: autoscalePolicy.Generation,
			BindingGeneration:         binding.Generation,
		},
	}
}

func (optimizer *Optimizer) NeedUpdate(policy *workload.AutoscalingPolicy, binding *workload.AutoscalingPolicyBinding) bool {
	return optimizer.Generations.AutoscalePolicyGeneration != policy.Generation ||
		optimizer.Generations.BindingGeneration != binding.Generation
}

func (optimizer *Optimizer) Optimize(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentInstancesCounts map[string]int32) (map[string]int32, error) {
	size := len(optimizer.Meta.Config.Params)
	unreadyInstancesCount := int32(0)
	readyInstancesMetrics := make([]algorithm.Metrics, 0, size)
	// readyMetricsByName mirrors readyInstancesMetrics but keyed by target name
	// so coordination can correlate pressure signals with each role.
	readyMetricsByName := make(map[string]algorithm.Metrics, size)
	instancesCountSum := int32(0)
	// Update all model serving instances' metrics
	for _, param := range optimizer.Meta.Config.Params {
		collector, exists := optimizer.Collectors[param.Target.TargetRef.Name]
		if !exists {
			klog.Warningf("collector for target %s not exists", param.Target.TargetRef.Name)
			continue
		}

		instancesCountSum += currentInstancesCounts[param.Target.TargetRef.Name]
		currentUnreadyInstancesCount, currentReadyInstancesMetrics, err := collector.UpdateMetrics(ctx, podLister)
		if err != nil {
			klog.Warningf("update metrics error: %v", err)
			continue
		}
		unreadyInstancesCount += currentUnreadyInstancesCount
		readyInstancesMetrics = append(readyInstancesMetrics, currentReadyInstancesMetrics)
		readyMetricsByName[param.Target.TargetRef.Name] = currentReadyInstancesMetrics
	}
	// Get recommended replicas of all model serving instances
	instancesAlgorithm := algorithm.RecommendedInstancesAlgorithm{
		MinInstances:          optimizer.Meta.MinReplicas,
		MaxInstances:          optimizer.Meta.MaxReplicas,
		CurrentInstancesCount: instancesCountSum,
		Tolerance:             float64(autoscalePolicy.Spec.TolerancePercent) * 0.01,
		MetricTargets:         optimizer.Meta.MetricTargets,
		UnreadyInstancesCount: unreadyInstancesCount,
		ReadyInstancesMetrics: readyInstancesMetrics,
		ExternalMetrics:       make(algorithm.Metrics),
	}
	recommendedInstances, skip := instancesAlgorithm.GetRecommendedInstances()
	if skip {
		klog.Warning("skip recommended instances")
		return nil, nil
	}
	if recommendedInstances*100 >= instancesCountSum*(*autoscalePolicy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent) {
		optimizer.Status.RefreshPanicMode()
	}
	CorrectedInstancesAlgorithm := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              optimizer.Status.IsPanicMode(),
		History:              optimizer.Status.History,
		Behavior:             &autoscalePolicy.Spec.Behavior,
		MinInstances:         optimizer.Meta.MinReplicas,
		MaxInstances:         optimizer.Meta.MaxReplicas,
		CurrentInstances:     instancesCountSum,
		RecommendedInstances: recommendedInstances}
	recommendedInstances = CorrectedInstancesAlgorithm.GetCorrectedInstances()

	klog.InfoS("autoscale controller", "recommendedInstances", recommendedInstances, "correctedInstances", recommendedInstances)
	optimizer.Status.AppendRecommendation(recommendedInstances)
	optimizer.Status.AppendCorrected(recommendedInstances)

	replicasMap := optimizer.Meta.RestoreReplicasOfEachBackend(recommendedInstances)

	// PD-aware coordination (Phase 1): when enabled and not in panic mode,
	// bias the cost-based distribution using pressure signals and a soft
	// preferred-ratio band. Disabled by default — behavior is unchanged.
	if coord := optimizer.Meta.Config.Coordination; coord != nil &&
		coord.Mode == workload.CoordinationModePreferred &&
		!optimizer.Status.IsPanicMode() {
		replicasMap = optimizer.Meta.applyCoordination(coord, recommendedInstances, replicasMap, readyMetricsByName)
	}
	return replicasMap, nil
}

// pressureMetricNames are the Phase 1 placeholder signals the optimizer
// consults to estimate per-role load. They are read by name from whatever
// the configured MetricCollector exposes — there is no hard coupling: a
// missing metric simply contributes zero. Phase 2 will let users configure
// these per role; for now keeping them as a small constant set keeps the
// blast radius small and the diff reviewable.
var pressureMetricNames = []string{
	"queue_depth",          // prefill + decode: pending requests
	"kv_cache_utilization", // decode: KV-cache pressure (typically 0..1)
	"ttft",                 // prefill: time-to-first-token (seconds)
}

// coordinationAlpha is the fixed blend weight in Phase 1 (0.5 = equal cost
// and pressure influence). Made configurable in Phase 2.
const coordinationAlpha = 0.5

// applyCoordination biases the cost-based baseline allocation toward roles
// under pressure, clipped to a per-role preferred ratio band.
//
// Phase 1 simplifications worth flagging for reviewers:
//   - alpha is hardcoded.
//   - pressure metric names are a fixed set (see pressureMetricNames).
//   - infeasible bands (e.g. mins summing > 1) cause "drift" rather than an
//     error: the caller still gets a best-effort allocation.
//   - panic mode is handled by the caller (it bypasses this function), per
//     the proposal.
func (meta *OptimizerMeta) applyCoordination(
	coord *workload.Coordination,
	total int32,
	baseline map[string]int32,
	metricsByName map[string]algorithm.Metrics,
) map[string]int32 {
	params := meta.Config.Params
	n := len(params)
	if n == 0 || total <= 0 {
		return baseline
	}

	names := make([]string, 0, n)
	minReps := make(map[string]int32, n)
	maxReps := make(map[string]int32, n)
	for _, p := range params {
		nm := p.Target.TargetRef.Name
		names = append(names, nm)
		minReps[nm] = p.MinReplicas
		maxReps[nm] = p.MaxReplicas
	}

	costShare := costShareFromBaseline(names, baseline)
	pressureShare := pressureShareNormalized(names, metricsByName)
	if pressureShare == nil {
		// No usable pressure data — degrade gracefully to cost-only blend,
		// which is equivalent to the existing cost-based behavior.
		pressureShare = costShare
	}

	// Blend, clip into the preferred band, renormalize.
	raw := make(map[string]float64, n)
	for _, nm := range names {
		raw[nm] = (1-coordinationAlpha)*costShare[nm] + coordinationAlpha*pressureShare[nm]
	}
	for _, nm := range names {
		r, ok := coord.PreferredRatio[nm]
		if !ok {
			continue
		}
		lo, hi := float64(r.Min)/100.0, float64(r.Max)/100.0
		if raw[nm] < lo {
			raw[nm] = lo
		}
		if raw[nm] > hi {
			raw[nm] = hi
		}
	}
	var sumRaw float64
	for _, v := range raw {
		sumRaw += v
	}
	if sumRaw <= 0 {
		return baseline
	}
	for nm := range raw {
		raw[nm] /= sumRaw
	}

	// Convert to integers via largest-remainder so the sum is preserved,
	// then clamp to each param's Min/Max. Drift after clamping is allowed.
	return roundSharesToReplicas(names, raw, total, minReps, maxReps)
}

// costShareFromBaseline derives a per-role share in [0,1] summing to 1 from
// the cost-based replica baseline. If the baseline is empty, fall back to a
// uniform split so blending still has a sensible cost component.
func costShareFromBaseline(names []string, baseline map[string]int32) map[string]float64 {
	out := make(map[string]float64, len(names))
	var sum float64
	for _, nm := range names {
		sum += float64(baseline[nm])
	}
	if sum <= 0 {
		eq := 1.0 / float64(len(names))
		for _, nm := range names {
			out[nm] = eq
		}
		return out
	}
	for _, nm := range names {
		out[nm] = float64(baseline[nm]) / sum
	}
	return out
}

// pressureShareNormalized produces a per-role pressure share summing to 1.
//
// Each metric in pressureMetricNames is normalized *across roles
// independently* before being summed, so signals on different scales
// (counts, ratios, seconds) contribute equally instead of the largest-scale
// metric dominating. Returns nil when no role reports any usable signal —
// the caller falls back to cost-only.
func pressureShareNormalized(names []string, metricsByName map[string]algorithm.Metrics) map[string]float64 {
	contrib := make(map[string]float64, len(names))
	any := false
	for _, key := range pressureMetricNames {
		var sum float64
		vals := make(map[string]float64, len(names))
		for _, nm := range names {
			// Treat a missing per-role map as "no signal" rather than
			// indexing into a nil map and relying on Go's zero-value read.
			m, ok := metricsByName[nm]
			if !ok || m == nil {
				continue
			}
			v, ok := m[key]
			if !ok || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			vals[nm] = v
			sum += v
		}
		if sum <= 0 {
			continue
		}
		any = true
		for nm, v := range vals {
			contrib[nm] += v / sum
		}
	}
	if !any {
		return nil
	}
	var total float64
	for _, v := range contrib {
		total += v
	}
	if total <= 0 {
		return nil
	}
	out := make(map[string]float64, len(names))
	for _, nm := range names {
		out[nm] = contrib[nm] / total
	}
	return out
}

// roundSharesToReplicas converts fractional shares to integer replica counts
// via the largest-remainder method (preserving the total), then clamps each
// to its [min,max] band. If clamping drifts the total, that drift is
// accepted: Phase 1 prefers a feasible, predictable allocation over a
// retry/repair loop.
func roundSharesToReplicas(names []string, shares map[string]float64, total int32, minReps, maxReps map[string]int32) map[string]int32 {
	out := make(map[string]int32, len(names))
	type rem struct {
		name string
		r    float64
	}
	rems := make([]rem, 0, len(names))
	var assigned int32
	for _, nm := range names {
		v := shares[nm] * float64(total)
		floor := int32(v)
		out[nm] = floor
		assigned += floor
		rems = append(rems, rem{nm, v - float64(floor)})
	}
	// Stable + name tiebreak so equal remainders produce a deterministic
	// allocation (avoids flaky tests and unexplained run-to-run drift).
	sort.SliceStable(rems, func(i, j int) bool {
		if rems[i].r != rems[j].r {
			return rems[i].r > rems[j].r
		}
		return rems[i].name < rems[j].name
	})
	for i := 0; assigned < total && i < len(rems); i++ {
		out[rems[i].name]++
		assigned++
	}
	for _, nm := range names {
		if out[nm] < minReps[nm] {
			out[nm] = minReps[nm]
		} else if out[nm] > maxReps[nm] {
			out[nm] = maxReps[nm]
		}
	}
	return out
}
