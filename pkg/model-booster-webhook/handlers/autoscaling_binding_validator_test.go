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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateAutoscalingBinding(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(&v1alpha1.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dummy-policy",
			Namespace: "default",
		},
		Spec: v1alpha1.AutoscalingPolicySpec{},
	})
	validator := NewAutoscalingBindingValidator(fakeClient)

	tests := []struct {
		name     string
		input    *v1alpha1.AutoscalingPolicyBinding
		expected []string
	}{
		{
			name: "optimizer and scaling config both set to nil",
			input: &v1alpha1.AutoscalingPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dummy-model",
					Namespace: "default",
				},
				Spec: v1alpha1.AutoscalingPolicyBindingSpec{
					PolicyRef: corev1.LocalObjectReference{
						Name: "dummy-policy",
					},
					HeterogeneousTarget: nil,
					HomogeneousTarget:   nil,
				},
			},
			expected: []string{"  - spec.homogeneousTarget: Required value: spec.homogeneousTarget should be set if spec.heterogeneousTarget does not exist"},
		},
		{
			name: "optimizer and scaling config both are not nil",
			input: &v1alpha1.AutoscalingPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dummy-model",
					Namespace: "default",
				},
				Spec: v1alpha1.AutoscalingPolicyBindingSpec{
					PolicyRef: corev1.LocalObjectReference{
						Name: "dummy-policy",
					},
					HeterogeneousTarget: &v1alpha1.HeterogeneousTarget{
						Params: []v1alpha1.HeterogeneousTargetParam{
							{
								Target: v1alpha1.Target{
									TargetRef: corev1.ObjectReference{
										Name: "target-name",
									},
								},
								MinReplicas: 1,
								MaxReplicas: 2,
							},
						},
						CostExpansionRatePercent: 100,
					},
					HomogeneousTarget: &v1alpha1.HomogeneousTarget{
						Target: v1alpha1.Target{
							TargetRef: corev1.ObjectReference{
								Name: "target-name",
							},
						},
						MinReplicas: 1,
						MaxReplicas: 2,
					},
				},
			},
			expected: []string{"  - spec.homogeneousTarget: Forbidden: both spec.heterogeneousTarget and spec.homogeneousTarget can not be set at the same time"},
		},
		{
			name: "different autoscaling policy name",
			input: &v1alpha1.AutoscalingPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dummy-model",
					Namespace: "default",
				},
				Spec: v1alpha1.AutoscalingPolicyBindingSpec{
					PolicyRef: corev1.LocalObjectReference{
						Name: "not-exist-policy",
					},
					HeterogeneousTarget: nil,
					HomogeneousTarget: &v1alpha1.HomogeneousTarget{
						Target: v1alpha1.Target{
							TargetRef: corev1.ObjectReference{
								Name: "target-name",
							},
						},
						MinReplicas: 1,
						MaxReplicas: 2,
					},
				},
			},
			expected: []string{"  - spec.PolicyRef: Invalid value: \"not-exist-policy\": autoscaling policy resource not-exist-policy does not exist"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, errorMsg := validator.validateAutoscalingBinding(tt.input)
			if len(tt.expected) == 0 {
				assert.True(t, valid)
				return
			}
			// Should not be valid due to multiple errors
			assert.False(t, valid)
			assert.NotEmpty(t, errorMsg)

			// Check that the error message is properly formatted
			assert.True(t, strings.HasPrefix(errorMsg, "validation failed:\n"))
			errorMsg = strings.TrimPrefix(errorMsg, "validation failed:\n")

			lines := strings.Split(errorMsg, "\n")

			assert.Equal(t, tt.expected, lines)
		})
	}
}

func TestValidateBindingTargetKind_HeterogeneousValidModelServing(t *testing.T) {
	asp := &v1alpha1.AutoscalingPolicyBinding{
		Spec: v1alpha1.AutoscalingPolicyBindingSpec{
			HeterogeneousTarget: &v1alpha1.HeterogeneousTarget{
				Params: []v1alpha1.HeterogeneousTargetParam{
					{
						Target: v1alpha1.Target{
							TargetRef: corev1.ObjectReference{Kind: v1alpha1.ModelServingKind.Kind},
						},
						MinReplicas: 0,
						MaxReplicas: 1,
					},
				},
			},
		},
	}
	errs := validateBindingTargetKind(asp)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %d: %v", len(errs), errs)
	}
}

func TestValidateBindingTargetKind_HeterogeneousValidRole(t *testing.T) {
	asp := &v1alpha1.AutoscalingPolicyBinding{
		Spec: v1alpha1.AutoscalingPolicyBindingSpec{
			HeterogeneousTarget: &v1alpha1.HeterogeneousTarget{
				Params: []v1alpha1.HeterogeneousTargetParam{
					{
						Target: v1alpha1.Target{
							TargetRef: corev1.ObjectReference{Kind: v1alpha1.ModelServingKind.Kind + ModelServingRoleKindSuffix},
						},
						MinReplicas: 0,
						MaxReplicas: 1,
					},
				},
			},
		},
	}
	errs := validateBindingTargetKind(asp)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %d: %v", len(errs), errs)
	}
}

func TestValidateBindingTargetKind_HeterogeneousInvalid(t *testing.T) {
	invalidKind := "Deployment"
	asp := &v1alpha1.AutoscalingPolicyBinding{
		Spec: v1alpha1.AutoscalingPolicyBindingSpec{
			HeterogeneousTarget: &v1alpha1.HeterogeneousTarget{
				Params: []v1alpha1.HeterogeneousTargetParam{
					{
						Target: v1alpha1.Target{
							TargetRef: corev1.ObjectReference{Kind: invalidKind},
						},
						MinReplicas: 0,
						MaxReplicas: 1,
					},
				},
			},
		},
	}
	errs := validateBindingTargetKind(asp)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].Type != field.ErrorTypeInvalid {
		t.Fatalf("expected invalid error type, got %v", errs[0].Type)
	}
	if errs[0].Field != "spec.heterogeneousTarget.params[0].targetRef.kind" {
		t.Fatalf("unexpected field path: %s", errs[0].Field)
	}
}

func TestValidateBindingTargetKind_HomogeneousValidModelServing(t *testing.T) {
	asp := &v1alpha1.AutoscalingPolicyBinding{
		Spec: v1alpha1.AutoscalingPolicyBindingSpec{
			HomogeneousTarget: &v1alpha1.HomogeneousTarget{
				Target: v1alpha1.Target{
					TargetRef: corev1.ObjectReference{Kind: v1alpha1.ModelServingKind.Kind},
				},
				MinReplicas: 0,
				MaxReplicas: 1,
			},
		},
	}
	errs := validateBindingTargetKind(asp)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %d: %v", len(errs), errs)
	}
}

func TestValidateBindingTargetKind_HomogeneousInvalid(t *testing.T) {
	invalidKind := "Unknown"
	asp := &v1alpha1.AutoscalingPolicyBinding{
		Spec: v1alpha1.AutoscalingPolicyBindingSpec{
			HomogeneousTarget: &v1alpha1.HomogeneousTarget{
				Target: v1alpha1.Target{
					TargetRef: corev1.ObjectReference{Kind: invalidKind},
				},
				MinReplicas: 0,
				MaxReplicas: 1,
			},
		},
	}
	errs := validateBindingTargetKind(asp)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].Type != field.ErrorTypeInvalid {
		t.Fatalf("expected invalid error type, got %v", errs[0].Type)
	}
	if errs[0].Field != "spec.homogeneousTarget.targetRef.kind" {
		t.Fatalf("unexpected field path: %s", errs[0].Field)
	}
}
