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
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

	if err := utils.CreateTestNamespace(kubeClient, testNamespace); err != nil {
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
	}

	if err := waitForControllerManagerToStop(kubeClient, kthenaNamespace, 2*time.Minute); err != nil {
		fmt.Printf("Warning: controller-manager did not fully stop before namespace deletion: %v\n", err)
	}

	os.Exit(code)
}

func setupControllerManagerE2ETest(t *testing.T) (context.Context, *clientset.Clientset, *kubernetes.Clientset) {
	t.Helper()
	ctx := context.Background()
	config, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")
	kubeClient, err := kubernetes.NewForConfig(config)
	require.NoError(t, err, "Failed to create Kubernetes client")
	return ctx, kthenaClient, kubeClient
}

func waitForWebhookReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace string) {
	t.Helper()
	t.Log("Waiting for webhook server to accept requests")

	waitCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := createValidModelBoosterForWebhookTest()
		probe.Namespace = namespace
		probe.Name = "webhook-ready-probe-" + utils.RandomString(5)

		_, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(namespace).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "connect: connection refused") {
				t.Logf("Webhook not ready yet (connection refused), retrying: %v", err)
				return false, nil
			}
			return false, err
		}

		return true, nil
	})
	require.NoError(t, err, "Webhook did not become ready in time")
}

func waitForControllerManagerToStop(kubeClient *kubernetes.Clientset, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	labelSelector := "app.kubernetes.io/component=kthena-controller-manager"
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	})
}


function RetryOnConflict(retryFunc):
    maxRetries = 5
    attempts = 0

    loop:
        err = retryFunc()

        if err == nil:
            return success

        if err is NOT a conflict error (HTTP 409 / "object has been modified"):
            return err  // unretryable, bail immediately

        attempts++

        if attempts >= maxRetries:
            return err  // gave up

        backoff(attempts)  // exponential or fixed sleep
        
        // re-fetch the latest object before retrying
        // so you're applying changes on top of current state
        refreshObject()

    end loop