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

func NewAutoscaler(autoscalePolicy *workload.AutoscalingPolicy) *Autoscaler {
	return &Autoscaler{
		Status:    NewStatus(&autoscalePolicy.Spec.Behavior),
		Collector: NewMetricCollector(&autoscalePolicy.Spec.HomogeneousTarget.Target, autoscalePolicy, GetMetricTargets(autoscalePolicy)),
		Meta: &ScalingMeta{
			Config:    autoscalePolicy.Spec.HomogeneousTarget,
			Namespace: autoscalePolicy.Namespace,
			Generations: Generations{
				AutoscalePolicyGeneration: autoscalePolicy.Generation,
			},
		},
	}
}

func (autoscaler *Autoscaler) NeedUpdate(autoscalePolicy *workload.AutoscalingPolicy) bool {
	return autoscaler.Meta.Generations.AutoscalePolicyGeneration != autoscalePolicy.Generation
}

func (autoscaler *Autoscaler) UpdateAutoscalePolicy(autoscalePolicy *workload.AutoscalingPolicy) {
	if autoscaler.Meta.Generations.AutoscalePolicyGeneration == autoscalePolicy.Generation {
		return
	}
	autoscaler.Meta.Generations.AutoscalePolicyGeneration = autoscalePolicy.Generation
}

func (autoscaler *Autoscaler) Scale(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentInstancesCount int32) (int32, error) {
	unreadyInstancesCount, readyInstancesMetrics, externalMetrics, err := autoscaler.Collector.UpdateMetrics(ctx, podLister, autoscaler.Meta.Config.Target.MetricSources)
	if err != nil {
		klog.Errorf("update metrics error: %v", err)
		return -1, err
	}
	result := scaleOneTarget(scaleOneTargetInput{
		Status:                autoscaler.Status,
		Behavior:              &autoscalePolicy.Spec.Behavior,
		MinReplicas:           autoscaler.Meta.Config.MinReplicas,
		MaxReplicas:           autoscaler.Meta.Config.MaxReplicas,
		CurrentReplicas:       currentInstancesCount,
		TolerancePercent:      autoscalePolicy.Spec.TolerancePercent,
		MetricTargets:         autoscaler.Collector.MetricTargets,
		UnreadyInstancesCount: unreadyInstancesCount,
		ReadyInstancesMetrics: readyInstancesMetrics,
		ExternalMetrics:       externalMetrics,
	})
	if result.Skip {
		klog.InfoS("skip recommended instances")
		return -1, nil
	}
	recordScaleOneTargetResult(autoscaler.Status, result)

	klog.InfoS("autoscale controller", "currentInstancesCount", currentInstancesCount, "recommendedInstances", result.RecommendedReplicas, "correctedInstances", result.CorrectedReplicas)
	return result.CorrectedReplicas, nil
}

type scaleOneTargetInput struct {
	Status                *Status
	Behavior              *workload.AutoscalingPolicyBehavior
	MinReplicas           int32
	MaxReplicas           int32
	CurrentReplicas       int32
	TolerancePercent      int32
	MetricTargets         algorithm.Metrics
	UnreadyInstancesCount int32
	ReadyInstancesMetrics algorithm.Metrics
	ExternalMetrics       algorithm.Metrics
}

type scaleOneTargetResult struct {
	RecommendedReplicas int32
	CorrectedReplicas   int32
	RefreshPanicMode    bool
	Skip                bool
}

func scaleOneTarget(input scaleOneTargetInput) scaleOneTargetResult {
	instancesAlgorithm := algorithm.RecommendedInstancesAlgorithm{
		MinInstances:          input.MinReplicas,
		MaxInstances:          input.MaxReplicas,
		CurrentInstancesCount: input.CurrentReplicas,
		Tolerance:             float64(input.TolerancePercent) * 0.01,
		MetricTargets:         input.MetricTargets,
		UnreadyInstancesCount: input.UnreadyInstancesCount,
		ReadyInstancesMetrics: []algorithm.Metrics{input.ReadyInstancesMetrics},
		ExternalMetrics:       input.ExternalMetrics,
	}
	recommendedReplicas, skip := instancesAlgorithm.GetRecommendedInstances()
	if skip {
		return scaleOneTargetResult{Skip: true}
	}

	isPanic := input.Status.IsPanicMode()
	refreshPanicMode := false
	if input.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent != nil && recommendedReplicas*100 >= input.CurrentReplicas*(*input.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent) {
		refreshPanicMode = true
		isPanic = input.Status.PanicModeHoldMilliseconds > 0
	}
	correctedAlgorithm := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              isPanic,
		History:              input.Status.History,
		Behavior:             input.Behavior,
		MinInstances:         input.MinReplicas,
		MaxInstances:         input.MaxReplicas,
		CurrentInstances:     input.CurrentReplicas,
		RecommendedInstances: recommendedReplicas,
	}
	correctedReplicas := correctedAlgorithm.GetCorrectedInstances()

	return scaleOneTargetResult{
		RecommendedReplicas: recommendedReplicas,
		CorrectedReplicas:   correctedReplicas,
		RefreshPanicMode:    refreshPanicMode,
	}
}

func recordScaleOneTargetResult(status *Status, result scaleOneTargetResult) {
	if result.RefreshPanicMode {
		status.RefreshPanicMode()
	}
	status.AppendRecommendation(result.RecommendedReplicas)
	status.AppendCorrected(result.CorrectedReplicas)
}
