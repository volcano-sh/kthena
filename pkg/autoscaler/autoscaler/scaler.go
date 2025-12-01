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

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type Autoscaler struct {
	Collector *MetricCollector
	Status    *Status
	Meta      *ScalingMeta
}

type ScalingMeta struct {
	Config    *workload.HomogeneousTarget
	Namespace string
	Generations
}

func NewAutoscaler(autoscalePolicy *workload.AutoscalingPolicy, binding *workload.AutoscalingPolicyBinding) *Autoscaler {
	return &Autoscaler{
		Status:    NewStatus(&autoscalePolicy.Spec.Behavior),
		Collector: NewMetricCollector(&binding.Spec.HomogeneousTarget.Target, binding, GetMetricTargets(autoscalePolicy)),
		Meta: &ScalingMeta{
			Config:    binding.Spec.HomogeneousTarget,
			Namespace: binding.Namespace,
			Generations: Generations{
				AutoscalePolicyGeneration: autoscalePolicy.Generation,
				BindingGeneration:         binding.Generation,
			},
		},
	}
}

func (autoscaler *Autoscaler) NeedUpdate(autoscalePolicy *workload.AutoscalingPolicy, binding *workload.AutoscalingPolicyBinding) bool {
	return autoscaler.Meta.Generations.AutoscalePolicyGeneration != autoscalePolicy.Generation ||
		autoscaler.Meta.Generations.BindingGeneration != binding.Generation
}

func (autoscaler *Autoscaler) UpdateAutoscalePolicy(autoscalePolicy *workload.AutoscalingPolicy) {
	if autoscaler.Meta.Generations.AutoscalePolicyGeneration == autoscalePolicy.Generation {
		return
	}
	autoscaler.Meta.Generations.AutoscalePolicyGeneration = autoscalePolicy.Generation
}

func (autoscaler *Autoscaler) Scale(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentInstancesCount int32) (int32, error) {
	unreadyInstancesCount, readyInstancesMetrics, err := autoscaler.Collector.UpdateMetrics(ctx, podLister)
	if err != nil {
		klog.Errorf("update metrics error: %v", err)
		return -1, err
	}
	// minInstance <- AutoscaleScope, currentInstancesCount(replicas) <- workload
	instancesAlgorithm := algorithm.RecommendedInstancesAlgorithm{
		MinInstances:          autoscaler.Meta.Config.MinReplicas,
		MaxInstances:          autoscaler.Meta.Config.MaxReplicas,
		CurrentInstancesCount: currentInstancesCount,
		Tolerance:             float64(autoscalePolicy.Spec.TolerancePercent) * 0.01,
		MetricTargets:         autoscaler.Collector.MetricTargets,
		UnreadyInstancesCount: unreadyInstancesCount,
		ReadyInstancesMetrics: []algorithm.Metrics{readyInstancesMetrics},
		ExternalMetrics:       make(algorithm.Metrics),
	}
	recommendedInstances, skip := instancesAlgorithm.GetRecommendedInstances()
	if skip {
		klog.InfoS("skip recommended instances")
		return -1, nil
	}
	if autoscalePolicy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent != nil && recommendedInstances*100 >= currentInstancesCount*(*autoscalePolicy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent) {
		autoscaler.Status.RefreshPanicMode()
	}
	CorrectedInstancesAlgorithm := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              autoscaler.Status.IsPanicMode(),
		History:              autoscaler.Status.History,
		Behavior:             &autoscalePolicy.Spec.Behavior,
		MinInstances:         autoscaler.Meta.Config.MinReplicas,
		MaxInstances:         autoscaler.Meta.Config.MaxReplicas,
		CurrentInstances:     currentInstancesCount,
		RecommendedInstances: recommendedInstances,
	}
	correctedInstances := CorrectedInstancesAlgorithm.GetCorrectedInstances()

	klog.InfoS("autoscale controller", "currentInstancesCount", currentInstancesCount, "recommendedInstances", recommendedInstances, "correctedInstances", correctedInstances)
	autoscaler.Status.AppendRecommendation(recommendedInstances)
	autoscaler.Status.AppendCorrected(correctedInstances)
	return correctedInstances, nil
}
