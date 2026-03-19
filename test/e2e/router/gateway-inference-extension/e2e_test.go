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
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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

const testDataDir = "test/e2e/router/testdata"

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
	inferencePool := utils.LoadYAMLFromFile[inferencev1.InferencePool](filepath.Join(testDataDir, "InferencePool.yaml"))
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
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute](filepath.Join(testDataDir, "HTTPRoute.yaml"))
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
	modelRoute := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute](filepath.Join(testDataDir, "ModelRoute-binding-gateway.yaml"))
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
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute](filepath.Join(testDataDir, "HTTPRoute.yaml"))
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
}

// TestHTTPRouteNotSkippedAfterRouterRestart verifies that HTTPRoute volcano-router-gateway
// is not skipped when HTTPRouteController sync runs (e.g. after router pod restart).
func TestHTTPRouteNotSkippedAfterRouterRestart(t *testing.T) {
	ctx := context.Background()
	httpRouteName := "volcano-router-gateway"
	ktNamespace := gatewayv1.Namespace(kthenaNamespace)

	// 1. Deploy InferencePool
	t.Log("Deploying InferencePool...")
	inferencePool := utils.LoadYAMLFromFile[inferencev1.InferencePool](filepath.Join(testDataDir, "InferencePool.yaml"))
	inferencePool.Namespace = testNamespace

	createdInferencePool, err := testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Create(ctx, inferencePool, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create InferencePool")

	t.Cleanup(func() {
		_ = testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Delete(context.Background(), createdInferencePool.Name, metav1.DeleteOptions{})
	})

	// 2. Create HTTPRoute volcano-router-gateway (parentRef to default Gateway, which listens on port 80)
	t.Log("Creating HTTPRoute volcano-router-gateway...")
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute](filepath.Join(testDataDir, "HTTPRoute.yaml"))
	httpRoute.Name, httpRoute.Namespace = httpRouteName, testNamespace
	httpRoute.Spec.ParentRefs = []gatewayv1.ParentReference{
		{Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")), Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName("default"), Namespace: &ktNamespace},
	}

	_, err = testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Create(ctx, httpRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create HTTPRoute")

	t.Cleanup(func() {
		_ = testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Delete(context.Background(), httpRouteName, metav1.DeleteOptions{})
	})

	// 3. Restart router to trigger sync race
	t.Log("Restarting kthena-router...")
	require.NoError(t, exec.Command("kubectl", "rollout", "restart", "deployment/kthena-router", "-n", kthenaNamespace).Run())
	require.NoError(t, exec.Command("kubectl", "rollout", "status", "deployment/kthena-router", "-n", kthenaNamespace, "--timeout=120s").Run())

	// Wait for old pod to fully terminate before port-forward
	time.Sleep(5 * time.Second)

	// 4. Re-establish port-forward (original breaks when pod restarts)
	pf, err := utils.SetupPortForward(kthenaNamespace, "kthena-router", "9080", "80")
	require.NoError(t, err, "Failed to setup port-forward after restart")
	t.Cleanup(func() { pf.Close() })

	// 5. Verify HTTPRoute was not skipped
	t.Log("Verifying HTTPRoute volcano-router-gateway is processed...")
	utils.CheckChatCompletionsWithURL(t, "http://127.0.0.1:9080/v1/chat/completions", "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B", []utils.ChatMessage{utils.NewChatMessage("user", "Hello")})
}

// TestGatewayCreatedLaterThanHTTPRoute verifies that when HTTPRoute is created before its Gateway,
// the HTTPRoute is correctly processed after the Gateway is created (via Gateway event handler).
func TestGatewayCreatedLaterThanHTTPRoute(t *testing.T) {
	ctx := context.Background()
	gatewayName := "late-gateway"
	httpRouteName := "late-route"
	ktNamespace := gatewayv1.Namespace(kthenaNamespace)

	// 1. Create HTTPRoute first (parentRef to Gateway that does not exist yet)
	t.Log("Creating HTTPRoute before Gateway...")
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute](filepath.Join(testDataDir, "HTTPRoute.yaml"))
	httpRoute.Name, httpRoute.Namespace = httpRouteName, testNamespace
	httpRoute.Spec.ParentRefs = []gatewayv1.ParentReference{
		{Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")), Kind: ptr(gatewayv1.Kind("Gateway")), Name: gatewayv1.ObjectName(gatewayName), Namespace: &ktNamespace},
	}

	_, err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Create(ctx, httpRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create HTTPRoute")

	t.Cleanup(func() {
		_ = testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Delete(context.Background(), httpRouteName, metav1.DeleteOptions{})
	})

	// 2. Deploy InferencePool
	t.Log("Deploying InferencePool...")
	inferencePool := utils.LoadYAMLFromFile[inferencev1.InferencePool](filepath.Join(testDataDir, "InferencePool.yaml"))
	inferencePool.Namespace = testNamespace

	createdInferencePool, err := testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Create(ctx, inferencePool, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create InferencePool")

	t.Cleanup(func() {
		_ = testCtx.InferenceClient.InferenceV1().InferencePools(testNamespace).Delete(context.Background(), createdInferencePool.Name, metav1.DeleteOptions{})
	})

	// 3. Create Gateway (triggers HTTPRoute re-enqueue)
	t.Log("Creating Gateway...")
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: kthenaNamespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName("kthena-router"),
			Listeners: []gatewayv1.Listener{
				{Name: gatewayv1.SectionName("http"), Port: gatewayv1.PortNumber(8082), Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	_, err = testCtx.GatewayClient.GatewayV1().Gateways(kthenaNamespace).Create(ctx, gateway, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Gateway")

	t.Cleanup(func() {
		_ = testCtx.GatewayClient.GatewayV1().Gateways(kthenaNamespace).Delete(context.Background(), gatewayName, metav1.DeleteOptions{})
	})

	// 4. Wait for router to process, then verify via port 8082 (pod port, Service does not expose it)
	time.Sleep(5 * time.Second)

	routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)
	pf, err := utils.SetupPortForwardToPod(kthenaNamespace, routerPod.Name, "9081", "8082")
	require.NoError(t, err, "Failed to setup port-forward to late-gateway listener")
	t.Cleanup(func() { pf.Close() })

	t.Log("Verifying HTTPRoute is processed after Gateway creation...")
	utils.CheckChatCompletionsWithURL(t, "http://127.0.0.1:9081/v1/chat/completions", "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B", []utils.ChatMessage{utils.NewChatMessage("user", "Hello late gateway")})
}

func ptr[T any](v T) *T { return &v }
