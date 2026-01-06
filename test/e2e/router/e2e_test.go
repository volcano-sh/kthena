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

package router

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	testCtx       *routercontext.RouterTestContext
	testNamespace string
)

// TestMain runs setup and cleanup for all tests in this package.
func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-router-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	// Router tests need networking enabled
	config.NetworkingEnabled = true

	if err := framework.InstallKthena(config); err != nil {
		fmt.Printf("Failed to install kthena: %v\n", err)
		os.Exit(1)
	}

	var err error
	testCtx, err = routercontext.NewRouterTestContext(testNamespace)
	if err != nil {
		fmt.Printf("Failed to create router test context: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	// Create test namespace
	if err := testCtx.CreateTestNamespace(); err != nil {
		fmt.Printf("Failed to create test namespace: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	// Setup common components
	if err := testCtx.SetupCommonComponents(); err != nil {
		fmt.Printf("Failed to setup common components: %v\n", err)
		_ = testCtx.DeleteTestNamespace()
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	// Cleanup common components
	if err := testCtx.CleanupCommonComponents(); err != nil {
		fmt.Printf("Failed to cleanup common components: %v\n", err)
	}

	// Delete test namespace
	if err := testCtx.DeleteTestNamespace(); err != nil {
		fmt.Printf("Failed to delete test namespace: %v\n", err)
	}

	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
	}

	os.Exit(code)
}

// TestModelRouteSimple tests a simple ModelRoute deployment and access.
func TestModelRouteSimple(t *testing.T) {
	ctx := context.Background()

	// Deploy ModelRoute
	t.Log("Deploying ModelRoute...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("test/e2e/router/testdata/kthena-router/ModelRouteSimple.yaml")
	modelRoute.Namespace = testNamespace
	createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute")
	assert.NotNil(t, createdModelRoute)
	t.Logf("Created ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)

	// Register cleanup function to delete ModelRoute after test completes
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", createdModelRoute.Namespace, createdModelRoute.Name, err)
		}
	})

	// Test accessing the model route (with retry logic)
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}
	utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
}

func TestModelRouteMultiModels(t *testing.T) {
	ctx := context.Background()

	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("test/e2e/router/testdata/kthena-router/ModelRouteMultiModels.yaml")
	modelRoute.Namespace = testNamespace
	createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute")
	assert.NotNil(t, createdModelRoute)
	t.Logf("Created ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", createdModelRoute.Namespace, createdModelRoute.Name, err)
		}
	})

	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}

	t.Run("PremiumHeaderRoutesTo7BModel", func(t *testing.T) {
		headers := map[string]string{"user-type": "premium"}
		resp := utils.CheckChatCompletionsWithHeaders(t, modelRoute.Spec.ModelName, messages, headers)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.Contains(t, resp.Body, "DeepSeek-R1-Distill-Qwen-7B", "Expected response from 7B model")
	})

	t.Run("DefaultRequestsRouteTo1_5BModel", func(t *testing.T) {
		resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.Contains(t, resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B", "Expected response from 1.5B model")
	})

	t.Run("HeaderMatchingRulePriority", func(t *testing.T) {
		headers := map[string]string{"user-type": "premium"}
		resp := utils.CheckChatCompletionsWithHeaders(t, modelRoute.Spec.ModelName, messages, headers)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.Contains(t, resp.Body, "DeepSeek-R1-Distill-Qwen-7B", "Premium header should route to 7B model")
	})

	t.Run("DefaultBehaviorWhenNoRulesMatch", func(t *testing.T) {
		headers := map[string]string{"user-type": "basic"}
		resp := utils.CheckChatCompletionsWithHeaders(t, modelRoute.Spec.ModelName, messages, headers)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.Contains(t, resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B", "Non-matching header should fall back to 1.5B model")
	})

	t.Run("EmptyHeaderValueFallsToDefault", func(t *testing.T) {
		headers := map[string]string{"user-type": ""}
		resp := utils.CheckChatCompletionsWithHeaders(t, modelRoute.Spec.ModelName, messages, headers)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.Contains(t, resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B", "Empty header should fall back to 1.5B model")
	})
}

// TestModelRoutePrefillDecodeDisaggregation tests PD disaggregation with ModelServing, ModelServer, and ModelRoute.
func TestModelRoutePrefillDecodeDisaggregation(t *testing.T) {
	ctx := context.Background()

	// Deploy ModelServing
	t.Log("Deploying ModelServing for PD disaggregation...")
	modelServing := utils.LoadYAMLFromFile[workloadv1alpha1.ModelServing]("test/e2e/router/testdata/kthena-router/ModelServing-ds1.5b-pd-disaggragation.yaml")
	modelServing.Namespace = testNamespace
	createdModelServing, err := testCtx.KthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")
	assert.NotNil(t, createdModelServing)
	t.Logf("Created ModelServing: %s/%s", createdModelServing.Namespace, createdModelServing.Name)

	// Register cleanup function to delete ModelServing after test completes
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", createdModelServing.Namespace, createdModelServing.Name)
		if err := testCtx.KthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, createdModelServing.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", createdModelServing.Namespace, createdModelServing.Name, err)
		}
	})

	// Wait for ModelServing to be ready
	utils.WaitForModelServingReady(t, ctx, testCtx.KthenaClient, testNamespace, createdModelServing.Name)

	// Deploy ModelServer
	t.Log("Deploying ModelServer for PD disaggregation...")
	modelServer := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer]("test/e2e/router/testdata/kthena-router/ModelServer-ds1.5b-pd-disaggragation.yaml")
	modelServer.Namespace = testNamespace
	createdModelServer, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Create(ctx, modelServer, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServer")
	assert.NotNil(t, createdModelServer)
	t.Logf("Created ModelServer: %s/%s", createdModelServer.Namespace, createdModelServer.Name)

	// Register cleanup function to delete ModelServer after test completes
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServer: %s/%s", createdModelServer.Namespace, createdModelServer.Name)
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Delete(cleanupCtx, createdModelServer.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServer %s/%s: %v", createdModelServer.Namespace, createdModelServer.Name, err)
		}
	})

	// Deploy ModelRoute
	t.Log("Deploying ModelRoute for PD disaggregation...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("test/e2e/router/testdata/kthena-router/ModelRoute-ds1.5b-pd-disaggragation.yaml")
	modelRoute.Namespace = testNamespace
	createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute")
	assert.NotNil(t, createdModelRoute)
	t.Logf("Created ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)

	// Register cleanup function to delete ModelRoute after test completes
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", createdModelRoute.Namespace, createdModelRoute.Name, err)
		}
	})

	// Test accessing the model route (with retry logic)
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}
	utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
}
