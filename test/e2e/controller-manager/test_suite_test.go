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

package controller_manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

var (
	testNamespace   string
	kthenaNamespace string
)

func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-controller-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	kthenaNamespace = config.Namespace
	// Controller manager tests need workload enabled
	config.WorkloadEnabled = true

	if err := framework.InstallKthena(config); err != nil {
		fmt.Printf("Failed to install kthena: %v\n", err)
		os.Exit(1)
	}

	// Create test namespace
	kubeConfig, err := utils.GetKubeConfig()
	if err != nil {
		fmt.Printf("Failed to get kubeconfig: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Printf("Failed to create Kubernetes client: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}
	kthenaClient, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Printf("Failed to create Kthena client: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	if err := utils.CreateTestNamespace(kubeClient, testNamespace); err != nil {
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}
	if err := waitForWebhookReadyE(context.Background(), kthenaClient, testNamespace); err != nil {
		fmt.Printf("Controller-manager webhook did not become ready: %v\n", err)
		_ = utils.DeleteTestNamespaceAndWait(kubeClient, testNamespace, 2*time.Minute)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	// Cleanup test namespace
	if err := utils.DeleteTestNamespaceAndWait(kubeClient, testNamespace, 2*time.Minute); err != nil {
		fmt.Printf("Warning: Failed to delete test namespace %s: %v\n", testNamespace, err)
	}

	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
	}

	os.Exit(code)
}

func setupControllerManagerE2ETest(t *testing.T) (context.Context, *clientset.Clientset, *kubernetes.Clientset) {
	t.Helper()
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		ctxWithDeadline, cancel := context.WithDeadline(ctx, deadline)
		t.Cleanup(cancel)
		ctx = ctxWithDeadline
	}
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")
	kubeClient, err := kubernetes.NewForConfig(config)
	require.NoError(t, err, "Failed to create Kubernetes client")
	return ctx, kthenaClient, kubeClient
}

// waitForPodsGone waits until no Pods matching labelSelector remain in the shared test namespace.
// The controller-manager E2E suite runs all tests sequentially against one shared namespace, so
// labelSelector must scope the wait to the resource under cleanup (e.g. a specific ModelServing or
// ModelBooster instance) rather than the namespace as a whole - an unscoped wait could be satisfied
// by, or block on, pods belonging to an unrelated test.
func waitForPodsGone(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, labelSelector string, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			t.Logf("waitForPodsGone: failed to list pods (selector=%q): %v", labelSelector, err)
			return false
		}
		if len(pods.Items) == 0 {
			return true
		}
		remaining := make([]string, 0, len(pods.Items))
		for _, pod := range pods.Items {
			remaining = append(remaining, fmt.Sprintf("%s(phase=%s,deleting=%t)", pod.Name, pod.Status.Phase, pod.DeletionTimestamp != nil))
		}
		t.Logf("waitForPodsGone: %d pod(s) still present (selector=%q): %v", len(pods.Items), labelSelector, remaining)
		return false
	}, timeout, 5*time.Second, "pods matching selector %q were not cleaned up within %s", labelSelector, timeout)
}

func waitForWebhookReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace string) {
	t.Helper()
	t.Log("Waiting for webhook server to accept requests")
	require.NoError(t, waitForWebhookReadyE(ctx, kthenaClient, namespace), "Webhook did not become ready in time")
}

func waitForWebhookReadyE(ctx context.Context, kthenaClient *clientset.Clientset, namespace string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()

	return wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := createValidModelBoosterForWebhookTest()
		probe.Namespace = namespace
		probe.Name = "webhook-ready-probe-" + utils.RandomString(5)

		createCtx, cancel := context.WithTimeout(ctx, utils.DefaultAPICallTimeout)
		defer cancel()
		_, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(namespace).Create(createCtx, probe, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "failed calling webhook") || strings.Contains(errStr, "connect: connection refused") ||
				errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
				return false, nil
			}
			return false, err
		}

		return true, nil
	})
}
