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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	registryv1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// AutoscalingPolicyMutator handles mutation of AutoscalingPolicy resources
type AutoscalingPolicyMutator struct {
}

// NewAutoscalingPolicyMutator creates a new AutoscalingPolicyMutator
func NewAutoscalingPolicyMutator() *AutoscalingPolicyMutator {
	return &AutoscalingPolicyMutator{}
}

// Handle handles admission requests for AutoscalingPolicy resources
func (m *AutoscalingPolicyMutator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, policy, err := parseAdmissionRequest[registryv1.AutoscalingPolicy](r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	patch := createPolicyBatch(policy)

	patchBytes, err := createPolicyPatchBytes(patch)
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
		Patch:     patchBytes,
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

func createPolicyBatch(policy *registryv1.AutoscalingPolicy) []jsonpatch.Operation {
	// Define default values
	DefaultScaleDown := registryv1.AutoscalingPolicyStablePolicy{
		Instances:           ptr.To(int32(0)),
		Percent:             ptr.To(int32(100)),
		Period:              &metav1.Duration{Duration: time.Minute},
		SelectPolicy:        registryv1.SelectPolicyOr,
		StabilizationWindow: &metav1.Duration{Duration: time.Minute * 5},
	}
	DefaultScaleUpStablePolicy := registryv1.AutoscalingPolicyStablePolicy{
		Instances:           ptr.To(int32(4)),
		Percent:             ptr.To(int32(100)),
		Period:              &metav1.Duration{Duration: time.Minute},
		SelectPolicy:        registryv1.SelectPolicyOr,
		StabilizationWindow: &metav1.Duration{Duration: 0},
	}
	DefaultScaleUpPanicPolicy := registryv1.AutoscalingPolicyPanicPolicy{
		Percent:               ptr.To(int32(0)),
		Period:                metav1.Duration{Duration: 0},
		PanicThresholdPercent: ptr.To(int32(200)),
		PanicModeHold:         &metav1.Duration{Duration: 0},
	}

	DefaultScaleUp := registryv1.AutoscalingPolicyScaleUpPolicy{
		StablePolicy: DefaultScaleUpStablePolicy,
		PanicPolicy:  DefaultScaleUpPanicPolicy,
	}
	var patch []jsonpatch.Operation

	// Only set default behavior if behavior doesn't exist
	if policy.Spec.Behavior == (registryv1.AutoscalingPolicyBehavior{}) {
		DefaultBehavior := registryv1.AutoscalingPolicyBehavior{
			ScaleUp:   DefaultScaleUp,
			ScaleDown: DefaultScaleDown,
		}
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior", DefaultBehavior))
		return patch
	}

	// Only set default scaleDown if it doesn't exist
	if policy.Spec.Behavior.ScaleDown == (registryv1.AutoscalingPolicyStablePolicy{}) {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior/scaleDown", DefaultScaleDown))
	} else if policy.Spec.Behavior.ScaleDown.StabilizationWindow == nil {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior/scaleDown/stabilizationWindow", "5m"))
	}

	if policy.Spec.Behavior.ScaleUp == (registryv1.AutoscalingPolicyScaleUpPolicy{}) {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior/scaleUp", DefaultScaleUp))
		return patch
	}

	// Only set default scaleUp/stablePolicy if it doesn't exist
	if policy.Spec.Behavior.ScaleUp.StablePolicy == (registryv1.AutoscalingPolicyStablePolicy{}) {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior/scaleUp/stablePolicy", DefaultScaleUpStablePolicy))
	} else if policy.Spec.Behavior.ScaleUp.StablePolicy.StabilizationWindow == nil {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior/scaleUp/stablePolicy/stabilizationWindow", "0s"))
	}

	// Only set default scaleUp/panicPolicy if it doesn't exist
	if policy.Spec.Behavior.ScaleUp.PanicPolicy == (registryv1.AutoscalingPolicyPanicPolicy{}) {
		patch = append(patch, jsonpatch.NewOperation("add", "/spec/behavior/scaleUp/panicPolicy", DefaultScaleUpPanicPolicy))
	}
	return patch
}

func createPolicyPatchBytes(patch []jsonpatch.Operation) ([]byte, error) {
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal patch: %v", err)
	}

	return patchBytes, nil
}
