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
	"sort"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

const (
	// ScaleModeStable reports that a role is using the normal stable scaling path.
	ScaleModeStable = "Stable"
	// ScaleModePanic reports that a role is temporarily using panic-mode scale-up rules.
	ScaleModePanic = "Panic"
)

// DisaggregatedAutoscaler scales roles in one ModelServing independently, then
// applies an optional cross-role ratio constraint before returning final replicas.
// Although the API was introduced for P/D disaggregation, it also supports a
// single role so operators can use the same role-level autoscaling path without
// configuring an artificial peer role.
type DisaggregatedAutoscaler struct {
	Meta       *DisaggregatedScalingMeta
	Collectors map[string]*MetricCollector
	Statuses   map[string]*Status
	Generations
}

// DisaggregatedScalingMeta stores immutable policy-derived configuration used by
// the scaler instance. A new scaler is created when the policy generation changes.
type DisaggregatedScalingMeta struct {
	Config        *workload.DisaggregatedTarget
	MetricTargets map[string]algorithm.Metrics
	Namespace     string
}

// RoleScaleResult records both metric-derived and final replica decisions for
// one role. FinalReplicas may differ from DesiredReplicas after ratio repair.
type RoleScaleResult struct {
	Name            string
	CurrentReplicas int32
	DesiredReplicas int32
	FinalReplicas   int32
	Mode            string
}

// DisaggregatedScaleResult is the controller-facing result of one disaggregated
// autoscaling cycle, including the role patch payload and status information.
type DisaggregatedScaleResult struct {
	Roles         []RoleScaleResult
	RatioStatus   *workload.RoleRatioStatus
	RatioAdjusted bool
}

// NewDisaggregatedAutoscaler builds per-role collectors and per-role scaling
// histories from a disaggregated autoscaling policy. Fixed roles still get an
// entry in the scaler metadata for status and ratio enforcement, but their
// metric target map is empty and Scale will skip collection for them.
func NewDisaggregatedAutoscaler(policy *workload.AutoscalingPolicy) *DisaggregatedAutoscaler {
	if policy == nil || policy.Spec.DisaggregatedTarget == nil {
		return nil
	}
	metricTargetsByRole := GetDisaggregatedMetricTargets(policy)
	collectors := make(map[string]*MetricCollector, len(policy.Spec.DisaggregatedTarget.Roles))
	statuses := make(map[string]*Status, len(policy.Spec.DisaggregatedTarget.Roles))

	for roleName, roleParam := range policy.Spec.DisaggregatedTarget.Roles {
		target := workload.Target{
			TargetRef:     policy.Spec.DisaggregatedTarget.TargetRef,
			MetricSources: metricSourcesForRole(roleName, roleParam.MetricSources),
		}
		collectors[roleName] = NewMetricCollector(&target, policy, metricTargetsByRole[roleName])
		statuses[roleName] = NewStatus(&policy.Spec.Behavior)
	}

	return &DisaggregatedAutoscaler{
		Meta: &DisaggregatedScalingMeta{
			Config:        policy.Spec.DisaggregatedTarget,
			MetricTargets: metricTargetsByRole,
			Namespace:     policy.Namespace,
		},
		Collectors: collectors,
		Statuses:   statuses,
		Generations: Generations{
			AutoscalePolicyGeneration: policy.Generation,
		},
	}
}

// NeedUpdate reports whether the cached scaler should be rebuilt for the latest policy.
func (autoscaler *DisaggregatedAutoscaler) NeedUpdate(policy *workload.AutoscalingPolicy) bool {
	return autoscaler.Generations.AutoscalePolicyGeneration != policy.Generation
}

// Scale computes final replicas for every role in a DisaggregatedTarget.
//
// The flow mirrors homogeneous scaling per role:
//  1. collect role-scoped metrics,
//  2. calculate a metric-derived recommendation,
//  3. apply per-role behavior/stabilization and panic-mode limits,
//  4. enforce the optional role ratio constraint across the corrected results.
//
// DesiredReplicas in the returned role results is the metric-derived value before
// ratio enforcement, while FinalReplicas is the value that should be patched to
// ModelServing.spec.template.roles[*].replicas.
func (autoscaler *DisaggregatedAutoscaler) Scale(ctx context.Context, podLister listerv1.PodLister, policy *workload.AutoscalingPolicy, currentReplicas map[string]int32) (*DisaggregatedScaleResult, error) {
	if autoscaler == nil || autoscaler.Meta == nil || autoscaler.Meta.Config == nil {
		return nil, fmt.Errorf("scaler is not properly initialized")
	}

	correctedReplicas := make(map[string]int32, len(autoscaler.Meta.Config.Roles))
	desiredReplicas := make(map[string]int32, len(autoscaler.Meta.Config.Roles))
	scaleResults := make(map[string]scaleOneTargetResult, len(autoscaler.Meta.Config.Roles))
	bounds := make(map[string]algorithm.ReplicaBounds, len(autoscaler.Meta.Config.Roles))
	roleNames := make([]string, 0, len(autoscaler.Meta.Config.Roles))

	// Build deterministic role order for stable status output and collect each
	// role's replica bounds for the later ratio projection.
	for roleName, roleParam := range autoscaler.Meta.Config.Roles {
		roleNames = append(roleNames, roleName)
		bounds[roleName] = algorithm.ReplicaBounds{Min: roleParam.MinReplicas, Max: roleParam.MaxReplicas}
	}
	sort.Strings(roleNames)

	for _, roleName := range roleNames {
		roleParam := autoscaler.Meta.Config.Roles[roleName]
		current := currentReplicas[roleName]
		if isFixedRole(roleParam) {
			// A fixed role is intentionally not autoscaled. This allows policies that
			// scale only one role while keeping peer roles at a constant size.
			desiredReplicas[roleName] = roleParam.MinReplicas
			correctedReplicas[roleName] = roleParam.MinReplicas
			continue
		}

		collector, exists := autoscaler.Collectors[roleName]
		if !exists {
			return nil, fmt.Errorf("collector for role %s not found", roleName)
		}
		status, exists := autoscaler.Statuses[roleName]
		if !exists {
			status = NewStatus(&policy.Spec.Behavior)
			autoscaler.Statuses[roleName] = status
		}

		// Metric sources are role-scoped before each collection so pod scraping is
		// automatically filtered to pods carrying the current role label.
		unreadyInstancesCount, readyInstancesMetrics, externalMetrics, err := collector.UpdateMetrics(ctx, podLister, metricSourcesForRole(roleName, roleParam.MetricSources))
		if err != nil {
			return nil, fmt.Errorf("update metrics for role %s: %w", roleName, err)
		}

		scaleResult := scaleOneTarget(scaleOneTargetInput{
			Status:                status,
			Behavior:              &policy.Spec.Behavior,
			MinReplicas:           roleParam.MinReplicas,
			MaxReplicas:           roleParam.MaxReplicas,
			CurrentReplicas:       current,
			TolerancePercent:      policy.Spec.TolerancePercent,
			MetricTargets:         collector.MetricTargets,
			UnreadyInstancesCount: unreadyInstancesCount,
			ReadyInstancesMetrics: readyInstancesMetrics,
			ExternalMetrics:       externalMetrics,
		})
		if scaleResult.Skip {
			klog.InfoS("skip disaggregated scaling because role metrics are unavailable", "role", roleName)
			return nil, nil
		}
		desiredReplicas[roleName] = scaleResult.RecommendedReplicas
		correctedReplicas[roleName] = scaleResult.CorrectedReplicas
		scaleResults[roleName] = scaleResult
	}

	// Ratio enforcement is intentionally applied after per-role behavior
	// correction, because it is the final cross-role guardrail before patching.
	finalReplicas, ratioAdjusted, currentRatio, err := algorithm.EnforceRoleRatio(correctedReplicas, bounds, autoscaler.Meta.Config.RatioConstraint)
	if err != nil {
		return nil, err
	}
	for roleName, scaleResult := range scaleResults {
		recordScaleOneTargetResult(autoscaler.Statuses[roleName], scaleResult)
	}

	// Build the status payload after ratio enforcement so the controller can show
	// both the metric-derived desired value and the final value that was patched.
	roles := make([]RoleScaleResult, 0, len(roleNames))
	for _, roleName := range roleNames {
		mode := ScaleModeStable
		if autoscaler.Statuses[roleName].IsPanicMode() {
			mode = ScaleModePanic
		}
		roles = append(roles, RoleScaleResult{
			Name:            roleName,
			CurrentReplicas: currentReplicas[roleName],
			DesiredReplicas: desiredReplicas[roleName],
			FinalReplicas:   finalReplicas[roleName],
			Mode:            mode,
		})
	}

	var ratioStatus *workload.RoleRatioStatus
	if constraint := autoscaler.Meta.Config.RatioConstraint; constraint != nil {
		ratioStatus = &workload.RoleRatioStatus{
			NumeratorRole:   constraint.NumeratorRole,
			DenominatorRole: constraint.DenominatorRole,
			CurrentRatio:    currentRatio,
		}
	}

	return &DisaggregatedScaleResult{
		Roles:         roles,
		RatioStatus:   ratioStatus,
		RatioAdjusted: ratioAdjusted,
	}, nil
}

// GetDisaggregatedMetricTargets resolves effective metric targets for each role.
// Role-level metrics override policy-level metrics; when a role omits metrics,
// it inherits spec.metrics. Fixed roles intentionally receive no metric targets
// because their min/max bounds already define the desired replica count.
func GetDisaggregatedMetricTargets(policy *workload.AutoscalingPolicy) map[string]algorithm.Metrics {
	metricTargetsByRole := make(map[string]algorithm.Metrics)
	if policy == nil || policy.Spec.DisaggregatedTarget == nil {
		return metricTargetsByRole
	}
	for roleName, roleParam := range policy.Spec.DisaggregatedTarget.Roles {
		if isFixedRole(roleParam) {
			metricTargetsByRole[roleName] = algorithm.Metrics{}
			continue
		}
		metrics := roleParam.Metrics
		if len(metrics) == 0 {
			metrics = policy.Spec.Metrics
		}
		metricTargets := algorithm.Metrics{}
		for _, metric := range metrics {
			metricTargets[metric.Name] = metric.TargetValue.AsFloat64Slow()
		}
		metricTargetsByRole[roleName] = metricTargets
	}
	return metricTargetsByRole
}

// isFixedRole reports whether a role is a capacity placeholder rather than an
// autoscaled unit. This is needed for partial role autoscaling: for example,
// scale only "prefill" while keeping "decode" fixed, or run a single-role
// disaggregated policy with no ratio constraint.
func isFixedRole(roleParam workload.RoleScalingParam) bool {
	return roleParam.MaxReplicas > 0 && roleParam.MinReplicas == roleParam.MaxReplicas
}

// metricSourcesForRole returns a defensive copy of metric sources with pod
// sources narrowed to the given ModelServing role. This keeps role selectors
// implicit for users while preserving any selector labels they already set.
func metricSourcesForRole(roleName string, sources map[string]workload.MetricSource) map[string]workload.MetricSource {
	if sources == nil {
		return nil
	}
	result := make(map[string]workload.MetricSource, len(sources))
	for metricName, source := range sources {
		copied := source
		if source.Pod != nil {
			pod := *source.Pod
			if pod.LabelSelector != nil {
				pod.LabelSelector = pod.LabelSelector.DeepCopy()
			} else {
				pod.LabelSelector = &metav1.LabelSelector{}
			}
			if pod.LabelSelector.MatchLabels == nil {
				pod.LabelSelector.MatchLabels = map[string]string{}
			}
			pod.LabelSelector.MatchLabels[workload.RoleLabelKey] = roleName
			copied.Pod = &pod
		}
		result[metricName] = copied
	}
	return result
}
