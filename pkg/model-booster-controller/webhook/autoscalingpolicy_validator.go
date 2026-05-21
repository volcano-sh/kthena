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
