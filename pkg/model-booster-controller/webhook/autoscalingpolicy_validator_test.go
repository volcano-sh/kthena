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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	registryv1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestValidateAutoscalingPolicy_ErrorFormatting(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()
	policy := validAutoscalingPolicy()
	policy.Spec.TolerancePercent = 101
	policy.Spec.Metrics = []registryv1.AutoscalingPolicyMetric{
		{
			MetricName:  "cpu",
			TargetValue: resource.MustParse("0"),
		},
		{
			MetricName:  "cpu",
			TargetValue: resource.MustParse("80"),
		},
	}
	policy.Spec.Behavior.ScaleDown.Period = &metav1.Duration{Duration: -time.Minute}
	policy.Spec.Behavior.ScaleDown.StabilizationWindow = &metav1.Duration{Duration: time.Minute * 35}
	policy.Spec.Behavior.ScaleUp.StablePolicy.Period = &metav1.Duration{Duration: time.Minute * 35}
	policy.Spec.Behavior.ScaleUp.StablePolicy.StabilizationWindow = &metav1.Duration{Duration: -time.Second}
	policy.Spec.Behavior.ScaleUp.PanicPolicy.Period = metav1.Duration{Duration: time.Minute * 35}
	policy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent = ptr.To(int32(99))
	policy.Spec.Behavior.ScaleUp.PanicPolicy.PanicModeHold = &metav1.Duration{Duration: -time.Minute}

	allowed, errorMsg := validator.validateAutoscalingPolicy(policy)

	require.False(t, allowed)
	require.NotEmpty(t, errorMsg)

	assert.True(t, strings.HasPrefix(errorMsg, "validation failed:\n"))

	lines := strings.Split(errorMsg, "\n")
	require.Greater(t, len(lines), 1, "Error message should be multi-line")

	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			assert.True(t, strings.HasPrefix(lines[i], "  - "),
				"Each error line should start with '  - ', but got: %q", lines[i])
		}
	}

	assert.False(t, strings.HasPrefix(strings.TrimSpace(strings.Split(errorMsg, "\n")[1]), "[") &&
		strings.HasSuffix(strings.TrimSpace(errorMsg), "]"),
		"Error message should not be in Go slice format")
}

func TestValidateAutoscalingPolicy_NoErrors(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()
	policy := validAutoscalingPolicy()
	policy.Spec.Metrics = append(policy.Spec.Metrics, registryv1.AutoscalingPolicyMetric{
		MetricName:  "memory",
		TargetValue: resource.MustParse("75"),
	})

	allowed, errorMsg := validator.validateAutoscalingPolicy(policy)

	// Should be valid with no errors
	assert.True(t, allowed)
	assert.Empty(t, errorMsg)
}

func TestValidateAutoscalingPolicy_TolerancePercentRange(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()
	tests := []struct {
		name             string
		tolerancePercent int32
		wantError        string
	}{
		{
			name:             "negative tolerance percent",
			tolerancePercent: -1,
			wantError:        "tolerance percent must be between 0 and 100",
		},
		{
			name:             "tolerance percent greater than 100",
			tolerancePercent: 101,
			wantError:        "tolerance percent must be between 0 and 100",
		},
		{
			name:             "minimum tolerance percent",
			tolerancePercent: 0,
			wantError:        "",
		},
		{
			name:             "maximum tolerance percent",
			tolerancePercent: 100,
			wantError:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := validAutoscalingPolicy()
			policy.Spec.TolerancePercent = tt.tolerancePercent

			allowed, errorMsg := validator.validateAutoscalingPolicy(policy)
			if tt.wantError == "" {
				assert.True(t, allowed)
				assert.Empty(t, errorMsg)
				return
			}

			assert.False(t, allowed)
			assert.Contains(t, errorMsg, tt.wantError)
		})
	}
}

func TestValidateAutoscalingPolicy_PanicThresholdPercentRange(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()
	tests := []struct {
		name                  string
		panicThresholdPercent *int32
		wantError             string
	}{
		{
			name:                  "panic threshold below minimum",
			panicThresholdPercent: ptr.To(int32(109)),
			wantError:             "panic threshold percent must be between 110 and 1000",
		},
		{
			name:                  "panic threshold above maximum",
			panicThresholdPercent: ptr.To(int32(1001)),
			wantError:             "panic threshold percent must be between 110 and 1000",
		},
		{
			name:                  "minimum panic threshold",
			panicThresholdPercent: ptr.To(int32(110)),
			wantError:             "",
		},
		{
			name:                  "maximum panic threshold",
			panicThresholdPercent: ptr.To(int32(1000)),
			wantError:             "",
		},
		{
			name:                  "nil panic threshold",
			panicThresholdPercent: nil,
			wantError:             "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := validAutoscalingPolicy()
			policy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent = tt.panicThresholdPercent

			allowed, errorMsg := validator.validateAutoscalingPolicy(policy)
			if tt.wantError == "" {
				assert.True(t, allowed)
				assert.Empty(t, errorMsg)
				return
			}

			assert.False(t, allowed)
			assert.Contains(t, errorMsg, tt.wantError)
		})
	}
}

func TestAutoscalingPolicyValidator_Handle_ValidPolicy(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()
	req := autoscalingPolicyAdmissionRequest(t, validAutoscalingPolicy())
	w := httptest.NewRecorder()

	validator.Handle(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Parse response
	var responseReview admissionv1.AdmissionReview
	err := json.Unmarshal(w.Body.Bytes(), &responseReview)
	require.NoError(t, err)

	assert.True(t, responseReview.Response.Allowed)
	assert.Equal(t, types.UID("test-uid"), responseReview.Response.UID)
	if responseReview.Response.Result != nil {
		assert.Empty(t, responseReview.Response.Result.Message)
	}
}

func TestAutoscalingPolicyValidator_Handle_InvalidThresholds(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()
	policy := validAutoscalingPolicy()
	policy.Spec.TolerancePercent = 101
	policy.Spec.Behavior.ScaleUp.PanicPolicy.PanicThresholdPercent = ptr.To(int32(109))

	req := autoscalingPolicyAdmissionRequest(t, policy)
	w := httptest.NewRecorder()

	validator.Handle(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var responseReview admissionv1.AdmissionReview
	err := json.Unmarshal(w.Body.Bytes(), &responseReview)
	require.NoError(t, err)
	require.NotNil(t, responseReview.Response)
	require.NotNil(t, responseReview.Response.Result)

	assert.False(t, responseReview.Response.Allowed)
	assert.Equal(t, types.UID("test-uid"), responseReview.Response.UID)
	assert.Contains(t, responseReview.Response.Result.Message, "spec.tolerancePercent")
	assert.Contains(t, responseReview.Response.Result.Message, "tolerance percent must be between 0 and 100")
	assert.Contains(t, responseReview.Response.Result.Message, "spec.behavior.scaleUp.panicPolicy.panicThresholdPercent")
	assert.Contains(t, responseReview.Response.Result.Message, "panic threshold percent must be between 110 and 1000")
}

func autoscalingPolicyAdmissionRequest(t *testing.T, policy *registryv1.AutoscalingPolicy) *http.Request {
	t.Helper()

	policyBytes, err := json.Marshal(policy)
	require.NoError(t, err)

	admissionReview := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: types.UID("test-uid"),
			Object: runtime.RawExtension{
				Raw: policyBytes,
			},
		},
	}

	body, err := json.Marshal(admissionReview)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/validate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func validAutoscalingPolicy() *registryv1.AutoscalingPolicy {
	return &registryv1.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: registryv1.AutoscalingPolicySpec{
			TolerancePercent: 10,
			Metrics: []registryv1.AutoscalingPolicyMetric{
				{
					MetricName:  "cpu",
					TargetValue: resource.MustParse("80"),
				},
			},
			Behavior: registryv1.AutoscalingPolicyBehavior{
				ScaleDown: registryv1.AutoscalingPolicyStablePolicy{
					Instances:           ptr.To(int32(1)),
					Percent:             ptr.To(int32(10)),
					Period:              &metav1.Duration{Duration: time.Minute},
					SelectPolicy:        registryv1.SelectPolicyOr,
					StabilizationWindow: &metav1.Duration{Duration: time.Minute * 5},
				},
				ScaleUp: registryv1.AutoscalingPolicyScaleUpPolicy{
					StablePolicy: registryv1.AutoscalingPolicyStablePolicy{
						Instances:           ptr.To(int32(2)),
						Percent:             ptr.To(int32(20)),
						Period:              &metav1.Duration{Duration: time.Minute},
						SelectPolicy:        registryv1.SelectPolicyOr,
						StabilizationWindow: &metav1.Duration{Duration: time.Second * 30},
					},
					PanicPolicy: registryv1.AutoscalingPolicyPanicPolicy{
						Percent:               ptr.To(int32(50)),
						Period:                metav1.Duration{Duration: time.Second * 10},
						PanicThresholdPercent: ptr.To(int32(200)),
						PanicModeHold:         &metav1.Duration{Duration: time.Minute},
					},
				},
			},
		},
	}
}
