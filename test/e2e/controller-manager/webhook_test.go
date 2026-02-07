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

package controller_manager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestWebhook tests that the webhooks (validation and mutation) work as expected.
func TestWebhook(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	testCases := []struct {
		name          string
		resource      metav1.Object
		expectError   bool
		errorMsg      string
		checkMutation func(t *testing.T, obj interface{})
	}{
		{
			name:        "Invalid ModelBooster (minReplicas > maxReplicas)",
			resource:    createInvalidModel(),
			expectError: true,
			errorMsg:    "minReplicas cannot be greater than maxReplicas",
		},
		{
			name:        "Valid ModelBooster (DryRun)",
			resource:    createValidModelBoosterForWebhookTest(),
			expectError: false,
		},
		{
			name:        "Invalid AutoscalingPolicy (duplicate metric names)",
			resource:    createInvalidAutoscalingPolicy(),
			expectError: true,
			errorMsg:    "duplicate metric name",
		},
		{
			name:        "Invalid AutoscalingPolicy (negative target value)",
			resource:    createAutoscalingPolicyWithNegativeTarget(),
			expectError: true,
			errorMsg:    "metric target value must be greater than 0",
		},
		{
			name:        "AutoscalingPolicy Mutation (defaulting behavior)",
			resource:    createAutoscalingPolicyWithEmptyBehavior(),
			expectError: false,
			checkMutation: func(t *testing.T, obj interface{}) {
				policy := obj.(*workload.AutoscalingPolicy)
				assert.NotNil(t, policy.Spec.Behavior.ScaleDown)
				assert.NotNil(t, policy.Spec.Behavior.ScaleUp)
				if policy.Spec.Behavior.ScaleDown.StabilizationWindow != nil {
					assert.Equal(t, 5*time.Minute, policy.Spec.Behavior.ScaleDown.StabilizationWindow.Duration)
				}
			},
		},
		{
			name:        "Invalid AutoscalingPolicyBinding (non-existent policy)",
			resource:    createTestAutoscalingPolicyBinding("non-existent-policy"),
			expectError: true,
			errorMsg:    "autoscaling policy resource non-existent-policy does not exist",
		},
		{
			name:        "Invalid ModelServing (negative replicas)",
			resource:    createInvalidModelServing(),
			expectError: true,
			errorMsg:    "should be a non-negative integer",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			var created interface{}

			switch r := tc.resource.(type) {
			case *workload.ModelBooster:
				created, err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, r, metav1.CreateOptions{DryRun: []string{"All"}})
			case *workload.AutoscalingPolicy:
				created, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Create(ctx, r, metav1.CreateOptions{DryRun: []string{"All"}})
			case *workload.AutoscalingPolicyBinding:
				created, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Create(ctx, r, metav1.CreateOptions{DryRun: []string{"All"}})
			case *workload.ModelServing:
				created, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, r, metav1.CreateOptions{DryRun: []string{"All"}})
			default:
				t.Fatalf("Unknown resource type: %T", tc.resource)
			}

			if tc.expectError {
				require.Error(t, err, "Expected validation error")
				assert.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err, "Failed to create resource")
				if tc.checkMutation != nil {
					tc.checkMutation(t, created)
				}
			}
		})
	}
}
