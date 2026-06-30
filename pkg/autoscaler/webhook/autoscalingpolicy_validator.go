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

package webhook

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	registryv1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
)

// AutoscalingPolicyValidator handles validation of AutoscalingPolicy resources
type AutoscalingPolicyValidator struct {
}

// NewAutoscalingPolicyValidator creates a new AutoscalingPolicyValidator
func NewAutoscalingPolicyValidator() *AutoscalingPolicyValidator {
	return &AutoscalingPolicyValidator{}
}

// Handle handles admission requests for AutoscalingPolicy resources
func (v *AutoscalingPolicyValidator) Handle(w http.ResponseWriter, r *http.Request) {
	klog.V(4).Info("Handling AutoscalingPolicy validation request")

	// Parse the admission request
	admissionReview, policy, err := parseAdmissionRequest[registryv1.AutoscalingPolicy](r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	klog.V(4).Infof("Validating AutoscalingPolicy: %s/%s", policy.Namespace, policy.Name)

	// Validate the policy
	allowed, reason := v.validateAutoscalingPolicy(policy)

	// Create the admission response
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}

	if !allowed {
		admissionResponse.Result = &metav1.Status{
			Message: reason,
		}
		klog.V(2).Infof("AutoscalingPolicy validation failed: %s", reason)
	} else {
		klog.V(4).Info("AutoscalingPolicy validation passed")
	}

	// Create the admission review response
	admissionReview.Response = &admissionResponse

	// Send the response
	if err := sendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
		return
	}
}

// validateAutoscalingPolicy validates the AutoscalingPolicy resource
func (v *AutoscalingPolicyValidator) validateAutoscalingPolicy(policy *registryv1.AutoscalingPolicy) (bool, string) {
	var allErrs field.ErrorList

	// Validate metrics
	allErrs = append(allErrs, v.validateMetrics(policy)...)

	// Validate target configuration (exactly one target, valid kind/name)
	allErrs = append(allErrs, v.validateTarget(policy)...)

	// Require metrics for homogeneous/heterogeneous targets. DisaggregatedTarget may use per-role metrics.
	if policy.Spec.HomogeneousTarget != nil || policy.Spec.HeterogeneousTarget != nil {
		if len(policy.Spec.Metrics) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("spec").Child("metrics"), "at least one metric must be set"))
		}
	}

	// Validate disaggregated target role-level metric and ratio configuration.
	allErrs = append(allErrs, v.validateDisaggregatedTarget(policy)...)

	// Validate scale down behavior
	allErrs = append(allErrs, v.validateScaleDownBehavior(policy)...)

	// Validate scale up behavior
	allErrs = append(allErrs, v.validateScaleUpBehavior(policy)...)

	if len(allErrs) > 0 {
		var messages []string
		for _, err := range allErrs {
			messages = append(messages, fmt.Sprintf("  - %s", err.Error()))
		}
		return false, fmt.Sprintf("validation failed:\n%s", strings.Join(messages, "\n"))
	}
	return true, ""
}

// validateTarget validates the target configuration of an AutoscalingPolicy.
//
// Exactly one of homogeneousTarget, heterogeneousTarget, or disaggregatedTarget
// must be set.
func (v *AutoscalingPolicyValidator) validateTarget(policy *registryv1.AutoscalingPolicy) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	targetCount := 0
	if policy.Spec.HomogeneousTarget != nil {
		targetCount++
	}
	if policy.Spec.HeterogeneousTarget != nil {
		targetCount++
	}
	if policy.Spec.DisaggregatedTarget != nil {
		targetCount++
	}
	if targetCount != 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath,
			targetCount,
			"exactly one of homogeneousTarget, heterogeneousTarget, or disaggregatedTarget must be set",
		))
		return allErrs
	}

	switch {
	case policy.Spec.HomogeneousTarget != nil:
		allErrs = append(allErrs, validateTargetRef(
			&policy.Spec.HomogeneousTarget.Target.TargetRef,
			specPath.Child("homogeneousTarget").Child("target").Child("targetRef"))...)
	case policy.Spec.HeterogeneousTarget != nil:
		for idx, param := range policy.Spec.HeterogeneousTarget.Params {
			allErrs = append(allErrs, validateTargetRef(
				&param.Target.TargetRef,
				specPath.Child("heterogeneousTarget").Child("params").Index(idx).Child("target").Child("targetRef"))...)
		}
	case policy.Spec.DisaggregatedTarget != nil:
		allErrs = append(allErrs, validateTargetRef(
			&policy.Spec.DisaggregatedTarget.TargetRef,
			specPath.Child("disaggregatedTarget").Child("targetRef"))...)
	}

	return allErrs
}

// validateTargetRef ensures the target ref kind is ModelServing (or empty) and name is set.
func validateTargetRef(targetRef *corev1.ObjectReference, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if targetRef.Kind != "" && targetRef.Kind != registryv1.ModelServingKind.Kind {
		allErrs = append(allErrs, field.Invalid(
			path.Child("kind"),
			targetRef.Kind,
			fmt.Sprintf("targetRef.kind must be ModelServing, but got %s", targetRef.Kind),
		))
	}
	if targetRef.Name == "" {
		allErrs = append(allErrs, field.Invalid(
			path.Child("name"),
			targetRef.Name,
			"targetRef.name must be set, but got empty",
		))
	}
	return allErrs
}

// validateMetrics validates the metrics configuration
func (v *AutoscalingPolicyValidator) validateMetrics(policy *registryv1.AutoscalingPolicy) field.ErrorList {
	var allErrs field.ErrorList
	metricNames := make(map[string]struct{})

	for i, metric := range policy.Spec.Metrics {
		metricPath := field.NewPath("spec").Child("metrics").Index(i)

		// Validate target value
		if metric.TargetValue.AsFloat64Slow() <= 0 || math.IsInf(metric.TargetValue.AsFloat64Slow(), 0) {
			allErrs = append(allErrs, field.Invalid(
				metricPath.Child("targetValue"),
				metric.TargetValue,
				"metric target value must be greater than 0 and not equal to infinity",
			))
		}

		// Validate metric name uniqueness
		if _, exists := metricNames[metric.Name]; exists {
			allErrs = append(allErrs, field.Invalid(
				metricPath.Child("name"),
				metric.Name,
				fmt.Sprintf("duplicate metric name %s is not allowed", metric.Name),
			))
		}
		metricNames[metric.Name] = struct{}{}
	}

	return allErrs
}

func (v *AutoscalingPolicyValidator) validateDisaggregatedTarget(policy *registryv1.AutoscalingPolicy) field.ErrorList {
	var allErrs field.ErrorList
	target := policy.Spec.DisaggregatedTarget
	if target == nil {
		return allErrs
	}

	disaggregatedPath := field.NewPath("spec").Child("disaggregatedTarget")

	for roleName, roleParam := range target.Roles {
		if roleName == "" {
			allErrs = append(allErrs, field.Invalid(disaggregatedPath.Child("roles"), roleName, "role name must refer to a ModelServing.spec.template.roles[].name and must not be empty"))
			continue
		}
		rolePath := disaggregatedPath.Child("roles").Key(roleName)
		fixedRole := isFixedRoleScalingParam(roleParam)
		if roleParam.MinReplicas > roleParam.MaxReplicas {
			allErrs = append(allErrs, field.Invalid(rolePath.Child("minReplicas"), roleParam.MinReplicas, "minReplicas must be <= maxReplicas"))
		}
		// Fixed roles are declared with equal min/max bounds. They are intentionally
		// exempt from the metrics requirement because the autoscaler never computes a
		// recommendation for them; it always returns the fixed replica count.
		if !fixedRole && len(roleParam.Metrics) == 0 && len(policy.Spec.Metrics) == 0 {
			allErrs = append(allErrs, field.Required(rolePath.Child("metrics"), "metrics must be set on every non-fixed role when spec.metrics is empty"))
		}

		effectiveMetricNames := make(map[string]struct{})
		if len(roleParam.Metrics) == 0 && !fixedRole {
			for _, metric := range policy.Spec.Metrics {
				effectiveMetricNames[metric.Name] = struct{}{}
			}
		}
		metricNames := make(map[string]struct{})
		for idx, metric := range roleParam.Metrics {
			metricPath := rolePath.Child("metrics").Index(idx)
			if metric.TargetValue.AsFloat64Slow() <= 0 || math.IsInf(metric.TargetValue.AsFloat64Slow(), 0) {
				allErrs = append(allErrs, field.Invalid(metricPath.Child("targetValue"), metric.TargetValue, "metric target value must be greater than 0 and not equal to infinity"))
			}
			if _, exists := metricNames[metric.Name]; exists {
				allErrs = append(allErrs, field.Invalid(metricPath.Child("name"), metric.Name, fmt.Sprintf("duplicate metric name %s is not allowed", metric.Name)))
			}
			metricNames[metric.Name] = struct{}{}
			effectiveMetricNames[metric.Name] = struct{}{}
		}
		if !fixedRole {
			if len(roleParam.MetricSources) == 0 && len(effectiveMetricNames) > 0 {
				allErrs = append(allErrs, field.Invalid(rolePath.Child("metricSources"), len(roleParam.MetricSources), "metricSources must be set on every non-fixed role when metrics are configured"))
				continue
			}
			for sourceKey := range roleParam.MetricSources {
				if _, exists := effectiveMetricNames[sourceKey]; !exists {
					allErrs = append(allErrs, field.Invalid(rolePath.Child("metricSources").Key(sourceKey), sourceKey, "metricSources key must match an effective metric name"))
				}
			}
		}
	}

	if target.RatioConstraint != nil {
		constraint := target.RatioConstraint
		ratioPath := disaggregatedPath.Child("ratioConstraint")
		if constraint.NumeratorRole == constraint.DenominatorRole {
			allErrs = append(allErrs, field.Invalid(ratioPath.Child("denominatorRole"), constraint.DenominatorRole, "numeratorRole and denominatorRole must differ"))
		}
		numeratorParam, numeratorExists := target.Roles[constraint.NumeratorRole]
		denominatorParam, denominatorExists := target.Roles[constraint.DenominatorRole]
		if !numeratorExists {
			allErrs = append(allErrs, field.Invalid(ratioPath.Child("numeratorRole"), constraint.NumeratorRole, "numeratorRole must exist in roles"))
		}
		if !denominatorExists {
			allErrs = append(allErrs, field.Invalid(ratioPath.Child("denominatorRole"), constraint.DenominatorRole, "denominatorRole must exist in roles"))
		}
		if constraint.MinRatio.Cmp(constraint.MaxRatio) > 0 {
			allErrs = append(allErrs, field.Invalid(ratioPath.Child("minRatio"), constraint.MinRatio.String(), "minRatio must be <= maxRatio"))
		}
		if numeratorExists && denominatorExists {
			if (numeratorParam.MinReplicas == 0) != (denominatorParam.MinReplicas == 0) {
				allErrs = append(allErrs, field.Invalid(ratioPath, "scale-to-zero bounds", "ratio roles must be scalable-to-zero together"))
			}
			minRatio := constraint.MinRatio.AsFloat64Slow()
			maxRatio := constraint.MaxRatio.AsFloat64Slow()
			if math.IsNaN(minRatio) || math.IsNaN(maxRatio) || math.IsInf(minRatio, 0) || math.IsInf(maxRatio, 0) {
				allErrs = append(allErrs, field.Invalid(ratioPath, "ratio", "ratio values must be finite"))
			} else if minRatio < 0 || maxRatio <= 0 {
				allErrs = append(allErrs, field.Invalid(ratioPath, "ratio", "minRatio must be >= 0 and maxRatio must be > 0"))
			} else {
				if denominatorParam.MaxReplicas > 0 && float64(numeratorParam.MinReplicas)/float64(denominatorParam.MaxReplicas) > maxRatio {
					allErrs = append(allErrs, field.Invalid(ratioPath, "bounds", "ratioConstraint is not achievable within role replica bounds"))
				}
				if denominatorParam.MinReplicas > 0 && float64(numeratorParam.MaxReplicas)/float64(denominatorParam.MinReplicas) < minRatio {
					allErrs = append(allErrs, field.Invalid(ratioPath, "bounds", "ratioConstraint is not achievable within role replica bounds"))
				}
			}
		}
	}

	return allErrs
}

// isFixedRoleScalingParam mirrors the runtime fixed-role check in the webhook.
// Keeping the definition here lets admission accept policies that intentionally
// keep a role at a constant size without forcing dummy metrics or metric sources.
func isFixedRoleScalingParam(roleParam registryv1.RoleScalingParam) bool {
	return roleParam.MaxReplicas > 0 && roleParam.MinReplicas == roleParam.MaxReplicas
}

// validateScaleDownBehavior validates the scale down behavior configuration
func (v *AutoscalingPolicyValidator) validateScaleDownBehavior(policy *registryv1.AutoscalingPolicy) field.ErrorList {
	var allErrs field.ErrorList
	scaleDownPath := field.NewPath("spec").Child("behavior").Child("scaleDown")
	stablePolicy := policy.Spec.Behavior.ScaleDown

	// Validate period
	if stablePolicy.Period != nil && (stablePolicy.Period.Seconds() < 0 || stablePolicy.Period.Minutes() > 30) {
		allErrs = append(allErrs, field.Invalid(
			scaleDownPath.Child("period"),
			stablePolicy.Period,
			"stable policy period must be between 0 and 30 minutes",
		))
	}

	// Validate stabilization window
	if stablePolicy.StabilizationWindow != nil &&
		(stablePolicy.StabilizationWindow.Seconds() < 0 || stablePolicy.StabilizationWindow.Minutes() > 30) {
		allErrs = append(allErrs, field.Invalid(
			scaleDownPath.Child("stabilizationWindow"),
			stablePolicy.StabilizationWindow,
			"stable policy stabilization window must be between 0 and 30 minutes",
		))
	}

	return allErrs
}

// validateScaleUpBehavior validates the scale up behavior configuration
func (v *AutoscalingPolicyValidator) validateScaleUpBehavior(policy *registryv1.AutoscalingPolicy) field.ErrorList {
	var allErrs field.ErrorList
	scaleUpPath := field.NewPath("spec").Child("behavior").Child("scaleUp")

	// Validate stable policy
	allErrs = append(allErrs, v.validateStablePolicy(policy, scaleUpPath)...)

	// Validate panic policy
	allErrs = append(allErrs, v.validatePanicPolicy(policy, scaleUpPath)...)

	return allErrs
}

// validateStablePolicy validates the stable policy configuration for scale up
func (v *AutoscalingPolicyValidator) validateStablePolicy(policy *registryv1.AutoscalingPolicy, scaleUpPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	stablePolicyPath := scaleUpPath.Child("stablePolicy")
	stablePolicy := policy.Spec.Behavior.ScaleUp.StablePolicy

	// Validate period
	if stablePolicy.Period != nil && (stablePolicy.Period.Seconds() < 0 || stablePolicy.Period.Minutes() > 30) {
		allErrs = append(allErrs, field.Invalid(
			stablePolicyPath.Child("period"),
			stablePolicy.Period,
			"stable policy period must be between 0 and 30 minutes",
		))
	}

	// Validate stabilization window
	if stablePolicy.StabilizationWindow != nil &&
		(stablePolicy.StabilizationWindow.Seconds() < 0 || stablePolicy.StabilizationWindow.Minutes() > 30) {
		allErrs = append(allErrs, field.Invalid(
			stablePolicyPath.Child("stabilizationWindow"),
			stablePolicy.StabilizationWindow,
			"stable policy stabilization window must be between 0 and 30 minutes",
		))
	}

	return allErrs
}

// validatePanicPolicy validates the panic policy configuration for scale up
func (v *AutoscalingPolicyValidator) validatePanicPolicy(policy *registryv1.AutoscalingPolicy, scaleUpPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	panicPolicyPath := scaleUpPath.Child("panicPolicy")
	panicPolicy := policy.Spec.Behavior.ScaleUp.PanicPolicy

	// Validate period
	if panicPolicy.Period.Seconds() < 0 || panicPolicy.Period.Minutes() > 30 {
		allErrs = append(allErrs, field.Invalid(
			panicPolicyPath.Child("period"),
			panicPolicy.Period,
			"panic policy period must be between 0 and 30 minutes",
		))
	}

	// Validate panic mode hold
	if panicPolicy.PanicModeHold != nil && (panicPolicy.PanicModeHold.Seconds() < 0 || panicPolicy.PanicModeHold.Minutes() > 30) {
		allErrs = append(allErrs, field.Invalid(
			panicPolicyPath.Child("panicModeHold"),
			panicPolicy.PanicModeHold,
			"panic policy panic mode hold must be between 0 and 30 minutes",
		))
	}

	return allErrs
}
