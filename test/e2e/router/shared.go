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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// setupModelRouteWithGatewayAPI configures ModelRoute with ParentRefs to default Gateway if useGatewayAPI is true.
func setupModelRouteWithGatewayAPI(modelRoute *networkingv1alpha1.ModelRoute, useGatewayAPI bool, kthenaNamespace string) {
	if useGatewayAPI {
		ktNamespace := gatewayv1.Namespace(kthenaNamespace)
		if len(modelRoute.Spec.ParentRefs) > 0 {
			// Update existing parentRefs namespace
			for i := range modelRoute.Spec.ParentRefs {
				modelRoute.Spec.ParentRefs[i].Namespace = &ktNamespace
			}
		} else {
			// Add parentRefs to default Gateway
			modelRoute.Spec.ParentRefs = []gatewayv1.ParentReference{
				{
					Name:      "default",
					Namespace: &ktNamespace,
					Kind:      func() *gatewayv1.Kind { k := gatewayv1.Kind("Gateway"); return &k }(),
				},
			}
		}
	}
}

// TestModelRouteSimpleShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestModelRouteSimpleShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	// Deploy ModelRoute
	t.Log("Deploying ModelRoute...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteSimple.yaml")
	modelRoute.Namespace = testNamespace

	// Configure ParentRefs if using Gateway API
	setupModelRouteWithGatewayAPI(modelRoute, useGatewayAPI, kthenaNamespace)

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

// TestModelRouteMultiModelsShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestModelRouteMultiModelsShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteMultiModels.yaml")
	modelRoute.Namespace = testNamespace

	// Configure ParentRefs if using Gateway API
	setupModelRouteWithGatewayAPI(modelRoute, useGatewayAPI, kthenaNamespace)

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

// TestModelRoutePrefillDecodeDisaggregationShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestModelRoutePrefillDecodeDisaggregationShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	// Deploy ModelServing
	t.Log("Deploying ModelServing for PD disaggregation...")
	modelServing := utils.LoadYAMLFromFile[workloadv1alpha1.ModelServing]("examples/kthena-router/ModelServing-ds1.5b-pd-disaggragation.yaml")
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
	modelServer := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer]("examples/kthena-router/ModelServer-ds1.5b-pd-disaggragation.yaml")
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
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRoute-ds1.5b-pd-disaggragation.yaml")
	modelRoute.Namespace = testNamespace

	// Configure ParentRefs if using Gateway API
	setupModelRouteWithGatewayAPI(modelRoute, useGatewayAPI, kthenaNamespace)

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

// TestModelRouteSubsetShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestModelRouteSubsetShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	// Deploy Canary versions of ModelServer and LLM-Mock
	t.Log("Deploying Canary ModelServers and LLM-Mock deployments...")

	// Deploy Canary LLM-Mock deployments from YAML file
	canaryDeployments := utils.LoadMultiResourceYAMLFromFile[appsv1.Deployment]("examples/kthena-router/LLM-Mock-ds1.5b-Canary.yaml")
	require.Len(t, canaryDeployments, 2, "Canary YAML should contain 2 deployments")

	deploymentV1 := canaryDeployments[0]
	deploymentV1.Namespace = testNamespace
	_, err := testCtx.KubeClient.AppsV1().Deployments(testNamespace).Create(ctx, deploymentV1, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Canary deployment v1")

	deploymentV2 := canaryDeployments[1]
	deploymentV2.Namespace = testNamespace
	_, err = testCtx.KubeClient.AppsV1().Deployments(testNamespace).Create(ctx, deploymentV2, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Canary deployment v2")

	// Wait for deployments to be ready
	require.Eventually(t, func() bool {
		deployV1, err := testCtx.KubeClient.AppsV1().Deployments(testNamespace).Get(ctx, "deepseek-r1-1-5b-v1", metav1.GetOptions{})
		if err != nil {
			return false
		}
		deployV2, err := testCtx.KubeClient.AppsV1().Deployments(testNamespace).Get(ctx, "deepseek-r1-1-5b-v2", metav1.GetOptions{})
		if err != nil {
			return false
		}
		return deployV1.Status.ReadyReplicas == *deployV1.Spec.Replicas &&
			deployV2.Status.ReadyReplicas == *deployV2.Spec.Replicas
	}, 5*time.Minute, 5*time.Second, "Canary deployments should be ready")

	// Deploy Canary ModelServers from YAML file
	canaryModelServers := utils.LoadMultiResourceYAMLFromFile[networkingv1alpha1.ModelServer]("examples/kthena-router/ModelServer-ds1.5b-Canary.yaml")
	require.Len(t, canaryModelServers, 2, "Canary YAML should contain 2 ModelServers")

	modelServerV1 := canaryModelServers[0]
	modelServerV1.Namespace = testNamespace
	_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Create(ctx, modelServerV1, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Canary ModelServer v1")

	modelServerV2 := canaryModelServers[1]
	modelServerV2.Namespace = testNamespace
	_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Create(ctx, modelServerV2, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Canary ModelServer v2")

	// Cleanup Canary resources
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Log("Cleaning up Canary resources...")
		_ = testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Delete(cleanupCtx, "deepseek-r1-1-5b-v1", metav1.DeleteOptions{})
		_ = testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Delete(cleanupCtx, "deepseek-r1-1-5b-v2", metav1.DeleteOptions{})
		_ = testCtx.KubeClient.AppsV1().Deployments(testNamespace).Delete(cleanupCtx, "deepseek-r1-1-5b-v1", metav1.DeleteOptions{})
		_ = testCtx.KubeClient.AppsV1().Deployments(testNamespace).Delete(cleanupCtx, "deepseek-r1-1-5b-v2", metav1.DeleteOptions{})
	})

	// Create ModelRoute with Canary ModelServer names
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteSubset.yaml")
	modelRoute.Namespace = testNamespace

	// Configure ParentRefs if using Gateway API
	setupModelRouteWithGatewayAPI(modelRoute, useGatewayAPI, kthenaNamespace)

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

	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}

	t.Run("WeightedTrafficDistribution", func(t *testing.T) {
		// Send multiple requests and verify weight distribution statistics
		// Use more requests to reduce randomness impact
		const totalRequests = 500
		v1Count := 0
		v2Count := 0

		for i := 0; i < totalRequests; i++ {
			resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
			assert.Equal(t, 200, resp.StatusCode)
			assert.NotEmpty(t, resp.Body)

			if strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B-v1") {
				v1Count++
			} else if strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B-v2") {
				v2Count++
			}
		}

		// Verify weight distribution statistics across multiple requests
		// 1. Verify statistics completeness: all requests are accounted for
		totalCounted := v1Count + v2Count
		assert.Equal(t, totalRequests, totalCounted, "All requests should be accounted for in statistics")

		// 2. Calculate and verify distribution ratios
		v1Ratio := float64(v1Count) / float64(totalRequests)
		v2Ratio := float64(v2Count) / float64(totalRequests)
		expectedV1Ratio := 0.70
		expectedV2Ratio := 0.30
		maxDeviation := 0.05 // Allow ±5% deviation for randomness

		// 3. Verify weight distribution statistics match expected ratio (70:30)
		assert.GreaterOrEqual(t, v1Ratio, expectedV1Ratio-maxDeviation,
			"deepseek-r1-1-5b ratio should be at least %.1f%% (expected %.1f%%)", (expectedV1Ratio-maxDeviation)*100, expectedV1Ratio*100)
		assert.LessOrEqual(t, v1Ratio, expectedV1Ratio+maxDeviation,
			"deepseek-r1-1-5b ratio should be at most %.1f%% (expected %.1f%%)", (expectedV1Ratio+maxDeviation)*100, expectedV1Ratio*100)
		assert.GreaterOrEqual(t, v2Ratio, expectedV2Ratio-maxDeviation,
			"deepseek-r1-7b ratio should be at least %.1f%% (expected %.1f%%)", (expectedV2Ratio-maxDeviation)*100, expectedV2Ratio*100)
		assert.LessOrEqual(t, v2Ratio, expectedV2Ratio+maxDeviation,
			"deepseek-r1-7b ratio should be at most %.1f%% (expected %.1f%%)", (expectedV2Ratio+maxDeviation)*100, expectedV2Ratio*100)

		// 4. Verify statistics sum to 100%
		assert.Equal(t, 1.0, v1Ratio+v2Ratio, "Distribution ratios should sum to exactly 100%")

		// Log statistics for debugging
		t.Logf("Weight distribution statistics verified:")
		t.Logf("  Total requests: %d, Counted: %d", totalRequests, totalCounted)
		t.Logf("  deepseek-r1-1-5b-v1: %d requests (%.1f%%, expected %.1f%%)", v1Count, v1Ratio*100, expectedV1Ratio*100)
		t.Logf("  deepseek-r1-1-5b-v2: %d requests (%.1f%%, expected %.1f%%)", v2Count, v2Ratio*100, expectedV2Ratio*100)
	})

	t.Run("WeightSumNot100Percent", func(t *testing.T) {
		// Update ModelRoute with weights that don't sum to 100%
		// Weights are relative, so 50:30 means 5/8 and 3/8 traffic distribution
		updatedModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
		require.NoError(t, err)

		// Modify weights to 50:30 (relative weights, will result in 5/8 and 3/8 distribution)
		weight50 := uint32(50)
		weight30 := uint32(30)
		updatedModelRoute.Spec.Rules[0].TargetModels[0].Weight = &weight50
		updatedModelRoute.Spec.Rules[0].TargetModels[1].Weight = &weight30

		_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Update(ctx, updatedModelRoute, metav1.UpdateOptions{})
		require.NoError(t, err, "Failed to update ModelRoute")

		// Wait for the update to propagate - verify by sending test requests until they succeed
		require.Eventually(t, func() bool {
			resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
			return resp.StatusCode == 200 && resp.Body != "" &&
				(strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B-v1") || strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B-v2"))
		}, 1*time.Minute, 2*time.Second, "ModelRoute update should propagate and requests should route successfully")

		// Verify requests still work and verify the normalized weight distribution (50:30 = 5/8:3/8)
		// Send multiple requests to verify weight distribution statistics
		const totalRequests = 500
		v1Count := 0
		v2Count := 0

		for i := 0; i < totalRequests; i++ {
			resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
			assert.Equal(t, 200, resp.StatusCode)
			assert.NotEmpty(t, resp.Body)

			if strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B-v1") {
				v1Count++
			} else if strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B-v2") {
				v2Count++
			}
		}

		// Verify weight distribution statistics
		totalCounted := v1Count + v2Count
		assert.Equal(t, totalRequests, totalCounted, "All requests should be accounted for in statistics")

		// Calculate and verify distribution ratios (50:30 should normalize to 5/8:3/8 = 62.5%:37.5%)
		v1Ratio := float64(v1Count) / float64(totalRequests)
		v2Ratio := float64(v2Count) / float64(totalRequests)
		expectedV1Ratio := 0.625 // 5/8 = 62.5%
		expectedV2Ratio := 0.375 // 3/8 = 37.5%
		maxDeviation := 0.05     // Allow ±5% deviation for randomness

		// Verify weight distribution matches expected normalized ratio (5/8:3/8)
		assert.GreaterOrEqual(t, v1Ratio, expectedV1Ratio-maxDeviation,
			"deepseek-r1-1-5b-v1 ratio should be at least %.1f%% (expected %.1f%%)", (expectedV1Ratio-maxDeviation)*100, expectedV1Ratio*100)
		assert.LessOrEqual(t, v1Ratio, expectedV1Ratio+maxDeviation,
			"deepseek-r1-1-5b-v1 ratio should be at most %.1f%% (expected %.1f%%)", (expectedV1Ratio+maxDeviation)*100, expectedV1Ratio*100)
		assert.GreaterOrEqual(t, v2Ratio, expectedV2Ratio-maxDeviation,
			"deepseek-r1-1-5b-v2 ratio should be at least %.1f%% (expected %.1f%%)", (expectedV2Ratio-maxDeviation)*100, expectedV2Ratio*100)
		assert.LessOrEqual(t, v2Ratio, expectedV2Ratio+maxDeviation,
			"deepseek-r1-1-5b-v2 ratio should be at most %.1f%% (expected %.1f%%)", (expectedV2Ratio+maxDeviation)*100, expectedV2Ratio*100)

		// Verify statistics sum to 100%
		assert.Equal(t, 1.0, v1Ratio+v2Ratio, "Distribution ratios should sum to exactly 100%")

		// Log statistics for debugging
		t.Logf("Normalized weight distribution verified (50:30 -> 5/8:3/8):")
		t.Logf("  Total requests: %d, Counted: %d", totalRequests, totalCounted)
		t.Logf("  deepseek-r1-1-5b-v1: %d requests (%.1f%%, expected %.1f%%)", v1Count, v1Ratio*100, expectedV1Ratio*100)
		t.Logf("  deepseek-r1-1-5b-v2: %d requests (%.1f%%, expected %.1f%%)", v2Count, v2Ratio*100, expectedV2Ratio*100)

		// Restore original weights - re-fetch to avoid conflict
		updatedModelRoute, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
		require.NoError(t, err)
		weight70 := uint32(70)
		weight30Restore := uint32(30)
		updatedModelRoute.Spec.Rules[0].TargetModels[0].Weight = &weight70
		updatedModelRoute.Spec.Rules[0].TargetModels[1].Weight = &weight30Restore
		_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Update(ctx, updatedModelRoute, metav1.UpdateOptions{})
		require.NoError(t, err, "Failed to restore ModelRoute weights")
	})
}

// TestModelRouteLoraShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestModelRouteLoraShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	// Deploy ModelRoute with LoRA adapters
	t.Log("Deploying ModelRoute with LoRA adapters...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteLora.yaml")
	modelRoute.Namespace = testNamespace

	// Configure ParentRefs if using Gateway API
	setupModelRouteWithGatewayAPI(modelRoute, useGatewayAPI, kthenaNamespace)

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

	t.Log("Loading LoRA adapters on backend...")
	utils.LoadLoRAAdapter(t, utils.DefaultRouterURL, "lora-A", "/models/lora-A")
	utils.LoadLoRAAdapter(t, utils.DefaultRouterURL, "lora-B", "/models/lora-B")
	t.Log("LoRA adapters loaded successfully")

	t.Log("Waiting for Router to discover LoRA adapters on pods...")
	time.Sleep(5 * time.Second)

	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}

	// Verify LoRA adapter parameter passing and support for multiple LoRA adapters
	t.Run("VerifyLoRAAdapterParameterPassing", func(t *testing.T) {
		t.Log("Testing LoRA adapter parameter passing in requests...")

		// Test with lora-A - verify route matching works
		t.Run("TestWithLoraA", func(t *testing.T) {
			t.Log("Testing request with lora-A adapter...")
			resp := utils.CheckChatCompletions(t, "lora-A", messages)

			// Verify LLM-Mock accepts LoRA adapter names and processes the request successfully
			assert.Equal(t, 200, resp.StatusCode, "Expected HTTP 200 for successful LoRA adapter request")
			assert.NotEmpty(t, resp.Body, "Response body should not be empty")
			assert.NotContains(t, resp.Body, "route not found", "Route should be matched, not 'route not found'")
			// Verify response contains the LoRA adapter name in the model field
			assert.Contains(t, resp.Body, "lora-A", "Response should contain the LoRA adapter name 'lora-A'")
		})

		// Test with lora-B - verify route matching works
		t.Run("TestWithLoraB", func(t *testing.T) {
			t.Log("Testing request with lora-B adapter...")
			resp := utils.CheckChatCompletions(t, "lora-B", messages)

			// Verify LLM-Mock accepts LoRA adapter names and processes the request successfully
			assert.Equal(t, 200, resp.StatusCode, "Expected HTTP 200 for successful LoRA adapter request")
			assert.NotEmpty(t, resp.Body, "Response body should not be empty")
			assert.NotContains(t, resp.Body, "route not found", "Route should be matched, not 'route not found'")
			// Verify response contains the LoRA adapter name in the model field
			assert.Contains(t, resp.Body, "lora-B", "Response should contain the LoRA adapter name 'lora-B'")
		})
	})

	// Verify error handling when LoRA adapter doesn't exist
	t.Run("VerifyErrorHandlingForNonExistentAdapter", func(t *testing.T) {
		t.Log("Testing error handling for non-existent LoRA adapter...")
		messages := []utils.ChatMessage{
			utils.NewChatMessage("user", "Hello"),
		}

		resp := utils.CheckChatCompletions(t, "lora-NonExistent", messages)

		// Non-existent LoRA adapter should return 404
		assert.Equal(t, 404, resp.StatusCode, "Expected HTTP 404 status code for non-existent LoRA adapter")
		t.Logf("Non-existent adapter error handling verified: StatusCode=%d, Response=%s", resp.StatusCode, resp.Body)
	})
}
