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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestModelCR creates a ModelBooster CR, waits for it to become active, and tests chat functionality.
func TestModelCR(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

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
		return meta.IsStatusConditionPresentAndEqual(model.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue)
	}, 5*time.Minute, 5*time.Second, "Model did not become Active")
	// Test chat via port-forward
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Where is the capital of China?"),
	}
	utils.CheckChatCompletions(t, "test-model", messages)
	// Test updating ModelBooster
	t.Log("Testing update of ModelBooster")
	require.Eventually(t, func() bool {
		m, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Get model error: %v", err)
			return false
		}
		m.Spec.Backend.Workers[0].Replicas = 2
		_, err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Update(ctx, m, metav1.UpdateOptions{})
		if err != nil {
			t.Logf("Update model error: %v", err)
			return false
		}
		return true
	}, 2*time.Minute, 5*time.Second, "Failed to update ModelBooster")

	// Verify the update took effect
	require.Eventually(t, func() bool {
		m, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return m.Spec.Backend.Workers[0].Replicas == 2
	}, 2*time.Minute, 5*time.Second, "ModelBooster update was not reflected")

	// Test deleting ModelBooster
	t.Log("Testing deletion of ModelBooster")
	err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Delete(ctx, model.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete Model CR")

	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			// We expect a NotFound error here
			return apierrors.IsNotFound(err)
		}
		return false
	}, 2*time.Minute, 5*time.Second, "ModelBooster was not deleted")
}

// TestModelBoosterSelfHealing validates that the controller instantly self-heals deleted child resources.
func TestModelBoosterSelfHealing(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create a Model CR in the test namespace
	model := createTestModel()
	model.Name = "self-healing-test-model"
	model.Spec.Name = "self-healing-test-model"

	waitForWebhookReady(t, ctx, kthenaClient, model.Namespace)

	createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, model, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Model CR")
	assert.NotNil(t, createdModel)

	t.Cleanup(func() {
		if err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Delete(context.Background(), model.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("cleanup: failed to delete ModelBooster %s: %v", model.Name, err)
		}
	})

	t.Logf("Created Model CR: %s/%s", createdModel.Namespace, createdModel.Name)

	// Wait for the Model to be Active
	require.Eventually(t, func() bool {
		m, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return meta.IsStatusConditionPresentAndEqual(m.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue)
	}, 5*time.Minute, 5*time.Second, "Model did not become Active")

	t.Log("Model is active. Testing self-healing of ModelServing...")

	// The controller contract guarantees the child resource is named: {modelName}-{backendName}
	expectedChildName := fmt.Sprintf("%s-%s", model.Name, model.Spec.Backend.Name)

	// Fetch the generated ModelServing deterministically by name
	servingToDelete, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
	require.NoError(t, err, "Expected ModelServing to be generated with deterministic name")

	err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, servingToDelete.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServing")

	// Wait for the controller to self-heal and recreate it
	require.Eventually(t, func() bool {
		recreated, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, servingToDelete.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return recreated.UID != servingToDelete.UID
	}, 1*time.Minute, 2*time.Second, "Controller failed to self-heal deleted ModelServing")

	t.Log("ModelServing was successfully self-healed. Test complete.")
}

func createValidModelBoosterForWebhookTest() *workload.ModelBooster {
	model := createTestModel()
	model.Name = "webhook-test-model"
	model.Spec.Name = "webhook-test-model"
	return model
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
				Name:     "backend1",
				Type:     workload.ModelBackendTypeVLLM,
				ModelURI: "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI: "hostpath:///tmp/cache",
				Replicas: 1,
				Workers: []workload.ModelWorker{
					{
						Type:      workload.ModelWorkerTypeServer,
						Image:     "ghcr.io/huntersman/vllm-cpu-env:latest",
						Replicas:  1,
						Pods:      1,
						Config:    *config,
						Resources: corev1ResourceRequirements(),
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
				Name:     "backend1",
				Type:     workload.ModelBackendTypeVLLM,
				ModelURI: "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI: "hostpath:///tmp/cache",
				Replicas: 1000001, // invalid: greater than maximum
				Workers: []workload.ModelWorker{
					{
						Type:      workload.ModelWorkerTypeServer,
						Image:     "ghcr.io/huntersman/vllm-cpu-env:latest",
						Replicas:  1,
						Pods:      1,
						Config:    *config,
						Resources: corev1ResourceRequirements(),
					},
				},
			},
		},
	}
}

// corev1ResourceRequirements is a helper to avoid duplication and keep imports clean
func corev1ResourceRequirements() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	}
}
