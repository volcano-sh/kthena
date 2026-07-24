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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestKthenaRouterValidatingWebhook ensures the networking chart's ValidatingWebhookConfiguration
// targets the real API group and the router webhook rejects invalid ModelRoute specs.
// Invalid case uses an empty string in loraAdapters (CRD CEL allows non-empty list; webhook rejects item).
func TestKthenaRouterValidatingWebhook(t *testing.T) {
	ctx := context.Background()
	WaitForKthenaRouterValidatingWebhook(t, ctx, testCtx.KthenaClient, testNamespace)

	weight100 := uint32(100)
	validRoute := &networkingv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      "webhook-valid-dryrun-" + utils.RandomString(5),
		},
		Spec: networkingv1alpha1.ModelRouteSpec{
			ModelName: "webhook-valid",
			Rules: []*networkingv1alpha1.Rule{
				{
					Name: "default",
					TargetModels: []*networkingv1alpha1.TargetModel{
						{ModelServerName: routercontext.ModelServer1_5bName, Weight: &weight100},
					},
				},
			},
		},
	}
	_, err := testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, validRoute, metav1.CreateOptions{DryRun: []string{"All"}})
	require.NoError(t, err, "expected validating webhook to allow a valid ModelRoute (DryRun)")

	invalidRoute := &networkingv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      "webhook-invalid-dryrun-" + utils.RandomString(5),
		},
		Spec: networkingv1alpha1.ModelRouteSpec{
			ModelName:    "",
			LoraAdapters: []string{""},
			Rules: []*networkingv1alpha1.Rule{
				{
					Name: "default",
					TargetModels: []*networkingv1alpha1.TargetModel{
						{ModelServerName: routercontext.ModelServer1_5bName, Weight: &weight100},
					},
				},
			},
		},
	}
	_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, invalidRoute, metav1.CreateOptions{DryRun: []string{"All"}})
	require.Error(t, err, "expected validating webhook to reject invalid ModelRoute")
	assert.Contains(t, err.Error(), "lora adapter name cannot be an empty string")
}
