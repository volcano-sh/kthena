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
	"fmt"
	"os"
	"testing"

	"github.com/volcano-sh/kthena/test/e2e/framework"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

var (
	testCtx         *routercontext.RouterTestContext
	testNamespace   string
	kthenaNamespace string
)

// TestMain runs setup and cleanup for all tests in this package.
func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-router-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	kthenaNamespace = config.Namespace
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

// NOTE: Most test cases in this package should follow the same pattern as the existing cases:
// 1. Implement the test logic in shared.go as a Shared function (e.g., TestModelRouteSimpleShared)
// 2. Call the shared function from router/e2e_test.go with useGatewayAPI=false (no ParentRefs)
// 3. Call the shared function from gateway-api/e2e_test.go with useGatewayAPI=true (with ParentRefs to default Gateway)
//
// This pattern allows code reuse and ensures that tests work both with and without Gateway API.
// Only Gateway API-specific tests (like TestDuplicateModelName)
// should be implemented directly in gateway-api/e2e_test.go without sharing.

// TestModelRouteSimple tests a simple ModelRoute deployment and access.
// This test runs the shared test function without Gateway API (no ParentRefs).
func TestModelRouteSimple(t *testing.T) {
	TestModelRouteSimpleShared(t, testCtx, testNamespace, false, "")
}

// TestModelRouteMultiModels tests ModelRoute with multiple models.
// This test runs the shared test function without Gateway API (no ParentRefs).
func TestModelRouteMultiModels(t *testing.T) {
	TestModelRouteMultiModelsShared(t, testCtx, testNamespace, false, "")
}

// TestModelRoutePrefillDecodeDisaggregation tests PD disaggregation with ModelServing, ModelServer, and ModelRoute.
// This test runs the shared test function without Gateway API (no ParentRefs).
func TestModelRoutePrefillDecodeDisaggregation(t *testing.T) {
	TestModelRoutePrefillDecodeDisaggregationShared(t, testCtx, testNamespace, false, "")
}

// TestModelRouteSubset tests ModelRoute with subset routing.
// This test runs the shared test function without Gateway API (no ParentRefs).
func TestModelRouteSubset(t *testing.T) {
	TestModelRouteSubsetShared(t, testCtx, testNamespace, false, "")
}

// TestModelRouteWithRateLimit tests local rate limiting enforced by the Kthena Router.
// This test runs the shared test function without Gateway API (no ParentRefs).
func TestModelRouteWithRateLimit(t *testing.T) {
	TestModelRouteWithRateLimitShared(t, testCtx, testNamespace, false, "")
}
