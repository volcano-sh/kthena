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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// WaitForGatewayReady waits for a Gateway to become ready (Programmed and Accepted).
func WaitForGatewayReady(t *testing.T, ctx context.Context, gatewayClient *gatewayclientset.Clientset, namespace, name string) error {
	t.Logf("Waiting for Gateway %s/%s to be ready...", namespace, name)
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	err := wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		gw, err := gatewayClient.GatewayV1().Gateways(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

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

		return accepted && programmed, nil
	})
	return err
}

// WaitForModelServingReady waits for a ModelServing to become ready by checking
// if all expected replicas are available.
func WaitForModelServingReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace, name string) {
	t.Log("Waiting for ModelServing to be ready...")
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Error getting ModelServing %s, retrying: %v", name, err)
			return false, nil
		}
		// Check if all replicas are available
		expectedReplicas := int32(1)
		if ms.Spec.Replicas != nil {
			expectedReplicas = *ms.Spec.Replicas
		}
		return ms.Status.AvailableReplicas >= expectedReplicas, nil
	})
	require.NoError(t, err, "ModelServing did not become ready")
}

// WaitForModelRouteReady waits for a ModelRoute to become ready.
// Since ModelRoute does not currently have a status field, we wait for it to exist
// and optionally perform a connectivity check if a model name is available.
func WaitForModelRouteReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace, name string) error {
	t.Logf("Waiting for ModelRoute %s/%s to be ready...", namespace, name)
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var modelName string
	err := wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		mr, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		modelName = mr.Spec.ModelName
		return true, nil
	})
	if err != nil {
		return err
	}

	// If a model name is present, we try a few times to see if the router has picked it up.
	// We use a simple ping request.
	if modelName != "" {
		t.Logf("ModelRoute %s found (model: %s), waiting for router to propagate...", name, modelName)
		messages := []ChatMessage{{Role: "user", Content: "ping"}}
		err = wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			// We don't use CheckChatCompletions here because it calls require.NoError which breaks the loop on failure.
			// Instead, we use a simple HTTP check.
			resp, err := pingModel(modelName, messages)
			if err != nil {
				return false, nil
			}
			return resp.StatusCode == 200, nil
		})
		if err != nil {
			t.Logf("Warning: ModelRoute %s created but responsiveness check failed: %v. Continuing anyway...", name, err)
		}
	}

	return nil
}

// WaitForModelServerReady waits for a ModelServer to become ready by checking
// if at least one associated pod is ready.
func WaitForModelServerReady(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, kthenaClient *clientset.Clientset, namespace, name string) error {
	t.Logf("Waiting for ModelServer %s/%s to be ready...", namespace, name)
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	err := wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		ms, err := kthenaClient.NetworkingV1alpha1().ModelServers(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		if ms.Spec.WorkloadSelector == nil || len(ms.Spec.WorkloadSelector.MatchLabels) == 0 {
			return true, nil // No selector, assume ready if exists
		}

		selector := ""
		for k, v := range ms.Spec.WorkloadSelector.MatchLabels {
			if selector != "" {
				selector += ","
			}
			selector += k + "=" + v
		}

		pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase == "Running" {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == "Ready" && cond.Status == "True" {
						return true, nil
					}
				}
			}
		}
		return false, nil
	})
	return err
}

// pingModel is a helper for WaitForModelRouteReady to check responsiveness without failing the test.
func pingModel(modelName string, messages []ChatMessage) (*ChatCompletionsResponse, error) {
	// Re-implementing a simple version of CheckChatCompletions without dependencies on t or require
	requestBody := ChatCompletionsRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(DefaultRouterURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return &ChatCompletionsResponse{
		StatusCode: resp.StatusCode,
	}, nil
}
