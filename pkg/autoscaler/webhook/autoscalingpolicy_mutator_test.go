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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	registryv1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// TestNewAutoscalingPolicyMutator tests the constructor
func TestNewAutoscalingPolicyMutator(t *testing.T) {
	mutator := NewAutoscalingPolicyMutator()
	assert.NotNil(t, mutator)
}

// TestAutoscalingPolicyMutator_Handle_InvalidJSON tests handling of invalid JSON input
func TestAutoscalingPolicyMutator_Handle_InvalidJSON(t *testing.T) {
	mutator := NewAutoscalingPolicyMutator()

	// Test with invalid JSON
	req := httptest.NewRequest("POST", "/mutate", bytes.NewBufferString("invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mutator.Handle(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "failed to decode body")
}

// TestAutoscalingPolicyMutator_Handle_InvalidObject tests handling of invalid object structure
func TestAutoscalingPolicyMutator_Handle_InvalidObject(t *testing.T) {
	mutator := NewAutoscalingPolicyMutator()

	// Create admission review with invalid field types that will cause JSON unmarshaling errors
	admissionReview := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID("test-uid"),
			Object: runtime.RawExtension{
				Raw: []byte(`{"spec": {"tolerancePercent": "invalid_number"}}`),
			},
		},
	}

	body, _ := json.Marshal(admissionReview)
	req := httptest.NewRequest("POST", "/mutate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mutator.Handle(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "failed to decode object")
}

// TestAutoscalingPolicyMutator_Handle_Success tests successful mutation of a policy
func TestAutoscalingPolicyMutator_Handle_Success(t *testing.T) {
	mutator := NewAutoscalingPolicyMutator()

	// Create a minimal AutoscalingPolicy without behavior - should be mutated
	policy := &registryv1.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: registryv1.AutoscalingPolicySpec{
			TolerancePercent: 10,
			Metrics: []registryv1.AutoscalingPolicyMetric{
				{
					Name:        "cpu",
					TargetValue: resource.MustParse("80"),
				},
			},
			// No behavior specified - should be mutated
		},
	}

	policyBytes, _ := json.Marshal(policy)
	admissionReview := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID("test-uid"),
			Object: runtime.RawExtension{
				Raw: policyBytes,
			},
		},
	}

	body, _ := json.Marshal(admissionReview)
	req := httptest.NewRequest("POST", "/mutate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mutator.Handle(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Parse response
	var responseReview admissionv1.AdmissionReview
	err := json.Unmarshal(w.Body.Bytes(), &responseReview)
	require.NoError(t, err)

	assert.True(t, responseReview.Response.Allowed)
	assert.Equal(t, types.UID("test-uid"), responseReview.Response.UID)
	assert.NotNil(t, responseReview.Response.Patch)
	assert.Equal(t, admissionv1.PatchTypeJSONPatch, *responseReview.Response.PatchType)
}

// TestMutateAutoscalingPolicy_EmptyBehavior tests mutation when behavior is completely empty
func TestMutateAutoscalingPolicy_EmptyBehavior(t *testing.T) {
	// Test policy with no behavior - should add complete default behavior
	policy := &registryv1.AutoscalingPolicy{
		Spec: registryv1.AutoscalingPolicySpec{
			TolerancePercent: 10,
			Metrics: []registryv1.AutoscalingPolicyMetric{
				{
					Name:        "cpu",
					TargetValue: resource.MustParse("80"),
				},
			},
			// Empty behavior
		},
	}

	patch := createPolicyBatch(policy)

	// Should have exactly one patch operation to add the entire behavior
	assert.Len(t, patch, 1)
	assert.Equal(t, "add", patch[0].Operation)
	assert.Equal(t, "/spec/behavior", patch[0].Path)

	// Verify the default behavior structure
	behavior, ok := patch[0].Value.(registryv1.AutoscalingPolicyBehavior)
	assert.True(t, ok)

	// Check ScaleDown defaults
	assert.Equal(t, ptr.To(int32(0)), behavior.ScaleDown.Instances)
	assert.Equal(t, ptr.To(int32(100)), behavior.ScaleDown.Percent)
	assert.Equal(t, time.Minute*5, behavior.ScaleDown.StabilizationWindow.Duration)

	// Check ScaleUp StablePolicy defaults
	assert.Equal(t, ptr.To(int32(4)), behavior.ScaleUp.StablePolicy.Instances)
	assert.Equal(t, ptr.To(int32(100)), behavior.ScaleUp.StablePolicy.Percent)
	assert.Equal(t, time.Duration(0), behavior.ScaleUp.StablePolicy.StabilizationWindow.Duration)

	// Check ScaleUp PanicPolicy defaults
	assert.Equal(t, ptr.To(int32(0)), behavior.ScaleUp.PanicPolicy.Percent)
	assert.Equal(t, ptr.To(int32(200)), behavior.ScaleUp.PanicPolicy.PanicThresholdPercent)
}

// TestMutateAutoscalingPolicy_NoChangesNeeded tests when no mutation is required
func TestMutateAutoscalingPolicy_NoChangesNeeded(t *testing.T) {
	// Test policy that already has all required fields
	policy := &registryv1.AutoscalingPolicy{
		Spec: registryv1.AutoscalingPolicySpec{
			Behavior: registryv1.AutoscalingPolicyBehavior{
				ScaleDown: registryv1.AutoscalingPolicyStablePolicy{
					Instances:           ptr.To(int32(1)),
					StabilizationWindow: &metav1.Duration{Duration: time.Minute * 3},
				},
				ScaleUp: registryv1.AutoscalingPolicyScaleUpPolicy{
					StablePolicy: registryv1.AutoscalingPolicyStablePolicy{
						Instances:           ptr.To(int32(2)),
						StabilizationWindow: &metav1.Duration{Duration: time.Second * 30},
					},
					PanicPolicy: registryv1.AutoscalingPolicyPanicPolicy{
						Percent: ptr.To(int32(50)),
					},
				},
			},
		},
	}

	patch := createPolicyBatch(policy)

	// Should have no patch operations
	assert.Len(t, patch, 0)
}

// TestCreatePolicyPatch tests the patch creation functionality
func TestCreatePolicyPatch(t *testing.T) {
	// Test creating patch from operations
	operations := []jsonpatch.Operation{
		jsonpatch.NewOperation("add", "/spec/behavior/scaleDown/stabilizationWindow", "5m"),
		jsonpatch.NewOperation("add", "/spec/behavior/scaleUp/stablePolicy/stabilizationWindow", "0s"),
	}

	patchBytes, err := createPolicyPatchBytes(operations)
	require.NoError(t, err)

	// Verify it's valid JSON
	var patchArray []interface{}
	err = json.Unmarshal(patchBytes, &patchArray)
	require.NoError(t, err)

	assert.Len(t, patchArray, 2)
}

// TestCreatePolicyPatch_EmptyOperations tests patch creation with no operations
func TestCreatePolicyPatch_EmptyOperations(t *testing.T) {
	// Test creating patch with no operations
	operations := []jsonpatch.Operation{}

	patchBytes, err := createPolicyPatchBytes(operations)
	require.NoError(t, err)

	// Should be empty JSON array
	assert.Equal(t, "[]", string(patchBytes))
}
