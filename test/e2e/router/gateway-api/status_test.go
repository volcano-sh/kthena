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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayAndHTTPRouteStatus(t *testing.T) {
	ctx := context.Background()

	// 1. Verify Default Gateway Status
	t.Log("Verifying default Gateway status...")
	require.Eventually(t, func() bool {
		gw, err := testCtx.GatewayClient.GatewayV1().Gateways(kthenaNamespace).Get(ctx, "default", metav1.GetOptions{})
		if err != nil {
			return false
		}

		// Check Gateway conditions
		accepted := false
		programmed := false
		for _, cond := range gw.Status.Conditions {
			if cond.Type == string(gatewayv1.GatewayConditionAccepted) && cond.Status == metav1.ConditionTrue {
				accepted = true
			}
			if cond.Type == string(gatewayv1.GatewayConditionProgrammed) && cond.Status == metav1.ConditionTrue {
				programmed = true
			}
		}

		if !accepted || !programmed {
			return false
		}

		// Check Listener status
		if len(gw.Status.Listeners) == 0 {
			return false
		}

		for _, lStatus := range gw.Status.Listeners {
			lAccepted := false
			lProgrammed := false
			for _, cond := range lStatus.Conditions {
				if cond.Type == string(gatewayv1.ListenerConditionAccepted) && cond.Status == metav1.ConditionTrue {
					lAccepted = true
				}
				if cond.Type == string(gatewayv1.ListenerConditionProgrammed) && cond.Status == metav1.ConditionTrue {
					lProgrammed = true
				}
			}
			if !lAccepted || !lProgrammed {
				return false
			}
		}

		return true
	}, 2*time.Minute, 5*time.Second, "Gateway status should be updated correctly")

	// 2. Deploy an HTTPRoute and verify its status
	t.Log("Deploying HTTPRoute and verifying status...")
	httpRoute := utils.LoadYAMLFromFile[gatewayv1.HTTPRoute]("examples/kthena-router/HTTPRoute.yaml")
	httpRoute.Namespace = testNamespace
	
	// Update parentRefs to point to kthenaNamespace and the "default" Gateway
	ktNs := gatewayv1.Namespace(kthenaNamespace)
	httpRoute.Spec.ParentRefs = []gatewayv1.ParentReference{
		{
			Name:      "default",
			Namespace: &ktNs,
		},
	}

	createdHTTPRoute, err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Create(ctx, httpRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create HTTPRoute")
	
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Delete(cleanupCtx, createdHTTPRoute.Name, metav1.DeleteOptions{})
	})

	require.Eventually(t, func() bool {
		hr, err := testCtx.GatewayClient.GatewayV1().HTTPRoutes(testNamespace).Get(ctx, createdHTTPRoute.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}

		if len(hr.Status.Parents) == 0 {
			return false
		}

		for _, parent := range hr.Status.Parents {
			if parent.ControllerName != gatewayv1.GatewayController(routercontext.ControllerName) {
				continue
			}

			accepted := false
			resolved := false
			for _, cond := range parent.Conditions {
				if cond.Type == string(gatewayv1.RouteConditionAccepted) && cond.Status == metav1.ConditionTrue {
					accepted = true
				}
				if cond.Type == string(gatewayv1.RouteConditionResolvedRefs) && cond.Status == metav1.ConditionTrue {
					resolved = true
				}
			}
			if accepted && resolved {
				return true
			}
		}
		return false
	}, 2*time.Minute, 5*time.Second, "HTTPRoute status should be updated correctly by kthena-router")
}
