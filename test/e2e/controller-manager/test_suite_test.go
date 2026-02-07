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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
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

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Failed to create test namespace %s: %v\n", testNamespace, err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}
	fmt.Printf("Created test namespace: %s\n", testNamespace)

	// Run tests
	code := m.Run()

	// Cleanup test namespace
	fmt.Printf("Deleting test namespace: %s\n", testNamespace)
	err = kubeClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
	if err != nil {
		fmt.Printf("Failed to delete test namespace %s: %v\n", testNamespace, err)
	}

	// Wait for namespace to be deleted
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := kubeClient.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
		if err != nil {
			return true, nil // namespace is gone
		}
		return false, nil
	})
	if err != nil {
		fmt.Printf("Timeout waiting for namespace %s deletion: %v\n", testNamespace, err)
	}

	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
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
