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
	"io"
	"net/http"
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
		const sumTolerance = 0.01 // Allow ±1% deviation for floating-point rounding errors
		v1Count := 0
		v2Count := 0

		for i := 0; i < totalRequests; i++ {
			resp := utils.CheckChatCompletionsQuiet(t, modelRoute.Spec.ModelName, messages)
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

		// 4. Verify statistics sum to 100% (with tolerance for floating-point rounding)
		assert.InDelta(t, 1.0, v1Ratio+v2Ratio, sumTolerance, "Distribution ratios should sum to approximately 100%")

		// Log statistics for debugging
		t.Logf("Weight distribution statistics verified:")
		t.Logf("  Total requests: %d, Counted: %d", totalRequests, totalCounted)
		t.Logf("  deepseek-r1-1-5b-v1: %d requests (%.1f%%, expected %.1f%%)", v1Count, v1Ratio*100, expectedV1Ratio*100)
		t.Logf("  deepseek-r1-1-5b-v2: %d requests (%.1f%%, expected %.1f%%)", v2Count, v2Ratio*100, expectedV2Ratio*100)
	})

	t.Run("WeightSumNot100Percent", func(t *testing.T) {
		// Update ModelRoute with weights that don't sum to 100%
		// Weights are relative, so 50:30 means 5/8 and 3/8 traffic distribution
		const sumTolerance = 0.01 // Allow ±1% deviation for floating-point rounding errors
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
			resp := utils.CheckChatCompletionsQuiet(t, modelRoute.Spec.ModelName, messages)
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

		// Verify statistics sum to 100% (with tolerance for floating-point rounding)
		assert.InDelta(t, 1.0, v1Ratio+v2Ratio, sumTolerance, "Distribution ratios should sum to approximately 100%")

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

// TestModelRouteWithRateLimitShared tests ModelRoute rate limiting (input/output tokens, reset, window).
func TestModelRouteWithRateLimitShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayApi bool, kthenaNamespace string) {
	const (
		rateLimitWindowSeconds = 60
		windowResetBuffer      = 10 * time.Second
		inputTokenLimit        = 30
		outputTokenLimit       = 100
		tokensPerRequest       = 10
	)
	ctx := context.Background()

	standardMessage := []utils.ChatMessage{
		utils.NewChatMessage("user", "hello world"),
	}

	// Test 1: Verify input token rate limit enforcement
	t.Run("VerifyInputTokenRateLimitEnforcement", func(t *testing.T) {
		t.Log("Test 1: Verifying input token rate limit")

		modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteWithRateLimit.yaml")
		modelRoute.Namespace = testNamespace
		// Only test input rate limit; remove output limit to avoid 429 "output token rate limit exceeded"
		if modelRoute.Spec.RateLimit != nil {
			modelRoute.Spec.RateLimit.OutputTokensPerUnit = nil
		}
		setupModelRouteWithGatewayAPI(modelRoute, useGatewayApi, kthenaNamespace)

		createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
		require.NoError(t, err, "Failed to create ModelRoute")

		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
				t.Logf("Warning: Failed to delete ModelRoute: %v", err)
			}
		})

		require.Eventually(t, func() bool {
			mr, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
			return err == nil && mr != nil
		}, 2*time.Minute, 2*time.Second, "ModelRoute should be created")

		// First request: use CheckChatCompletions to handle router reconciliation
		resp := utils.CheckChatCompletions(t, createdModelRoute.Spec.ModelName, standardMessage)
		tokensConsumed := resp.Attempts * tokensPerRequest
		t.Logf("Router reconciliation complete (consumed %d tokens in %d attempts)", tokensConsumed, resp.Attempts)

		// Calculate remaining quota
		remainingQuota := inputTokenLimit - tokensConsumed
		expectedSuccessfulRequests := remainingQuota / tokensPerRequest

		// Send remaining requests until quota exhausted
		for i := 0; i < expectedSuccessfulRequests; i++ {
			resp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
			responseBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()

			require.NoError(t, readErr, "Failed to read response body on request %d", i+1)
			require.Equal(t, http.StatusOK, resp.StatusCode,
				"Request %d should succeed. Response: %s", i+1, string(responseBody))
			t.Logf("Request %d succeeded", i+1)
		}

		// Next request should be rate limited
		rateLimitedResp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
		responseBody, readErr := io.ReadAll(rateLimitedResp.Body)
		rateLimitedResp.Body.Close()

		require.NoError(t, readErr, "Failed to read rate limit response body")
		assert.Equal(t, http.StatusTooManyRequests, rateLimitedResp.StatusCode,
			"Request should be rate limited after exhausting quota")
		assert.Contains(t, strings.ToLower(string(responseBody)), "rate limit",
			"Rate limit error response must contain descriptive message")

		t.Logf("Input token rate limit enforced after %d total requests", resp.Attempts+expectedSuccessfulRequests)
	})

	// Test 2 Verify rate limit window accuracy and persistence
	t.Run("VerifyRateLimitWindowAccuracy", func(t *testing.T) {
		t.Log("Test 2: Verifying rate limit window accuracy...")

		modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteWithRateLimit.yaml")
		modelRoute.Namespace = testNamespace
		// Only test input rate limit; remove output limit to avoid 429 "output token rate limit exceeded"
		if modelRoute.Spec.RateLimit != nil {
			modelRoute.Spec.RateLimit.OutputTokensPerUnit = nil
		}
		setupModelRouteWithGatewayAPI(modelRoute, useGatewayApi, kthenaNamespace)

		createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
		require.NoError(t, err, "Failed to create ModelRoute")

		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
				t.Logf("Warning: Failed to delete ModelRoute: %v", err)
			}
		})

		require.Eventually(t, func() bool {
			mr, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
			return err == nil && mr != nil
		}, 2*time.Minute, 2*time.Second, "ModelRoute should be created")

		// First request: handle reconciliation and track tokens
		resp := utils.CheckChatCompletions(t, createdModelRoute.Spec.ModelName, standardMessage)
		tokensConsumed := resp.Attempts * tokensPerRequest

		// Exhaust remaining quota
		remainingQuota := inputTokenLimit - tokensConsumed
		expectedSuccessfulRequests := remainingQuota / tokensPerRequest
		for i := 0; i < expectedSuccessfulRequests; i++ {
			resp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode, "Request %d should succeed", i+1)
		}

		// Verify rate limit is active
		rateLimitedResp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
		rateLimitedResp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, rateLimitedResp.StatusCode,
			"Rate limit should be active after exhausting quota")

		const halfWindowDuration = 10 * time.Second
		t.Logf("Waiting %v (within rate limit window)...", halfWindowDuration)
		time.Sleep(halfWindowDuration)

		midWindowResp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
		midWindowResp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, midWindowResp.StatusCode,
			"Rate limit should persist within the time window")

		// Verify rate limit resets after window expiration
		remainingWindowDuration := (rateLimitWindowSeconds * time.Second) - halfWindowDuration + windowResetBuffer
		t.Logf("Waiting additional %v for window reset (total: %v)...",
			remainingWindowDuration, halfWindowDuration+remainingWindowDuration)
		time.Sleep(remainingWindowDuration)

		postWindowResp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
		postWindowResp.Body.Close()
		assert.Equal(t, http.StatusOK, postWindowResp.StatusCode,
			"Request should succeed after rate limit window expires")

		t.Log(" Rate limit window accuracy verified")
	})

	// Test 3: Verify rate limit reset mechanism
	t.Run("VerifyRateLimitResetMechanism", func(t *testing.T) {
		t.Log("Test 3: Verifying rate limit reset mechanism...")

		modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteWithRateLimit.yaml")
		modelRoute.Namespace = testNamespace
		// Only test input rate limit; remove output limit to avoid 429 "output token rate limit exceeded"
		if modelRoute.Spec.RateLimit != nil {
			modelRoute.Spec.RateLimit.OutputTokensPerUnit = nil
		}
		setupModelRouteWithGatewayAPI(modelRoute, useGatewayApi, kthenaNamespace)

		createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
		require.NoError(t, err, "Failed to create ModelRoute")

		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
				t.Logf("Warning: Failed to delete ModelRoute: %v", err)
			}
		})

		require.Eventually(t, func() bool {
			mr, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
			return err == nil && mr != nil
		}, 2*time.Minute, 2*time.Second, "ModelRoute should be created")

		// First request: handle reconciliation and track tokens
		resp := utils.CheckChatCompletions(t, createdModelRoute.Spec.ModelName, standardMessage)
		tokensConsumed := resp.Attempts * tokensPerRequest

		// Consume remaining quota
		remainingQuota := inputTokenLimit - tokensConsumed
		expectedSuccessfulRequests := remainingQuota / tokensPerRequest
		for i := 0; i < expectedSuccessfulRequests; i++ {
			resp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode,
				"Request %d should succeed", i+1)
		}

		// Confirm rate limiting is active
		preResetResp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
		preResetResp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, preResetResp.StatusCode,
			"Rate limit should be active before window reset")

		// Wait for complete window reset
		windowResetDuration := (rateLimitWindowSeconds * time.Second) + windowResetBuffer
		t.Logf("Waiting %v for complete rate limit window reset...", windowResetDuration)
		time.Sleep(windowResetDuration)

		// After window reset, full quota is restored (30 tokens = 3 requests)
		fullQuotaRequests := inputTokenLimit / tokensPerRequest
		for i := 0; i < fullQuotaRequests; i++ {
			resp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode,
				"Request %d should succeed after reset", i+1)
		}

		// Verify rate limiting kicks in again after consuming quota
		postResetRateLimitedResp := utils.SendChatRequest(t, createdModelRoute.Spec.ModelName, standardMessage)
		postResetRateLimitedResp.Body.Close()
		assert.Equal(t, http.StatusTooManyRequests, postResetRateLimitedResp.StatusCode,
			"Rate limit should be active again after consuming quota")

		t.Logf("Rate limit reset mechanism verified (quota restored: %d requests)", fullQuotaRequests)
	})

	// Test 4: Verify output token rate limit enforcement
	t.Run("VerifyOutputTokenRateLimitEnforcement", func(t *testing.T) {
		t.Log("Test 4: Verifying output token rate limit (100 tokens/minute)...")

		modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteWithRateLimit.yaml")
		modelRoute.Namespace = testNamespace
		setupModelRouteWithGatewayAPI(modelRoute, useGatewayApi, kthenaNamespace)

		createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
		require.NoError(t, err, "Failed to create ModelRoute")

		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
				t.Logf("Warning: Failed to delete ModelRoute: %v", err)
			}
		})

		require.Eventually(t, func() bool {
			mr, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
			return err == nil && mr != nil
		}, 2*time.Minute, 2*time.Second, "ModelRoute should be created")

		// Update ModelRoute to disable input token limit
		createdModelRoute.Spec.RateLimit.InputTokensPerUnit = nil
		outputLimit := uint32(outputTokenLimit)
		createdModelRoute.Spec.RateLimit.OutputTokensPerUnit = &outputLimit

		updatedModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Update(ctx, createdModelRoute, metav1.UpdateOptions{})
		require.NoError(t, err, "Failed to update ModelRoute")

		// Wait for update to propagate
		time.Sleep(2 * time.Second)

		longerPrompt := []utils.ChatMessage{
			utils.NewChatMessage("user", "Write a detailed explanation of rate limiting"),
		}

		// Send requests until we hit the output token limit
		var successfulRequests int
		var totalResponseSize int
		var rateLimited bool

		for attempt := 0; attempt < 20; attempt++ {
			resp := utils.SendChatRequest(t, updatedModelRoute.Spec.ModelName, longerPrompt)
			responseBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()

			require.NoError(t, readErr, "Failed to read response body")

			if resp.StatusCode == http.StatusOK {
				successfulRequests++
				totalResponseSize += len(responseBody)
				t.Logf("Request %d succeeded, response size: %d bytes (total: %d bytes)",
					attempt+1, len(responseBody), totalResponseSize)
			} else if resp.StatusCode == http.StatusTooManyRequests {
				t.Logf("Output rate limited after %d requests", successfulRequests)
				assert.Contains(t, strings.ToLower(string(responseBody)), "rate limit",
					"Output rate limit error should mention rate limit")
				rateLimited = true
				break
			} else {
				t.Fatalf("Unexpected HTTP status code %d on attempt %d", resp.StatusCode, attempt+1)
			}
		}

		// Verify output rate limiting was enforced
		assert.True(t, rateLimited, "Expected output rate limiting to be enforced")
		assert.Greater(t, successfulRequests, 0,
			"Expected at least one successful request before output rate limiting")

		t.Logf(" Output token rate limit enforced after %d requests", successfulRequests)
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

	// Set up port-forward to LLM-Mock pod to load LoRA adapters directly
	// Note: /v1/load_lora_adapter is a management endpoint that should be called directly on the pod, not through the router
	t.Log("Setting up port-forward to LLM-Mock pod for LoRA adapter loading...")
	podList, err := testCtx.KubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=deepseek-r1-7b",
	})
	require.NoError(t, err, "Failed to list LLM-Mock pods")
	require.Greater(t, len(podList.Items), 0, "At least one LLM-Mock pod should be available")

	podName := podList.Items[0].Name
	t.Logf("Using pod %s for LoRA adapter loading", podName)

	pf, err := utils.SetupPortForwardToPod(testNamespace, podName, "9000", "8000")
	require.NoError(t, err, "Failed to setup port-forward to LLM-Mock pod")
	defer pf.Close()

	t.Log("Loading LoRA adapters on backend...")
	utils.LoadLoRAAdapter(t, "http://127.0.0.1:9000", "lora-A", "/models/lora-A")
	utils.LoadLoRAAdapter(t, "http://127.0.0.1:9000", "lora-B", "/models/lora-B")
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

		resp := utils.SendChatRequestWithRetry(t, utils.DefaultRouterURL, "lora-NonExistent", messages, nil)

		// Non-existent LoRA adapter should return 404
		assert.Equal(t, 404, resp.StatusCode, "Expected HTTP 404 status code for non-existent LoRA adapter")
		t.Logf("Non-existent adapter error handling verified: StatusCode=%d, Response=%s", resp.StatusCode, resp.Body)
	})

	// Unload LoRA adapters after test is complete
	t.Log("Unloading LoRA adapters after test...")
	utils.UnloadLoRAAdapter(t, "http://127.0.0.1:9000", "lora-A")
	utils.UnloadLoRAAdapter(t, "http://127.0.0.1:9000", "lora-B")
	t.Log("LoRA adapters unloaded successfully")
}

// TestMetricsShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestMetricsShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	// Deploy ModelRoute
	t.Log("Deploying ModelRoute...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteSimple.yaml")
	modelRoute.Namespace = testNamespace

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

	fetchMetrics := func() string {
		resp, err := http.Get("http://127.0.0.1:8080/metrics")
		require.NoError(t, err, "Failed to fetch metrics")
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Failed to read metrics response")
		return string(body)
	}

	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}

	t.Run("VerifyRequestCountMetrics", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
			assert.Equal(t, 200, resp.StatusCode)
		}

		require.Eventually(t, func() bool {
			metricsBody := fetchMetrics()
			return strings.Contains(metricsBody, "kthena_router_requests_total") &&
				strings.Contains(metricsBody, fmt.Sprintf(`model="%s"`, modelRoute.Spec.ModelName)) &&
				strings.Contains(metricsBody, `status_code="200"`)
		}, 15*time.Second, time.Second, "Expected metrics for successful requests were not found in time")
	})

	t.Run("VerifyLatencyMetrics", func(t *testing.T) {
		resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
		assert.Equal(t, 200, resp.StatusCode)

		require.Eventually(t, func() bool {
			metricsBody := fetchMetrics()
			return strings.Contains(metricsBody, "kthena_router_request_duration_seconds")
		}, 15*time.Second, time.Second, "Duration histogram metric was not found in time")
	})

	t.Run("VerifyErrorMetrics", func(t *testing.T) {
		resp := utils.SendChatRequest(t, "non-existent-model-xyz", messages)
		defer resp.Body.Close()
		assert.Equal(t, 404, resp.StatusCode)

		require.Eventually(t, func() bool {
			metricsBody := fetchMetrics()
			return strings.Contains(metricsBody, `model="non-existent-model-xyz"`)
		}, 15*time.Second, time.Second, "Error model metric was not found in time")
	})
}

// TestRateLimitMetricsShared is a shared test function that can be used by both
// router and gateway-api test suites. When useGatewayAPI is true, it configures ModelRoute
// with ParentRefs to the default Gateway.
func TestRateLimitMetricsShared(t *testing.T, testCtx *routercontext.RouterTestContext, testNamespace string, useGatewayAPI bool, kthenaNamespace string) {
	ctx := context.Background()

	// Deploy ModelRoute with rate limiting
	t.Log("Deploying ModelRoute with rate limiting...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteWithRateLimit.yaml")
	modelRoute.Namespace = testNamespace

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

	fetchMetrics := func() string {
		resp, err := http.Get("http://127.0.0.1:8080/metrics")
		require.NoError(t, err, "Failed to fetch metrics")
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "Failed to read metrics response")
		return string(body)
	}

	t.Run("VerifyRateLimitExceededMetrics", func(t *testing.T) {
		messages := []utils.ChatMessage{
			utils.NewChatMessage("user", "hello world"),
		}

		// First request with retry to ensure route is ready
		utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)

		// Subsequent requests without retry to capture 429 responses
		var successCount, rateLimitedCount int
		for i := 0; i < 10; i++ {
			resp := utils.SendChatRequest(t, modelRoute.Spec.ModelName, messages)
			switch resp.StatusCode {
			case 200:
				successCount++
			case 429:
				rateLimitedCount++
			}
			resp.Body.Close()
			time.Sleep(100 * time.Millisecond)
		}

		t.Logf("Requests: %d successful, %d rate-limited", successCount, rateLimitedCount)

		require.Eventually(t, func() bool {
			metricsBody := fetchMetrics()
			return strings.Contains(metricsBody, "kthena_router_rate_limit_exceeded_total") &&
				strings.Contains(metricsBody, fmt.Sprintf(`model="%s"`, modelRoute.Spec.ModelName))
		}, 15*time.Second, time.Second, "Rate limit exceeded metrics were not found in time")
	})
}
