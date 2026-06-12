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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	DeploymentName  = "router-plugin-mock"
	ModelServerName = "router-plugin-mock"
	ModelName       = "router-plugin-model"
	AppLabel        = "router-plugin-mock"
	TestDataDir     = "test/e2e/router/router-plugins/testdata"

	SlowMockDeploymentName = "router-plugin-mock-slow"
	SlowMockAppLabel       = "router-plugin-mock-slow"
	SlowModelServerName    = "router-plugin-mock-slow"
)

// PluginTestContext holds clients for router plugin e2e tests.
type PluginTestContext struct {
	KubeClient   *kubernetes.Clientset
	KthenaClient *clientset.Clientset
	Namespace    string
}

// NewPluginTestContext creates a PluginTestContext.
func NewPluginTestContext(namespace string) (*PluginTestContext, error) {
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
	return &PluginTestContext{
		KubeClient:   kubeClient,
		KthenaClient: kthenaClient,
		Namespace:    namespace,
	}, nil
}

// CreateTestNamespace creates the test namespace.
func (c *PluginTestContext) CreateTestNamespace() error {
	return utils.CreateTestNamespace(c.KubeClient, c.Namespace)
}

// DeleteTestNamespace deletes the test namespace.
func (c *PluginTestContext) DeleteTestNamespace() error {
	return utils.DeleteTestNamespaceAndWait(c.KubeClient, c.Namespace, 2*time.Minute)
}

// SetupPluginComponents deploys the plugin test mock backend and ModelServer.
func (c *PluginTestContext) SetupPluginComponents() error {
	ctx := stdcontext.Background()

	deployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(TestDataDir, "LLM-Mock-plugins.yaml"))
	deployment.Namespace = c.Namespace
	if _, err := c.KubeClient.AppsV1().Deployments(c.Namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment: %w", err)
	}
	if err := utils.WaitForDeploymentReadyE(ctx, c.KubeClient, c.Namespace, DeploymentName, 5*time.Minute); err != nil {
		return err
	}

	probe := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(TestDataDir, "ModelServer-plugins.yaml"))
	probe.Namespace = c.Namespace
	waitCtx, cancel := stdcontext.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := wait.PollUntilContextCancel(waitCtx, 2*time.Second, true, func(ctx stdcontext.Context) (bool, error) {
		p := probe.DeepCopy()
		p.Name = "webhook-ready-probe-" + utils.RandomString(5)
		_, err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, p, metav1.CreateOptions{DryRun: []string{"All"}})
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "failed calling webhook") ||
				strings.Contains(errStr, "connect: connection refused") ||
				strings.Contains(errStr, "i/o timeout") ||
				strings.Contains(errStr, "context deadline exceeded") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}); err != nil {
		return err
	}

	modelServer := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(TestDataDir, "ModelServer-plugins.yaml"))
	modelServer.Namespace = c.Namespace
	if _, err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, modelServer, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create model server: %w", err)
	}
	return nil
}

// CleanupPluginComponents removes plugin test resources.
func (c *PluginTestContext) CleanupPluginComponents() error {
	ctx := stdcontext.Background()
	_ = c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Delete(ctx, ModelServerName, metav1.DeleteOptions{})
	_ = c.KubeClient.AppsV1().Deployments(c.Namespace).Delete(ctx, DeploymentName, metav1.DeleteOptions{})
	return nil
}
