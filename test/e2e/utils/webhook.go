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

package utils

import (
	"context"
	"fmt"
	"strings"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// KthenaRouterModelRouteWebhookErrProbablyNotReady reports whether err likely means the
// kthena-router validate-modelroute webhook is not ready yet (safe to retry).
func KthenaRouterModelRouteWebhookErrProbablyNotReady(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connect: connection refused") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "Client.Timeout exceeded") ||
		strings.Contains(s, "Timeout exceeded while awaiting headers") ||
		strings.Contains(s, "failed calling webhook") ||
		strings.Contains(s, "no endpoints available")
}

// WaitForKthenaRouterModelRouteValidatingWebhook polls by issuing ModelRoute Create with DryRun until
// the apiserver reaches the kthena-router webhook or the deadline is exceeded.
// modelServerName must reference an existing ModelServer in namespace. probeNamePrefix is prepended to a random suffix.
// logf may be nil. Zero pollInterval defaults to 2s; zero timeout defaults to 4m.
func WaitForKthenaRouterModelRouteValidatingWebhook(ctx context.Context, kthenaClient *clientset.Clientset, namespace, modelServerName, probeNamePrefix string, pollInterval, timeout time.Duration, logf func(string, ...any)) error {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 4 * time.Minute
	}
	if probeNamePrefix == "" {
		probeNamePrefix = "webhook-ready-probe-"
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	err := wait.PollUntilContextCancel(waitCtx, pollInterval, true, func(ctx context.Context) (bool, error) {
		weight100 := uint32(100)
		probe := &networkingv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      probeNamePrefix + RandomString(5),
			},
			Spec: networkingv1alpha1.ModelRouteSpec{
				ModelName: "probe-model",
				Rules: []*networkingv1alpha1.Rule{
					{
						Name: "default",
						TargetModels: []*networkingv1alpha1.TargetModel{
							{ModelServerName: modelServerName, Weight: &weight100},
						},
					},
				},
			},
		}
		_, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(namespace).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			lastErr = err
			if KthenaRouterModelRouteWebhookErrProbablyNotReady(err) {
				if logf != nil {
					logf("kthena-router ModelRoute validating webhook not ready yet, retrying: %v", err)
				}
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		if lastErr != nil {
			return fmt.Errorf("%w (last error: %v)", err, lastErr)
		}
		return err
	}
	return nil
}
