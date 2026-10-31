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
	"errors"
	"fmt"
	"os"
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
	// TestDataDir is the path to testdata, relative to the project root.
	// LoadYAMLFromFile resolves paths against the project root via runtime.Caller.
	TestDataDir           = "test/e2e/router/tokenizer/testdata"
	EngineDeploymentName  = "tokenizer-test-engine"
	ModelServerName       = "tokenizer-test-model"
	ModelRouteName        = "tokenizer-test-model"
	TokenizerReadyTimeout = 5 * time.Minute
)

// TokenizerTestContext holds clients and state for tokenizer E2E tests.
type TokenizerTestContext struct {
	KubeClient      *kubernetes.Clientset
	KthenaClient    *clientset.Clientset
	Namespace       string
	KthenaNamespace string
	// TokenizerBaseURL is the port-forwarded URL to the standalone tokenizer service.
	TokenizerBaseURL string
	tokenizerPF      utils.PortForwarder
	// tokenizerLocalPort is the local port used for the tokenizer port-forward.
	// Stored so RefreshPortForward can reuse the same port after a kthena reinstall.
	tokenizerLocalPort string
}

// NewTokenizerTestContextWithPortForward creates the context and allocates a local port for
// the tokenizer service. Call this variant in TestMain after framework.InstallKthena.
func NewTokenizerTestContextWithPortForward(namespace, kthenaNamespace string) (*TokenizerTestContext, error) {
	config, err := utils.GetKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kube client: %w", err)
	}
	kthenaClient, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kthena client: %w", err)
	}

	// Allocate an ephemeral local port rather than using a hardcoded one to avoid
	// conflicts when multiple test runs or processes execute concurrently.
	localPort, err := utils.AllocateLocalPortE()
	if err != nil {
		return nil, fmt.Errorf("allocate local port for tokenizer port-forward: %w", err)
	}
	pf, err := utils.SetupPortForward(kthenaNamespace, utils.TokenizerServiceName, localPort, utils.TokenizerPort)
	if err != nil {
		return nil, fmt.Errorf("setup tokenizer port-forward on :%s: %w", localPort, err)
	}

	return &TokenizerTestContext{
		KubeClient:         kubeClient,
		KthenaClient:       kthenaClient,
		Namespace:          namespace,
		KthenaNamespace:    kthenaNamespace,
		TokenizerBaseURL:   fmt.Sprintf("http://127.0.0.1:%s", localPort),
		tokenizerPF:        pf,
		tokenizerLocalPort: localPort,
	}, nil
}

// ClosePortForward stops the tokenizer port-forward.
func (c *TokenizerTestContext) ClosePortForward() {
	if c.tokenizerPF != nil {
		c.tokenizerPF.Close()
		c.tokenizerPF = nil
	}
}

// RefreshPortForward closes the existing tokenizer port-forward and opens a new one
// on the same local port. Call this after any kthena reinstall that replaces the
// tokenizer pod, which breaks the tunnel the TestMain port-forward was established on.
func (c *TokenizerTestContext) RefreshPortForward() error {
	c.ClosePortForward()
	pf, err := utils.SetupPortForward(c.KthenaNamespace, utils.TokenizerServiceName, c.tokenizerLocalPort, utils.TokenizerPort)
	if err != nil {
		return fmt.Errorf("refresh tokenizer port-forward on :%s: %w", c.tokenizerLocalPort, err)
	}
	c.tokenizerPF = pf
	return nil
}

// CreateTestNamespace creates the test namespace.
func (c *TokenizerTestContext) CreateTestNamespace() error {
	return utils.CreateTestNamespace(c.KubeClient, c.Namespace)
}

// DeleteTestNamespace deletes the test namespace and waits for it to disappear.
func (c *TokenizerTestContext) DeleteTestNamespace() error {
	return utils.DeleteTestNamespaceAndWait(c.KubeClient, c.Namespace, 2*time.Minute)
}

// SetupCommonComponents deploys the tokenizer-engine-mock Deployment and its ModelServer.
func (c *TokenizerTestContext) SetupCommonComponents() error {
	fmt.Printf("Setting up tokenizer test components in namespace: %s\n", c.Namespace)
	ctx := stdcontext.Background()

	engineDeployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(TestDataDir, "LLM-Mock-tokenizer.yaml"))
	engineDeployment.Namespace = c.Namespace

	// Allow the image to be overridden by the CI environment.
	if img := os.Getenv("IMG_TOKENIZER_ENGINE_MOCK"); img != "" {
		for i := range engineDeployment.Spec.Template.Spec.Containers {
			engineDeployment.Spec.Template.Spec.Containers[i].Image = img
		}
	}

	_, err := c.KubeClient.AppsV1().Deployments(c.Namespace).Create(ctx, engineDeployment, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create tokenizer-engine-mock deployment: %w", err)
	}

	if err := utils.WaitForDeploymentReadyE(ctx, c.KubeClient, c.Namespace, EngineDeploymentName, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for tokenizer-engine-mock ready: %w", err)
	}

	ms := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(TestDataDir, "ModelServer-tokenizer-test.yaml"))
	ms.Namespace = c.Namespace
	_, err = c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Create(ctx, ms, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create tokenizer ModelServer: %w", err)
	}

	mr := utils.LoadYAMLFromFile[networkingv1alpha1.ModelRoute](filepath.Join(TestDataDir, "ModelRoute-tokenizer-test.yaml"))
	mr.Namespace = c.Namespace
	_, err = c.KthenaClient.NetworkingV1alpha1().ModelRoutes(c.Namespace).Create(ctx, mr, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create tokenizer ModelRoute: %w", err)
	}

	fmt.Println("Tokenizer test components deployed successfully")
	return nil
}

// CleanupCommonComponents removes the test ModelServer and engine deployment.
// NotFound errors are treated as successful cleanup. Both deletions are attempted
// even if the first fails; any unexpected errors are combined and returned.
func (c *TokenizerTestContext) CleanupCommonComponents() error {
	ctx := stdcontext.Background()
	var errs []error

	if err := c.KthenaClient.NetworkingV1alpha1().ModelRoutes(c.Namespace).Delete(ctx, ModelRouteName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete ModelRoute %s: %w", ModelRouteName, err))
	}

	if err := c.KthenaClient.NetworkingV1alpha1().ModelServers(c.Namespace).Delete(ctx, ModelServerName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete ModelServer %s: %w", ModelServerName, err))
	}

	if err := c.KubeClient.AppsV1().Deployments(c.Namespace).Delete(ctx, EngineDeploymentName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete engine deployment %s: %w", EngineDeploymentName, err))
	}

	return errors.Join(errs...)
}
