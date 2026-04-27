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

package context

import (
	stdcontext "context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	inferenceclientset "sigs.k8s.io/gateway-api-inference-extension/client-go/clientset/versioned"
	gatewayclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	Deployment1_5bName    = "deepseek-r1-1-5b"
	Deployment7bName      = "deepseek-r1-7b"
	DeploymentSglangName  = "sglang-mock"
	ModelServer1_5bName   = "deepseek-r1-1-5b"
	ModelServer7bName     = "deepseek-r1-7b"
	ModelServerSglangName = "sglang-mock"
	testDataDir           = "test/e2e/router/testdata"
)

// RouterTestContext holds the clients needed for router tests
type RouterTestContext struct {
	KubeClient      *kubernetes.Clientset
	KthenaClient    *clientset.Clientset
	GatewayClient   *gatewayclientset.Clientset
	InferenceClient *inferenceclientset.Clientset
	Namespace       string
}

// NewRouterTestContext creates a new RouterTestContext
func NewRouterTestContext(namespace string) (*RouterTestContext, error) {
	config, err := utils.GetKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	kthenaClient, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kthena client: %w", err)
	}
	gatewayClient, err := gatewayclientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Gateway client: %w", err)
	}
	inferenceClient, err := inferenceclientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Inference client: %w", err)
	}

	return &RouterTestContext{
		KubeClient:      kubeClient,
		KthenaClient:    kthenaClient,
		GatewayClient:   gatewayClient,
		InferenceClient: inferenceClient,
		Namespace:       namespace,
	}, nil
}

// CreateTestNamespace creates the test namespace if it doesn't exist.
func (c *RouterTestContext) CreateTestNamespace() error {
	fmt.Printf("Creating test namespace: %s\n", c.Namespace)
	ctx := stdcontext.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.Namespace,
		},
	}
	_, err := c.KubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace %s: %w", c.Namespace, err)
	}
	return nil
}

// DeleteTestNamespace deletes the test namespace.
func (c *RouterTestContext) DeleteTestNamespace() error {
	fmt.Printf("Deleting test namespace: %s\n", c.Namespace)
	ctx := stdcontext.Background()
	err := c.KubeClient.CoreV1().Namespaces().Delete(ctx, c.Namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete namespace %s: %w", c.Namespace, err)
	}
	return nil
}

// SetupCommonComponents deploys common components that will be used by all router test cases.
func (c *RouterTestContext) SetupCommonComponents() error {
	fmt.Printf("Setting up common components in namespace: %s\n", c.Namespace)
	ctx := stdcontext.Background()

	// Deploy LLM Mock DS1.5B Deployment
	fmt.Println("Deploying LLM Mock DS1.5B Deployment...")
	deployment1_5b := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(testDataDir, "LLM-Mock-ds1.5b.yaml"))
	deployment1_5b.Namespace = c.Namespace
	_, err := c.KubeClient.AppsV1().Deployments(c.Namespace).Create(ctx, deployment1_5b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create DS1.5B Deployment: %w", err)
	}

	// Deploy LLM Mock DS7B Deployment
	fmt.Println("Deploying LLM Mock DS7B Deployment...")
	deployment7b := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(testDataDir, "LLM-Mock-ds7b.yaml"))
	deployment7b.Namespace = c.Namespace
	_, err = c.KubeClient.AppsV1().Deployments(c.Namespace).Create(ctx, deployment7b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create DS7B Deployment: %w", err)
	}

	// Deploy SGLang Mock Deployment
	fmt.Println("Deploying SGLang Mock Deployment...")
	deploymentSglang := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(testDataDir, "LLM-Mock-sglang.yaml"))
	deploymentSglang.Namespace = c.Namespace
	_, err = c.KubeClient.AppsV1().Deployments(c.Namespace).Create(ctx, deploymentSglang, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create SGLang Mock Deployment: %w", err)
	}

	// Wait for deployments to be ready
	fmt.Println("Waiting for deployments to be ready...")
	timeoutCtx, cancel := stdcontext.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextTimeout(timeoutCtx, 5*time.Second, 5*time.Minute, true, func(ctx stdcontext.Context) (bool, error) {
		deploy1_5b, err := c.KubeClient.AppsV1().Deployments(c.Namespace).Get(ctx, Deployment1_5bName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		deploy7b, err := c.KubeClient.AppsV1().Deployments(c.Namespace).Get(ctx, Deployment7bName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		deploySglang, err := c.KubeClient.AppsV1().Deployments(c.Namespace).Get(ctx, DeploymentSglangName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return deploy1_5b.Status.ReadyReplicas == *deploy1_5b.Spec.Replicas &&
			deploy7b.Status.ReadyReplicas == *deploy7b.Spec.Replicas &&
			deploySglang.Status.ReadyReplicas == *deploySglang.Spec.Replicas, nil
	})
	if err != nil {
		return fmt.Errorf("deployments did not become ready: %w", err)
	}

	if err := c.waitForRouterValidatingWebhook(ctx); err != nil {
		return err
	}

	// Deploy ModelServer DS1.5B
	fmt.Println("Deploying ModelServer DS1.5B...")
	modelServer1_5b := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(testDataDir, "ModelServer-ds1.5b.yaml"))
	modelServer1_5b.Namespace = c.Namespace
	_, err = c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, modelServer1_5b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ModelServer DS1.5B: %w", err)
	}

	// Deploy ModelServer DS7B
	fmt.Println("Deploying ModelServer DS7B...")
	modelServer7b := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(testDataDir, "ModelServer-ds7b.yaml"))
	modelServer7b.Namespace = c.Namespace
	_, err = c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, modelServer7b, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ModelServer DS7B: %w", err)
	}

	// Deploy ModelServer SGLang
	fmt.Println("Deploying ModelServer SGLang...")
	modelServerSglang := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(testDataDir, "ModelServer-sglang.yaml"))
	modelServerSglang.Namespace = c.Namespace
	_, err = c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, modelServerSglang, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ModelServer SGLang: %w", err)
	}

	fmt.Println("Common components deployed successfully")
	return nil
}

func (c *RouterTestContext) waitForRouterValidatingWebhook(ctx stdcontext.Context) error {
	fmt.Println("Waiting for kthena-router validating webhook to accept requests...")

	probeTemplate := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(testDataDir, "ModelServer-ds1.5b.yaml"))
	probeTemplate.Namespace = c.Namespace

	waitCtx, cancel := stdcontext.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx stdcontext.Context) (bool, error) {
		probe := probeTemplate.DeepCopy()
		probe.Name = "webhook-ready-probe-" + utils.RandomString(5)

		_, err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "failed calling webhook") ||
				strings.Contains(errStr, "connect: connection refused") ||
				strings.Contains(errStr, "i/o timeout") ||
				strings.Contains(errStr, "context deadline exceeded") {
				fmt.Printf("Router validating webhook not ready yet, retrying: %v\n", err)
				return false, nil
			}
			return false, err
		}

		fmt.Println("Router validating webhook is ready")
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("kthena-router validating webhook did not become ready in time: %w", err)
	}
	return nil
}

// CleanupCommonComponents cleans up common components deployed for router tests.
func (c *RouterTestContext) CleanupCommonComponents() error {
	if c.KubeClient == nil || c.KthenaClient == nil {
		return nil
	}

	ctx := stdcontext.Background()
	fmt.Printf("Cleaning up common components in namespace: %s\n", c.Namespace)

	// Delete ModelServers
	if err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Delete(ctx, ModelServer1_5bName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete ModelServer DS1.5B: %w", err)
	}
	if err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Delete(ctx, ModelServer7bName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete ModelServer DS7B: %w", err)
	}
	if err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Delete(ctx, ModelServerSglangName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete ModelServer SGLang: %w", err)
	}

	// Delete Deployments
	if err := c.KubeClient.AppsV1().Deployments(c.Namespace).Delete(ctx, Deployment1_5bName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete DS1.5B Deployment: %w", err)
	}
	if err := c.KubeClient.AppsV1().Deployments(c.Namespace).Delete(ctx, Deployment7bName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete DS7B Deployment: %w", err)
	}
	if err := c.KubeClient.AppsV1().Deployments(c.Namespace).Delete(ctx, DeploymentSglangName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("failed to delete SGLang Mock Deployment: %w", err)
	}

	fmt.Println("Common components cleanup completed")
	return nil
}
