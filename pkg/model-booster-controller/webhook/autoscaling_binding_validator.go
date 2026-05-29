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
	"context"
	"fmt"
	"net/http"
	"strings"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
)

const (
	ModelServingRoleKind = "Role"
)

// AutoscalingBindingValidator handles validation of AutoscalingPolicyBinding resources
type AutoscalingBindingValidator struct {
	client clientset.Interface
}

// NewAutoscalingBindingValidator creates a new AutoscalingBindingValidator
func NewAutoscalingBindingValidator(client clientset.Interface) *AutoscalingBindingValidator {
	return &AutoscalingBindingValidator{
		client: client,
	}
}

func (v *AutoscalingBindingValidator) Handle(w http.ResponseWriter, r *http.Request) {
	klog.V(3).Infof("received request: %s", r.URL.String())

	// Parse the admission request
	admissionReview, asp_binding, err := parseAdmissionRequest[workloadv1alpha1.AutoscalingPolicyBinding](r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the ModelBooster
	allowed, reason := v.validateAutoscalingBinding(asp_binding)
	// Create the admission response
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}

	if !allowed {
		admissionResponse.Result = &metav1.Status{
			Message: reason,
		}
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

// validateModel validates the AutoscalingBinding resource
func (v *AutoscalingBindingValidator) validateAutoscalingBinding(asp_binding *workloadv1alpha1.AutoscalingPolicyBinding) (bool, string) {
	ctx := context.Background()
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateOptimizeAndScalingPolicyExistence(asp_binding)...)
	allErrs = append(allErrs, v.validateAutoscalingPolicyExistence(ctx, asp_binding)...)
	allErrs = append(allErrs, validateBindingTargetKind(asp_binding)...)
	allErrs = append(allErrs, validateCoordination(asp_binding)...)

	if len(allErrs) > 0 {
		// Convert field errors to a formatted multi-line error message
		var messages []string
		for _, err := range allErrs {
			messages = append(messages, fmt.Sprintf("  - %s", err.Error()))
		}
		return false, fmt.Sprintf("validation failed:\n%s", strings.Join(messages, "\n"))
	}
	return true, ""
}

func (v *AutoscalingBindingValidator) validateAutoscalingPolicyExistence(ctx context.Context, asp_binding *workloadv1alpha1.AutoscalingPolicyBinding) field.ErrorList {
	var allErrs field.ErrorList

	if _, err := v.client.WorkloadV1alpha1().AutoscalingPolicies(asp_binding.Namespace).Get(ctx, asp_binding.Spec.PolicyRef.Name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("PolicyRef"), asp_binding.Spec.PolicyRef.Name, fmt.Sprintf("autoscaling policy resource %s does not exist", asp_binding.Spec.PolicyRef.Name)))
		} else {
			allErrs = append(allErrs, field.InternalError(field.NewPath("spec").Child("PolicyRef"), err))
		}
	}

	return allErrs
}

func validateOptimizeAndScalingPolicyExistence(asp_binding *workloadv1alpha1.AutoscalingPolicyBinding) field.ErrorList {
	var allErrs field.ErrorList
	if asp_binding.Spec.HeterogeneousTarget == nil && asp_binding.Spec.HomogeneousTarget == nil {
		allErrs = append(allErrs, field.Required(field.NewPath("spec").Child("homogeneousTarget"), "spec.homogeneousTarget should be set if spec.heterogeneousTarget does not exist"))
	}
	if asp_binding.Spec.HeterogeneousTarget != nil && asp_binding.Spec.HomogeneousTarget != nil {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec").Child("homogeneousTarget"), "both spec.heterogeneousTarget and spec.homogeneousTarget can not be set at the same time"))
	}
	return allErrs
}

func validateBindingTargetKind(asp_binding *workloadv1alpha1.AutoscalingPolicyBinding) field.ErrorList {
	var allErrs field.ErrorList
	if asp_binding.Spec.HeterogeneousTarget != nil {
		for idx, param := range asp_binding.Spec.HeterogeneousTarget.Params {
			if param.Target.TargetRef.Kind != "" && param.Target.TargetRef.Kind != workloadv1alpha1.ModelServingKind.Kind {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("heterogeneousTarget").Child("params").Index(idx).Child("targetRef").Child("kind"), param.Target.TargetRef.Kind, fmt.Sprintf("heterogeneousTarget.params[].targetRef.kind must be ModelServing, but got %s", param.Target.TargetRef.Kind)))
			}
			if param.Target.SubTarget != nil {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("heterogeneousTarget").Child("params").Index(idx).Child("targetRef").Child("subTarget"), param.Target.SubTarget, fmt.Sprintf("heterogeneousTarget.params[].targetRef.subTarget must be empty, but got %s", param.Target.SubTarget)))
			}
		}
	}

	if asp_binding.Spec.HomogeneousTarget != nil {
		switch asp_binding.Spec.HomogeneousTarget.Target.TargetRef.Kind {
		case "", workloadv1alpha1.ModelServingKind.Kind:
			if asp_binding.Spec.HomogeneousTarget.Target.TargetRef.Name == "" {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("homogeneousTarget").Child("targetRef").Child("name"), asp_binding.Spec.HomogeneousTarget.Target.TargetRef.Name, "homogeneousTarget.targetRef.name must be set, but got empty"))
			}
			if asp_binding.Spec.HomogeneousTarget.Target.SubTarget != nil {
				subTarget := asp_binding.Spec.HomogeneousTarget.Target.SubTarget
				if subTarget.Name == "" {
					allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("homogeneousTarget").Child("targetRef").Child("subTarget").Child("name"), subTarget.Name, "homogeneousTarget.targetRef.subTarget.name must be set, but got empty"))
				}
				if subTarget.Kind != "" && subTarget.Kind != ModelServingRoleKind {
					allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("homogeneousTarget").Child("targetRef").Child("subTarget").Child("kind"), subTarget.Kind, fmt.Sprintf("homogeneousTarget.targetRef.subTarget.kind must be `Role`, but got %s", subTarget.Kind)))
				}
			}
		default:
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("homogeneousTarget").Child("targetRef").Child("kind"), asp_binding.Spec.HomogeneousTarget.Target.TargetRef.Kind, fmt.Sprintf("homogeneousTarget.targetRef.kind must be ModelServing, but got %s", asp_binding.Spec.HomogeneousTarget.Target.TargetRef.Kind)))
		}
	}

	return allErrs
}

// validateCoordination checks the optional coordination block on
// HeterogeneousTarget. Default behavior (nil / "Off") is unrestricted.
func validateCoordination(asp_binding *workloadv1alpha1.AutoscalingPolicyBinding) field.ErrorList {
	var allErrs field.ErrorList
	if asp_binding.Spec.HeterogeneousTarget == nil || asp_binding.Spec.HeterogeneousTarget.Coordination == nil {
		return allErrs
	}
	coordPath := field.NewPath("spec").Child("heterogeneousTarget").Child("coordination")
	coord := asp_binding.Spec.HeterogeneousTarget.Coordination
	switch coord.Mode {
	case "", workloadv1alpha1.CoordinationModeOff, workloadv1alpha1.CoordinationModePreferred:
	default:
		allErrs = append(allErrs, field.Invalid(coordPath.Child("mode"), coord.Mode, "must be one of Off, Preferred"))
	}

	// Build the set of valid role names from existing params so we can flag typos.
	validNames := make(map[string]struct{}, len(asp_binding.Spec.HeterogeneousTarget.Params))
	for _, p := range asp_binding.Spec.HeterogeneousTarget.Params {
		validNames[p.Target.TargetRef.Name] = struct{}{}
	}

	ratioPath := coordPath.Child("preferredRatio")
	for name, r := range coord.PreferredRatio {
		entry := ratioPath.Key(name)
		if r.Min < 0 || r.Min > 100 {
			allErrs = append(allErrs, field.Invalid(entry.Child("min"), r.Min, "must be between 0 and 100"))
		}
		if r.Max < 0 || r.Max > 100 {
			allErrs = append(allErrs, field.Invalid(entry.Child("max"), r.Max, "must be between 0 and 100"))
		}
		if r.Min > r.Max {
			allErrs = append(allErrs, field.Invalid(entry, r, "min must be <= max"))
		}
		if _, ok := validNames[name]; !ok {
			allErrs = append(allErrs, field.Invalid(entry, name, "must match a heterogeneousTarget.params[].target.targetRef.name"))
		}
	}
	return allErrs
}
