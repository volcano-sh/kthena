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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	backendmetrics "github.com/volcano-sh/kthena/pkg/kthena-router/backend/metrics"
	routermetrics "github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	routercontext "github.com/volcano-sh/kthena/test/e2e/router/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	externalProviderMockImageEnv       = "E2E_EXTERNAL_PROVIDER_MOCK_IMAGE"
	externalProviderMockName           = "external-provider-mock"
	externalProviderSecretName         = "external-provider-mock-credentials"
	externalProviderOpenAIKey          = "e2e-openai-key"
	externalProviderAnthropicKey       = "e2e-anthropic-key"
	externalProviderOpenAISecretKey    = "openai-key"
	externalProviderAnthropicSecretKey = "anthropic-key"

	externalOpenAIChatProviderName = "external-openai-chat"
	externalResponsesProviderName  = "external-openai-responses"
	externalAnthropicProviderName  = "external-anthropic"

	externalOpenAIChatRouteName = "external-openai-chat"
	externalResponsesRouteName  = "external-openai-responses"
	externalAnthropicRouteName  = "external-anthropic"

	externalOpenAIChatModel = "e2e-openai-chat"
	externalResponsesModel  = "e2e-openai-responses"
	externalAnthropicModel  = "e2e-anthropic"

	externalOpenAIChatUpstreamModel = "mock-openai-chat"
	externalResponsesUpstreamModel  = "mock-responses"
	externalAnthropicUpstreamModel  = "mock-anthropic"
)

var (
	externalRouterBaseURL    = "http://127.0.0.1:8080"
	externalRouterMetricsURL = externalRouterBaseURL + "/metrics"

	externalProviderNames = []string{
		externalOpenAIChatProviderName,
		externalResponsesProviderName,
		externalAnthropicProviderName,
	}
	externalRouteNames = []string{
		externalOpenAIChatRouteName,
		externalResponsesRouteName,
		externalAnthropicRouteName,
	}
)

type externalProviderFixture struct {
	adminURL string
	close    func()
}

type mockCapture struct {
	RequestID        string          `json:"request_id"`
	Method           string          `json:"method"`
	Path             string          `json:"path"`
	RawQuery         string          `json:"raw_query"`
	Authorized       bool            `json:"authorized"`
	ContentType      string          `json:"content_type"`
	AnthropicVersion string          `json:"anthropic_version"`
	Body             json.RawMessage `json:"body"`
}

type externalHTTPResponse struct {
	statusCode int
	header     http.Header
	body       []byte
}

type externalMetricSnapshot struct {
	requests      float64
	durationCount uint64
	inputTokens   float64
	outputTokens  float64
}

type externalProviderFixtureCleanup struct {
	t               *testing.T
	namespace       string
	kubeClient      kubernetes.Interface
	kthenaClient    *clientset.Clientset
	portForwarder   utils.PortForwarder
	routerForwarder utils.PortForwarder
	once            sync.Once
}

func setupExternalProviderFixture(t *testing.T, testCtx *routercontext.RouterTestContext, namespace, routerNamespace string) externalProviderFixture {
	t.Helper()
	ctx := context.Background()
	image := os.Getenv(externalProviderMockImageEnv)
	require.NotEmpty(t, image, "%s must name the mock image loaded into Kind", externalProviderMockImageEnv)

	cleanup := &externalProviderFixtureCleanup{
		t:            t,
		namespace:    namespace,
		kubeClient:   testCtx.KubeClient,
		kthenaClient: testCtx.KthenaClient,
	}
	t.Cleanup(cleanup.close)

	routerPort := utils.AllocateLocalPort(t)
	routerForwarder, err := utils.SetupPortForward(routerNamespace, utils.RouterDeploymentName, routerPort, "80")
	require.NoError(t, err, "port-forward restarted Router for external provider tests")
	cleanup.routerForwarder = routerForwarder
	externalRouterBaseURL = fmt.Sprintf("http://127.0.0.1:%s", routerPort)
	externalRouterMetricsURL = externalRouterBaseURL + "/metrics"

	service := utils.LoadYAMLFromFile[corev1.Service](filepath.Join(routercontext.TestDataDir, "ExternalProvider-Mock-Service.yaml"))
	service.Namespace = namespace
	_, err = testCtx.KubeClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	require.NoError(t, err, "create external provider mock Service")

	deployment := utils.LoadYAMLFromFile[appsv1.Deployment](filepath.Join(routercontext.TestDataDir, "ExternalProvider-Mock-Deployment.yaml"))
	deployment.Namespace = namespace
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	deployment.Spec.Template.Spec.Containers[0].Image = image
	_, err = testCtx.KubeClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err, "create external provider mock Deployment")
	utils.WaitForDeploymentReady(t, ctx, testCtx.KubeClient, namespace, externalProviderMockName, 1, 2*time.Minute)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: externalProviderSecretName, Namespace: namespace},
		StringData: map[string]string{
			externalProviderOpenAISecretKey:    externalProviderOpenAIKey,
			externalProviderAnthropicSecretKey: externalProviderAnthropicKey,
		},
	}
	_, err = testCtx.KubeClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err, "create external provider mock credentials")

	baseURL := fmt.Sprintf("https://%s.%s.svc", externalProviderMockName, namespace)
	providers := []*networkingv1alpha1.ExternalModelProvider{
		newExternalProvider(namespace, externalOpenAIChatProviderName, networkingv1alpha1.OpenAI, baseURL, externalOpenAIChatUpstreamModel, externalProviderOpenAISecretKey, nil),
		newExternalProvider(namespace, externalResponsesProviderName, networkingv1alpha1.OpenAI, baseURL, externalResponsesUpstreamModel, externalProviderOpenAISecretKey, nil),
		newExternalProvider(namespace, externalAnthropicProviderName, networkingv1alpha1.Anthropic, baseURL, externalAnthropicUpstreamModel, externalProviderAnthropicSecretKey, map[string]string{
			"anthropic-version": "2023-06-01",
		}),
	}
	for _, provider := range providers {
		_, err = testCtx.KthenaClient.NetworkingV1alpha1().ExternalModelProviders(namespace).Create(ctx, provider, metav1.CreateOptions{})
		require.NoError(t, err, "create ExternalModelProvider %s", provider.Name)
		waitForExternalProviderReady(t, ctx, testCtx.KthenaClient, namespace, provider.Name)
	}

	routes := []*networkingv1alpha1.ModelRoute{
		newExternalModelRoute(namespace, externalOpenAIChatRouteName, externalOpenAIChatModel, externalOpenAIChatProviderName),
		newExternalModelRoute(namespace, externalResponsesRouteName, externalResponsesModel, externalResponsesProviderName),
		newExternalModelRoute(namespace, externalAnthropicRouteName, externalAnthropicModel, externalAnthropicProviderName),
	}
	for _, route := range routes {
		_, err = testCtx.KthenaClient.NetworkingV1alpha1().ModelRoutes(namespace).Create(ctx, route, metav1.CreateOptions{})
		require.NoError(t, err, "create ModelRoute %s", route.Name)
	}

	localPort := utils.AllocateLocalPort(t)
	cleanup.portForwarder, err = utils.SetupPortForward(namespace, externalProviderMockName, localPort, "8080")
	require.NoError(t, err, "port-forward external provider mock admin API")
	adminURL := fmt.Sprintf("http://127.0.0.1:%s", localPort)
	waitForExternalRoutesReady(t, adminURL)

	return externalProviderFixture{
		adminURL: adminURL,
		close:    cleanup.close,
	}
}

func waitForExternalRoutesReady(t *testing.T, adminURL string) {
	t.Helper()
	tests := []struct {
		name string
		path string
		body []byte
	}{
		{
			name: "chat",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"` + externalOpenAIChatModel + `","messages":[{"role":"user","content":"route ready"}],"stream":false}`),
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"` + externalResponsesModel + `","input":"route ready","stream":false}`),
		},
		{
			name: "anthropic",
			path: "/v1/messages",
			body: []byte(`{"model":"` + externalAnthropicModel + `","max_tokens":8,"messages":[{"role":"user","content":"route ready"}],"stream":false}`),
		},
	}
	for _, tt := range tests {
		requestID := "external-route-ready-" + tt.name + "-" + utils.RandomString(10)
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			response, err := doExternalJSONRequest(ctx, tt.path, requestID, tt.body)
			return err == nil && response.statusCode == http.StatusOK
		}, 30*time.Second, 250*time.Millisecond, "ModelRoute for %s was not programmed by the Router", tt.name)
		deleteMockCapture(t, adminURL, requestID)
	}
}

func newExternalProvider(namespace, name string, providerType networkingv1alpha1.ExternalProviderType, baseURL, model, secretKey string, headers map[string]string) *networkingv1alpha1.ExternalModelProvider {
	return &networkingv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType:       providerType,
			Model:              &model,
			BaseURL:            baseURL,
			InsecureSkipVerify: true,
			Auth: &networkingv1alpha1.ProviderAuth{SecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: externalProviderSecretName},
				Key:                  secretKey,
			}},
			Headers: headers,
		},
	}
}

func newExternalModelRoute(namespace, name, model, provider string) *networkingv1alpha1.ModelRoute {
	return &networkingv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1alpha1.ModelRouteSpec{
			ModelName: model,
			Rules: []*networkingv1alpha1.Rule{{
				Name: "default",
				TargetModels: []*networkingv1alpha1.TargetModel{{
					ExternalModelProviderName: provider,
				}},
			}},
		},
	}
}

func waitForExternalProviderReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		provider, err := kthenaClient.NetworkingV1alpha1().ExternalModelProviders(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return provider.Status.ObservedGeneration >= provider.Generation &&
			apimeta.IsStatusConditionTrue(provider.Status.Conditions, networkingv1alpha1.ExternalModelProviderConditionReady) &&
			apimeta.IsStatusConditionTrue(provider.Status.Conditions, networkingv1alpha1.ExternalModelProviderConditionCredentialsResolved), nil
	})
	require.NoError(t, err, "wait for ExternalModelProvider %s to become ready", name)
}

func sendExternalJSONRequest(t *testing.T, path, requestID string, body []byte) externalHTTPResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := doExternalJSONRequest(ctx, path, requestID, body)
	require.NoError(t, err, "send external provider request")
	return response
}

func doExternalJSONRequest(ctx context.Context, path, requestID string, body []byte) (externalHTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, externalRouterBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return externalHTTPResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return externalHTTPResponse{}, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return externalHTTPResponse{}, err
	}
	return externalHTTPResponse{
		statusCode: resp.StatusCode,
		header:     resp.Header.Clone(),
		body:       responseBody,
	}, nil
}

func fetchMockCapture(t *testing.T, adminURL, requestID string) mockCapture {
	t.Helper()
	var capture mockCapture
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, mockCaptureURL(adminURL, requestID), nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		return json.NewDecoder(resp.Body).Decode(&capture) == nil
	}, 10*time.Second, 200*time.Millisecond, "mock did not capture request %q", requestID)
	return capture
}

func deleteMockCapture(t *testing.T, adminURL, requestID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, mockCaptureURL(adminURL, requestID), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "delete mock capture %q", requestID)
	defer resp.Body.Close()
	require.Contains(t, []int{http.StatusNoContent, http.StatusNotFound}, resp.StatusCode)
}

func mockCaptureURL(adminURL, requestID string) string {
	return strings.TrimRight(adminURL, "/") + "/__test/requests/" + url.PathEscape(requestID)
}

func readExternalMetricSnapshot(t *testing.T, model, path string, statusCode int, errorType, namespace, route, provider, upstreamModel string) externalMetricSnapshot {
	t.Helper()
	snapshot, err := tryReadExternalMetricSnapshot(model, path, statusCode, errorType, namespace, route, provider, upstreamModel)
	require.NoError(t, err, "fetch Router metrics")
	return snapshot
}

func tryReadExternalMetricSnapshot(model, path string, statusCode int, errorType, namespace, route, provider, upstreamModel string) (externalMetricSnapshot, error) {
	allMetrics, err := backendmetrics.ParseMetricsURL(externalRouterMetricsURL)
	if err != nil {
		return externalMetricSnapshot{}, err
	}

	modelRoute := namespace + "/" + route
	backendName := namespace + "/" + provider
	destinationLabels := map[string]string{
		"model":          model,
		"path":           path,
		"model_route":    modelRoute,
		"backend_type":   routermetrics.BackendTypeExternalProvider,
		"backend_name":   backendName,
		"upstream_model": upstreamModel,
	}
	requestLabels := cloneStringMap(destinationLabels)
	requestLabels["status_code"] = strconv.Itoa(statusCode)
	requestLabels["error_type"] = errorType
	durationLabels := cloneStringMap(destinationLabels)
	durationLabels["status_code"] = strconv.Itoa(statusCode)
	inputLabels := cloneStringMap(destinationLabels)
	inputLabels["token_type"] = routermetrics.TokenTypeInput
	outputLabels := cloneStringMap(destinationLabels)
	outputLabels["token_type"] = routermetrics.TokenTypeOutput

	return externalMetricSnapshot{
		requests:      utils.GetCounterValue(allMetrics, "kthena_router_requests_total", requestLabels),
		durationCount: utils.GetHistogramCount(allMetrics, "kthena_router_request_duration_seconds", durationLabels),
		inputTokens:   utils.GetCounterValue(allMetrics, "kthena_router_tokens_total", inputLabels),
		outputTokens:  utils.GetCounterValue(allMetrics, "kthena_router_tokens_total", outputLabels),
	}, nil
}

func readExternalActiveGauges(t *testing.T, model, namespace, route, provider, upstreamModel string) (downstream, upstream float64) {
	t.Helper()
	downstream, upstream, err := tryReadExternalActiveGauges(model, namespace, route, provider, upstreamModel)
	require.NoError(t, err, "fetch Router active-request metrics")
	return downstream, upstream
}

func tryReadExternalActiveGauges(model, namespace, route, provider, upstreamModel string) (downstream, upstream float64, err error) {
	allMetrics, err := backendmetrics.ParseMetricsURL(externalRouterMetricsURL)
	if err != nil {
		return 0, 0, err
	}
	downstream = utils.GetGaugeValue(allMetrics, "kthena_router_active_downstream_requests", map[string]string{
		"model": model,
	})
	upstream = utils.GetGaugeValue(allMetrics, "kthena_router_active_upstream_requests", map[string]string{
		"model_server":   routermetrics.DestinationLabelValueNone,
		"model_route":    namespace + "/" + route,
		"backend_type":   routermetrics.BackendTypeExternalProvider,
		"backend_name":   namespace + "/" + provider,
		"upstream_model": upstreamModel,
	})
	return downstream, upstream, nil
}

func waitForExternalAccessLog(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace, requestID string) accesslog.AccessLogEntry {
	t.Helper()
	readyPods := utils.GetReadyRouterPods(t, kubeClient, kthenaNamespace)
	var found accesslog.AccessLogEntry
	require.Eventually(t, func() bool {
		for _, pod := range readyPods {
			sinceSeconds := int64((5 * time.Minute).Seconds())
			logs, err := kubeClient.CoreV1().Pods(kthenaNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				SinceSeconds: &sinceSeconds,
			}).Do(context.Background()).Raw()
			if err != nil {
				continue
			}
			if entry, ok := findExternalAccessLog(logs, requestID); ok {
				found = entry
				return true
			}
		}
		return false
	}, 15*time.Second, 500*time.Millisecond, "Router access log not found for request %q", requestID)
	return found
}

func findExternalAccessLog(logs []byte, requestID string) (accesslog.AccessLogEntry, bool) {
	for _, line := range bytes.Split(logs, []byte{'\n'}) {
		if entry, ok := utils.ParseRouterAccessLogLine(line); ok && entry.RequestID == requestID {
			return entry, true
		}
	}
	return accesslog.AccessLogEntry{}, false
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (c *externalProviderFixtureCleanup) close() {
	c.once.Do(func() {
		ctx := context.Background()
		for _, name := range externalRouteNames {
			c.reportDeleteError("ModelRoute", name, c.kthenaClient.NetworkingV1alpha1().ModelRoutes(c.namespace).Delete(ctx, name, metav1.DeleteOptions{}))
		}
		for _, name := range externalProviderNames {
			c.reportDeleteError("ExternalModelProvider", name, c.kthenaClient.NetworkingV1alpha1().ExternalModelProviders(c.namespace).Delete(ctx, name, metav1.DeleteOptions{}))
		}
		c.reportDeleteError("Secret", externalProviderSecretName, c.kubeClient.CoreV1().Secrets(c.namespace).Delete(ctx, externalProviderSecretName, metav1.DeleteOptions{}))
		if c.portForwarder != nil {
			c.portForwarder.Close()
		}
		if c.routerForwarder != nil {
			c.routerForwarder.Close()
		}
		c.reportDeleteError("Deployment", externalProviderMockName, c.kubeClient.AppsV1().Deployments(c.namespace).Delete(ctx, externalProviderMockName, metav1.DeleteOptions{}))
		c.reportDeleteError("Service", externalProviderMockName, c.kubeClient.CoreV1().Services(c.namespace).Delete(ctx, externalProviderMockName, metav1.DeleteOptions{}))
	})
}

func (c *externalProviderFixtureCleanup) reportDeleteError(kind, name string, err error) {
	if err != nil && !apierrors.IsNotFound(err) {
		c.t.Errorf("delete %s %s/%s: %v", kind, c.namespace, name, err)
	}
}
