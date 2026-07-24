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
	"sort"
	"strings"

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

func NewOptimizerMeta(policy *workload.AutoscalingPolicy) *OptimizerMeta {
	if policy.Spec.HeterogeneousTarget == nil {
		klog.Warningf("OptimizerConfig not configured in policy: %s", policy.Name)
		return nil
	}
	costExpansionRatePercent := policy.Spec.HeterogeneousTarget.CostExpansionRatePercent
	minReplicas := int32(0)
	maxReplicas := int32(0)
	var scalingOrder []*ReplicaBlock
	for index, param := range policy.Spec.HeterogeneousTarget.Params {
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
		Config:       policy.Spec.HeterogeneousTarget,
		MinReplicas:  minReplicas,
		MaxReplicas:  maxReplicas,
		ScalingOrder: scalingOrder,
		Scope: Scope{
			OwnedPolicyId: policy.UID,
			Namespace:     policy.Namespace,
		},
	}
}

func NewOptimizer(autoscalePolicy *workload.AutoscalingPolicy) *Optimizer {
	metricTargets := GetMetricTargets(autoscalePolicy)
	collectors := make(map[string]*MetricCollector)
	for _, param := range autoscalePolicy.Spec.HeterogeneousTarget.Params {
		collectors[param.Target.TargetRef.Name] = NewMetricCollector(&param.Target, autoscalePolicy, metricTargets)
	}

	meta := NewOptimizerMeta(autoscalePolicy)
	meta.MetricTargets = metricTargets
	return &Optimizer{
		Meta:       meta,
		Collectors: collectors,
		Status:     NewStatus(&autoscalePolicy.Spec.Behavior),
		Generations: Generations{
			AutoscalePolicyGeneration: autoscalePolicy.Generation,
		},
	}
}

func (optimizer *Optimizer) NeedUpdate(policy *workload.AutoscalingPolicy) bool {
	return optimizer.Generations.AutoscalePolicyGeneration != policy.Generation
}

func (optimizer *Optimizer) Optimize(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentInstancesCounts map[string]int32) (map[string]int32, error) {
	size := len(optimizer.Meta.Config.Params)
	unreadyInstancesCount := int32(0)
	readyInstancesMetrics := make([]algorithm.Metrics, 0, size)
	// externalSamples accumulates per-backend (value, replicas) pairs for each
	// external metric so that the correct aggregation can be applied afterwards.
	externalSamples := make(map[string][]backendExternalSample)
	instancesCountSum := int32(0)
	// Update all model serving instances' metrics
	for _, param := range optimizer.Meta.Config.Params {
		collector, exists := optimizer.Collectors[param.Target.TargetRef.Name]
		if !exists {
			klog.Warningf("collector for target %s not exists", param.Target.TargetRef.Name)
			continue
		}

		backendReplicas := currentInstancesCounts[param.Target.TargetRef.Name]
		instancesCountSum += backendReplicas
		currentUnreadyInstancesCount, currentReadyInstancesMetrics, currentExternalMetrics, err := collector.UpdateMetrics(ctx, podLister, param.Target.MetricSources)
		if err != nil {
			klog.Warningf("update metrics error: %v", err)
			continue
		}
		unreadyInstancesCount += currentUnreadyInstancesCount
		readyInstancesMetrics = append(readyInstancesMetrics, currentReadyInstancesMetrics)
		for metricName, metricValue := range currentExternalMetrics {
			externalSamples[metricName] = append(externalSamples[metricName], backendExternalSample{
				value:    metricValue,
				replicas: backendReplicas,
			})
		}
	}
	// Aggregate external metrics using the semantics appropriate for each metric
	// type: additive metrics are summed; ratio metrics use a replica-weighted
	// average so that heterogeneous backends (different replica counts) do not
	// distort the result.
	externalMetrics := make(algorithm.Metrics, len(externalSamples))
	for name, samples := range externalSamples {
		externalMetrics[name] = aggregateExternalSamples(name, samples)
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
		ExternalMetrics:       externalMetrics,
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
	return replicasMap, nil
}

// backendExternalSample pairs an external metric value with the replica count
// of the backend that reported it.
type backendExternalSample struct {
	value    float64
	replicas int32
}

// isRatioMetric reports whether name represents a ratio (bounded) metric that
// must be aggregated as a weighted average across backends rather than summed.
// Recognised suffixes: _utilization, _usage, _ratio, _percent, _saturation.
// The "rate" suffix is intentionally excluded: throughput and request-rate
// metrics are additive across backends.
func isRatioMetric(name string) bool {
	lowerName := strings.ToLower(name)
	for _, sfx := range []string{"_utilization", "_usage", "_ratio", "_percent", "_saturation"} {
		if strings.HasSuffix(lowerName, sfx) {
			return true
		}
	}
	return false
}

// aggregateExternalSamples combines per-backend metric samples into a single
// value using the semantics appropriate for the metric:
//
//   - Additive metrics (queue length, request count, …): sum across backends.
//   - Ratio metrics (utilization, usage, …): replica-count weighted average,
//     Σ(value_i × replicas_i) / Σ(replicas_i), matching Kubernetes HPA
//     semantics for cross-instance metric aggregation.
//
// When all backend replica counts are zero the function falls back to a plain
// unweighted average so that ratio metrics are never silently dropped.
func aggregateExternalSamples(name string, samples []backendExternalSample) float64 {
	if !isRatioMetric(name) {
		total := 0.0
		for _, s := range samples {
			total += s.value
		}
		return total
	}

	// Weighted average: Σ(value_i × replicas_i) / Σ(replicas_i)
	weightedSum := 0.0
	totalReplicas := int32(0)
	for _, s := range samples {
		weightedSum += s.value * float64(s.replicas)
		totalReplicas += s.replicas
	}
	if totalReplicas == 0 {
		// Replica counts are unavailable; fall back to unweighted average so
		// the metric still influences scaling decisions.
		sum := 0.0
		for _, s := range samples {
			sum += s.value
		}
		return sum / float64(len(samples))
	}
	return weightedSum / float64(totalReplicas)
}
