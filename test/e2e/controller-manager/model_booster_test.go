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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	mbutils "github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	testDataDir                   = "test/e2e/controller-manager/testdata"
	modelBoosterTestSchedulerName = "volcano"
)

// TestModelCR creates a ModelBooster CR from YAML, waits for it to become active,
// tests chat functionality, verifies generated child resources and ownership,
// validates self-healing of deleted child resources, updates the CR,
// and verifies cascading deletion.
func TestModelCR(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Load the ModelBooster CR from YAML fixture
	model := utils.LoadYAMLFromFile[workload.ModelBooster](filepath.Join(testDataDir, "ModelBooster-vllm.yaml"))
	model.Namespace = testNamespace

	createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, model, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Model CR")
	require.NotNil(t, createdModel)
	t.Cleanup(func() {
		if err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Delete(context.Background(), model.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: failed to delete ModelBooster %s: %v", model.Name, err)
		}
	})
	t.Logf("Created Model CR: %s/%s", createdModel.Namespace, createdModel.Name)

	// Wait for the Model to be Active
	require.Eventually(t, func() bool {
		m, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Get model error: %v", err)
			return false
		}
		return meta.IsStatusConditionPresentAndEqual(m.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue)
	}, 5*time.Minute, 5*time.Second, "Model did not become Active")

	// Test chat via port-forward
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Where is the capital of China?"),
	}
	utils.WaitForChatModelReady(t, utils.DefaultRouterURL, "test-model", messages, 2*time.Minute)
	utils.CheckChatCompletions(t, "test-model", messages)

	expectedChildName := mbutils.GetBackendResourceName(model.Name, model.Spec.Backend.Name)

	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
	require.NoError(t, err, "Generated ModelServing should exist")
	assertOwnedByModelBooster(t, ms.OwnerReferences, createdModel)
	assertModelBoosterLabels(t, ms.Labels, createdModel, model.Spec.Backend.Name)

	mserver, err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
	require.NoError(t, err, "Generated ModelServer should exist")
	assertOwnedByModelBooster(t, mserver.OwnerReferences, createdModel)
	assertModelBoosterLabels(t, mserver.Labels, createdModel, model.Spec.Backend.Name)

	// ModelRoute expects an empty backendName
	mroute, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
	require.NoError(t, err, "Generated ModelRoute should exist")
	assertOwnedByModelBooster(t, mroute.OwnerReferences, createdModel)
	assertModelBoosterLabels(t, mroute.Labels, createdModel, "")

	// Record revision label for comparison
	msRevisionBefore := ms.Labels[mbutils.RevisionLabelKey]
	require.NotEmpty(t, msRevisionBefore, "ModelServing revision label should be set")

	t.Log("Testing self-healing of child resources")
	assertModelBoosterChildrenSelfHeal(t, ctx, kthenaClient, model)

	t.Log("Testing update of ModelBooster")
	require.Eventually(t, func() bool {
		m, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Get model error: %v", err)
			return false
		}
		m.Spec.Backend.SchedulerName = modelBoosterTestSchedulerName
		_, err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Update(ctx, m, metav1.UpdateOptions{})
		if err != nil {
			t.Logf("Update model error: %v", err)
			return false
		}
		return true
	}, 2*time.Minute, 5*time.Second, "Failed to update ModelBooster")

	require.Eventually(t, func() bool {
		m, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return m.Spec.Backend.SchedulerName == modelBoosterTestSchedulerName
	}, 2*time.Minute, 5*time.Second, "ModelBooster update was not reflected")

	// ModelServing hashes model.Spec.Backend, so backend changes should mutate the revision label.
	t.Log("Verifying revision label changed on ModelServing after update")
	require.Eventually(t, func() bool {
		updatedMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return updatedMS.Labels[mbutils.RevisionLabelKey] != msRevisionBefore
	}, 2*time.Minute, 5*time.Second, "ModelServing revision label should change after spec update")

	t.Log("Testing explicit deletion of ModelBooster")
	err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Delete(ctx, model.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete Model CR")

	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			return apierrors.IsNotFound(err)
		}
		return false
	}, 2*time.Minute, 5*time.Second, "ModelBooster was not deleted")

	t.Log("Verifying generated resources were garbage-collected")
	ownerUIDSelector := labels.SelectorFromSet(map[string]string{
		mbutils.OwnerUIDKey: string(createdModel.UID),
	})
	listOpts := metav1.ListOptions{LabelSelector: ownerUIDSelector.String()}

	require.Eventually(t, func() bool {
		list, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).List(ctx, listOpts)
		return err == nil && len(list.Items) == 0
	}, 2*time.Minute, 5*time.Second, "Orphan ModelServings should be garbage-collected")

	require.Eventually(t, func() bool {
		list, err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).List(ctx, listOpts)
		return err == nil && len(list.Items) == 0
	}, 2*time.Minute, 5*time.Second, "Orphan ModelServers should be garbage-collected")

	require.Eventually(t, func() bool {
		list, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).List(ctx, listOpts)
		return err == nil && len(list.Items) == 0
	}, 2*time.Minute, 5*time.Second, "Orphan ModelRoutes should be garbage-collected")
}

// TestModelCRDisaggregated verifies that a vLLMDisaggregated ModelBooster is
// reconciled into the split prefill/decode ModelServing shape.
func TestModelCRDisaggregated(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	model := utils.LoadYAMLFromFile[workload.ModelBooster](filepath.Join(testDataDir, "ModelBooster-vllm-disaggregated.yaml"))
	model.Namespace = testNamespace

	createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, model, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create disaggregated ModelBooster")
	require.NotNil(t, createdModel)
	t.Cleanup(func() {
		if err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Delete(context.Background(), model.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: failed to delete ModelBooster %s: %v", model.Name, err)
		}
	})

	expectedChildName := mbutils.GetBackendResourceName(model.Name, model.Spec.Backend.Name)

	var modelServing *workload.ModelServing
	require.Eventually(t, func() bool {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		modelServing = ms
		return true
	}, 2*time.Minute, 2*time.Second, "Disaggregated ModelServing should be created")

	assertOwnedByModelBooster(t, modelServing.OwnerReferences, createdModel)
	assertModelBoosterLabels(t, modelServing.Labels, createdModel, model.Spec.Backend.Name)
	require.Len(t, modelServing.Spec.Template.Roles, 2, "Disaggregated ModelServing should contain prefill and decode roles")
	assertModelServingRoleNames(t, modelServing, workload.ModelWorkerTypePrefill, workload.ModelWorkerTypeDecode)

	var modelServerLabels map[string]string
	var modelServerOwnerReferences []metav1.OwnerReference
	require.Eventually(t, func() bool {
		modelServer, err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		modelServerOwnerReferences = modelServer.OwnerReferences
		modelServerLabels = modelServer.Labels
		return true
	}, 2*time.Minute, 2*time.Second, "Disaggregated ModelServer should be created")
	assertOwnedByModelBooster(t, modelServerOwnerReferences, createdModel)
	assertModelBoosterLabels(t, modelServerLabels, createdModel, model.Spec.Backend.Name)

	err = kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Delete(ctx, model.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete disaggregated ModelBooster")

	ownerUIDSelector := labels.SelectorFromSet(map[string]string{
		mbutils.OwnerUIDKey: string(createdModel.UID),
	})
	listOpts := metav1.ListOptions{LabelSelector: ownerUIDSelector.String()}

	require.Eventually(t, func() bool {
		list, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).List(ctx, listOpts)
		return err == nil && len(list.Items) == 0
	}, 2*time.Minute, 2*time.Second, "Disaggregated ModelServing should be garbage-collected")

	require.Eventually(t, func() bool {
		list, err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).List(ctx, listOpts)
		return err == nil && len(list.Items) == 0
	}, 2*time.Minute, 2*time.Second, "Disaggregated ModelServer should be garbage-collected")
}

// assertModelBoosterChildrenSelfHeal verifies that deleting child resources triggers controller recreation.
func assertModelBoosterChildrenSelfHeal(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, model *workload.ModelBooster) {
	t.Helper()
	expectedChildName := mbutils.GetBackendResourceName(model.Name, model.Spec.Backend.Name)

	t.Log("Testing self-healing of ModelServing...")
	servingToDelete, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
	require.NoError(t, err, "Expected ModelServing to be generated with deterministic name")

	err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, servingToDelete.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServing")

	require.Eventually(t, func() bool {
		recreated, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, servingToDelete.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return recreated.UID != servingToDelete.UID
	}, 1*time.Minute, 2*time.Second, "Controller failed to self-heal deleted ModelServing")

	t.Log("Testing self-healing of ModelServer...")
	serverToDelete, err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Get(ctx, expectedChildName, metav1.GetOptions{})
	require.NoError(t, err, "Expected ModelServer to be generated with deterministic name")

	err = kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Delete(ctx, serverToDelete.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServer")

	require.Eventually(t, func() bool {
		recreated, err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Get(ctx, serverToDelete.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return recreated.UID != serverToDelete.UID
	}, 1*time.Minute, 2*time.Second, "Controller failed to self-heal deleted ModelServer")

	t.Log("Testing self-healing of ModelRoute...")
	routeToDelete, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
	require.NoError(t, err, "Expected ModelRoute to be generated with model name")

	err = kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(ctx, routeToDelete.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelRoute")

	require.Eventually(t, func() bool {
		recreated, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, routeToDelete.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return recreated.UID != routeToDelete.UID
	}, 1*time.Minute, 2*time.Second, "Controller failed to self-heal deleted ModelRoute")
}

func createValidModelBoosterForWebhookTest() *workload.ModelBooster {
	model := utils.LoadYAMLFromFile[workload.ModelBooster](filepath.Join(testDataDir, "ModelBooster-vllm.yaml"))
	model.Name = "webhook-test-model"
	model.Spec.Name = "webhook-test-model"
	return model
}

func createInvalidModel() *workload.ModelBooster {
	model := utils.LoadYAMLFromFile[workload.ModelBooster](filepath.Join(testDataDir, "ModelBooster-vllm.yaml"))
	model.Name = "invalid-model"
	model.Spec.Name = "invalid-model"
	model.Spec.Backend.Replicas = 1000001 // invalid: exceeds maximum allowed replicas
	return model
}

// assertOwnedByModelBooster verifies that an OwnerReference list contains exactly one entry
// pointing to the given ModelBooster with controller=true and blockOwnerDeletion=true.
func assertOwnedByModelBooster(t *testing.T, refs []metav1.OwnerReference, booster *workload.ModelBooster) {
	t.Helper()
	require.Len(t, refs, 1, "Expected exactly one OwnerReference")
	ref := refs[0]
	assert.Equal(t, booster.Name, ref.Name, "OwnerReference name should match ModelBooster")
	assert.Equal(t, string(booster.UID), string(ref.UID), "OwnerReference UID should match ModelBooster")
	assert.Equal(t, workload.ModelKind.Kind, ref.Kind, "OwnerReference kind should be ModelBooster")
	require.NotNil(t, ref.Controller, "OwnerReference controller flag should be set")
	assert.True(t, *ref.Controller, "OwnerReference controller should be true")
	require.NotNil(t, ref.BlockOwnerDeletion, "OwnerReference blockOwnerDeletion flag should be set")
	assert.True(t, *ref.BlockOwnerDeletion, "OwnerReference blockOwnerDeletion should be true")
}

// assertModelBoosterLabels verifies that a resource's labels contain the expected
// ModelBooster controller labels. backendName should be "" for ModelRoute.
func assertModelBoosterLabels(t *testing.T, resourceLabels map[string]string, booster *workload.ModelBooster, backendName string) {
	t.Helper()
	assert.Equal(t, booster.Name, resourceLabels[mbutils.ModelNameLabelKey], "label model-name")
	assert.Equal(t, backendName, resourceLabels[mbutils.BackendNameLabelKey], "label backend-name")
	assert.Equal(t, string(booster.UID), resourceLabels[mbutils.OwnerUIDKey], "label model-uid")
	assert.Equal(t, workload.GroupName, resourceLabels[mbutils.ManageBy], "label managed-by")
	assert.NotEmpty(t, resourceLabels[mbutils.RevisionLabelKey], "label revision should not be empty")
}

func assertModelServingRoleNames(t *testing.T, modelServing *workload.ModelServing, expectedRoles ...workload.ModelWorkerType) {
	t.Helper()
	actual := make(map[workload.ModelWorkerType]struct{}, len(modelServing.Spec.Template.Roles))
	for _, role := range modelServing.Spec.Template.Roles {
		actual[workload.ModelWorkerType(role.Name)] = struct{}{}
	}
	for _, expectedRole := range expectedRoles {
		_, ok := actual[expectedRole]
		assert.True(t, ok, "ModelServing should contain %s role", expectedRole)
	}
}
