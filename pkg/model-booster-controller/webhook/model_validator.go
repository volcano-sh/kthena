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
	"net/http"
	"strings"

	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
)

// ModelValidator handles validation of ModelBooster resources
type ModelValidator struct {
}

// NewModelValidator creates a new ModelValidator
func NewModelValidator() *ModelValidator {
	return &ModelValidator{}
}

// Handle handles admission requests for ModelBooster resources
func (v *ModelValidator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, model, err := parseAdmissionRequest[registryv1alpha1.ModelBooster](r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the ModelBooster
	allowed, reason := v.validateModel(model)

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

// validateModel validates the ModelBooster resource
func (v *ModelValidator) validateModel(model *registryv1alpha1.ModelBooster) (bool, string) {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateBackendReplicaBounds(model)...)
	allErrs = append(allErrs, validateWorkerImages(model)...)
	allErrs = append(allErrs, validateBackendWorkerTypes(model)...)

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

func validateBackendWorkerTypes(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	backendPath := field.NewPath("spec").Child("backend")
	backend := model.Spec.Backend
	workers := backend.Workers

	if backend.Type == registryv1alpha1.ModelBackendTypeVLLM ||
		backend.Type == registryv1alpha1.ModelBackendTypeSGLang ||
		backend.Type == registryv1alpha1.ModelBackendTypeMindIE {
		if len(workers) != 1 {
			allErrs = append(allErrs, field.Invalid(
				backendPath.Child("workers"),
				len(workers),
				fmt.Sprintf("If backend type is '%s', there must be exactly one worker", backend.Type),
			))
		} else if workers[0].Type != registryv1alpha1.ModelWorkerTypeServer {
			allErrs = append(allErrs, field.Invalid(
				backendPath.Child("workers").Index(0).Child("type"),
				workers[0].Type,
				fmt.Sprintf("If backend type is '%s', the worker type must be 'server'", backend.Type),
			))
		}
	}

	if backend.Type == registryv1alpha1.ModelBackendTypeVLLMDisaggregated {
		for j, w := range workers {
			if w.Type != registryv1alpha1.ModelWorkerTypePrefill && w.Type != registryv1alpha1.ModelWorkerTypeDecode {
				allErrs = append(allErrs, field.Invalid(
					backendPath.Child("workers").Index(j).Child("type"),
					w.Type,
					"If backend type is 'vLLMDisaggregated', all workers must be type 'prefill' or 'decode'",
				))
			}
		}
	}

	// Rule 3: MindIEDisaggregated -> all workers must be 'prefill', 'decode', 'controller', or 'coordinator'
	if backend.Type == registryv1alpha1.ModelBackendTypeMindIEDisaggregated {
		validTypes := map[registryv1alpha1.ModelWorkerType]struct{}{
			registryv1alpha1.ModelWorkerTypePrefill:     {},
			registryv1alpha1.ModelWorkerTypeDecode:      {},
			registryv1alpha1.ModelWorkerTypeController:  {},
			registryv1alpha1.ModelWorkerTypeCoordinator: {},
		}
		for j, w := range workers {
			if _, ok := validTypes[w.Type]; !ok {
				allErrs = append(allErrs, field.Invalid(
					backendPath.Child("workers").Index(j).Child("type"),
					w.Type,
					"If backend type is 'MindIEDisaggregated', all workers must be type 'prefill', 'decode', 'controller', or 'coordinator' (not 'server')",
				))
			}
		}
	}
	return allErrs
}

func validateBackendReplicaBounds(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	path := field.NewPath("spec").Child("backend")
	const maxTotalReplicas = 1000000
	backend := model.Spec.Backend
	if backend.Replicas > maxTotalReplicas {
		allErrs = append(allErrs, field.Invalid(
			path.Child("replicas"),
			backend.Replicas,
			fmt.Sprintf("replicas (%d) cannot exceed %d", backend.Replicas, maxTotalReplicas),
		))
	}
	return allErrs
}

func validateWorkerImages(model *registryv1alpha1.ModelBooster) field.ErrorList {
	var allErrs field.ErrorList
	backend := model.Spec.Backend
	for j, worker := range backend.Workers {
		if worker.Image != "" {
			if err := validateImageField(worker.Image); err != nil {
				allErrs = append(allErrs, field.Invalid(
					field.NewPath("spec").Child("backend").Child("workers").Index(j).Child("image"),
					worker.Image,
					fmt.Sprintf("invalid container image reference: %v", err),
				))
			}
		}
	}
	return allErrs
}

// validateImageField checks if a container image string is a valid Docker reference.
func validateImageField(image string) error {
	if image == "" {
		// Optional: return the error if you want to require the image field
		return nil
	}

	// Simple validation: check if image contains at least one character and no spaces
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image cannot be empty or whitespace only")
	}

	if strings.Contains(image, " ") {
		return fmt.Errorf("image cannot contain spaces")
	}

	// Basic format check: should contain at least one character
	if len(strings.TrimSpace(image)) == 0 {
		return fmt.Errorf("invalid image format")
	}

	return nil
}
