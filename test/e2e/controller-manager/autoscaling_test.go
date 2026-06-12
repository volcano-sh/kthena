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
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const autoscaleTestDataRel = "test/e2e/controller-manager/testdata"

// TestAutoscalingPolicyLifecycle uses fixed AutoscalingPolicy targets in testdata/ (no metrics scrape).
// It exercises the full single-resource AutoscalingPolicy lifecycle: create (scale down to min),
// update metric target (scale up), then delete.
func TestAutoscalingPolicyLifecycle(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	ms := utils.LoadYAMLFromFile[workload.ModelServing](filepath.Join(autoscaleTestDataRel, "model-serving-sglang-mocker-policy-lifecycle.yaml"))
	ms.Namespace = testNamespace
	createAndWaitForModelServing(t, ctx, kthenaClient, ms)

	policy := utils.LoadYAMLFromFile[workload.AutoscalingPolicy](filepath.Join(autoscaleTestDataRel, "autoscaling-policy-e2e-sglang-policy-lifecycle.yaml"))
	policy.Namespace = testNamespace
	_, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Create(ctx, policy, metav1.CreateOptions{})
	require.NoError(t, err, "create AutoscalingPolicy")
	t.Cleanup(func() {
		cctx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Delete(cctx, "autoscale-policy-lifecycle-pol", metav1.DeleteOptions{})
	})

	// Large targetValue drives desired replicas down to minReplicas (1).
	utils.WaitForModelServingSpecReplicas(t, ctx, kthenaClient, testNamespace, ms.Name, 1, 8*time.Minute)

	// Lower the metric target to trigger scale-up towards maxReplicas (5).
	updatedPol, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Get(ctx, policy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	updatedPol.Spec.Metrics[0].TargetValue = resource.MustParse("100m")
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Update(ctx, updatedPol, metav1.UpdateOptions{})
	require.NoError(t, err, "update AutoscalingPolicy for scale-up")

	utils.WaitForModelServingSpecReplicas(t, ctx, kthenaClient, testNamespace, ms.Name, 5, 8*time.Minute)

	require.NoError(t, kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Delete(ctx, policy.Name, metav1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Get(ctx, policy.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 3*time.Second, "AutoscalingPolicy should be deleted")
}

func createAutoscalingPolicyWithNegativeTarget() *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "negative-target-policy",
			Namespace: testNamespace,
		},
		Spec: workload.AutoscalingPolicySpec{
			TolerancePercent:  10,
			HomogeneousTarget: webhookTestHomogeneousTarget(),
			Metrics: []workload.AutoscalingPolicyMetric{
				{
					Name:        "cpu",
					TargetValue: resource.MustParse("-100m"),
				},
			},
		},
	}
}

func createInvalidAutoscalingPolicy() *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-policy",
			Namespace: testNamespace,
		},
		Spec: workload.AutoscalingPolicySpec{
			TolerancePercent:  10,
			HomogeneousTarget: webhookTestHomogeneousTarget(),
			Metrics: []workload.AutoscalingPolicyMetric{
				{
					Name:        "cpu",
					TargetValue: resource.MustParse("100m"),
				},
				{
					Name:        "cpu", // duplicate metric name
					TargetValue: resource.MustParse("200m"),
				},
			},
			Behavior: workload.AutoscalingPolicyBehavior{},
		},
	}
}

func createAutoscalingPolicyWithEmptyBehavior() *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "defaulted-policy",
			Namespace: testNamespace,
		},
		Spec: workload.AutoscalingPolicySpec{
			TolerancePercent:  10,
			HomogeneousTarget: webhookTestHomogeneousTarget(),
			Metrics: []workload.AutoscalingPolicyMetric{
				{
					Name:        "cpu",
					TargetValue: resource.MustParse("100m"),
				},
			},
			// Behavior is empty, should be defaulted by mutator
		},
	}
}

func webhookTestHomogeneousTarget() *workload.HomogeneousTarget {
	return &workload.HomogeneousTarget{
		Target: workload.Target{
			TargetRef: corev1.ObjectReference{
				Name: "some-model-serving",
				Kind: workload.ModelServingKind.Kind,
			},
		},
		MinReplicas: 1,
		MaxReplicas: 10,
	}
}
