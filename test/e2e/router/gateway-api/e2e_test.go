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

package gateway_api

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	"github.com/volcano-sh/kthena/test/e2e/router"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var (
	testCtx         *routercontext.RouterTestContext
	testNamespace   string
	kthenaNamespace string
)

// TestMain runs setup and cleanup for all tests in this package.
func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-gateway-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	kthenaNamespace = config.Namespace
	// Gateway API tests need networking and gateway API enabled
	config.NetworkingEnabled = true
	config.GatewayAPIEnabled = true

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

	// Setup common components (LLM Mocks and ModelServers)
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
// This test runs the shared test function with Gateway API enabled (with ParentRefs).
func TestModelRouteSimple(t *testing.T) {
	router.TestModelRouteSimpleShared(t, testCtx, testNamespace, true, kthenaNamespace)
}

// TestModelRouteMultiModels tests ModelRoute with multiple models.
// This test runs the shared test function with Gateway API enabled (with ParentRefs).
func TestModelRouteMultiModels(t *testing.T) {
	router.TestModelRouteMultiModelsShared(t, testCtx, testNamespace, true, kthenaNamespace)
}

// TestModelRoutePrefillDecodeDisaggregation tests PD disaggregation with ModelServing, ModelServer, and ModelRoute.
// This test runs the shared test function with Gateway API enabled (with ParentRefs).
func TestModelRoutePrefillDecodeDisaggregation(t *testing.T) {
	router.TestModelRoutePrefillDecodeDisaggregationShared(t, testCtx, testNamespace, true, kthenaNamespace)
}

// TestModelRouteSubset tests ModelRoute with subset routing.
// This test runs the shared test function with Gateway API enabled (with ParentRefs).
func TestModelRouteSubset(t *testing.T) {
	router.TestModelRouteSubsetShared(t, testCtx, testNamespace, true, kthenaNamespace)
}

// TestModelRouteLora tests ModelRoute with LoRA adapter routing.
// This test runs the shared test function with Gateway API enabled (with ParentRefs).
func TestModelRouteLora(t *testing.T) {
	router.TestModelRouteLoraShared(t, testCtx, testNamespace, true, kthenaNamespace)
}

// TestDuplicateModelName tests that the same modelName can route to different backend models
// when accessed through different Gateways (ports). This demonstrates how Gateway API resolves
// the global modelName conflict problem by allowing modelName isolation per Gateway.
func TestDuplicateModelName(t *testing.T) {
	ctx := context.Background()

	// 1. Deploy ModelRouteSimple.yaml with parentRefs to default Gateway
	t.Log("Deploying ModelRouteSimple binding to default Gateway...")
	modelRoute1 := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRouteSimple.yaml")
	modelRoute1.Namespace = testNamespace
	modelRoute1.Name = "deepseek-simple-default"

	// Add parentRefs to default Gateway
	ktNamespace := gatewayv1.Namespace(kthenaNamespace)
	modelRoute1.Spec.ParentRefs = []gatewayv1.ParentReference{
		{
			Name:      "default",
			Namespace: &ktNamespace,
			Kind:      func() *gatewayv1.Kind { k := gatewayv1.Kind("Gateway"); return &k }(),
		},
	}

	createdModelRoute1, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute1, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute for default Gateway")
	assert.NotNil(t, createdModelRoute1)
	t.Logf("Created ModelRoute: %s/%s", createdModelRoute1.Namespace, createdModelRoute1.Name)

	// Register cleanup for ModelRoute1
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute1.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", createdModelRoute1.Namespace, createdModelRoute1.Name, err)
		}
	})

	// 2. Create custom Gateway with port 8081
	t.Log("Creating custom Gateway with port 8081...")
	customGateway := utils.LoadYAMLFromFile[gatewayv1.Gateway]("examples/kthena-router/Gateway.yaml")
	customGateway.Namespace = kthenaNamespace
	customGateway.Name = "kthena-gateway-custom"
	customGateway.Spec.Listeners[0].Port = gatewayv1.PortNumber(8081)

	createdGateway, err := testCtx.GatewayClient.GatewayV1().Gateways(kthenaNamespace).Create(ctx, customGateway, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create custom Gateway")
	assert.NotNil(t, createdGateway)
	t.Logf("Created Gateway: %s/%s", createdGateway.Namespace, createdGateway.Name)

	// Register cleanup for Gateway
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := testCtx.GatewayClient.GatewayV1().Gateways(kthenaNamespace).Delete(cleanupCtx, createdGateway.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete Gateway %s/%s: %v", createdGateway.Namespace, createdGateway.Name, err)
		}
	})

	// 3. Update kthena-router Service to add port 8081
	t.Log("Updating kthena-router Service to add port 8081...")
	svc, err := testCtx.KubeClient.CoreV1().Services(kthenaNamespace).Get(ctx, "kthena-router", metav1.GetOptions{})
	require.NoError(t, err, "Failed to get kthena-router Service")

	// Add port 8081 to the Service
	port8081 := corev1.ServicePort{
		Name:       "http-8081",
		Port:       8081,
		TargetPort: intstr.FromInt(8081),
		Protocol:   corev1.ProtocolTCP,
	}
	svc.Spec.Ports = append(svc.Spec.Ports, port8081)

	_, err = testCtx.KubeClient.CoreV1().Services(kthenaNamespace).Update(ctx, svc, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update kthena-router Service")
	t.Log("Updated kthena-router Service with port 8081")

	// Register cleanup to restore Service
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		svc, err := testCtx.KubeClient.CoreV1().Services(kthenaNamespace).Get(cleanupCtx, "kthena-router", metav1.GetOptions{})
		if err != nil {
			t.Logf("Warning: Failed to get Service for cleanup: %v", err)
			return
		}
		// Remove port 8081
		var ports []corev1.ServicePort
		for _, p := range svc.Spec.Ports {
			if p.Name != "http-8081" {
				ports = append(ports, p)
			}
		}
		svc.Spec.Ports = ports
		if _, err := testCtx.KubeClient.CoreV1().Services(kthenaNamespace).Update(cleanupCtx, svc, metav1.UpdateOptions{}); err != nil {
			t.Logf("Warning: Failed to restore kthena-router Service during cleanup: %v", err)
		}
	})

	// 4. Setup port-forward for port 8081
	t.Log("Setting up port-forward for port 8081...")
	pf, err := utils.SetupPortForward(kthenaNamespace, "kthena-router", "8081", "8081")
	require.NoError(t, err, "Failed to setup port-forward for 8081")

	// Register cleanup to kill port-forward
	t.Cleanup(func() {
		if pf != nil {
			pf.Close()
		}
	})

	// 5. Create second ModelRoute with same modelName but bound to custom Gateway
	t.Log("Creating second ModelRoute with same modelName bound to custom Gateway...")
	modelRoute2 := modelRoute1.DeepCopy()
	modelRoute2.Name = "deepseek-simple-custom"
	modelRoute2.Spec.Rules[0].TargetModels[0].ModelServerName = "deepseek-r1-7b"
	// Update parentRefs to point to custom Gateway
	ktNsForGateway := gatewayv1.Namespace(kthenaNamespace)
	modelRoute2.Spec.ParentRefs = []gatewayv1.ParentReference{
		{
			Name:      gatewayv1.ObjectName(createdGateway.Name),
			Namespace: &ktNsForGateway,
			Kind:      func() *gatewayv1.Kind { k := gatewayv1.Kind("Gateway"); return &k }(),
		},
	}

	createdModelRoute2, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute2, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute for custom Gateway")
	assert.NotNil(t, createdModelRoute2)
	t.Logf("Created ModelRoute: %s/%s", createdModelRoute2.Namespace, createdModelRoute2.Name)

	// Register cleanup for ModelRoute2
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute2.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", createdModelRoute2.Namespace, createdModelRoute2.Name, err)
		}
	})

	// 6. Verify access through different ports routes to different models
	t.Log("Verifying access through default Gateway (port 8080) routes to deepseek-r1-1-5b...")
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello from default Gateway"),
	}
	response1 := utils.CheckChatCompletionsWithURL(t, "http://127.0.0.1:8080/v1/chat/completions", modelRoute1.Spec.ModelName, messages)
	require.Equal(t, 200, response1.StatusCode, "Expected HTTP 200 from default Gateway")
	// Verify response contains indication of 1.5B model (check response body)
	assert.Contains(t, response1.Body, "DeepSeek-R1-Distill-Qwen-1.5B", "Response should indicate 1.5B model")

	t.Log("Verifying access through custom Gateway (port 8081) routes to deepseek-r1-7b...")
	response2 := utils.CheckChatCompletionsWithURL(t, "http://127.0.0.1:8081/v1/chat/completions", modelRoute2.Spec.ModelName, messages)
	require.Equal(t, 200, response2.StatusCode, "Expected HTTP 200 from custom Gateway")
	// Verify response contains indication of 7B model (check response body)
	assert.Contains(t, response2.Body, "DeepSeek-R1-Distill-Qwen-7B", "Response should indicate 7B model")

	t.Log("Test completed successfully: same modelName routes to different models via different ports")
}
