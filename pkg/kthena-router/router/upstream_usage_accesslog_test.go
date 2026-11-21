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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/connectors"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/providers"
)

func upstreamUsageIntPtr(v int) *int { return &v }

func TestNewRouterReadsIncludeUpstreamUsage(t *testing.T) {
	base, err := os.ReadFile("../scheduler/testdata/configmap.yaml")
	require.NoError(t, err)

	tests := []struct {
		name  string
		extra string
		want  bool
	}{
		{name: "section absent", want: false},
		{name: "disabled", extra: "accessLog:\n  includeUpstreamUsage: false\n", want: false},
		{name: "enabled", extra: "accessLog:\n  includeUpstreamUsage: true\n", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "routerConfiguration.yaml")
			require.NoError(t, os.WriteFile(configPath, append(append([]byte{}, base...), tt.extra...), 0o600))
			router := NewRouter(datastore.New(), configPath, common.NewTransportRegistry())
			assert.Equal(t, tt.want, router.includeUpstreamUsage)
		})
	}
}

func TestRecordUpstreamUsage(t *testing.T) {
	reported := providers.TokenUsage{
		PromptTokens: 1830, CompletionTokens: 412, TotalTokens: 2242,
		PromptTokensReported: true, CachedPromptTokens: 1536, CachedPromptTokensReported: true,
	}
	tests := []struct {
		name string
		// path defaults to /v1/chat/completions.
		path       string
		enabled    bool
		usage      providers.TokenUsage
		wantInput  *int
		wantCached *int
	}{
		{name: "disabled", usage: reported},
		{
			name: "enabled for completions", path: "/v1/completions", enabled: true, usage: reported,
			wantInput: upstreamUsageIntPtr(1830), wantCached: upstreamUsageIntPtr(1536),
		},
		// Other paths share the OpenAI usage parser but are out of scope.
		{name: "enabled for embeddings", path: "/v1/embeddings", enabled: true, usage: reported},
		{name: "enabled for responses", path: "/v1/responses", enabled: true, usage: reported},
		{name: "enabled with both counts", enabled: true, usage: reported, wantInput: upstreamUsageIntPtr(1830), wantCached: upstreamUsageIntPtr(1536)},
		{
			name:      "enabled with explicit zero cached tokens",
			enabled:   true,
			usage:     providers.TokenUsage{PromptTokens: 20, TotalTokens: 20, PromptTokensReported: true, CachedPromptTokensReported: true},
			wantInput: upstreamUsageIntPtr(20), wantCached: upstreamUsageIntPtr(0),
		},
		{
			name:      "enabled without cached tokens",
			enabled:   true,
			usage:     providers.TokenUsage{PromptTokens: 20, TotalTokens: 20, PromptTokensReported: true},
			wantInput: upstreamUsageIntPtr(20),
		},
		{
			// Protocols that do not report presence yet leave both fields out.
			name:    "enabled with presence not reported",
			enabled: true,
			usage:   providers.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "/v1/chat/completions"
			}
			router := &Router{includeUpstreamUsage: tt.enabled}
			accessCtx := accesslog.NewAccessLogContext("req", http.MethodPost, path, "HTTP/1.1", "model")
			router.recordUpstreamUsage(accessCtx, tt.usage)
			assert.Equal(t, tt.wantInput, accessCtx.UpstreamInputTokens)
			assert.Equal(t, tt.wantCached, accessCtx.CachedInputTokens)
		})
	}
}

// setupUpstreamUsagePool routes /v1 requests on Gateway default/gw to an
// InferencePool backed by backend, which goes through proxyModelEndpoint like
// ModelServer traffic. backendHandler serves every path, unlike
// setupTestRouter's backend, which answers paths other than Chat Completions
// and Responses itself.
func setupUpstreamUsagePool(t *testing.T, backendHandler http.Handler) (*Router, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	backend := httptest.NewServer(backendHandler)
	store := datastore.New()
	router := NewRouter(store, "../scheduler/testdata/configmap.yaml", common.NewTransportRegistry())
	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	backendPort, err := strconv.Atoi(backendURL.Port())
	require.NoError(t, err)

	pathType := gatewayv1.PathMatchPathPrefix
	prefix := "/v1"
	parentKind := gatewayv1.Kind("Gateway")
	backendGroup := inferencePoolBackendGroup
	backendKind := inferencePoolBackendKind
	require.NoError(t, store.AddOrUpdateInferencePool(&inferencev1.InferencePool{
		ObjectMeta: v1.ObjectMeta{Name: "usage-pool", Namespace: "default"},
		Spec: inferencev1.InferencePoolSpec{
			TargetPorts: []inferencev1.Port{{Number: inferencev1.PortNumber(backendPort)}},
			Selector: inferencev1.LabelSelector{MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
				"app": "usage-pool",
			}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{Name: "picker"},
		},
	}))
	require.NoError(t, store.AddOrUpdatePod(&corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "usage-pool-pod", Namespace: "default", Labels: map[string]string{"app": "usage-pool"}},
		Status:     corev1.PodStatus{PodIP: backendURL.Hostname(), Phase: corev1.PodRunning},
	}, nil))
	require.NoError(t, store.AddOrUpdateHTTPRoute(&gatewayv1.HTTPRoute{
		ObjectMeta: v1.ObjectMeta{Name: "usage-pool-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gw", Kind: &parentKind}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &prefix}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: &backendGroup, Kind: &backendKind, Name: "usage-pool",
				}}}},
			}},
		},
	}))
	return router, backend
}

func runUpstreamUsagePoolRequest(t *testing.T, router *Router, path, body string) *accesslog.AccessLogContext {
	t.Helper()
	w := connectors.CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(GatewayKey, "default/gw")
	accessCtx := accesslog.NewAccessLogContext("usage-pool", http.MethodPost, c.Request.URL.Path, c.Request.Proto, "")
	c.Set(accesslog.AccessLogContextKey, accessCtx)
	router.HandlerFunc()(c)
	assert.Equal(t, http.StatusOK, w.Code)
	return accessCtx
}

func TestUpstreamUsageAccessLogInCluster(t *testing.T) {
	const (
		cachedUsage   = `{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":16}}`
		zeroCached    = `{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":0}}`
		noDetails     = `{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}`
		contentChunk  = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
		doneChunk     = "data: [DONE]\n\n"
		streamRequest = `{"model":"pool-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
		bodyRequest   = `{"model":"pool-model","messages":[{"role":"user","content":"hi"}]}`
	)
	usageChunk := func(usage string) string {
		return "data: {\"choices\":[],\"usage\":" + usage + "}\n\n"
	}

	tests := []struct {
		name    string
		enabled bool
		request string
		// response is written by the backend; truncate declares a longer
		// Content-Length so the stream breaks off after response.
		response    string
		stream      bool
		truncate    bool
		wantInput   *int
		wantCached  *int
		wantOutputs int
	}{
		{name: "disabled body", request: bodyRequest, response: `{"usage":` + cachedUsage + `}`, wantOutputs: 5},
		{name: "disabled stream", request: streamRequest, response: contentChunk + usageChunk(cachedUsage) + doneChunk, stream: true, wantOutputs: 5},
		{
			name: "enabled body", enabled: true, request: bodyRequest, response: `{"usage":` + cachedUsage + `}`,
			wantInput: upstreamUsageIntPtr(20), wantCached: upstreamUsageIntPtr(16), wantOutputs: 5,
		},
		{
			name: "enabled body with explicit zero cached tokens", enabled: true, request: bodyRequest, response: `{"usage":` + zeroCached + `}`,
			wantInput: upstreamUsageIntPtr(20), wantCached: upstreamUsageIntPtr(0), wantOutputs: 5,
		},
		{
			name: "enabled body without cached tokens", enabled: true, request: bodyRequest, response: `{"usage":` + noDetails + `}`,
			wantInput: upstreamUsageIntPtr(20), wantOutputs: 5,
		},
		{name: "enabled body without usage", enabled: true, request: bodyRequest, response: `{"id":"no-usage"}`},
		{
			name: "enabled stream", enabled: true, request: streamRequest, response: contentChunk + usageChunk(cachedUsage) + doneChunk, stream: true,
			wantInput: upstreamUsageIntPtr(20), wantCached: upstreamUsageIntPtr(16), wantOutputs: 5,
		},
		{
			name: "enabled stream broken off after usage", enabled: true, request: streamRequest, response: contentChunk + usageChunk(cachedUsage),
			stream: true, truncate: true, wantInput: upstreamUsageIntPtr(20), wantCached: upstreamUsageIntPtr(16), wantOutputs: 5,
		},
		{
			name: "enabled stream broken off before usage", enabled: true, request: streamRequest, response: contentChunk,
			stream: true, truncate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var backendCalls atomic.Int32
			router, backend := setupUpstreamUsagePool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backendCalls.Add(1)
				if tt.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				if tt.truncate {
					w.Header().Set("Content-Length", strconv.Itoa(len(tt.response)+1024))
				}
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tt.response)
			}))
			defer backend.Close()
			router.includeUpstreamUsage = tt.enabled

			accessCtx := runUpstreamUsagePoolRequest(t, router, "/v1/chat/completions", tt.request)

			assert.Equal(t, int32(1), backendCalls.Load())
			assert.Equal(t, tt.wantInput, accessCtx.UpstreamInputTokens)
			assert.Equal(t, tt.wantCached, accessCtx.CachedInputTokens)
			assert.Equal(t, tt.wantOutputs, accessCtx.OutputTokens)
		})
	}
}

func TestUpstreamUsageAccessLogExternalProvider(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		wantInput  *int
		wantCached *int
	}{
		{name: "disabled"},
		{name: "enabled", enabled: true, wantInput: upstreamUsageIntPtr(3), wantCached: upstreamUsageIntPtr(2)},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"id":"chat-usage","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"prompt_tokens_details":{"cached_tokens":2}}}`)
			}))
			defer upstream.Close()

			fixture := newExternalObservabilityFixture(t, fmt.Sprintf("upstream-usage-%d", i), aiv1alpha1.OpenAI, upstream.URL)
			fixture.router.includeUpstreamUsage = tt.enabled
			requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)

			w, accessCtx := executeExternalObservabilityRequest(t, fixture, "/v1/chat/completions", requestBody)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, 5, accessCtx.OutputTokens)
			assert.Equal(t, tt.wantInput, accessCtx.UpstreamInputTokens)
			assert.Equal(t, tt.wantCached, accessCtx.CachedInputTokens)
		})
	}
}

// The in-cluster path parses every non-Responses /v1/* response with the OpenAI
// usage parser, so the fields must be limited by path, not by parser.
func TestUpstreamUsageAccessLogInClusterPathScope(t *testing.T) {
	const usage = `{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":16}}`
	tests := []struct {
		name       string
		path       string
		request    string
		response   string
		wantInput  *int
		wantCached *int
		wantOutput int
	}{
		{
			name: "completions", path: "/v1/completions",
			request: `{"model":"pool-model","prompt":"hi"}`, response: `{"usage":` + usage + `}`,
			wantInput: upstreamUsageIntPtr(20), wantCached: upstreamUsageIntPtr(16), wantOutput: 5,
		},
		{
			name: "embeddings", path: "/v1/embeddings",
			request:  `{"model":"pool-model","input":"hi"}`,
			response: `{"object":"list","data":[],"usage":{"prompt_tokens":20,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":16}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var backendCalls atomic.Int32
			router, backend := setupUpstreamUsagePool(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backendCalls.Add(1)
				assert.Equal(t, tt.path, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tt.response)
			}))
			defer backend.Close()
			router.includeUpstreamUsage = true

			accessCtx := runUpstreamUsagePoolRequest(t, router, tt.path, tt.request)

			assert.Equal(t, int32(1), backendCalls.Load(), "the request must reach the backend")
			assert.Equal(t, tt.wantInput, accessCtx.UpstreamInputTokens)
			assert.Equal(t, tt.wantCached, accessCtx.CachedInputTokens)
			assert.Equal(t, tt.wantOutput, accessCtx.OutputTokens)
		})
	}
}

// External streaming responses keep the same accounting with the switch on or
// off, and only the switch decides whether the upstream counts are logged.
func TestUpstreamUsageAccessLogExternalProviderStream(t *testing.T) {
	const (
		contentChunk = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
		usageChunk   = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n"
		doneChunk    = "data: [DONE]\n\n"
	)
	tests := []struct {
		name string
		// response is written by the upstream; truncate declares a longer
		// Content-Length so the stream breaks off after response.
		response      string
		truncate      bool
		wantErrorType string
		wantInput     *int
		wantCached    *int
		wantOutput    int
		// multipleReports marks a stream with more than one usage report.
		// Existing accounting adds every report to the output-token metric,
		// so for these only equality between switch settings is checked.
		multipleReports bool
	}{
		{
			name:      "complete stream",
			response:  contentChunk + usageChunk + doneChunk,
			wantInput: upstreamUsageIntPtr(3), wantCached: upstreamUsageIntPtr(2), wantOutput: 5,
		},
		{
			name:          "broken off after usage",
			response:      contentChunk + usageChunk,
			truncate:      true,
			wantErrorType: "response_forwarding",
			wantInput:     upstreamUsageIntPtr(3), wantCached: upstreamUsageIntPtr(2), wantOutput: 5,
		},
		{
			name:          "broken off before usage",
			response:      contentChunk,
			truncate:      true,
			wantErrorType: "response_forwarding",
		},
		{
			// With continuous usage reporting each chunk carries its own usage.
			// The logged pair comes from the last report, so a cached count
			// from an earlier report is not carried over.
			name: "later usage report without cached tokens",
			response: "data: {\"choices\":[{\"delta\":{\"content\":\"h\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"i\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
				doneChunk,
			wantInput: upstreamUsageIntPtr(3), wantOutput: 2, multipleReports: true,
		},
	}

	for i, tt := range tests {
		metricOutputDelta := map[bool]float64{}
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", tt.name, enabled), func(t *testing.T) {
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if tt.truncate {
						w.Header().Set("Content-Length", strconv.Itoa(len(tt.response)+1024))
					}
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, tt.response)
				}))
				defer upstream.Close()

				fixture := newExternalObservabilityFixture(t, fmt.Sprintf("upstream-usage-stream-%d-%t", i, enabled), aiv1alpha1.OpenAI, upstream.URL)
				fixture.router.includeUpstreamUsage = enabled
				path := "/v1/chat/completions"
				requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}],"stream":true}`, fixture.clientModel)
				metricErrorType := tt.wantErrorType
				if metricErrorType == "" {
					metricErrorType = "successful_request"
				}
				before := observeExternalMetrics(t, fixture, path, http.StatusOK, metricErrorType)

				w, accessCtx := executeExternalObservabilityStreamRequest(t, fixture, path, requestBody)

				assert.Equal(t, http.StatusOK, w.Code)
				assert.Contains(t, w.Body.String(), `"content":"`)
				if tt.wantErrorType == "" {
					assert.Nil(t, accessCtx.Error)
				} else if assert.NotNil(t, accessCtx.Error) {
					assert.Equal(t, tt.wantErrorType, accessCtx.Error.Type)
				}
				assert.Equal(t, tt.wantOutput, accessCtx.OutputTokens)
				if !tt.multipleReports {
					assertExternalMetricDeltas(t, fixture, path, http.StatusOK, metricErrorType, before, accessCtx.InputTokens, tt.wantOutput)
				}
				metricOutputDelta[enabled] = observeExternalMetrics(t, fixture, path, http.StatusOK, metricErrorType).outputTokens - before.outputTokens
				if enabled {
					assert.Equal(t, tt.wantInput, accessCtx.UpstreamInputTokens)
					assert.Equal(t, tt.wantCached, accessCtx.CachedInputTokens)
				} else {
					assert.Nil(t, accessCtx.UpstreamInputTokens)
					assert.Nil(t, accessCtx.CachedInputTokens)
				}
			})
		}
		assert.Equal(t, metricOutputDelta[false], metricOutputDelta[true],
			"%s: the switch must not change output-token accounting", tt.name)
	}
}
