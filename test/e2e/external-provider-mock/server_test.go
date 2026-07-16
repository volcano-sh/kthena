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

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMockServer() *mockServer {
	return newMockServer(mockConfig{
		OpenAIKey:    "e2e-openai-key",
		AnthropicKey: "e2e-anthropic-key",
		MaxCaptures:  2,
		MaxDelay:     2 * time.Second,
	})
}

func TestOpenAIChatCapturesAuthorizedRequest(t *testing.T) {
	server := newTestMockServer()
	body := `{"model":"mock-openai-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer e2e-openai-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "chat-1")
	recorder := httptest.NewRecorder()

	server.providerHandler().ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"id":"chat-mock","object":"chat.completion","model":"mock-openai-chat","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`, recorder.Body.String())
	capture, ok := server.captures.get("chat-1")
	require.True(t, ok)
	assert.True(t, capture.Authorized)
	assert.Equal(t, "/v1/chat/completions", capture.Path)
	assert.JSONEq(t, body, string(capture.Body))
}

func TestOpenAIChatRejectsStreamingOptionsForNonStreamingRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level include_usage",
			body: `{"model":"mock-openai-chat","stream":false,"include_usage":true}`,
		},
		{
			name: "stream_options",
			body: `{"model":"mock-openai-chat","stream":false,"stream_options":{"include_usage":true}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestMockServer()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer e2e-openai-key")
			req.Header.Set("X-Request-Id", "non-streaming-options")
			recorder := httptest.NewRecorder()

			server.providerHandler().ServeHTTP(recorder, req)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Contains(t, recorder.Body.String(), "streaming-only option")
		})
	}
}

func TestProviderRejectsWrongAuthentication(t *testing.T) {
	server := newTestMockServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("X-Request-Id", "unauthorized-1")
	recorder := httptest.NewRecorder()

	server.providerHandler().ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "e2e-openai-key")
}

func TestCaptureStoreEvictsOldestRecord(t *testing.T) {
	store := newCaptureStore(2)
	store.put(capturedRequest{RequestID: "one"})
	store.put(capturedRequest{RequestID: "two"})
	store.put(capturedRequest{RequestID: "three"})

	_, oneExists := store.get("one")
	_, twoExists := store.get("two")
	_, threeExists := store.get("three")
	assert.False(t, oneExists)
	assert.True(t, twoExists)
	assert.True(t, threeExists)
}

func TestCaptureStoreReplacesAndCopiesRecord(t *testing.T) {
	store := newCaptureStore(2)
	body := json.RawMessage(`{"value":1}`)
	store.put(capturedRequest{RequestID: "same", Body: body})
	body[0] = '['
	store.put(capturedRequest{RequestID: "same", Body: json.RawMessage(`{"value":2}`)})
	store.put(capturedRequest{RequestID: "other"})

	capture, ok := store.get("same")
	require.True(t, ok)
	assert.JSONEq(t, `{"value":2}`, string(capture.Body))
	capture.Body[0] = '['
	again, ok := store.get("same")
	require.True(t, ok)
	assert.JSONEq(t, `{"value":2}`, string(again.Body))
}

func TestHealthEndpoint(t *testing.T) {
	server := newTestMockServer()
	recorder := httptest.NewRecorder()

	server.adminHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "ok\n", recorder.Body.String())
}

func TestAdminCaptureLifecycle(t *testing.T) {
	server := newTestMockServer()
	server.captures.put(capturedRequest{RequestID: "capture-1", Method: http.MethodPost, Path: "/v1/responses", Body: json.RawMessage(`{"model":"m"}`)})

	getRecorder := httptest.NewRecorder()
	server.adminHandler().ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/__test/requests/capture-1", nil))
	require.Equal(t, http.StatusOK, getRecorder.Code)
	var got capturedRequest
	require.NoError(t, json.Unmarshal(getRecorder.Body.Bytes(), &got))
	assert.Equal(t, "capture-1", got.RequestID)
	assert.JSONEq(t, `{"model":"m"}`, string(got.Body))

	deleteRecorder := httptest.NewRecorder()
	server.adminHandler().ServeHTTP(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/__test/requests/capture-1", nil))
	assert.Equal(t, http.StatusNoContent, deleteRecorder.Code)

	missingRecorder := httptest.NewRecorder()
	server.adminHandler().ServeHTTP(missingRecorder, httptest.NewRequest(http.MethodGet, "/__test/requests/capture-1", nil))
	assert.Equal(t, http.StatusNotFound, missingRecorder.Code)
}

func TestProviderRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		requestID  string
		wantStatus int
	}{
		{name: "method", method: http.MethodGet, body: `{}`, requestID: "method-1", wantStatus: http.StatusMethodNotAllowed},
		{name: "missing request id", method: http.MethodPost, body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "malformed json", method: http.MethodPost, body: `{`, requestID: "malformed-1", wantStatus: http.StatusBadRequest},
		{name: "multiple json values", method: http.MethodPost, body: `{} {}`, requestID: "multiple-1", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestMockServer()
			req := httptest.NewRequest(tt.method, "/v1/chat/completions", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer e2e-openai-key")
			req.Header.Set("X-Request-Id", tt.requestID)
			recorder := httptest.NewRecorder()

			server.providerHandler().ServeHTTP(recorder, req)

			assert.Equal(t, tt.wantStatus, recorder.Code)
		})
	}
}

func TestStreamingProviderProtocols(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		headers     map[string]string
		body        string
		wantMarkers []string
	}{
		{
			name:    "chat completions",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearer e2e-openai-key"},
			body:    `{"model":"mock-openai-chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			wantMarkers: []string{
				`"object":"chat.completion.chunk"`,
				`"finish_reason":"stop"`,
				`"prompt_tokens":11`,
				`"completion_tokens":5`,
				`data: [DONE]`,
			},
		},
		{
			name:    "responses",
			path:    "/v1/responses",
			headers: map[string]string{"Authorization": "Bearer e2e-openai-key"},
			body:    `{"model":"mock-responses","stream":true,"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"shell"}]},{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}]}`,
			wantMarkers: []string{
				`"type":"response.output_text.delta"`,
				`"type":"response.completed"`,
				`"input_tokens":12`,
				`"output_tokens":4`,
			},
		},
		{
			name: "anthropic",
			path: "/v1/messages",
			headers: map[string]string{
				"X-Api-Key":         "e2e-anthropic-key",
				"Anthropic-Version": "2023-06-01",
			},
			body: `{"model":"mock-anthropic","stream":true,"max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}}]}]}`,
			wantMarkers: []string{
				`"type":"message_start"`,
				`"input_tokens":13`,
				`"type":"message_delta"`,
				`"output_tokens":6`,
				`"type":"message_stop"`,
			},
		},
	}

	server := newTestMockServer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID := tt.name + "-1"
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-Id", requestID)
			for key, value := range tt.headers {
				req.Header.Set(key, value)
			}
			recorder := httptest.NewRecorder()

			server.providerHandler().ServeHTTP(recorder, req)

			require.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			for _, marker := range tt.wantMarkers {
				assert.Contains(t, recorder.Body.String(), marker)
			}
			capture, ok := server.captures.get(requestID)
			require.True(t, ok)
			assert.True(t, capture.Authorized)
			assert.JSONEq(t, tt.body, string(capture.Body))
		})
	}
}

func TestMockStatusControl(t *testing.T) {
	server := newTestMockServer()

	t.Run("rate limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?mock_status=429", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer e2e-openai-key")
		req.Header.Set("X-Request-Id", "rate-limit-1")
		recorder := httptest.NewRecorder()

		server.providerHandler().ServeHTTP(recorder, req)

		assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
		assert.JSONEq(t, `{"error":{"type":"rate_limit_error","message":"mock rate limit"}}`, recorder.Body.String())
	})

	t.Run("unsupported status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?mock_status=500", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer e2e-openai-key")
		req.Header.Set("X-Request-Id", "bad-status-1")
		recorder := httptest.NewRecorder()

		server.providerHandler().ServeHTTP(recorder, req)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})
}

func TestMockDelayControl(t *testing.T) {
	server := newTestMockServer()

	t.Run("bounded delay", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?mock_delay_ms=50", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer e2e-openai-key")
		req.Header.Set("X-Request-Id", "delay-1")
		recorder := httptest.NewRecorder()
		started := time.Now()

		server.providerHandler().ServeHTTP(recorder, req)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.GreaterOrEqual(t, time.Since(started), 50*time.Millisecond)
	})

	t.Run("delay above limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?mock_delay_ms=2001", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer e2e-openai-key")
		req.Header.Set("X-Request-Id", "delay-limit-1")
		recorder := httptest.NewRecorder()
		started := time.Now()

		server.providerHandler().ServeHTTP(recorder, req)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.Less(t, time.Since(started), 500*time.Millisecond)
	})
}

func TestAnthropicRequiresKeyAndVersion(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		version    string
		wantStatus int
	}{
		{name: "wrong key", key: "wrong", version: "2023-06-01", wantStatus: http.StatusUnauthorized},
		{name: "missing version", key: "e2e-anthropic-key", wantStatus: http.StatusBadRequest},
		{name: "wrong version", key: "e2e-anthropic-key", version: "2024-01-01", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestMockServer()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
			req.Header.Set("X-Api-Key", tt.key)
			req.Header.Set("Anthropic-Version", tt.version)
			req.Header.Set("X-Request-Id", "anthropic-auth-1")
			recorder := httptest.NewRecorder()

			server.providerHandler().ServeHTTP(recorder, req)

			assert.Equal(t, tt.wantStatus, recorder.Code)
			assert.NotContains(t, recorder.Body.String(), "e2e-anthropic-key")
		})
	}
}
