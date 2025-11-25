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

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type Optimizer struct {
	Meta       *OptimizerMeta
	Collectors map[string]*MetricCollector
	Status     *Status
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
    if binding.Spec.OptimizerConfiguration == nil {
        klog.Warningf("OptimizerConfig not configured in binding: %s", binding.Name)
        return nil
    }
    costExpansionRatePercent := binding.Spec.OptimizerConfiguration.CostExpansionRatePercent
	minReplicas := int32(0)
	maxReplicas := int32(0)
	var scalingOrder []*ReplicaBlock
    for index, param := range binding.Spec.OptimizerConfiguration.Params {
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
        Config:       binding.Spec.OptimizerConfiguration,
		MinReplicas:  minReplicas,
		MaxReplicas:  maxReplicas,
		ScalingOrder: scalingOrder,
		Scope: Scope{
			OwnedBindingId: binding.UID,
			Namespace:      binding.Namespace,
		},
	}
}

func NewOptimizer(behavior *workload.AutoscalingPolicyBehavior, binding *workload.AutoscalingPolicyBinding, metricTargets map[string]float64) *Optimizer {
	collectors := make(map[string]*MetricCollector)
    for _, param := range binding.Spec.OptimizerConfiguration.Params {
        collectors[param.Target.TargetRef.Name] = NewMetricCollector(&param.Target, binding, metricTargets)
    }

	meta := NewOptimizerMeta(binding)
	meta.MetricTargets = metricTargets
	return &Optimizer{
		Meta:       meta,
		Collectors: collectors,
		Status:     NewStatus(behavior),
	}
}

func (optimizer *Optimizer) Optimize(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentInstancesCounts map[string]int32) (map[string]int32, error) {
	size := len(optimizer.Meta.Config.Params)
	unreadyInstancesCount := int32(0)
	readyInstancesMetrics := make([]algorithm.Metrics, 0, size)
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
	return replicasMap, nil
}
