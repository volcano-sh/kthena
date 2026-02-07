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
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
		return true == meta.IsStatusConditionPresentAndEqual(model.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue)
	}, 5*time.Minute, 5*time.Second, "Model did not become Active")
	// Test chat via port-forward
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Where is the capital of China?"),
	}
	utils.CheckChatCompletions(t, "test-model", messages)
	// TODO(user): Add tests for updating and deleting ModelBooster
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
				Name:        "backend1",
				Type:        workload.ModelBackendTypeVLLM,
				ModelURI:    "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI:    "hostpath:///tmp/cache",
				MinReplicas: 1,
				MaxReplicas: 1,
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
				Name:        "backend1",
				Type:        workload.ModelBackendTypeVLLM,
				ModelURI:    "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI:    "hostpath:///tmp/cache",
				MinReplicas: 5, // invalid: greater than maxReplicas
				MaxReplicas: 1,
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
