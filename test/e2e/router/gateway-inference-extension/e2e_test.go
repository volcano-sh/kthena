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

package gie

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var (
	testCtx         *routercontext.RouterTestContext
	testNamespace   string
	kthenaNamespace string
)

func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-gie-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	kthenaNamespace = config.Namespace
	config.NetworkingEnabled = true
	config.GatewayAPIEnabled = true
	config.InferenceExtensionEnabled = true

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

	if err := testCtx.CreateTestNamespace(); err != nil {
		fmt.Printf("Failed to create test namespace: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	if err := testCtx.SetupCommonComponents(); err != nil {
		fmt.Printf("Failed to setup common components: %v\n", err)
		_ = testCtx.DeleteTestNamespace()
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	code := m.Run()

	if err := testCtx.CleanupCommonComponents(); err != nil {
		fmt.Printf("Failed to cleanup common components: %v\n", err)
	}

	if err := testCtx.DeleteTestNamespace(); err != nil {
		fmt.Printf("Failed to delete test namespace: %v\n", err)
	}

	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
	}

	os.Exit(code)
}

func TestGatewayInferenceExtension(t *testing.T) {
	ctx := context.Background()

	// 1. Deploy InferencePool
	t.Log("Deploying InferencePool...")
	inferencePool := utils.LoadYAMLFromFile[inferencev1.InferencePool]("examples/kthena-router/InferencePool.yaml")
	inferencePool.Namespace = testNamespace

	createdInferencePool, err := testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Create(ctx, inferencePool, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create InferencePool")

	t.Cleanup(func() {
		if err := testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Delete(context.Background(), createdInferencePool.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete InferencePool %s/%s: %v", testNamespace, createdInferencePool.Name, err)
		}
	})

	// 2. Deploy HTTPRoute
	t.Log("Deploying HTTPRoute...")
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute]("examples/kthena-router/HTTPRoute.yaml")
	httpRoute.Namespace = testNamespace

	// Update parentRefs to point to the kthena installation namespace
	ktNamespace := gatewayv1.Namespace(kthenaNamespace)
	if len(httpRoute.Spec.ParentRefs) > 0 {
		for i := range httpRoute.Spec.ParentRefs {
			httpRoute.Spec.ParentRefs[i].Namespace = &ktNamespace
		}
	}

	createdHTTPRoute, err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Create(ctx, httpRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create HTTPRoute")

	t.Cleanup(func() {
		if err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Delete(context.Background(), createdHTTPRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete HTTPRoute %s/%s: %v", testNamespace, createdHTTPRoute.Name, err)
		}
	})

	// 3. Test accessing the route
	t.Log("Testing chat completions via HTTPRoute and InferencePool...")
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello GIE"),
	}

	utils.CheckChatCompletions(t, "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B", messages)
}

// TestBothAPIsConfigured tests both ModelRoute/ModelServer and HTTPRoute/InferencePool APIs configured together.
// It verifies that deepseek-r1-1-5b can be accessed via ModelRoute and deepseek-r1-7b can be accessed via HTTPRoute.
func TestBothAPIsConfigured(t *testing.T) {
	ctx := context.Background()

	// 1. Deploy ModelRoute and ModelServer for ModelRoute/ModelServer API
	t.Log("Deploying ModelRoute...")
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute]("examples/kthena-router/ModelRoute-binding-gateway.yaml")
	modelRoute.Namespace = testNamespace

	// Update parentRefs to point to the kthena installation namespace
	ktNamespace := gatewayv1.Namespace(kthenaNamespace)
	if len(modelRoute.Spec.ParentRefs) > 0 {
		for i := range modelRoute.Spec.ParentRefs {
			modelRoute.Spec.ParentRefs[i].Namespace = &ktNamespace
		}
	}

	createdModelRoute, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute")

	t.Cleanup(func() {
		if err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(context.Background(), createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", testNamespace, createdModelRoute.Name, err)
		}
	})

	// ModelServer-ds1.5b.yaml is already deployed by SetupCommonComponents

	// 2. Deploy InferencePool for HTTPRoute/InferencePool API (pointing to deepseek-r1-7b)
	t.Log("Deploying InferencePool for deepseek-r1-7b...")
	inferencePool7b := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deepseek-r1-7b",
			Namespace: testNamespace,
		},
		Spec: inferencev1.InferencePoolSpec{
			TargetPorts: []inferencev1.Port{
				{Number: 8000},
			},
			Selector: inferencev1.LabelSelector{
				MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
					inferencev1.LabelKey("app"): inferencev1.LabelValue("deepseek-r1-7b"),
				},
			},
			EndpointPickerRef: inferencev1.EndpointPickerRef{
				Name: "deepseek-r1-7b",
				Port: &inferencev1.Port{
					Number: 8080,
				},
			},
		},
	}

	createdInferencePool7b, err := testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Create(ctx, inferencePool7b, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create InferencePool for 7b")

	t.Cleanup(func() {
		if err := testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Delete(context.Background(), createdInferencePool7b.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete InferencePool %s/%s: %v", testNamespace, createdInferencePool7b.Name, err)
		}
	})

	// 3. Deploy HTTPRoute pointing to the 7b InferencePool
	t.Log("Deploying HTTPRoute...")
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute]("examples/kthena-router/HTTPRoute.yaml")
	httpRoute.Namespace = testNamespace
	httpRoute.Name = "llm-route-7b"

	// Update parentRefs to point to the kthena installation namespace (reuse ktNamespace from above)
	ktNamespace = gatewayv1.Namespace(kthenaNamespace)
	if len(httpRoute.Spec.ParentRefs) > 0 {
		for i := range httpRoute.Spec.ParentRefs {
			httpRoute.Spec.ParentRefs[i].Namespace = &ktNamespace
		}
	}

	// Update backendRefs to point to the 7b InferencePool
	if len(httpRoute.Spec.Rules) > 0 && len(httpRoute.Spec.Rules[0].BackendRefs) > 0 {
		backendRefName := gatewayv1.ObjectName("deepseek-r1-7b")
		httpRoute.Spec.Rules[0].BackendRefs[0].Name = backendRefName
	}

	createdHTTPRoute, err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Create(ctx, httpRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create HTTPRoute")

	t.Cleanup(func() {
		if err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Delete(context.Background(), createdHTTPRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete HTTPRoute %s/%s: %v", testNamespace, createdHTTPRoute.Name, err)
		}
	})

	// 4. Test accessing both models
	// Test ModelRoute/ModelServer API - deepseek-r1-1-5b via ModelRoute
	t.Log("Testing ModelRoute/ModelServer API - accessing deepseek-r1-1-5b via ModelRoute...")
	messages1_5b := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello ModelRoute"),
	}
	utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages1_5b)

	// Test HTTPRoute/InferencePool API - deepseek-r1-7b via HTTPRoute
	t.Log("Testing HTTPRoute/InferencePool API - accessing deepseek-r1-7b via HTTPRoute...")
	messages7b := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello HTTPRoute"),
	}
	utils.CheckChatCompletions(t, "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", messages7b)

	// 5. Verify access logs contain correct AI-specific and Gateway API fields with mutual exclusivity
	t.Log("Verifying router access logs for AI-specific and Gateway API fields...")
	routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)

	// Read a reasonable tail of logs to find both requests
	var modelRouteLogLine string
	var httpRouteLogLine string

	const tailLines int64 = 2000
	logs := utils.FetchPodLogs(t, testCtx.KubeClient, kthenaNamespace, routerPod.Name, tailLines)
	require.NotEmpty(t, logs, "Router logs should not be empty")
	t.Logf("==== Router logs (tail %d lines) ====\n%s\n==== End router logs ====", tailLines, logs)

	for _, line := range strings.Split(logs, "\n") {
		// Match both text format (model_route=...) and JSON format ("model_route": ...)
		if modelRouteLogLine == "" && strings.Contains(line, "model_route") && strings.Contains(line, "model_server") {
			modelRouteLogLine = line
		}
		if httpRouteLogLine == "" && strings.Contains(line, "http_route") && strings.Contains(line, "inference_pool") {
			httpRouteLogLine = line
		}
	}

	t.Logf("ModelRoute log line: %s", modelRouteLogLine)
	t.Logf("HTTPRoute log line: %s", httpRouteLogLine)
	require.NotEmpty(t, modelRouteLogLine, "Expected to find an access log line for ModelRoute/ModelServer request")
	require.NotEmpty(t, httpRouteLogLine, "Expected to find an access log line for HTTPRoute/InferencePool request")

	// Verify AI-specific fields are present (support both text and JSON formats)
	assert.Contains(t, modelRouteLogLine, "model_name", "ModelRoute access log should contain model_name")
	assert.Contains(t, httpRouteLogLine, "model_name", "HTTPRoute access log should contain model_name")

	// Verify ModelRoute/ModelServer vs HTTPRoute/InferencePool mutual exclusivity by field presence
	assert.Contains(t, modelRouteLogLine, "model_route", "ModelRoute log should contain model_route")
	assert.Contains(t, modelRouteLogLine, "model_server", "ModelRoute log should contain model_server")
	assert.NotContains(t, modelRouteLogLine, "http_route", "ModelRoute log should not contain http_route")
	assert.NotContains(t, modelRouteLogLine, "inference_pool", "ModelRoute log should not contain inference_pool")

	assert.Contains(t, httpRouteLogLine, "http_route", "HTTPRoute log should contain http_route")
	assert.Contains(t, httpRouteLogLine, "inference_pool", "HTTPRoute log should contain inference_pool")
	assert.NotContains(t, httpRouteLogLine, "model_route", "HTTPRoute log should not contain model_route")
	assert.NotContains(t, httpRouteLogLine, "model_server", "HTTPRoute log should not contain model_server")
}
