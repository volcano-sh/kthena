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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestValidateAutoscalingPolicy_ErrorFormatting(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()

	// Create an autoscaling policy that will trigger multiple validation errors
	policy := &registryv1.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
		Spec: registryv1.AutoscalingPolicySpec{
			TolerancePercent: 101, // This should trigger error: tolerance percent must be between 0 and 100
			Metrics: []registryv1.AutoscalingPolicyMetric{
				{
					Name:        "cpu",
					TargetValue: resource.MustParse("0"), // This should trigger error: target value must be greater than 0
				},
				{
					Name:        "cpu", // This should trigger error: duplicate metric name
					TargetValue: resource.MustParse("80"),
				},
			},
			Behavior: registryv1.AutoscalingPolicyBehavior{
				ScaleDown: registryv1.AutoscalingPolicyStablePolicy{
					Instances:           ptr.To(int32(1)),
					Period:              &metav1.Duration{Duration: -time.Minute},     // This should trigger error: negative period
					StabilizationWindow: &metav1.Duration{Duration: time.Minute * 35}, // This should trigger error: period too long
				},
				ScaleUp: registryv1.AutoscalingPolicyScaleUpPolicy{
					StablePolicy: registryv1.AutoscalingPolicyStablePolicy{
						Instances:           ptr.To(int32(2)),
						Period:              &metav1.Duration{Duration: time.Minute * 35}, // This should trigger error: period too long
						StabilizationWindow: &metav1.Duration{Duration: -time.Second},     // This should trigger error: negative stabilization window
					},
					PanicPolicy: registryv1.AutoscalingPolicyPanicPolicy{
						Percent:               ptr.To(int32(50)),
						Period:                metav1.Duration{Duration: time.Minute * 35}, // This should trigger error: period too long
						PanicThresholdPercent: ptr.To(int32(99)),                           // This should trigger error: threshold must be >= 100
						PanicModeHold:         &metav1.Duration{Duration: -time.Minute},    // This should trigger error: negative panic mode hold
					},
				},
			},
		},
	}

	allowed, errorMsg := validator.validateAutoscalingPolicy(policy)

	// Should not be valid due to multiple errors
	assert.False(t, allowed)
	assert.NotEmpty(t, errorMsg)

	// Check that the error message is properly formatted
	assert.True(t, strings.HasPrefix(errorMsg, "validation failed:\n"))

	// Check that errors are formatted with bullet points and line breaks
	lines := strings.Split(errorMsg, "\n")
	assert.True(t, len(lines) > 1, "Error message should be multi-line")

	// Check that each error line (except the first) starts with "  - "
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" { // Skip empty lines
			assert.True(t, strings.HasPrefix(lines[i], "  - "),
				"Each error line should start with '  - ', but got: %q", lines[i])
		}
	}

	// Verify that the error message is more readable than the old format
	// (should not be in Go slice format like [error1 error2 error3])
	assert.False(t, strings.HasPrefix(strings.TrimSpace(strings.Split(errorMsg, "\n")[1]), "[") &&
		strings.HasSuffix(strings.TrimSpace(errorMsg), "]"),
		"Error message should not be in Go slice format")

	t.Logf("Formatted error message:\n%s", errorMsg)
}

func TestValidateAutoscalingPolicy_NoErrors(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()

	// Create a valid autoscaling policy
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
				{
					Name:        "memory",
					TargetValue: resource.MustParse("75"),
				},
			},
			HomogeneousTarget: &registryv1.HomogeneousTarget{
				Target: registryv1.Target{
					TargetRef: corev1.ObjectReference{
						Kind: registryv1.ModelServingKind.Kind,
						Name: "test-target",
					},
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

	allowed, errorMsg := validator.validateAutoscalingPolicy(policy)

	// Should be valid with no errors
	assert.True(t, allowed)
	assert.Empty(t, errorMsg)
}

func TestValidateAutoscalingPolicy_DisaggregatedTarget(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()

	validPolicy := &registryv1.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pd-policy", Namespace: "default"},
		Spec: registryv1.AutoscalingPolicySpec{
			TolerancePercent: 10,
			DisaggregatedTarget: &registryv1.DisaggregatedTarget{
				TargetRef: corev1.ObjectReference{Kind: registryv1.ModelServingKind.Kind, Name: "pd-model"},
				Roles: map[string]registryv1.RoleScalingParam{
					"prefill": {
						MinReplicas: 1,
						MaxReplicas: 8,
						Metrics:     []registryv1.AutoscalingPolicyMetric{{Name: "prefill_load", TargetValue: resource.MustParse("5")}},
						MetricSources: map[string]registryv1.MetricSource{
							"prefill_load": {Prometheus: &registryv1.PrometheusMetricSource{ServerURL: "http://prometheus.default.svc:9090", Query: "prefill_load"}},
						},
					},
					"decode": {
						MinReplicas: 1,
						MaxReplicas: 16,
						Metrics:     []registryv1.AutoscalingPolicyMetric{{Name: "decode_load", TargetValue: resource.MustParse("80")}},
						MetricSources: map[string]registryv1.MetricSource{
							"decode_load": {Prometheus: &registryv1.PrometheusMetricSource{ServerURL: "http://prometheus.default.svc:9090", Query: "decode_load"}},
						},
					},
				},
				RatioConstraint: &registryv1.RoleRatioConstraint{
					NumeratorRole:   "prefill",
					DenominatorRole: "decode",
					MinRatio:        resource.MustParse("0.25"),
					MaxRatio:        resource.MustParse("1"),
				},
			},
		},
	}
	allowed, msg := validator.validateAutoscalingPolicy(validPolicy)
	assert.True(t, allowed, msg)

	overridePolicy := validPolicy.DeepCopy()
	overridePolicy.Spec.Metrics = []registryv1.AutoscalingPolicyMetric{{Name: "shared", TargetValue: resource.MustParse("1")}}
	allowed, msg = validator.validateAutoscalingPolicy(overridePolicy)
	assert.True(t, allowed, msg)

	invalidPolicy := validPolicy.DeepCopy()
	invalidPolicy.Spec.Metrics = []registryv1.AutoscalingPolicyMetric{{Name: "shared", TargetValue: resource.MustParse("1")}}
	invalidPolicy.Spec.DisaggregatedTarget.Roles["prefill"] = registryv1.RoleScalingParam{
		MinReplicas:   9,
		MaxReplicas:   8,
		Metrics:       []registryv1.AutoscalingPolicyMetric{{Name: "prefill_load", TargetValue: resource.MustParse("5")}},
		MetricSources: map[string]registryv1.MetricSource{"unknown": {Prometheus: &registryv1.PrometheusMetricSource{ServerURL: "http://prometheus.default.svc:9090", Query: "unknown"}}},
	}
	allowed, msg = validator.validateAutoscalingPolicy(invalidPolicy)
	assert.False(t, allowed)
	assert.NotContains(t, msg, "spec.metrics and per-role metrics are mutually exclusive")
	assert.Contains(t, msg, "minReplicas must be <= maxReplicas")
	assert.Contains(t, msg, "metricSources key must match an effective metric name")

	missingSourcesPolicy := validPolicy.DeepCopy()
	missingSourcesPolicy.Spec.DisaggregatedTarget.Roles["prefill"] = registryv1.RoleScalingParam{
		MinReplicas: 1,
		MaxReplicas: 8,
		Metrics:     []registryv1.AutoscalingPolicyMetric{{Name: "prefill_load", TargetValue: resource.MustParse("5")}},
	}
	allowed, msg = validator.validateAutoscalingPolicy(missingSourcesPolicy)
	assert.False(t, allowed)
	assert.Contains(t, msg, "metricSources must be set on every non-fixed role when metrics are configured")

	missingInheritedSourcesPolicy := validPolicy.DeepCopy()
	missingInheritedSourcesPolicy.Spec.Metrics = []registryv1.AutoscalingPolicyMetric{{Name: "shared", TargetValue: resource.MustParse("1")}}
	missingInheritedSourcesPolicy.Spec.DisaggregatedTarget.Roles["prefill"] = registryv1.RoleScalingParam{MinReplicas: 1, MaxReplicas: 8}
	allowed, msg = validator.validateAutoscalingPolicy(missingInheritedSourcesPolicy)
	assert.False(t, allowed)
	assert.Contains(t, msg, "metricSources must be set on every non-fixed role when metrics are configured")
}

func TestValidateAutoscalingPolicy_DisaggregatedSingleRoleAndFixedRole(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()

	singleRolePolicy := &registryv1.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "single-role-policy", Namespace: "default"},
		Spec: registryv1.AutoscalingPolicySpec{
			TolerancePercent: 10,
			DisaggregatedTarget: &registryv1.DisaggregatedTarget{
				TargetRef: corev1.ObjectReference{Kind: registryv1.ModelServingKind.Kind, Name: "pd-model"},
				Roles: map[string]registryv1.RoleScalingParam{
					"prefill": {
						MinReplicas: 1,
						MaxReplicas: 8,
						Metrics:     []registryv1.AutoscalingPolicyMetric{{Name: "prefill_load", TargetValue: resource.MustParse("5")}},
						MetricSources: map[string]registryv1.MetricSource{
							"prefill_load": {Prometheus: &registryv1.PrometheusMetricSource{ServerURL: "http://prometheus.default.svc:9090", Query: "prefill_load"}},
						},
					},
				},
			},
		},
	}
	allowed, msg := validator.validateAutoscalingPolicy(singleRolePolicy)
	assert.True(t, allowed, msg)

	fixedPeerPolicy := singleRolePolicy.DeepCopy()
	fixedPeerPolicy.Spec.DisaggregatedTarget.Roles["decode"] = registryv1.RoleScalingParam{MinReplicas: 2, MaxReplicas: 2}
	allowed, msg = validator.validateAutoscalingPolicy(fixedPeerPolicy)
	assert.True(t, allowed, msg)

	missingMetricsPolicy := singleRolePolicy.DeepCopy()
	missingMetricsPolicy.Spec.DisaggregatedTarget.Roles["prefill"] = registryv1.RoleScalingParam{MinReplicas: 1, MaxReplicas: 8}
	allowed, msg = validator.validateAutoscalingPolicy(missingMetricsPolicy)
	assert.False(t, allowed)
	assert.Contains(t, msg, "metrics must be set on every non-fixed role when spec.metrics is empty")

	ratioSingleRolePolicy := singleRolePolicy.DeepCopy()
	ratioSingleRolePolicy.Spec.DisaggregatedTarget.RatioConstraint = &registryv1.RoleRatioConstraint{
		NumeratorRole:   "prefill",
		DenominatorRole: "decode",
		MinRatio:        resource.MustParse("0.25"),
		MaxRatio:        resource.MustParse("1"),
	}
	allowed, msg = validator.validateAutoscalingPolicy(ratioSingleRolePolicy)
	assert.False(t, allowed)
	assert.Contains(t, msg, "denominatorRole must exist in roles")
}

func TestAutoscalingPolicyValidator_Handle_ValidPolicy(t *testing.T) {
	validator := NewAutoscalingPolicyValidator()

	// Create a valid AutoscalingPolicy
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
			HomogeneousTarget: &registryv1.HomogeneousTarget{
				Target: registryv1.Target{
					TargetRef: corev1.ObjectReference{
						Kind: registryv1.ModelServingKind.Kind,
						Name: "test-target",
					},
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
	req := httptest.NewRequest("POST", "/validate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
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
