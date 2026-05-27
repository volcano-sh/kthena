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
		_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Delete(cctx, "autoscale-policy-lifecycle-bind", metav1.DeleteOptions{})
		_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Delete(cctx, "autoscale-policy-lifecycle-pol", metav1.DeleteOptions{})
	})

	binding := utils.LoadYAMLFromFile[workload.AutoscalingPolicyBinding](filepath.Join(autoscaleTestDataRel, "autoscaling-policy-binding-e2e-policy-lifecycle.yaml"))
	binding.Namespace = testNamespace
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Create(ctx, binding, metav1.CreateOptions{})
	require.NoError(t, err, "create AutoscalingPolicyBinding")

	utils.WaitForModelServingSpecReplicas(t, ctx, kthenaClient, testNamespace, ms.Name, 1, 8*time.Minute)

	updatedPol, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Get(ctx, policy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	updatedPol.Spec.Metrics[0].TargetValue = resource.MustParse("100m")
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Update(ctx, updatedPol, metav1.UpdateOptions{})
	require.NoError(t, err, "update AutoscalingPolicy for scale-up")

	utils.WaitForModelServingSpecReplicas(t, ctx, kthenaClient, testNamespace, ms.Name, 5, 8*time.Minute)

	require.NoError(t, kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Delete(ctx, binding.Name, metav1.DeleteOptions{}))
	require.NoError(t, kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Delete(ctx, policy.Name, metav1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Get(ctx, policy.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 3*time.Second, "AutoscalingPolicy should be deleted")
}

// TestAutoscalingPolicyBindingLifecycle uses fixed targets in testdata/; binding delete + recreate.
func TestAutoscalingPolicyBindingLifecycle(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	ms := utils.LoadYAMLFromFile[workload.ModelServing](filepath.Join(autoscaleTestDataRel, "model-serving-sglang-mocker-binding-lifecycle.yaml"))
	ms.Namespace = testNamespace
	createAndWaitForModelServing(t, ctx, kthenaClient, ms)

	policy := utils.LoadYAMLFromFile[workload.AutoscalingPolicy](filepath.Join(autoscaleTestDataRel, "autoscaling-policy-e2e-sglang-binding-lifecycle.yaml"))
	policy.Namespace = testNamespace
	_, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Create(ctx, policy, metav1.CreateOptions{})
	require.NoError(t, err, "create AutoscalingPolicy")
	t.Cleanup(func() {
		cctx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Delete(cctx, "autoscale-binding-lifecycle-a", metav1.DeleteOptions{})
		_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Delete(cctx, "autoscale-binding-lifecycle-b", metav1.DeleteOptions{})
		_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Delete(cctx, "autoscale-binding-lifecycle-pol", metav1.DeleteOptions{})
	})

	bindA := utils.LoadYAMLFromFile[workload.AutoscalingPolicyBinding](filepath.Join(autoscaleTestDataRel, "autoscaling-policy-binding-e2e-binding-lifecycle-a.yaml"))
	bindA.Namespace = testNamespace
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Create(ctx, bindA, metav1.CreateOptions{})
	require.NoError(t, err, "create first AutoscalingPolicyBinding")

	utils.WaitForModelServingSpecReplicas(t, ctx, kthenaClient, testNamespace, ms.Name, 1, 8*time.Minute)

	require.NoError(t, kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Delete(ctx, bindA.Name, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Get(ctx, bindA.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 3*time.Second, "first binding should be deleted")

	updatedPol, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Get(ctx, policy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	updatedPol.Spec.Metrics[0].TargetValue = resource.MustParse("100m")
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Update(ctx, updatedPol, metav1.UpdateOptions{})
	require.NoError(t, err, "update AutoscalingPolicy for scale-up")

	bindB := utils.LoadYAMLFromFile[workload.AutoscalingPolicyBinding](filepath.Join(autoscaleTestDataRel, "autoscaling-policy-binding-e2e-binding-lifecycle-b.yaml"))
	bindB.Namespace = testNamespace
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Create(ctx, bindB, metav1.CreateOptions{})
	require.NoError(t, err, "create second AutoscalingPolicyBinding")

	utils.WaitForModelServingSpecReplicas(t, ctx, kthenaClient, testNamespace, ms.Name, 5, 8*time.Minute)
}

func createAutoscalingPolicyWithNegativeTarget() *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "negative-target-policy",
			Namespace: testNamespace,
		},
		Spec: workload.AutoscalingPolicySpec{
			TolerancePercent: 10,
			Metrics: []workload.AutoscalingPolicyMetric{
				{
					MetricName:  "cpu",
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
			TolerancePercent: 10,
			Metrics: []workload.AutoscalingPolicyMetric{
				{
					MetricName:  "cpu",
					TargetValue: resource.MustParse("100m"),
				},
				{
					MetricName:  "cpu", // duplicate metric name
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
			TolerancePercent: 10,
			Metrics: []workload.AutoscalingPolicyMetric{
				{
					MetricName:  "cpu",
					TargetValue: resource.MustParse("100m"),
				},
			},
			// Behavior is empty, should be defaulted by mutator
		},
	}
}

func createTestAutoscalingPolicyBinding(policyName string) *workload.AutoscalingPolicyBinding {
	return &workload.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-binding",
			Namespace: testNamespace,
		},
		Spec: workload.AutoscalingPolicyBindingSpec{
			PolicyRef: corev1.LocalObjectReference{
				Name: policyName,
			},
			HomogeneousTarget: &workload.HomogeneousTarget{
				Target: workload.Target{
					TargetRef: corev1.ObjectReference{
						Name: "some-model-serving",
						Kind: workload.ModelServingKind.Kind,
					},
				},
				MinReplicas: 1,
				MaxReplicas: 10,
			},
		},
	}
}
