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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
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
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteSimple.yaml")
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

	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteMultiModels.yaml")
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

func TestModelRouteSubset(t *testing.T) {
	ctx := context.Background()

	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteSubset.yaml")
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

	t.Run("WeightedTrafficDistribution", func(t *testing.T) {
		// Send multiple requests and verify weight distribution statistics
		// Use more requests to reduce randomness impact
		const totalRequests = 50
		v1Count := 0
		v2Count := 0

		for i := 0; i < totalRequests; i++ {
			resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
			assert.Equal(t, 200, resp.StatusCode)
			assert.NotEmpty(t, resp.Body)

			if strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B") {
				v1Count++
			} else if strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-7B") {
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
		maxDeviation := 0.15 // Allow ±15% deviation for randomness

		// 3. Verify both ModelServers receive traffic
		assert.Greater(t, v1Count, 0, "deepseek-r1-1-5b should receive some traffic")
		assert.Greater(t, v2Count, 0, "deepseek-r1-7b should receive some traffic")

		// 4. Verify weight distribution statistics match expected ratio (70:30)
		assert.GreaterOrEqual(t, v1Ratio, expectedV1Ratio-maxDeviation,
			"deepseek-r1-1-5b ratio should be at least %.1f%% (expected %.1f%%)", (expectedV1Ratio-maxDeviation)*100, expectedV1Ratio*100)
		assert.LessOrEqual(t, v1Ratio, expectedV1Ratio+maxDeviation,
			"deepseek-r1-1-5b ratio should be at most %.1f%% (expected %.1f%%)", (expectedV1Ratio+maxDeviation)*100, expectedV1Ratio*100)
		assert.GreaterOrEqual(t, v2Ratio, expectedV2Ratio-maxDeviation,
			"deepseek-r1-7b ratio should be at least %.1f%% (expected %.1f%%)", (expectedV2Ratio-maxDeviation)*100, expectedV2Ratio*100)
		assert.LessOrEqual(t, v2Ratio, expectedV2Ratio+maxDeviation,
			"deepseek-r1-7b ratio should be at most %.1f%% (expected %.1f%%)", (expectedV2Ratio+maxDeviation)*100, expectedV2Ratio*100)

		// 5. Verify statistics sum to 100%
		assert.InDelta(t, 1.0, v1Ratio+v2Ratio, 0.01, "Distribution ratios should sum to 100%")

		// Log statistics for debugging
		t.Logf("Weight distribution statistics verified:")
		t.Logf("  Total requests: %d, Counted: %d", totalRequests, totalCounted)
		t.Logf("  deepseek-r1-1-5b: %d requests (%.1f%%, expected %.1f%%)", v1Count, v1Ratio*100, expectedV1Ratio*100)
		t.Logf("  deepseek-r1-7b: %d requests (%.1f%%, expected %.1f%%)", v2Count, v2Ratio*100, expectedV2Ratio*100)
	})

	t.Run("WeightSumNot100Percent", func(t *testing.T) {
		// Update ModelRoute with weights that don't sum to 100%
		updatedModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Get(ctx, createdModelRoute.Name, metav1.GetOptions{})
		require.NoError(t, err)

		// Modify weights to 50:30 (sum = 80%)
		weight50 := uint32(50)
		weight30 := uint32(30)
		updatedModelRoute.Spec.Rules[0].TargetModels[0].Weight = &weight50
		updatedModelRoute.Spec.Rules[0].TargetModels[1].Weight = &weight30

		_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Update(ctx, updatedModelRoute, metav1.UpdateOptions{})
		require.NoError(t, err, "Failed to update ModelRoute")

		// Wait a bit for the update to propagate
		time.Sleep(2 * time.Second)

		// Verify requests still work (should normalize weights internally)
		resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.True(t, strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-1.5B") || strings.Contains(resp.Body, "DeepSeek-R1-Distill-Qwen-7B"),
			"Request should still route to one of the ModelServers when weight sum is not 100%")

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

	t.Run("FailoverWhenSingleModelServerUnavailable", func(t *testing.T) {
		// Scale down 1.5B deployment to simulate unavailability
		deployment, err := testCtx.KubeClient.AppsV1().Deployments(testNamespace).Get(ctx, "deepseek-r1-1-5b", metav1.GetOptions{})
		require.NoError(t, err)

		originalReplicas := *deployment.Spec.Replicas
		zeroReplicas := int32(0)
		deployment.Spec.Replicas = &zeroReplicas
		_, err = testCtx.KubeClient.AppsV1().Deployments(testNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
		require.NoError(t, err, "Failed to scale down 1.5B deployment")

		// Wait for deployment to scale down
		time.Sleep(5 * time.Second)

		// Verify requests still work (should failover to 7B)
		resp := utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NotEmpty(t, resp.Body)
		assert.Contains(t, resp.Body, "DeepSeek-R1-Distill-Qwen-7B", "Request should failover to 7B when 1.5B is unavailable")

		// Restore deployment - re-fetch to avoid conflict
		deployment, err = testCtx.KubeClient.AppsV1().Deployments(testNamespace).Get(ctx, "deepseek-r1-1-5b", metav1.GetOptions{})
		require.NoError(t, err)
		deployment.Spec.Replicas = &originalReplicas
		_, err = testCtx.KubeClient.AppsV1().Deployments(testNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
		require.NoError(t, err, "Failed to restore 1.5B deployment")
	})
}
