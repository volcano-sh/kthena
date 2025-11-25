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

package handlers

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
    if asp_binding.Spec.OptimizerConfiguration == nil && asp_binding.Spec.ScalingConfiguration == nil {
        allErrs = append(allErrs, field.Required(field.NewPath("spec").Child("homogeneousTarget"), "spec.homogeneousTarget should be set if spec.heterogeneousTarget does not exist"))
    }
    if asp_binding.Spec.OptimizerConfiguration != nil && asp_binding.Spec.ScalingConfiguration != nil {
        allErrs = append(allErrs, field.Forbidden(field.NewPath("spec").Child("homogeneousTarget"), "both spec.heterogeneousTarget and spec.homogeneousTarget can not be set at the same time"))
    }
    return allErrs
}
