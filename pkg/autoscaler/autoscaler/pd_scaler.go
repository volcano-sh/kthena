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
	"fmt"
	"math"
	"strconv"
	"strings"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	corev1 "k8s.io/api/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type PDDisaggregatedAutoscaler struct {
	PrefillCollector *MetricCollector
	DecodeCollector  *MetricCollector
	Status           *Status
	Meta             *PDDisaggregatedScalingMeta
}

type PDDisaggregatedScalingMeta struct {
	Config        *workload.PDDisaggregatedTarget
	PrefillTarget *workload.Target
	DecodeTarget  *workload.Target
	Generations
}

func NewPDDisaggregatedAutoscaler(autoscalePolicy *workload.AutoscalingPolicy, binding *workload.AutoscalingPolicyBinding) *PDDisaggregatedAutoscaler {
	pdTarget := binding.Spec.PDDisaggregatedTarget
	prefillTarget := buildPDRoleMetricTarget(pdTarget, pdTarget.PrefillRole)
	decodeTarget := buildPDRoleMetricTarget(pdTarget, pdTarget.DecodeRole)
	metricTargets := GetMetricTargets(autoscalePolicy)

	return &PDDisaggregatedAutoscaler{
		PrefillCollector: NewMetricCollector(prefillTarget, binding, metricTargets),
		DecodeCollector:  NewMetricCollector(decodeTarget, binding, metricTargets),
		Status:           NewStatus(&autoscalePolicy.Spec.Behavior),
		Meta: &PDDisaggregatedScalingMeta{
			Config:        pdTarget,
			PrefillTarget: prefillTarget,
			DecodeTarget:  decodeTarget,
			Generations: Generations{
				AutoscalePolicyGeneration: autoscalePolicy.Generation,
				BindingGeneration:         binding.Generation,
			},
		},
	}
}

func (autoscaler *PDDisaggregatedAutoscaler) NeedUpdate(autoscalePolicy *workload.AutoscalingPolicy, binding *workload.AutoscalingPolicyBinding) bool {
	return autoscaler.Meta.Generations.AutoscalePolicyGeneration != autoscalePolicy.Generation ||
		autoscaler.Meta.Generations.BindingGeneration != binding.Generation
}

func (autoscaler *PDDisaggregatedAutoscaler) Scale(ctx context.Context, podLister listerv1.PodLister, autoscalePolicy *workload.AutoscalingPolicy, currentPrefillReplicas int32, currentDecodeReplicas int32) (int32, int32, string, error) {
	prefillUnready, prefillMetrics, err := autoscaler.PrefillCollector.UpdateMetrics(ctx, podLister)
	if err != nil {
		return -1, -1, "", err
	}
	decodeUnready, decodeMetrics, err := autoscaler.DecodeCollector.UpdateMetrics(ctx, podLister)
	if err != nil {
		return -1, -1, "", err
	}

	combinedMetrics := mergeMetrics(prefillMetrics, decodeMetrics)
	currentTotal := currentPrefillReplicas + currentDecodeReplicas
	minTotal := autoscaler.Meta.Config.PrefillRole.MinReplicas + autoscaler.Meta.Config.DecodeRole.MinReplicas
	maxTotal := autoscaler.Meta.Config.PrefillRole.MaxReplicas + autoscaler.Meta.Config.DecodeRole.MaxReplicas

	instancesAlgorithm := algorithm.RecommendedInstancesAlgorithm{
		MinInstances:          minTotal,
		MaxInstances:          maxTotal,
		CurrentInstancesCount: currentTotal,
		Tolerance:             float64(autoscalePolicy.Spec.TolerancePercent) * 0.01,
		MetricTargets:         autoscaler.PrefillCollector.MetricTargets,
		UnreadyInstancesCount: prefillUnready + decodeUnready,
		ReadyInstancesMetrics: []algorithm.Metrics{combinedMetrics},
		ExternalMetrics:       make(algorithm.Metrics),
	}
	recommendedInstances, skip := instancesAlgorithm.GetRecommendedInstances()
	if skip {
		klog.InfoS("skip recommended instances for pd disaggregated target")
		return -1, -1, "", nil
	}

	if autoscalePolicy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent != nil &&
		recommendedInstances*100 >= currentTotal*(*autoscalePolicy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent) {
		autoscaler.Status.RefreshPanicMode()
	}

	correctedAlgorithm := algorithm.CorrectedInstancesAlgorithm{
		IsPanic:              autoscaler.Status.IsPanicMode(),
		History:              autoscaler.Status.History,
		Behavior:             &autoscalePolicy.Spec.Behavior,
		MinInstances:         minTotal,
		MaxInstances:         maxTotal,
		CurrentInstances:     currentTotal,
		RecommendedInstances: recommendedInstances,
	}
	correctedTotal := correctedAlgorithm.GetCorrectedInstances()
	autoscaler.Status.AppendRecommendation(recommendedInstances)
	autoscaler.Status.AppendCorrected(correctedTotal)

	prefillReplicas, decodeReplicas, err := SplitPDReplicas(
		correctedTotal,
		autoscaler.Meta.Config.PrefillDecodeRatio,
		autoscaler.Meta.Config.PrefillRole.MinReplicas,
		autoscaler.Meta.Config.PrefillRole.MaxReplicas,
		autoscaler.Meta.Config.DecodeRole.MinReplicas,
		autoscaler.Meta.Config.DecodeRole.MaxReplicas,
		currentPrefillReplicas,
		currentDecodeReplicas,
	)
	if err != nil {
		return -1, -1, "", err
	}

	effectiveRatio := fmt.Sprintf("%d:%d", prefillReplicas, decodeReplicas)
	return prefillReplicas, decodeReplicas, effectiveRatio, nil
}

func buildPDRoleMetricTarget(pdTarget *workload.PDDisaggregatedTarget, role workload.PDRoleTarget) *workload.Target {
	metricEndpoint := role.MetricEndpoint
	if metricEndpoint.Uri == "" {
		metricEndpoint.Uri = "/metrics"
	}
	if metricEndpoint.Port == 0 {
		metricEndpoint.Port = 8100
	}
	return &workload.Target{
		TargetRef: corev1.ObjectReference{
			Kind: workload.ModelServingKind.Kind,
			Name: pdTarget.ModelServingRef.Name,
		},
		SubTarget: &workload.SubTarget{
			Kind: util.ModelServingRoleKind,
			Name: role.RoleName,
		},
		MetricEndpoint: metricEndpoint,
	}
}

func parsePrefillDecodeRatio(ratio string, currentPrefill int32, currentDecode int32) (int32, int32, error) {
	trimmed := strings.TrimSpace(ratio)
	if trimmed == "" {
		if currentPrefill > 0 || currentDecode > 0 {
			return max(currentPrefill, 1), max(currentDecode, 1), nil
		}
		return 1, 1, nil
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid prefillDecodeRatio %q, expected format P:D", ratio)
	}
	prefillRatio, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil || prefillRatio <= 0 {
		return 0, 0, fmt.Errorf("invalid prefillDecodeRatio %q, prefill ratio must be > 0", ratio)
	}
	decodeRatio, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || decodeRatio <= 0 {
		return 0, 0, fmt.Errorf("invalid prefillDecodeRatio %q, decode ratio must be > 0", ratio)
	}
	return int32(prefillRatio), int32(decodeRatio), nil
}

func SplitPDReplicas(total int32, ratio string, prefillMin int32, prefillMax int32, decodeMin int32, decodeMax int32, currentPrefill int32, currentDecode int32) (int32, int32, error) {
	if prefillMin > prefillMax {
		return 0, 0, fmt.Errorf("prefill minReplicas(%d) cannot be greater than maxReplicas(%d)", prefillMin, prefillMax)
	}
	if decodeMin > decodeMax {
		return 0, 0, fmt.Errorf("decode minReplicas(%d) cannot be greater than maxReplicas(%d)", decodeMin, decodeMax)
	}

	minTotal := prefillMin + decodeMin
	maxTotal := prefillMax + decodeMax
	total = min(max(total, minTotal), maxTotal)

	prefillRatio, decodeRatio, err := parsePrefillDecodeRatio(ratio, currentPrefill, currentDecode)
	if err != nil {
		return 0, 0, err
	}

	ratioSum := float64(prefillRatio + decodeRatio)
	prefill := int32(math.Ceil(float64(total) * float64(prefillRatio) / ratioSum))
	decode := total - prefill

	prefill = min(max(prefill, prefillMin), prefillMax)
	decode = min(max(decode, decodeMin), decodeMax)

	assigned := prefill + decode
	if assigned < total {
		remaining := total - assigned
		addToDecode := min(remaining, decodeMax-decode)
		decode += addToDecode
		remaining -= addToDecode
		if remaining > 0 {
			prefill += min(remaining, prefillMax-prefill)
		}
	} else if assigned > total {
		over := assigned - total
		reduceFromDecode := min(over, decode-decodeMin)
		decode -= reduceFromDecode
		over -= reduceFromDecode
		if over > 0 {
			prefill -= min(over, prefill-prefillMin)
		}
	}

	return prefill, decode, nil
}

func mergeMetrics(metricsList ...algorithm.Metrics) algorithm.Metrics {
	merged := make(algorithm.Metrics)
	for _, metrics := range metricsList {
		for name, value := range metrics {
			merged[name] += value
		}
	}
	return merged
}
