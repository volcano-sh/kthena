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
	"k8s.io/apimachinery/pkg/types"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type Autoscaler struct {
	Collector *MetricCollector
	Status    *Status
	Meta      *ScalingMeta
}
type ScalingMeta struct {
    Config        *workload.HomogeneousTarget
	MetricTargets map[string]float64
	BindingId     types.UID
	Namespace     string
}

func NewAutoscaler(behavior *workload.AutoscalingPolicyBehavior, binding *workload.AutoscalingPolicyBinding, metricTargets map[string]float64) *Autoscaler {
    return &Autoscaler{
        Status:    NewStatus(behavior),
        Collector: NewMetricCollector(&binding.Spec.ScalingConfiguration.Target, binding, metricTargets),
        Meta: &ScalingMeta{
            Config:        binding.Spec.ScalingConfiguration,
            BindingId:     binding.UID,
            Namespace:     binding.Namespace,
            MetricTargets: metricTargets,
        },
    }
}

func (autoscaler *Autoscaler) Scale(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentInstancesCount int32) (int32, error) {
	unreadyInstancesCount, readyInstancesMetrics, err := autoscaler.Collector.UpdateMetrics(ctx, podLister)
	if err != nil {
		klog.Errorf("update metrics error: %v", err)
		return 0, err
	}
	// minInstance <- AutoscaleScope, currentInstancesCount(replicas) <- workload
	instancesAlgorithm := algorithm.RecommendedInstancesAlgorithm{
		MinInstances:          autoscaler.Meta.Config.MinReplicas,
		MaxInstances:          autoscaler.Meta.Config.MaxReplicas,
		CurrentInstancesCount: currentInstancesCount,
		Tolerance:             float64(autoscalePolicy.Spec.TolerancePercent) * 0.01,
		MetricTargets:         autoscaler.Meta.MetricTargets,
		UnreadyInstancesCount: unreadyInstancesCount,
		ReadyInstancesMetrics: []algorithm.Metrics{readyInstancesMetrics},
		ExternalMetrics:       make(algorithm.Metrics),
	}
	recommendedInstances, skip := instancesAlgorithm.GetRecommendedInstances()
	if skip {
		klog.Warning("skip recommended instances")
		return 0, nil
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
	recommendedInstances = CorrectedInstancesAlgorithm.GetCorrectedInstances()

	klog.InfoS("autoscale controller", "recommendedInstances", recommendedInstances, "correctedInstances", recommendedInstances)
	autoscaler.Status.AppendRecommendation(recommendedInstances)
	autoscaler.Status.AppendCorrected(recommendedInstances)
	return recommendedInstances, nil
}
