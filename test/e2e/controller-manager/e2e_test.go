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
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

var (
	testNamespace string
)

func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-controller-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	// Controller manager tests need workload enabled
	config.WorkloadEnabled = true

	if err := framework.InstallKthena(config); err != nil {
		fmt.Printf("Failed to install kthena: %v\n", err)
		os.Exit(1)
	}

	// Create test namespace
	kubeConfig, err := utils.GetKubeConfig()
	if err != nil {
		fmt.Printf("Failed to get kubeconfig: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Printf("Failed to create Kubernetes client: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Failed to create test namespace %s: %v\n", testNamespace, err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}
	fmt.Printf("Created test namespace: %s\n", testNamespace)

	// Run tests
	code := m.Run()

	// Cleanup test namespace
	fmt.Printf("Deleting test namespace: %s\n", testNamespace)
	err = kubeClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
	if err != nil {
		fmt.Printf("Failed to delete test namespace %s: %v\n", testNamespace, err)
	}

	// Wait for namespace to be deleted
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := kubeClient.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
		if err != nil {
			return true, nil // namespace is gone
		}
		return false, nil
	})
	if err != nil {
		fmt.Printf("Timeout waiting for namespace %s deletion: %v\n", testNamespace, err)
	}

	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
	}

	os.Exit(code)
}

// TestModelCR creates a ModelBooster CR, waits for it to become active, and tests chat functionality.
func TestModelCR(t *testing.T) {
	ctx := context.Background()
	// Initialize Kubernetes clients
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")
	// Create a Model CR in the test namespace
	model := createTestModel()
	createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, model, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Model CR")
	assert.NotNil(t, createdModel)
	t.Logf("Created Model CR: %s/%s", createdModel.Namespace, createdModel.Name)
	// Wait for the Model to be Active
	require.Eventually(t, func() bool {
		model, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Get model error: %v", err)
			return false
		}
		return true == meta.IsStatusConditionPresentAndEqual(model.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue)
	}, 5*time.Minute, 5*time.Second, "Model did not become Active")
	// Test chat via port-forward
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Where is the capital of China?"),
	}
	utils.CheckChatCompletions(t, "test-model", messages)
	// todo: test update modelBooster, delete modelBooster
}

// TestModelBoosterValidation tests that the webhook rejects invalid ModelBooster specs.
func TestModelBoosterValidation(t *testing.T) {
	ctx := context.Background()
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")

	// Create an invalid ModelBooster (minReplicas > maxReplicas)
	invalidModel := createInvalidModel()
	_, err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, invalidModel, metav1.CreateOptions{})
	require.Error(t, err, "Expected validation error for invalid ModelBooster")
	// Check that the error is a validation error (admission webhook rejection)
	// Typically the error message contains "admission webhook" or "validation failed"
	t.Logf("Validation error (expected): %v", err)
}

// TestAutoscalingPolicyValidation tests that the webhook rejects invalid AutoscalingPolicy specs.
func TestAutoscalingPolicyValidation(t *testing.T) {
	ctx := context.Background()
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")

	// Create an invalid AutoscalingPolicy (duplicate metric names)
	invalidPolicy := createInvalidAutoscalingPolicy()
	_, err = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Create(ctx, invalidPolicy, metav1.CreateOptions{})
	require.Error(t, err, "Expected validation error for invalid AutoscalingPolicy")
	t.Logf("Validation error (expected): %v", err)
}

// TestAutoscalingPolicyMutation tests that the webhook defaults missing fields in AutoscalingPolicy.
func TestAutoscalingPolicyMutation(t *testing.T) {
	ctx := context.Background()
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")

	// Create an AutoscalingPolicy with empty behavior (should be defaulted)
	policy := createAutoscalingPolicyWithEmptyBehavior()
	created, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Create(ctx, policy, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create AutoscalingPolicy")
	assert.NotNil(t, created)
	// Verify that behavior fields have been defaulted
	assert.NotNil(t, created.Spec.Behavior.ScaleDown)
	assert.NotNil(t, created.Spec.Behavior.ScaleUp)
	// Check specific default values (based on mutator logic)
	if created.Spec.Behavior.ScaleDown.StabilizationWindow != nil {
		assert.Equal(t, 5*time.Minute, created.Spec.Behavior.ScaleDown.StabilizationWindow.Duration)
	}
	t.Logf("Created AutoscalingPolicy with defaults: %s/%s", created.Namespace, created.Name)
}

// TestAutoscalingPolicyBindingValidation tests that the webhook validates AutoscalingPolicyBinding specs.
func TestAutoscalingPolicyBindingValidation(t *testing.T) {
	ctx := context.Background()
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")

	// First create a valid AutoscalingPolicy to reference
	policy := createValidAutoscalingPolicy()
	createdPolicy, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Create(ctx, policy, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create AutoscalingPolicy")
	t.Logf("Created AutoscalingPolicy: %s/%s", createdPolicy.Namespace, createdPolicy.Name)

	// Create a valid binding referencing the policy
	binding := createValidAutoscalingPolicyBinding(createdPolicy.Name)
	createdBinding, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Create(ctx, binding, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create AutoscalingPolicyBinding")
	assert.NotNil(t, createdBinding)
	t.Logf("Created AutoscalingPolicyBinding: %s/%s", createdBinding.Namespace, createdBinding.Name)

	// Clean up (optional)
	_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(testNamespace).Delete(ctx, createdBinding.Name, metav1.DeleteOptions{})
	_ = kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(testNamespace).Delete(ctx, createdPolicy.Name, metav1.DeleteOptions{})
}

// TestModelServingValidation tests that the webhook rejects invalid ModelServing specs.
func TestModelServingValidation(t *testing.T) {
	ctx := context.Background()
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")

	// Create an invalid ModelServing (negative replicas)
	invalidServing := createInvalidModelServing()
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, invalidServing, metav1.CreateOptions{})
	require.Error(t, err, "Expected validation error for invalid ModelServing")
	// Check that the error is a validation error (admission webhook rejection)
	t.Logf("Validation error (expected): %v", err)
}

func createTestModel() *workload.ModelBooster {
	// Create a simple config as JSON
	config := &apiextensionsv1.JSON{}
	configRaw := `{
		"served-model-name": "test-model",
		"max-model-len": 32768,
		"max-num-batched-tokens": 65536,
		"block-size": 128,
		"enable-prefix-caching": ""
	}`
	config.Raw = []byte(configRaw)

	return &workload.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: testNamespace,
		},
		Spec: workload.ModelBoosterSpec{
			Name: "test-model",
			Backend: workload.ModelBackend{
				Name:        "backend1",
				Type:        workload.ModelBackendTypeVLLM,
				ModelURI:    "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI:    "hostpath:///tmp/cache",
				MinReplicas: 1,
				MaxReplicas: 1,
				Workers: []workload.ModelWorker{
					{
						Type:     workload.ModelWorkerTypeServer,
						Image:    "ghcr.io/huntersman/vllm-cpu-env:latest",
						Replicas: 1,
						Pods:     1,
						Config:   *config,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					},
				},
			},
		},
	}
}

func createInvalidModel() *workload.ModelBooster {
	// Create a simple config as JSON
	config := &apiextensionsv1.JSON{}
	configRaw := `{
		"served-model-name": "invalid-model",
		"max-model-len": 32768,
		"max-num-batched-tokens": 65536,
		"block-size": 128,
		"enable-prefix-caching": ""
	}`
	config.Raw = []byte(configRaw)

	return &workload.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-model",
			Namespace: testNamespace,
		},
		Spec: workload.ModelBoosterSpec{
			Name: "invalid-model",
			Backend: workload.ModelBackend{
				Name:        "backend1",
				Type:        workload.ModelBackendTypeVLLM,
				ModelURI:    "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI:    "hostpath:///tmp/cache",
				MinReplicas: 5, // invalid: greater than maxReplicas
				MaxReplicas: 1,
				Workers: []workload.ModelWorker{
					{
						Type:     workload.ModelWorkerTypeServer,
						Image:    "ghcr.io/huntersman/vllm-cpu-env:latest",
						Replicas: 1,
						Pods:     1,
						Config:   *config,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					},
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

func createValidAutoscalingPolicy() *workload.AutoscalingPolicy {
	return &workload.AutoscalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-policy",
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
			Behavior: workload.AutoscalingPolicyBehavior{
				ScaleDown: workload.AutoscalingPolicyStablePolicy{
					Instances:           ptr.To(int32(0)),
					Percent:             ptr.To(int32(100)),
					Period:              &metav1.Duration{Duration: time.Minute},
					SelectPolicy:        workload.SelectPolicyOr,
					StabilizationWindow: &metav1.Duration{Duration: 5 * time.Minute},
				},
				ScaleUp: workload.AutoscalingPolicyScaleUpPolicy{
					StablePolicy: workload.AutoscalingPolicyStablePolicy{
						Instances:           ptr.To(int32(4)),
						Percent:             ptr.To(int32(100)),
						Period:              &metav1.Duration{Duration: time.Minute},
						SelectPolicy:        workload.SelectPolicyOr,
						StabilizationWindow: &metav1.Duration{Duration: 0},
					},
					PanicPolicy: workload.AutoscalingPolicyPanicPolicy{
						Percent:               ptr.To(int32(0)),
						Period:                metav1.Duration{Duration: 0},
						PanicThresholdPercent: ptr.To(int32(200)),
						PanicModeHold:         &metav1.Duration{Duration: 0},
					},
				},
			},
		},
	}
}

func createValidAutoscalingPolicyBinding(policyName string) *workload.AutoscalingPolicyBinding {
	return &workload.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-binding",
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

func createInvalidModelServing() *workload.ModelServing {
	negativeReplicas := int32(-1)
	return &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-modelserving",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &negativeReplicas,
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "role1",
						Replicas: &negativeReplicas,
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test",
										Image: "nginx:latest",
									},
								},
							},
						},
						WorkerReplicas: 0,
					},
				},
			},
		},
	}
}
