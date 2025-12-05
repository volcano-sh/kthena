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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// ModelMutator handles mutation of ModelBooster resources
type ModelMutator struct {
}

// NewModelMutator creates a new ModelMutator
func NewModelMutator() *ModelMutator {
	return &ModelMutator{}
}

// Handle handles admission requests for ModelBooster resources
func (m *ModelMutator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, model, err := parseAdmissionRequest[registryv1alpha1.ModelBooster](r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create a copy of the model to mutate
	mutatedModel := model.DeepCopy()

	// Apply mutations
	m.mutateModel(mutatedModel)

	// Create the patch
	patch, err := createPatch(model, mutatedModel)
	if err != nil {
		klog.Errorf("Failed to create patch: %v", err)
		http.Error(w, fmt.Sprintf("could not create patch: %v", err), http.StatusInternalServerError)
		return
	}

	// Create the admission response
	patchType := admissionv1.PatchTypeJSONPatch
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed:   true,
		UID:       admissionReview.Request.UID,
		Patch:     patch,
		PatchType: &patchType,
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

// mutateModel applies mutations to the ModelBooster resource
func (m *ModelMutator) mutateModel(model *registryv1alpha1.ModelBooster) {
	klog.Infof("Defaulting for ModelBooster %s", model.GetName())

	// Default ScaleToZeroGracePeriod if AutoscalingPolicy is set
	if model.Spec.AutoscalingPolicy != nil {
		if model.Spec.Backend.ScaleToZeroGracePeriod == nil {
			model.Spec.Backend.ScaleToZeroGracePeriod = &metav1.Duration{Duration: 30 * time.Second}
		}

		if model.Spec.CostExpansionRatePercent == nil {
			var value int32 = 200
			model.Spec.CostExpansionRatePercent = &value
		}
	}
}

// createPatch creates a JSON patch between the original and mutated model
func createPatch(original, mutated *registryv1alpha1.ModelBooster) ([]byte, error) {
	// Convert both objects to JSON
	originalJSON, err := json.Marshal(original)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal original: %v", err)
	}

	mutatedJSON, err := json.Marshal(mutated)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal mutated: %v", err)
	}

	// Create a JSON patch using the jsonpatch library
	patch, err := jsonpatch.CreatePatch(originalJSON, mutatedJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to create patch: %v", err)
	}

	// Marshal the patch
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal patch: %v", err)
	}

	return patchBytes, nil
}
