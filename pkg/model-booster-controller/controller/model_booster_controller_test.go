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

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

// TestReconcile first creates a model and then checks if the ModelServing, ModelServer and ModelRoute are created as expected.
// Then the model is updated, check if ModelRoute is updated. At last, model will be deleted.
func TestReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Create fake clients for Kubernetes and Kthena
	kubeClient := fake.NewClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	controller := NewModelBoosterController(kubeClient, kthenaClient)
	assert.NotNil(t, controller)
	// Start controller
	go controller.Run(ctx, 1)
	// Load test data
	model := loadYaml[workload.ModelBooster](t, "../convert/testdata/input/model.yaml")

	// Case1: Create a model with ASP, and then model serving, model server, model route, ASP, ASP binding should be created.
	// Step1. Create model
	createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).Create(ctx, model, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, createdModel)
	// Step2. Check that ASP, ASP binding, model serving, model server, model route are created
	assert.True(t, waitForCondition(func() bool {
		aspBindings, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicyBindings(model.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		return len(aspBindings.Items) == 1
	}))
	// ASP should be created
	aspList, err := kthenaClient.WorkloadV1alpha1().AutoscalingPolicies(model.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Len(t, aspList.Items, 1, "Expected 1 AutoscalingPolicy to be created")
	// model serving should be created
	modelServingList, err := kthenaClient.WorkloadV1alpha1().ModelServings(model.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Len(t, modelServingList.Items, 1, "Expected 1 ModelServing to be created")
	// model server should be created
	modelServers, err := kthenaClient.NetworkingV1alpha1().ModelServers(model.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Len(t, modelServers.Items, 1, "Expected 1 ModelServer to be created")
	// model route should be created
	modelRoutes, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(model.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Len(t, modelRoutes.Items, 1, "Expected 1 ModelRoute to be create")
	// Step3. mock model serving status available
	modelServing := &modelServingList.Items[0]
	meta.SetStatusCondition(&modelServing.Status.Conditions, newCondition(string(workload.ModelServingAvailable),
		metav1.ConditionTrue, "AllGroupsReady", "AllGroupsReady"))
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(model.Namespace).UpdateStatus(ctx, modelServing, metav1.UpdateOptions{})
	assert.NoError(t, err)
	// Step4. Check that model condition should be active
	assert.True(t, waitForCondition(func() bool {
		model, err = kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return true == meta.IsStatusConditionPresentAndEqual(model.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue) && model.Generation == model.Status.ObservedGeneration
	}))

	// Case2: noop update and ensure route still exists
	model.Generation += 1
	_, err = kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).Update(ctx, model, metav1.UpdateOptions{})
	assert.NoError(t, err)
	assert.True(t, waitForCondition(func() bool {
		routes, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(model.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil || len(routes.Items) == 0 {
			return false
		}
		return true
	}))

	// Case3: delete model. Because we are not running in a real K8s cluster, model server, model route, model serving,
	// ASP and ASP binding will not be deleted automatically. So here only check if model is deleted.
	err = kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).Delete(ctx, model.Name, metav1.DeleteOptions{})
	assert.NoError(t, err)
	assert.True(t, waitForCondition(func() bool {
		modelList, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		return len(modelList.Items) == 0
	}))
}

func TestReconcile_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Create fake clients for Kubernetes and Kthena
	kubeClient := fake.NewClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	controller := NewModelBoosterController(kubeClient, kthenaClient)
	assert.NotNil(t, &controller)
	// start informers
	go controller.modelsInformer.RunWithContext(ctx)
	go controller.modelServingInformer.RunWithContext(ctx)
	go controller.autoscalingPoliciesInformer.RunWithContext(ctx)
	go controller.autoscalingPolicyBindingsInformer.RunWithContext(ctx)
	// Case1: Invalid namespaceAndName
	t.Run("InvalidNameSpaceAndName", func(t *testing.T) {
		err := controller.reconcile(ctx, "//")
		assert.Errorf(t, err, "invalid resource key: //")
	})

	// Case2: Create ModelServing failed
	t.Run("CreateModelServingFailed", func(t *testing.T) {
		model := &workload.ModelBooster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "not-supported-model",
				Namespace: "default",
			},
			Spec: workload.ModelBoosterSpec{
				Backend: workload.ModelBackend{
					Name: "not-supported-backend-type",
					Type: workload.ModelBackendTypeMindIEDisaggregated,
				},
			},
		}
		createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).Create(ctx, model, metav1.CreateOptions{})
		assert.NoError(t, err)
		assert.NotNil(t, createdModel)
		assert.True(t, waitForCondition(func() bool {
			err = controller.reconcile(ctx, model.Namespace+"/"+model.Name)
			return err.Error() == "not support model backend type: MindIEDisaggregated"
		}))
		get, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(model.Namespace).Get(ctx, model.Name, metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Equal(t, true, meta.IsStatusConditionPresentAndEqual(get.Status.Conditions,
			string(workload.ModelStatusConditionTypeFailed), metav1.ConditionTrue))
	})
}

func TestCreateModel(t *testing.T) {
	kubeClient := fake.NewClientset()
	kthenaClient := kthenafake.NewClientset()
	controller := NewModelBoosterController(kubeClient, kthenaClient)
	controller.createModelBooster("wrong")
}

func TestUpdateModel(t *testing.T) {
	kubeClient := fake.NewClientset()
	kthenaClient := kthenafake.NewClientset()
	controller := NewModelBoosterController(kubeClient, kthenaClient)
	assert.NotNil(t, &controller)
	model := loadYaml[workload.ModelBooster](t, "../convert/testdata/input/model.yaml")
	// invalid old
	controller.updateModelBooster("invalid", model)
	assert.Equal(t, 0, controller.workQueue.Len())
	// invalid new
	controller.updateModelBooster(model, "invalid")
	assert.Equal(t, 0, controller.workQueue.Len())
}

func TestDeleteModel(t *testing.T) {
	kubeClient := fake.NewClientset()
	kthenaClient := kthenafake.NewClientset()
	controller := NewModelBoosterController(kubeClient, kthenaClient)
	controller.deleteModelBooster("invalid")
}

func TestTriggerModel(t *testing.T) {
	kubeClient := fake.NewClientset()
	kthenaClient := kthenafake.NewClientset()
	controller := NewModelBoosterController(kubeClient, kthenaClient)
	assert.NotNil(t, &controller)
	modelServing := loadYaml[workload.ModelServing](t, "../convert/testdata/expected/model-serving.yaml")
	// invalid new
	controller.triggerModel(modelServing, "invalid")
	assert.Equal(t, 0, controller.workQueue.Len())
	// invalid old
	controller.triggerModel("invalid", modelServing)
	assert.Equal(t, 0, controller.workQueue.Len())
}

// Removed tests for LoRA adapter changes as the current API defines a single backend without loraAdapters

// loadYaml transfer yaml data into a struct of type T.
// Used for test.
func loadYaml[T any](t *testing.T, path string) *T {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read YAML: %v", err)
	}
	var expected T
	if err := yaml.Unmarshal(data, &expected); err != nil {
		t.Fatalf("Failed to unmarshal YAML: %v", err)
	}
	return &expected
}

// waitForCondition repeatedly checks a condition function until it returns true or a timeout occurs.
func waitForCondition(checkFunc func() bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if checkFunc() {
				return true
			}
		}
	}
}
