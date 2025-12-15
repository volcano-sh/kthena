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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const (
	testNamespace = "default"
)

var (
	kubeClient   *kubernetes.Clientset
	kthenaClient *clientset.Clientset
)

const (
	deployment1_5bName  = "deepseek-r1-1-5b"
	deployment7bName    = "deepseek-r1-7b"
	modelServer1_5bName = "deepseek-r1-1-5b"
	modelServer7bName   = "deepseek-r1-7b"
)

// setupCommonComponents deploys common components that will be used by all test cases.
// This includes the LLM mock deployments and ModelServers.
func setupCommonComponents() error {
	ctx := context.Background()

	// Initialize Kubernetes clients
	config, err := getKubeConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	kthenaClient, err = clientset.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kthena client: %w", err)
	}

	// Deploy LLM Mock DS1.5B Deployment
	fmt.Println("Deploying LLM Mock DS1.5B Deployment...")
	deployment1_5b := loadYAMLFromFile[appsv1.Deployment]("../../../examples/kthena-router/LLM-Mock-ds1.5b.yaml")
	_, err = kubeClient.AppsV1().Deployments(testNamespace).Create(ctx, deployment1_5b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create DS1.5B Deployment: %w", err)
	}
	if apierrors.IsAlreadyExists(err) {
		fmt.Println("DS1.5B Deployment already exists, skipping creation")
	}

	// Deploy LLM Mock DS7B Deployment
	fmt.Println("Deploying LLM Mock DS7B Deployment...")
	deployment7b := loadYAMLFromFile[appsv1.Deployment]("../../../examples/kthena-router/LLM-Mock-ds7b.yaml")
	_, err = kubeClient.AppsV1().Deployments(testNamespace).Create(ctx, deployment7b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create DS7B Deployment: %w", err)
	}
	if apierrors.IsAlreadyExists(err) {
		fmt.Println("DS7B Deployment already exists, skipping creation")
	}

	// Wait for deployments to be ready
	fmt.Println("Waiting for deployments to be ready...")
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		deploy1_5b, err := kubeClient.AppsV1().Deployments(testNamespace).Get(ctx, deployment1_5bName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		deploy7b, err := kubeClient.AppsV1().Deployments(testNamespace).Get(ctx, deployment7bName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return deploy1_5b.Status.ReadyReplicas == *deploy1_5b.Spec.Replicas &&
			deploy7b.Status.ReadyReplicas == *deploy7b.Spec.Replicas, nil
	})
	if err != nil {
		return fmt.Errorf("deployments did not become ready: %w", err)
	}

	// Deploy ModelServer DS1.5B
	fmt.Println("Deploying ModelServer DS1.5B...")
	modelServer1_5b := loadYAMLFromFile[networkingv1alpha1.ModelServer]("../../../examples/kthena-router/ModelServer-ds1.5b.yaml")
	_, err = kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Create(ctx, modelServer1_5b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ModelServer DS1.5B: %w", err)
	}
	if apierrors.IsAlreadyExists(err) {
		fmt.Println("ModelServer DS1.5B already exists, skipping creation")
	}

	// Deploy ModelServer DS7B
	fmt.Println("Deploying ModelServer DS7B...")
	modelServer7b := loadYAMLFromFile[networkingv1alpha1.ModelServer]("../../../examples/kthena-router/ModelServer-ds7b.yaml")
	_, err = kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Create(ctx, modelServer7b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ModelServer DS7B: %w", err)
	}
	if apierrors.IsAlreadyExists(err) {
		fmt.Println("ModelServer DS7B already exists, skipping creation")
	}

	fmt.Println("Common components deployed successfully")
	return nil
}

// cleanupCommonComponents cleans up common components deployed for tests.
func cleanupCommonComponents() error {
	if kubeClient == nil || kthenaClient == nil {
		return nil
	}

	ctx := context.Background()
	fmt.Println("Cleaning up common components...")

	// Delete ModelServers
	if err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Delete(ctx, modelServer1_5bName, metav1.DeleteOptions{}); err != nil {
		fmt.Printf("Warning: Failed to delete ModelServer %s: %v\n", modelServer1_5bName, err)
	}
	if err := kthenaClient.NetworkingV1alpha1().ModelServers(testNamespace).Delete(ctx, modelServer7bName, metav1.DeleteOptions{}); err != nil {
		fmt.Printf("Warning: Failed to delete ModelServer %s: %v\n", modelServer7bName, err)
	}

	// Delete Deployments
	if err := kubeClient.AppsV1().Deployments(testNamespace).Delete(ctx, deployment1_5bName, metav1.DeleteOptions{}); err != nil {
		fmt.Printf("Warning: Failed to delete Deployment %s: %v\n", deployment1_5bName, err)
	}
	if err := kubeClient.AppsV1().Deployments(testNamespace).Delete(ctx, deployment7bName, metav1.DeleteOptions{}); err != nil {
		fmt.Printf("Warning: Failed to delete Deployment %s: %v\n", deployment7bName, err)
	}

	fmt.Println("Common components cleanup completed")
	return nil
}

// TestMain runs setup and cleanup for all tests in this package.
func TestMain(m *testing.M) {
	// Setup common components
	if err := setupCommonComponents(); err != nil {
		fmt.Printf("Failed to setup common components: %v\n", err)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	// Cleanup common components
	if err := cleanupCommonComponents(); err != nil {
		fmt.Printf("Failed to cleanup common components: %v\n", err)
	}

	os.Exit(code)
}

// TestModelRouteSimple tests a simple ModelRoute deployment and access.
func TestModelRouteSimple(t *testing.T) {
	ctx := context.Background()

	// Deploy ModelRoute
	t.Log("Deploying ModelRoute...")
	modelRoute := loadYAMLFromFile[networkingv1alpha1.ModelRoute]("../../../examples/kthena-router/ModelRouteSimple.yaml")
	createdModelRoute, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelRoute")
	assert.NotNil(t, createdModelRoute)
	t.Logf("Created ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)

	// Register cleanup function to delete ModelRoute after test completes
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelRoute: %s/%s", createdModelRoute.Namespace, createdModelRoute.Name)
		if err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(cleanupCtx, createdModelRoute.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelRoute %s/%s: %v", createdModelRoute.Namespace, createdModelRoute.Name, err)
		}
	})

	// Test accessing the model route (with retry logic)
	messages := []utils.ChatMessage{
		utils.NewChatMessage("user", "Hello"),
	}
	utils.CheckChatCompletions(t, modelRoute.Spec.ModelName, messages)
}

func getKubeConfig() (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// Fall back to kubeconfig
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

// loadYAMLFromFile loads a YAML file and unmarshals it into the specified type.
// This function is used in package-level setup and returns errors instead of failing.
func loadYAMLFromFile[T any](path string) *T {
	// Get the absolute path relative to the current working directory
	// In test environment, working directory is typically the project root
	absPath, err := filepath.Abs(path)
	if err != nil {
		// Fallback to relative path
		absPath = path
	}

	// Try to read the file
	data, err := os.ReadFile(absPath)
	if err != nil {
		// If relative path doesn't work, try from test file location
		// Get the directory of this test file
		_, testFile, _, _ := runtime.Caller(1)
		testDir := filepath.Dir(testFile)
		absPath = filepath.Join(testDir, path)
		data, err = os.ReadFile(absPath)
		if err != nil {
			panic(fmt.Sprintf("Failed to read YAML file: %s (tried %s): %v", path, absPath, err))
		}
	}

	var obj T
	err = yaml.Unmarshal(data, &obj)
	if err != nil {
		panic(fmt.Sprintf("Failed to unmarshal YAML file: %s: %v", absPath, err))
	}

	return &obj
}
