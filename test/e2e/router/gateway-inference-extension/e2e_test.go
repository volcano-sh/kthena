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
	"testing"

	"github.com/stretchr/testify/require"
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
	inferencePool := utils.LoadYAMLFromFile[inferencev1.InferencePool]("test/e2e/testdata/kthena-router/InferencePool.yaml")
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
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute]("test/e2e/testdata/kthena-router/HTTPRoute.yaml")
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
