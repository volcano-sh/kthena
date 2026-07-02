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
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	DeploymentName         = "router-plugin-mock"
	ModelServerName        = DeploymentName
	ModelName              = "router-plugin-model"
	TestDataDir            = "test/e2e/router/router-plugins/testdata"
	SlowMockDeploymentName = "router-plugin-mock-slow"
	SlowMockAppLabel       = "router-plugin-mock-slow"
)

// SetupPluginComponents deploys fast/slow plugin mocks and ModelServers shared by plugin e2e tests.
func SetupPluginComponents(kubeClient *kubernetes.Clientset, kthenaClient *clientset.Clientset, namespace string) error {
	ctx := stdcontext.Background()

	deployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(TestDataDir, "LLM-Mock-plugins.yaml"))
	deployment.Namespace = namespace
	if _, err := kubeClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create fast deployment: %w", err)
	}
	if err := utils.WaitForDeploymentReadyE(ctx, kubeClient, namespace, DeploymentName, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for fast deployment: %w", err)
	}

	slowDeployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(TestDataDir, "LLM-Mock-plugins-slow.yaml"))
	slowDeployment.Namespace = namespace
	if _, err := kubeClient.AppsV1().Deployments(namespace).Create(ctx, slowDeployment, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create slow deployment: %w", err)
	}
	if err := utils.WaitForDeploymentReadyE(ctx, kubeClient, namespace, SlowMockDeploymentName, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for slow deployment: %w", err)
	}

	if err := utils.WaitForRouterValidatingWebhookE(ctx, kthenaClient, namespace, ModelServerName, ModelName); err != nil {
		return fmt.Errorf("wait for validating webhook: %w", err)
	}

	modelServer := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(TestDataDir, "ModelServer-plugins.yaml"))
	modelServer.Namespace = namespace
	if _, err := kthenaClient.NetworkingV1alpha1().ModelServers(namespace).Create(ctx, modelServer, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create model server: %w", err)
	}

	mixedModelServer := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(TestDataDir, "ModelServer-plugins-mixed.yaml"))
	mixedModelServer.Namespace = namespace
	if _, err := kthenaClient.NetworkingV1alpha1().ModelServers(namespace).Create(ctx, mixedModelServer, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create mixed model server: %w", err)
	}
	return nil
}
