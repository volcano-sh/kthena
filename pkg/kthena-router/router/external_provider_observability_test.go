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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

func TestExternalProviderObservabilitySuccess(t *testing.T) {
	tests := []struct {
		name             string
		providerType     aiv1alpha1.ExternalProviderType
		path             string
		requestBody      string
		responseBody     string
		wantOutputTokens int
	}{
		{
			name:             "openai chat completions",
			providerType:     aiv1alpha1.OpenAI,
			path:             "/v1/chat/completions",
			requestBody:      `{"messages":[{"role":"user","content":"hello"}]}`,
			responseBody:     `{"id":"chat-observability","usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
			wantOutputTokens: 5,
		},
		{
			name:             "openai responses",
			providerType:     aiv1alpha1.OpenAI,
			path:             "/v1/responses",
			requestBody:      `{"input":"hello","stream":false}`,
			responseBody:     `{"id":"resp-observability","object":"response","usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}`,
			wantOutputTokens: 3,
		},
		{
			name:             "anthropic messages",
			providerType:     aiv1alpha1.Anthropic,
			path:             "/v1/messages",
			requestBody:      `{"messages":[{"role":"user","content":"hello"}],"max_tokens":8}`,
			responseBody:     `{"id":"msg-observability","type":"message","usage":{"input_tokens":7,"output_tokens":9}}`,
			wantOutputTokens: 9,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suffix := fmt.Sprintf("success-%d", i)
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tt.responseBody)
			}))
			defer upstream.Close()

			fixture := newExternalObservabilityFixture(t, suffix, tt.providerType, upstream.URL)
			requestBody := addModelToRequestBody(tt.requestBody, fixture.clientModel)
			before := observeExternalMetrics(t, fixture, tt.path, http.StatusOK, "successful_request")

			w, accessCtx := executeExternalObservabilityRequest(t, fixture, tt.path, requestBody)

			assert.Equal(t, http.StatusOK, w.Code)
			assertExternalAccessLog(t, accessCtx, fixture, http.StatusOK, 1, http.StatusOK, "", tt.wantOutputTokens)
			assertExternalMetricDeltas(t, fixture, tt.path, http.StatusOK, "successful_request", before, accessCtx.InputTokens, tt.wantOutputTokens)
			assertExternalMetricsExposed(t, fixture, tt.path, http.StatusOK, "successful_request", accessCtx.InputTokens, tt.wantOutputTokens)
		})
	}
}

func TestExternalProviderObservabilityNonTextInputUsesLocalTextAccounting(t *testing.T) {
	tests := []struct {
		name             string
		providerType     aiv1alpha1.ExternalProviderType
		path             string
		requestBody      string
		responseBody     string
		wantOutputTokens int
	}{
		{
			name:             "openai chat image only",
			providerType:     aiv1alpha1.OpenAI,
			path:             "/v1/chat/completions",
			requestBody:      `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`,
			responseBody:     `{"id":"chat-non-text","usage":{"prompt_tokens":101,"completion_tokens":2,"total_tokens":103}}`,
			wantOutputTokens: 2,
		},
		{
			name:             "openai responses codex tool items only",
			providerType:     aiv1alpha1.OpenAI,
			path:             "/v1/responses",
			requestBody:      `{"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"shell"}]},{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}]}`,
			responseBody:     `{"id":"resp-non-text","object":"response","usage":{"input_tokens":202,"output_tokens":3,"total_tokens":205}}`,
			wantOutputTokens: 3,
		},
		{
			name:             "anthropic image only",
			providerType:     aiv1alpha1.Anthropic,
			path:             "/v1/messages",
			requestBody:      `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}],"max_tokens":8}`,
			responseBody:     `{"id":"msg-non-text","type":"message","usage":{"input_tokens":303,"output_tokens":4}}`,
			wantOutputTokens: 4,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamCalls := 0
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.responseBody)
			}))
			defer upstream.Close()

			fixture := newExternalObservabilityFixture(t, fmt.Sprintf("non-text-%d", i), tt.providerType, upstream.URL)
			requestBody := addModelToRequestBody(tt.requestBody, fixture.clientModel)
			before := observeExternalMetrics(t, fixture, tt.path, http.StatusOK, "successful_request")

			w, accessCtx := executeExternalObservabilityRequest(t, fixture, tt.path, requestBody)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, 1, upstreamCalls)
			assert.Zero(t, accessCtx.InputTokens)
			assertExternalAccessLog(t, accessCtx, fixture, http.StatusOK, 1, http.StatusOK, "", tt.wantOutputTokens)
			assertExternalMetricDeltas(t, fixture, tt.path, http.StatusOK, "successful_request", before, 0, tt.wantOutputTokens)
			assertExternalMetricsExposed(t, fixture, tt.path, http.StatusOK, "successful_request", 0, tt.wantOutputTokens)
		})
	}
}

func TestExternalProviderObservabilityStreamingResponses(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.created\n")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-observability-stream\"}}\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-observability-stream\",\"usage\":{\"input_tokens\":12,\"output_tokens\":4,\"total_tokens\":16}}}\n\n")
	}))
	defer upstream.Close()

	fixture := newExternalObservabilityFixture(t, "responses-stream", aiv1alpha1.OpenAI, upstream.URL)
	requestBody := addModelToRequestBody(`{"input":"hello","stream":true}`, fixture.clientModel)
	before := observeExternalMetrics(t, fixture, "/v1/responses", http.StatusOK, "successful_request")

	w, accessCtx := executeExternalObservabilityStreamRequest(t, fixture, "/v1/responses", requestBody)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "response.completed")
	assertExternalAccessLog(t, accessCtx, fixture, http.StatusOK, 1, http.StatusOK, "", 4)
	assertExternalMetricDeltas(t, fixture, "/v1/responses", http.StatusOK, "successful_request", before, accessCtx.InputTokens, 4)
}

func TestExternalProviderObservabilityStreamingUsageProtocols(t *testing.T) {
	tests := []struct {
		name             string
		providerType     aiv1alpha1.ExternalProviderType
		path             string
		requestBody      string
		responseBody     string
		responseMarker   string
		wantOutputTokens int
	}{
		{
			name:             "openai chat completions",
			providerType:     aiv1alpha1.OpenAI,
			path:             "/v1/chat/completions",
			requestBody:      `{"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			responseBody:     "data: {\"id\":\"chat-stream\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17}}\n\ndata: [DONE]\n\n",
			responseMarker:   "data: [DONE]",
			wantOutputTokens: 5,
		},
		{
			name:         "anthropic messages",
			providerType: aiv1alpha1.Anthropic,
			path:         "/v1/messages",
			requestBody:  `{"messages":[{"role":"user","content":"hello"}],"max_tokens":8,"stream":true}`,
			responseBody: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":13,\"output_tokens\":1}}}\n\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
			responseMarker:   `"type":"message_stop"`,
			wantOutputTokens: 6,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tt.responseBody)
			}))
			defer upstream.Close()

			fixture := newExternalObservabilityFixture(t, fmt.Sprintf("stream-protocol-%d", i), tt.providerType, upstream.URL)
			requestBody := addModelToRequestBody(tt.requestBody, fixture.clientModel)
			before := observeExternalMetrics(t, fixture, tt.path, http.StatusOK, "successful_request")

			w, accessCtx := executeExternalObservabilityStreamRequest(t, fixture, tt.path, requestBody)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), tt.responseMarker)
			assertExternalAccessLog(t, accessCtx, fixture, http.StatusOK, 1, http.StatusOK, "", tt.wantOutputTokens)
			assertExternalMetricDeltas(t, fixture, tt.path, http.StatusOK, "successful_request", before, accessCtx.InputTokens, tt.wantOutputTokens)
			assertExternalMetricsExposed(t, fixture, tt.path, http.StatusOK, "successful_request", accessCtx.InputTokens, tt.wantOutputTokens)
		})
	}
}

func TestForwardResponsesStreamTreatsCancellationAfterTerminalEventAsComplete(t *testing.T) {
	w := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closeCh: make(chan bool)}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &payloadThenErrorReadCloser{
			payload: []byte("event: response.completed\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":4,\"total_tokens\":16}}}\n\n"),
			err: context.Canceled,
		},
	}

	var usage TokenUsage
	err := forwardResponseWithUsageParser(c, resp, true, &openAIResponsesUsageParser{}, func(got TokenUsage) {
		usage = got
	})

	require.NoError(t, err)
	assert.Contains(t, w.Body.String(), `"type":"response.completed"`)
	assert.Equal(t, TokenUsage{PromptTokens: 12, CompletionTokens: 4, TotalTokens: 16}, usage)
}

func TestForwardResponsesStreamReportsCancellationBeforeTerminalEvent(t *testing.T) {
	w := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closeCh: make(chan bool)}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &payloadThenErrorReadCloser{
			payload: []byte("event: response.created\n" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-cancelled\"}}\n\n"),
			err: context.Canceled,
		},
	}

	err := forwardResponseWithUsageParser(c, resp, true, &openAIResponsesUsageParser{}, nil)

	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, w.Body.String(), `"type":"response.created"`)
}

func TestForwardResponsesStreamReportsCloseNotifyBeforeTerminalEvent(t *testing.T) {
	w := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closeCh: make(chan bool)}
	close(w.closeCh)
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       http.NoBody,
	}

	err := forwardResponseWithUsageParser(c, resp, true, &openAIResponsesUsageParser{}, nil)

	require.ErrorIs(t, err, context.Canceled)
}

func TestForwardResponsesStreamReportsTerminalWriteFailure(t *testing.T) {
	writeErr := errors.New("terminal write failed")
	base := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closeCh: make(chan bool)}
	w := &terminalWriteErrorRecorder{closeNotifyRecorder: base, err: writeErr}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &payloadThenErrorReadCloser{
			payload: []byte("event: response.completed\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":4,\"total_tokens\":16}}}\n\n"),
			err: context.Canceled,
		},
	}

	err := forwardResponseWithUsageParser(c, resp, true, &openAIResponsesUsageParser{}, nil)

	require.ErrorIs(t, err, writeErr)
}

func TestForwardProviderStreamsTreatCloseNotifyAfterTerminalEventAsComplete(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		marker string
		parser responseUsageParser
	}{
		{
			name:   "openai chat done",
			body:   "data: [DONE]\n\n",
			marker: "data: [DONE]",
			parser: &openAIUsageParser{},
		},
		{
			name:   "openai responses completed",
			body:   "data: {\"type\":\"response.completed\",\"response\":{}}\n\n",
			marker: `"type":"response.completed"`,
			parser: &openAIResponsesUsageParser{},
		},
		{
			name:   "anthropic message stop",
			body:   "data: {\"type\":\"message_stop\"}\n\n",
			marker: `"type":"message_stop"`,
			parser: &anthropicUsageParser{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closeCh: make(chan bool)}
			w := &closeAfterMarkerRecorder{closeNotifyRecorder: base, marker: []byte(tt.marker)}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewBufferString(tt.body)),
			}

			err := forwardResponseWithUsageParser(c, resp, true, tt.parser, nil)

			require.NoError(t, err)
		})
	}
}

type terminalWriteErrorRecorder struct {
	*closeNotifyRecorder
	err error
}

type closeAfterMarkerRecorder struct {
	*closeNotifyRecorder
	marker []byte
	closed bool
}

func (r *closeAfterMarkerRecorder) Write(p []byte) (int, error) {
	n, err := r.closeNotifyRecorder.Write(p)
	if err == nil && n == len(p) && !r.closed && bytes.Contains(p, r.marker) {
		close(r.closeCh)
		r.closed = true
	}
	return n, err
}

func (r *terminalWriteErrorRecorder) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"type":"response.completed"`)) {
		return 0, r.err
	}
	return r.closeNotifyRecorder.Write(p)
}

type payloadThenErrorReadCloser struct {
	payload []byte
	err     error
}

func (r *payloadThenErrorReadCloser) Read(p []byte) (int, error) {
	if len(r.payload) == 0 {
		return 0, r.err
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, nil
}

func (*payloadThenErrorReadCloser) Close() error {
	return nil
}

var _ io.ReadCloser = (*payloadThenErrorReadCloser)(nil)

func TestExternalProviderObservabilityUpstreamResponse(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer upstream.Close()

	fixture := newExternalObservabilityFixture(t, "upstream-429", aiv1alpha1.OpenAI, upstream.URL)
	path := "/v1/chat/completions"
	requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)
	before := observeExternalMetrics(t, fixture, path, http.StatusTooManyRequests, "upstream_response")

	w, accessCtx := executeExternalObservabilityRequest(t, fixture, path, requestBody)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.JSONEq(t, `{"error":"rate limited"}`, w.Body.String())
	assertExternalAccessLog(t, accessCtx, fixture, http.StatusTooManyRequests, 1, http.StatusTooManyRequests, "upstream", 0)
	assertExternalMetricDeltas(t, fixture, path, http.StatusTooManyRequests, "upstream_response", before, accessCtx.InputTokens, 0)
	assertExternalMetricsExposed(t, fixture, path, http.StatusTooManyRequests, "upstream_response", accessCtx.InputTokens, 0)
}

func TestExternalProviderObservabilityTransportFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := upstream.URL
	upstream.Close()

	fixture := newExternalObservabilityFixture(t, "transport-failure", aiv1alpha1.OpenAI, baseURL)
	path := "/v1/chat/completions"
	requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)
	before := observeExternalMetrics(t, fixture, path, http.StatusBadGateway, "upstream_transport")

	w, accessCtx := executeExternalObservabilityRequest(t, fixture, path, requestBody)

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), baseURL)
	assert.NotContains(t, w.Body.String(), "provider-key")
	assertExternalAccessLog(t, accessCtx, fixture, http.StatusBadGateway, 1, 0, "router", 0)
	require.NotNil(t, accessCtx.Error)
	assert.NotContains(t, accessCtx.Error.Message, baseURL)
	assert.NotContains(t, accessCtx.Error.Message, "provider-key")
	assertExternalMetricDeltas(t, fixture, path, http.StatusBadGateway, "upstream_transport", before, accessCtx.InputTokens, 0)
	assertExternalMetricsExposed(t, fixture, path, http.StatusBadGateway, "upstream_transport", accessCtx.InputTokens, 0)
}

func TestExternalProviderObservabilityStreamReadFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.created\n")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"truncated\"}}\n\n")
	}))
	defer upstream.Close()

	fixture := newExternalObservabilityFixture(t, "stream-read-failure", aiv1alpha1.OpenAI, upstream.URL)
	path := "/v1/responses"
	requestBody := addModelToRequestBody(`{"input":"hello","stream":true}`, fixture.clientModel)
	before := observeExternalMetrics(t, fixture, path, http.StatusOK, "proxy")

	w, accessCtx := executeExternalObservabilityStreamRequest(t, fixture, path, requestBody)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"type":"response.created"`)
	assertExternalAccessLog(t, accessCtx, fixture, http.StatusOK, 1, http.StatusOK, "router", 0)
	require.NotNil(t, accessCtx.Error)
	assert.Equal(t, "proxy", accessCtx.Error.Type)
	assertExternalMetricDeltas(t, fixture, path, http.StatusOK, "proxy", before, accessCtx.InputTokens, 0)
	assertExternalMetricsExposed(t, fixture, path, http.StatusOK, "proxy", accessCtx.InputTokens, 0)
}

func TestExternalProviderObservabilityConfigurationFailure(t *testing.T) {
	fixture := newExternalObservabilityFixture(t, "missing-secret", aiv1alpha1.OpenAI, "https://provider.invalid/v1")
	require.NoError(t, fixture.store.DeleteSecret(types.NamespacedName{Namespace: "default", Name: fixture.secretName}))
	path := "/v1/chat/completions"
	requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)
	before := observeExternalMetrics(t, fixture, path, http.StatusServiceUnavailable, "provider_config")

	w, accessCtx := executeExternalObservabilityRequest(t, fixture, path, requestBody)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), fixture.secretName)
	assert.NotContains(t, w.Body.String(), "api-key")
	assertExternalAccessLog(t, accessCtx, fixture, http.StatusServiceUnavailable, 0, 0, "router", 0)
	require.NotNil(t, accessCtx.Error)
	assert.Equal(t, "provider_config", accessCtx.Error.Type)
	assert.NotContains(t, accessCtx.Error.Message, fixture.secretName)
	assert.NotContains(t, accessCtx.Error.Message, "api-key")
	assertExternalMetricDeltas(t, fixture, path, http.StatusServiceUnavailable, "provider_config", before, accessCtx.InputTokens, 0)
	assertExternalMetricsExposed(t, fixture, path, http.StatusServiceUnavailable, "provider_config", accessCtx.InputTokens, 0)
}

func TestExternalProviderObservabilityDiscoveryFailure(t *testing.T) {
	const suffix = "discovery-failure"
	fixture := newExternalObservabilityFixture(t, suffix, aiv1alpha1.OpenAI, "https://provider.invalid/v1")
	require.NoError(t, fixture.store.DeleteExternalModelProvider(types.NamespacedName{
		Namespace: "default",
		Name:      "observability-provider-" + suffix,
	}))
	path := "/v1/chat/completions"
	requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)
	destination := []string{metrics.DestinationLabelValueNone, metrics.BackendTypeUnresolved, metrics.DestinationLabelValueNone, metrics.DestinationLabelValueNone}
	requestBefore := externalCounterValue(t, &fixture.router.metrics.RequestsTotal,
		append([]string{fixture.clientModel, path, "404", "provider_discovery"}, destination...)...)
	durationBefore := externalHistogramCount(t, &fixture.router.metrics.RequestDuration,
		append([]string{fixture.clientModel, path, "404"}, destination...)...)
	inputBefore := externalCounterValue(t, &fixture.router.metrics.TokensTotal,
		append([]string{fixture.clientModel, path, metrics.TokenTypeInput}, destination...)...)

	w, accessCtx := executeExternalObservabilityRequest(t, fixture, path, requestBody)

	assert.Equal(t, http.StatusNotFound, w.Code)
	require.NotNil(t, accessCtx.Error)
	assert.Equal(t, "provider_discovery", accessCtx.Error.Type)
	assert.Equal(t, "router", accessCtx.ErrorOrigin)
	assert.Equal(t, requestBefore+1, externalCounterValue(t, &fixture.router.metrics.RequestsTotal,
		append([]string{fixture.clientModel, path, "404", "provider_discovery"}, destination...)...))
	assert.Equal(t, durationBefore+1, externalHistogramCount(t, &fixture.router.metrics.RequestDuration,
		append([]string{fixture.clientModel, path, "404"}, destination...)...))
	assert.Equal(t, inputBefore+float64(accessCtx.InputTokens), externalCounterValue(t, &fixture.router.metrics.TokensTotal,
		append([]string{fixture.clientModel, path, metrics.TokenTypeInput}, destination...)...))
}

func TestExternalProviderObservabilityProtocolFailure(t *testing.T) {
	fixture := newExternalObservabilityFixture(t, "protocol-mismatch", aiv1alpha1.Anthropic, "https://provider.invalid")
	path := "/v1/chat/completions"
	requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)
	before := observeExternalMetrics(t, fixture, path, http.StatusBadRequest, "request_protocol")

	w, accessCtx := executeExternalObservabilityRequest(t, fixture, path, requestBody)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.NotContains(t, w.Body.String(), "provider-key")
	assertExternalAccessLog(t, accessCtx, fixture, http.StatusBadRequest, 0, 0, "router", 0)
	require.NotNil(t, accessCtx.Error)
	assert.Equal(t, "request_protocol", accessCtx.Error.Type)
	assert.NotContains(t, accessCtx.Error.Message, "provider-key")
	assertExternalMetricDeltas(t, fixture, path, http.StatusBadRequest, "request_protocol", before, accessCtx.InputTokens, 0)
	assertExternalMetricsExposed(t, fixture, path, http.StatusBadRequest, "request_protocol", accessCtx.InputTokens, 0)
}

func TestExternalProviderActiveMetricsTrackInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"active-observability","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	fixture := newExternalObservabilityFixture(t, "active", aiv1alpha1.OpenAI, upstream.URL)
	path := "/v1/chat/completions"
	requestBody := addModelToRequestBody(`{"messages":[{"role":"user","content":"hello"}]}`, fixture.clientModel)
	activeUpstreamBefore := externalGaugeValue(t, &fixture.router.metrics.ActiveUpstreamRequests, fixture.activeLabels()...)
	activeDownstreamBefore := externalGaugeValue(t, &fixture.router.metrics.ActiveDownstreamRequests, fixture.clientModel)
	activeRequestsBefore := fixture.router.metrics.ActiveRequestsCount()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w, _ := executeExternalObservabilityRequest(t, fixture, path, requestBody)
		done <- w
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for external upstream request")
	}
	assert.Equal(t, activeUpstreamBefore+1, externalGaugeValue(t, &fixture.router.metrics.ActiveUpstreamRequests, fixture.activeLabels()...))
	assert.Equal(t, activeDownstreamBefore+1, externalGaugeValue(t, &fixture.router.metrics.ActiveDownstreamRequests, fixture.clientModel))
	assert.Equal(t, activeRequestsBefore+1, fixture.router.metrics.ActiveRequestsCount())

	close(release)
	select {
	case w := <-done:
		assert.Equal(t, http.StatusOK, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for external request completion")
	}
	assert.Equal(t, activeUpstreamBefore, externalGaugeValue(t, &fixture.router.metrics.ActiveUpstreamRequests, fixture.activeLabels()...))
	assert.Equal(t, activeDownstreamBefore, externalGaugeValue(t, &fixture.router.metrics.ActiveDownstreamRequests, fixture.clientModel))
	assert.Equal(t, activeRequestsBefore, fixture.router.metrics.ActiveRequestsCount())
}

type externalObservabilityFixture struct {
	router        *Router
	store         datastore.Store
	clientModel   string
	providerModel string
	modelRoute    string
	backendName   string
	secretName    string
}

func newExternalObservabilityFixture(t *testing.T, suffix string, providerType aiv1alpha1.ExternalProviderType, baseURL string) externalObservabilityFixture {
	t.Helper()
	store := datastore.New()
	router := NewRouter(store, "../scheduler/testdata/configmap.yaml")
	clientModel := "observability-client-" + suffix
	providerModel := "observability-upstream-" + suffix
	providerName := "observability-provider-" + suffix
	secretName := "observability-secret-" + suffix
	routeName := "observability-route-" + suffix
	headers := map[string]string{}
	if providerType == aiv1alpha1.Anthropic {
		headers["anthropic-version"] = "2023-06-01"
	}

	require.NoError(t, store.AddOrUpdateExternalModelProvider(&aiv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: "default"},
		Spec: aiv1alpha1.ExternalModelProviderSpec{
			ProviderType:       providerType,
			BaseURL:            baseURL,
			Model:              &providerModel,
			InsecureSkipVerify: true,
			Auth: &aiv1alpha1.ProviderAuth{SecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "api-key",
			}},
			Headers: headers,
		},
	}))
	require.NoError(t, store.AddOrUpdateSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("provider-key")},
	}))
	require.NoError(t, store.AddOrUpdateModelRoute(&aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: clientModel,
			Rules: []*aiv1alpha1.Rule{{TargetModels: []*aiv1alpha1.TargetModel{{
				ExternalModelProviderName: providerName,
			}}}},
		},
	}))

	return externalObservabilityFixture{
		router:        router,
		store:         store,
		clientModel:   clientModel,
		providerModel: providerModel,
		modelRoute:    "default/" + routeName,
		backendName:   "default/" + providerName,
		secretName:    secretName,
	}
}

func (f externalObservabilityFixture) destinationLabels() []string {
	return []string{f.modelRoute, metrics.BackendTypeExternalProvider, f.backendName, f.providerModel}
}

func (f externalObservabilityFixture) activeLabels() []string {
	return []string{metrics.DestinationLabelValueNone, f.modelRoute, metrics.BackendTypeExternalProvider, f.backendName, f.providerModel}
}

func addModelToRequestBody(body, model string) string {
	return fmt.Sprintf(`{"model":%q,%s`, model, body[1:])
}

func executeExternalObservabilityRequest(t *testing.T, fixture externalObservabilityFixture, path, body string) (*httptest.ResponseRecorder, *accesslog.AccessLogContext) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	accessCtx := accesslog.NewAccessLogContext("external-observability", http.MethodPost, path, c.Request.Proto, fixture.clientModel)
	c.Set(accesslog.AccessLogContextKey, accessCtx)
	fixture.router.HandlerFunc()(c)
	return w, accessCtx
}

func executeExternalObservabilityStreamRequest(t *testing.T, fixture externalObservabilityFixture, path, body string) (*closeNotifyRecorder, *accesslog.AccessLogContext) {
	t.Helper()
	w := &closeNotifyRecorder{ResponseRecorder: httptest.NewRecorder(), closeCh: make(chan bool)}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	accessCtx := accesslog.NewAccessLogContext("external-observability-stream", http.MethodPost, path, c.Request.Proto, fixture.clientModel)
	c.Set(accesslog.AccessLogContextKey, accessCtx)
	fixture.router.HandlerFunc()(c)
	return w, accessCtx
}

type externalMetricSnapshot struct {
	requests       float64
	durationCount  uint64
	inputTokens    float64
	outputTokens   float64
	activeUpstream float64
	activeDown     float64
	activeRequests int64
}

func observeExternalMetrics(t *testing.T, fixture externalObservabilityFixture, path string, statusCode int, errorType string) externalMetricSnapshot {
	t.Helper()
	status := fmt.Sprint(statusCode)
	destination := fixture.destinationLabels()
	return externalMetricSnapshot{
		requests: externalCounterValue(t, &fixture.router.metrics.RequestsTotal,
			append([]string{fixture.clientModel, path, status, errorType}, destination...)...),
		durationCount: externalHistogramCount(t, &fixture.router.metrics.RequestDuration,
			append([]string{fixture.clientModel, path, status}, destination...)...),
		inputTokens: externalCounterValue(t, &fixture.router.metrics.TokensTotal,
			append([]string{fixture.clientModel, path, metrics.TokenTypeInput}, destination...)...),
		outputTokens: externalCounterValue(t, &fixture.router.metrics.TokensTotal,
			append([]string{fixture.clientModel, path, metrics.TokenTypeOutput}, destination...)...),
		activeUpstream: externalGaugeValue(t, &fixture.router.metrics.ActiveUpstreamRequests, fixture.activeLabels()...),
		activeDown:     externalGaugeValue(t, &fixture.router.metrics.ActiveDownstreamRequests, fixture.clientModel),
		activeRequests: fixture.router.metrics.ActiveRequestsCount(),
	}
}

func assertExternalMetricDeltas(t *testing.T, fixture externalObservabilityFixture, path string, statusCode int, errorType string, before externalMetricSnapshot, inputTokens, outputTokens int) {
	t.Helper()
	after := observeExternalMetrics(t, fixture, path, statusCode, errorType)
	assert.Equal(t, float64(1), after.requests-before.requests)
	assert.Equal(t, uint64(1), after.durationCount-before.durationCount)
	assert.Equal(t, float64(inputTokens), after.inputTokens-before.inputTokens)
	assert.Equal(t, float64(outputTokens), after.outputTokens-before.outputTokens)
	assert.Equal(t, before.activeUpstream, after.activeUpstream)
	assert.Equal(t, before.activeDown, after.activeDown)
	assert.Equal(t, before.activeRequests, after.activeRequests)
}

func assertExternalMetricsExposed(t *testing.T, fixture externalObservabilityFixture, path string, statusCode int, errorType string, inputTokens, outputTokens int) {
	t.Helper()
	requestLabels := map[string]string{
		"model": fixture.clientModel, "path": path, "status_code": fmt.Sprint(statusCode), "error_type": errorType,
		"model_route": fixture.modelRoute, "backend_type": metrics.BackendTypeExternalProvider,
		"backend_name": fixture.backendName, "upstream_model": fixture.providerModel,
	}
	assert.Equal(t, float64(1), gatheredExternalCounter(t, "kthena_router_requests_total", requestLabels))
	tokenLabels := map[string]string{
		"model": fixture.clientModel, "path": path, "model_route": fixture.modelRoute,
		"backend_type": metrics.BackendTypeExternalProvider, "backend_name": fixture.backendName,
		"upstream_model": fixture.providerModel,
	}
	tokenLabels["token_type"] = metrics.TokenTypeInput
	assert.Equal(t, float64(inputTokens), gatheredExternalCounter(t, "kthena_router_tokens_total", tokenLabels))
	tokenLabels["token_type"] = metrics.TokenTypeOutput
	assert.Equal(t, float64(outputTokens), gatheredExternalCounter(t, "kthena_router_tokens_total", tokenLabels))
}

func assertExternalAccessLog(t *testing.T, accessCtx *accesslog.AccessLogContext, fixture externalObservabilityFixture, statusCode, attempts, upstreamStatus int, errorOrigin string, outputTokens int) {
	t.Helper()
	entry := accessCtx.ToAccessLogEntry(statusCode)
	assert.Equal(t, fixture.clientModel, entry.ModelName)
	assert.Equal(t, fixture.modelRoute, entry.ModelRoute)
	assert.Equal(t, metrics.BackendTypeExternalProvider, entry.BackendType)
	assert.Equal(t, fixture.backendName, entry.BackendName)
	assert.Equal(t, fixture.providerModel, entry.UpstreamModel)
	assert.Equal(t, upstreamStatus, entry.UpstreamStatusCode)
	assert.Equal(t, attempts, entry.UpstreamAttempts)
	assert.Equal(t, errorOrigin, entry.ErrorOrigin)
	assert.Equal(t, accessCtx.InputTokens, entry.InputTokens)
	assert.Equal(t, outputTokens, entry.OutputTokens)
	assert.GreaterOrEqual(t, entry.DurationTotal, int64(0))
	assert.GreaterOrEqual(t, entry.DurationRequestProcessing, int64(0))
	assert.GreaterOrEqual(t, entry.DurationUpstreamProcessing, int64(0))
	assert.GreaterOrEqual(t, entry.DurationResponseProcessing, int64(0))
}

func externalCounterValue(t *testing.T, counter *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	metric, err := counter.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	var value dto.Metric
	require.NoError(t, metric.Write(&value))
	return value.GetCounter().GetValue()
}

func externalGaugeValue(t *testing.T, gauge *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	metric, err := gauge.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	var value dto.Metric
	require.NoError(t, metric.Write(&value))
	return value.GetGauge().GetValue()
}

func externalHistogramCount(t *testing.T, histogram *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	observer, err := histogram.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	metric, ok := observer.(prometheus.Metric)
	require.True(t, ok)
	var value dto.Metric
	require.NoError(t, metric.Write(&value))
	return value.GetHistogram().GetSampleCount()
}

func gatheredExternalCounter(t *testing.T, metricName string, wantLabels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			matches := true
			for name, value := range wantLabels {
				if labels[name] != value {
					matches = false
					break
				}
			}
			if matches {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("metric %s with labels %v was not gathered", metricName, wantLabels)
	return 0
}
