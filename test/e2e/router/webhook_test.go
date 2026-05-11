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
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// waitForKthenaRouterValidatingWebhook polls until a DryRun ModelRoute create reaches the
// validating webhook (avoids flaky tests while cert-manager / deployment finishes).
func waitForKthenaRouterValidatingWebhook(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace string) {
	t.Helper()
	t.Log("Waiting for kthena-router validating webhook to accept requests")

	weight100 := uint32(100)
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := &networkingv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "webhook-ready-probe-" + utils.RandomString(5),
			},
			Spec: networkingv1alpha1.ModelRouteSpec{
				ModelName: "probe-model",
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
		_, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(namespace).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "connect: connection refused") ||
				strings.Contains(errStr, "i/o timeout") ||
				strings.Contains(errStr, "context deadline exceeded") {
				t.Logf("Router validating webhook not ready yet, retrying: %v", err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	require.NoError(t, err, "kthena-router validating webhook did not become ready in time")
}

// TestKthenaRouterValidatingWebhook ensures the networking chart's ValidatingWebhookConfiguration
// targets the real API group and the router webhook rejects invalid ModelRoute specs.
// Invalid case uses an empty string in loraAdapters (CRD CEL allows non-empty list; webhook rejects item).
func TestKthenaRouterValidatingWebhook(t *testing.T) {
	ctx := context.Background()
	waitForKthenaRouterValidatingWebhook(t, ctx, testCtx.KthenaClient, testNamespace)

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
