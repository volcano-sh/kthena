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

package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

func TestIsTokenUsageEnabled(t *testing.T) {
	tests := []struct {
		name         string
		modelRequest map[string]interface{}
		expected     bool
	}{
		{
			name:         "no stream_options",
			modelRequest: map[string]interface{}{"model": "test"},
			expected:     false,
		},
		{
			name: "stream_options exists but no include_usage",
			modelRequest: map[string]interface{}{
				"model":          "test",
				"stream_options": map[string]interface{}{},
			},
			expected: false,
		},
		{
			name: "include_usage is not a boolean",
			modelRequest: map[string]interface{}{
				"model": "test",
				"stream_options": map[string]interface{}{
					"include_usage": "xxxx",
				},
			},
			expected: false,
		},
		{
			name: "include_usage is not a boolean",
			modelRequest: map[string]interface{}{
				"model": "test",
				"stream_options": map[string]interface{}{
					"include_usage": "true",
				},
			},
			expected: false,
		},
		{
			name: "include_usage is boolean false",
			modelRequest: map[string]interface{}{
				"model": "test",
				"stream_options": map[string]interface{}{
					"include_usage": false,
				},
			},
			expected: false,
		},
		{
			name: "include_usage is boolean true",
			modelRequest: map[string]interface{}{
				"model": "test",
				"stream_options": map[string]interface{}{
					"include_usage": true,
				},
			},
			expected: true,
		},
		{
			name: "stream_options is not a map",
			modelRequest: map[string]interface{}{
				"model":          "test",
				"stream_options": "invalid",
			},
			expected: false,
		},
		{
			name: "stream_options is not a map",
			modelRequest: map[string]interface{}{
				"model":          "test",
				"stream_options": nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isTokenUsageEnabled(tt.modelRequest)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsStreamingRequest(t *testing.T) {
	tests := []struct {
		name         string
		modelRequest map[string]interface{}
		expected     bool
	}{
		{
			name:         "no stream field",
			modelRequest: map[string]interface{}{"model": "test"},
			expected:     false,
		},
		{
			name: "stream field is not boolean",
			modelRequest: map[string]interface{}{
				"model":  "test",
				"stream": "true",
			},
			expected: false,
		},
		{
			name: "stream field is false",
			modelRequest: map[string]interface{}{
				"model":  "test",
				"stream": false,
			},
			expected: false,
		},
		{
			name: "stream field is true",
			modelRequest: map[string]interface{}{
				"model":  "test",
				"stream": true,
			},
			expected: true,
		},
		{
			name: "stream field is nil",
			modelRequest: map[string]interface{}{
				"model":  "test",
				"stream": nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStreamingRequest(tt.modelRequest)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsStreamingResponse(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		expected    bool
	}{
		{
			name:        "text/event-stream",
			contentType: "text/event-stream",
			expected:    true,
		},
		{
			name:        "application/x-ndjson",
			contentType: "application/x-ndjson",
			expected:    true,
		},
		{
			name:        "text/event-stream with charset",
			contentType: "text/event-stream; charset=utf-8",
			expected:    true,
		},
		{
			name:        "application/x-ndjson with charset",
			contentType: "application/x-ndjson; charset=utf-8",
			expected:    true,
		},
		{
			name:        "application/json",
			contentType: "application/json",
			expected:    false,
		},
		{
			name:        "text/plain",
			contentType: "text/plain",
			expected:    false,
		},
		{
			name:        "empty content type",
			contentType: "",
			expected:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: make(http.Header),
			}
			if tt.contentType != "" {
				resp.Header.Set("Content-Type", tt.contentType)
			}
			result := isStreamingResponse(resp)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildPrefillRequest(t *testing.T) {
	tests := []struct {
		name         string
		modelRequest map[string]interface{}
		expectNil    bool
	}{
		{
			name: "basic request",
			modelRequest: map[string]interface{}{
				"model":      "test-model",
				"stream":     true,
				"max_tokens": 100,
			},
			expectNil: false,
		},
		{
			name: "request with stream_options",
			modelRequest: map[string]interface{}{
				"model":  "test-model",
				"stream": true,
				"stream_options": map[string]interface{}{
					"include_usage": true,
				},
				"max_tokens": 100,
			},
			expectNil: false,
		},
		{
			name: "request with max_completion_tokens",
			modelRequest: map[string]interface{}{
				"model":                 "test-model",
				"max_completion_tokens": 200,
			},
			expectNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test HTTP request
			originalReq := httptest.NewRequest("POST", "/test", nil)

			result := buildPrefillRequest(nil, originalReq, tt.modelRequest)

			if tt.expectNil {
				assert.Nil(t, result)
				return
			}

			require.NotNil(t, result)

			// Verify the request body was modified correctly
			body, err := io.ReadAll(result.Body)
			require.NoError(t, err)

			var parsedRequest map[string]interface{}
			err = json.Unmarshal(body, &parsedRequest)
			require.NoError(t, err)

			// Verify stream and stream_options are removed
			assert.NotContains(t, parsedRequest, "stream")
			assert.NotContains(t, parsedRequest, "stream_options")

			// Verify max_tokens is set to 1
			assert.Equal(t, float64(1), parsedRequest["max_tokens"])

			// If max_completion_tokens existed, it should be set to 1
			if tt.modelRequest["max_completion_tokens"] != nil {
				assert.Equal(t, float64(1), parsedRequest["max_completion_tokens"])
			}

			// URL scheme should be http
			assert.Equal(t, "http", result.URL.Scheme)
		})
	}
}

func TestPreparePrefillBodyProtocolAware(t *testing.T) {
	tests := []struct {
		name string
		path string
		in   map[string]interface{}
		want map[string]interface{}
	}{
		{
			name: "chat completions caps max_tokens",
			path: "/v1/chat/completions",
			in:   map[string]interface{}{"model": "m", "stream": true, "stream_options": map[string]interface{}{"include_usage": true}},
			want: map[string]interface{}{"model": "m", "max_tokens": 1},
		},
		{
			name: "chat completions also caps max_completion_tokens when present",
			path: "/v1/chat/completions",
			in:   map[string]interface{}{"model": "m", "max_completion_tokens": 200},
			want: map[string]interface{}{"model": "m", "max_tokens": 1, "max_completion_tokens": 1},
		},
		{
			name: "legacy/non-v1 path keeps chat completions behavior",
			path: "/test",
			in:   map[string]interface{}{"model": "m", "stream": true},
			want: map[string]interface{}{"model": "m", "max_tokens": 1},
		},
		{
			name: "responses uses max_output_tokens and no chat completions fields",
			path: "/v1/responses",
			in: map[string]interface{}{
				"model": "m", "input": "hi", "stream": true,
				"stream_options":    map[string]interface{}{"include_usage": true},
				"max_output_tokens": 512,
			},
			want: map[string]interface{}{"model": "m", "input": "hi", "max_output_tokens": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preparePrefillBody(tt.in, tt.path)
			assert.Equal(t, tt.want, tt.in)
			assert.NotContains(t, tt.in, "stream")
			assert.NotContains(t, tt.in, "stream_options")
			if isResponsesPath(tt.path) {
				assert.NotContains(t, tt.in, "max_tokens")
				assert.NotContains(t, tt.in, "max_completion_tokens")
			}
		})
	}
}

func TestBuildPrefillRequestResponsesAPI(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	modelRequest := map[string]interface{}{
		"model": "m", "input": "hi", "stream": true,
		"previous_response_id": "resp_1",
	}

	result := buildPrefillRequest(nil, req, modelRequest)
	require.NotNil(t, result)

	body, err := io.ReadAll(result.Body)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &parsed))

	assert.Equal(t, float64(1), parsed["max_output_tokens"])
	assert.NotContains(t, parsed, "max_tokens")
	assert.NotContains(t, parsed, "max_completion_tokens")
	assert.NotContains(t, parsed, "stream")
	assert.NotContains(t, parsed, "stream_options")
	assert.Equal(t, "resp_1", parsed["previous_response_id"], "opaque fields preserved")
}

// TestBuildPrefillRequestResponsesAPIRewrittenToCanonicalPath covers the opposite
// URLRewrite direction: the original client-facing path is custom, but
// req.URL.Path has already been rewritten to the canonical "/v1/responses"
// upstream path by the time buildPrefillRequest runs.
func TestBuildPrefillRequestResponsesAPIRewrittenToCanonicalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/llm/v1/responses", nil)
	accessCtx := accesslog.NewAccessLogContext("req-1", http.MethodPost, "/llm/v1/responses", "HTTP/1.1", "")
	c.Set(accesslog.AccessLogContextKey, accessCtx)

	req := httptest.NewRequest("POST", "/v1/responses", nil)
	modelRequest := map[string]interface{}{
		"model": "m", "input": "hi", "stream": true,
	}

	result := buildPrefillRequest(c, req, modelRequest)
	require.NotNil(t, result)

	body, err := io.ReadAll(result.Body)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &parsed))

	assert.Equal(t, float64(1), parsed["max_output_tokens"], "Responses output cap must apply "+
		"once req.URL.Path is rewritten to canonical /v1/responses")
	assert.NotContains(t, parsed, "max_tokens")
}

func TestAddTokenUsageResponsesAPINoInjection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("responses streaming request is not mutated", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

		out := AddTokenUsage(c, map[string]interface{}{"model": "m", "input": "hi", "stream": true})

		assert.NotContains(t, out, "stream_options")
		assert.NotContains(t, out, "include_usage")
		_, tokenUsageSet := c.Get(common.TokenUsageKey)
		assert.False(t, tokenUsageSet)
	})

	t.Run("chat completions streaming request still gets include_usage", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

		out := AddTokenUsage(c, map[string]interface{}{"model": "m", "stream": true})

		streamOptions, ok := out["stream_options"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, true, streamOptions["include_usage"])
		value, exists := c.Get(common.TokenUsageKey)
		assert.True(t, exists)
		assert.Equal(t, true, value)
	})

	// Opposite URLRewrite direction: a custom public path rewritten to the
	// canonical "/v1/responses" upstream path. stream_options/include_usage must
	// still not be injected, even though the original client-facing path alone
	// does not look like a Responses request.
	t.Run("responses request is not mutated after rewrite to canonical path", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		accessCtx := accesslog.NewAccessLogContext("req-1", http.MethodPost, "/llm/v1/responses", "HTTP/1.1", "")
		c.Set(accesslog.AccessLogContextKey, accessCtx)

		out := AddTokenUsage(c, map[string]interface{}{"model": "m", "input": "hi", "stream": true})

		assert.NotContains(t, out, "stream_options")
		assert.NotContains(t, out, "include_usage")
		_, tokenUsageSet := c.Get(common.TokenUsageKey)
		assert.False(t, tokenUsageSet)
	})
}

func TestBuildDecodeRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		modelRequest map[string]interface{}
		expectUsage  bool
	}{
		{
			name: "streaming request without token usage",
			modelRequest: map[string]interface{}{
				"model":  "test-model",
				"stream": true,
			},
			expectUsage: true, // should add stream_options
		},
		{
			name: "streaming request with token usage already enabled",
			modelRequest: map[string]interface{}{
				"model":  "test-model",
				"stream": true,
				"stream_options": map[string]interface{}{
					"include_usage": true,
				},
			},
			expectUsage: false, // should not modify
		},
		{
			name: "non-streaming request",
			modelRequest: map[string]interface{}{
				"model":  "test-model",
				"stream": false,
			},
			expectUsage: false, // should add include_usage
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create gin context
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			// Create a test HTTP request
			originalReq := httptest.NewRequest("POST", "/test", nil)

			result := BuildDecodeRequest(c, originalReq, tt.modelRequest)
			require.NotNil(t, result)

			// Verify the request body was modified correctly
			body, err := io.ReadAll(result.Body)
			require.NoError(t, err)

			var parsedRequest map[string]interface{}
			err = json.Unmarshal(body, &parsedRequest)
			require.NoError(t, err)

			if isStreamingRequest(tt.modelRequest) && !isTokenUsageEnabled(tt.modelRequest) {
				// Should have added stream_options
				assert.Contains(t, parsedRequest, "stream_options")
				streamOptions, ok := parsedRequest["stream_options"].(map[string]interface{})
				require.True(t, ok)
				assert.Equal(t, true, streamOptions["include_usage"])

				// Context should have token usage key set
				if tt.expectUsage {
					value, exists := c.Get(common.TokenUsageKey)
					assert.True(t, exists)
					assert.Equal(t, true, value)
				}
			} else if !isStreamingRequest(tt.modelRequest) {
				// Non-streaming should have include_usage
				assert.Equal(t, true, parsedRequest["include_usage"])
			}

			// URL scheme should be http
			assert.Equal(t, "http", result.URL.Scheme)
		})
	}
}

func TestBuildDecodeRequestChatCompletionsInjectsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	originalReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	result := BuildDecodeRequest(c, originalReq, map[string]interface{}{
		"model":  "test-model",
		"stream": true,
	})
	require.NotNil(t, result)

	body, err := io.ReadAll(result.Body)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &parsed))

	streamOptions, ok := parsed["stream_options"].(map[string]interface{})
	require.True(t, ok, "chat completions streaming request must keep injecting stream_options")
	assert.Equal(t, true, streamOptions["include_usage"])

	value, exists := c.Get(common.TokenUsageKey)
	assert.True(t, exists)
	assert.Equal(t, true, value)
}

func TestBuildDecodeRequestResponsesAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A Responses request body carrying opaque/arbitrary fields the router must
	// not drop or reshape.
	rawBody := []byte(`{"model":"gpt-4o","input":"hello","previous_response_id":"resp_123",` +
		`"stream":true,"tools":[{"type":"web_search"}],"metadata":{"trace":"abc"},` +
		`"reasoning":{"effort":"high"},"x-vendor-extension":{"nested":[1,2,3]}}`)

	decode := func(t *testing.T, body []byte) map[string]interface{} {
		t.Helper()
		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &parsed))
		return parsed
	}

	t.Run("does not inject Chat Completions usage fields", func(t *testing.T) {
		for _, stream := range []bool{true, false} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req := httptest.NewRequest("POST", "/v1/responses", nil)

			modelRequest := map[string]interface{}{"model": "gpt-4o", "input": "hello", "stream": stream}
			result := BuildDecodeRequest(c, req, modelRequest)
			require.NotNil(t, result)

			body, err := io.ReadAll(result.Body)
			require.NoError(t, err)
			parsed := decode(t, body)

			assert.NotContains(t, parsed, "include_usage")
			assert.NotContains(t, parsed, "stream_options")
			_, tokenUsageSet := c.Get(common.TokenUsageKey)
			assert.False(t, tokenUsageSet, "responses requests must not set the injected-usage marker")
		}
	})

	t.Run("without model rewrite replays the original raw body", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(common.RawRequestBodyKey, rawBody)
		req := httptest.NewRequest("POST", "/v1/responses", nil)

		// modelRequest mirrors the parsed raw body; model is unchanged.
		modelRequest := decode(t, rawBody)

		result := BuildDecodeRequest(c, req, modelRequest)
		require.NotNil(t, result)

		body, err := io.ReadAll(result.Body)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(rawBody, body), "raw Responses body must be forwarded byte-for-byte")
		assert.Equal(t, int64(len(rawBody)), result.ContentLength)
	})

	t.Run("with model rewrite changes only the model and preserves opaque fields", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(common.RawRequestBodyKey, rawBody)
		req := httptest.NewRequest("POST", "/v1/responses", nil)

		modelRequest := decode(t, rawBody)
		modelRequest["model"] = "upstream-model" // router rewrote the model

		result := BuildDecodeRequest(c, req, modelRequest)
		require.NotNil(t, result)

		body, err := io.ReadAll(result.Body)
		require.NoError(t, err)
		parsed := decode(t, body)

		assert.Equal(t, "upstream-model", parsed["model"])
		assert.NotContains(t, parsed, "include_usage")
		assert.NotContains(t, parsed, "stream_options")

		// Every non-model field survives unchanged.
		original := decode(t, rawBody)
		for k, v := range original {
			if k == "model" {
				continue
			}
			assert.Equal(t, v, parsed[k], "opaque field %q must be preserved", k)
		}
	})

	t.Run("missing raw body falls back to marshalling the parsed request", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest("POST", "/v1/responses", nil)

		modelRequest := map[string]interface{}{"model": "gpt-4o", "input": "hello", "previous_response_id": "resp_1"}
		result := BuildDecodeRequest(c, req, modelRequest)
		require.NotNil(t, result)

		body, err := io.ReadAll(result.Body)
		require.NoError(t, err)
		parsed := decode(t, body)

		assert.Equal(t, "resp_1", parsed["previous_response_id"])
		assert.NotContains(t, parsed, "include_usage")
		assert.NotContains(t, parsed, "stream_options")
	})
}

func TestHandleNonStreamingResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		responseBody string
		statusCode   int
		headers      map[string]string
	}{
		{
			name: "valid OpenAI response with usage",
			responseBody: `{
				"id": "test-id",
				"object": "text_completion",
				"model": "test-model",
				"usage": {
					"prompt_tokens": 10,
					"completion_tokens": 20,
					"total_tokens": 30
				}
			}`,
			statusCode: 200,
			headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
		{
			name:         "invalid JSON response",
			responseBody: `invalid json`,
			statusCode:   200,
			headers: map[string]string{
				"Content-Type": "application/json",
			},
		},
		{
			name:         "empty response",
			responseBody: ``,
			statusCode:   200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create gin context
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			// Create mock HTTP response
			resp := &http.Response{
				StatusCode: tt.statusCode,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(tt.responseBody)),
			}

			for k, v := range tt.headers {
				resp.Header.Set(k, v)
			}

			_, err := handleNonStreamingResponse(c, resp)

			// Should not return error for any of these cases
			assert.NoError(t, err)

			// Verify response was written to gin context
			assert.Equal(t, tt.responseBody, w.Body.String())
		})
	}
}

func TestHandleStreamingResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		responseBody string
		tokenUsage   bool
	}{
		{
			name:         "streaming response with usage",
			responseBody: "data: {\"id\":\"test\",\"object\":\"text_completion\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\ndata: [DONE]\n\n",
			tokenUsage:   false,
		},
		{
			name:         "streaming response with token usage filtering",
			responseBody: "data: {\"id\":\"test\",\"object\":\"text_completion\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\ndata: [DONE]\n\n",
			tokenUsage:   true,
		},
		{
			name:         "streaming response without usage",
			responseBody: "data: {\"id\":\"test\",\"object\":\"text_completion\"}\n\ndata: [DONE]\n\n",
			tokenUsage:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create gin context with proper request
			w := CreateTestResponseRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			if tt.tokenUsage {
				c.Set(common.TokenUsageKey, true)
			}

			// Create mock HTTP response
			resp := &http.Response{
				StatusCode: 200,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(tt.responseBody)),
			}
			resp.Header.Set("Content-Type", "text/event-stream")

			_, err := handleStreamingResponse(c, resp)

			// Should not return error
			assert.NoError(t, err)
		})
	}
}

func TestPrefillerProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		statusCode    int
		responseBody  string
		expectError   bool
		errorContains string
	}{
		{
			name:         "successful prefill request",
			statusCode:   200,
			responseBody: `{"result": "success"}`,
			expectError:  false,
		},
		{
			name:          "prefill request with 4xx error",
			statusCode:    400,
			responseBody:  `{"error": "bad request"}`,
			expectError:   true,
			errorContains: "prefill request failed with status 400",
		},
		{
			name:          "prefill request with 5xx error",
			statusCode:    500,
			responseBody:  `{"error": "internal server error"}`,
			expectError:   true,
			errorContains: "prefill request failed with status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			// Create gin context
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			// Create request to test server
			testReq, err := http.NewRequest("POST", server.URL, bytes.NewBuffer([]byte(`{"model": "test"}`)))
			require.NoError(t, err)
			testReq.Header.Set("Content-Type", "application/json")

			err = prefillerProxy(c, testReq, 0)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPrefillerProxyHonorsTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL, nil)
	require.NoError(t, err)

	start := time.Now()
	err = prefillerProxy(nil, req, 100*time.Millisecond)

	assert.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestDecoderProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		statusCode      int
		responseBody    string
		responseHeaders map[string]string
		contentType     string
		expectError     bool
		errorContains   string
	}{
		{
			name:         "successful non-streaming response",
			statusCode:   200,
			responseBody: `{"id": "test", "object": "text_completion", "usage": {"total_tokens": 30}}`,
			responseHeaders: map[string]string{
				"Content-Type": "application/json",
			},
			contentType: "application/json",
			expectError: false,
		},
		{
			name:         "successful streaming response",
			statusCode:   200,
			responseBody: "data: {\"id\":\"test\"}\n\ndata: [DONE]\n\n",
			responseHeaders: map[string]string{
				"Content-Type": "text/event-stream",
			},
			contentType: "text/event-stream",
			expectError: false,
		},
		{
			name:          "decode request with 4xx error",
			statusCode:    400,
			responseBody:  `{"error": "bad request"}`,
			expectError:   true,
			errorContains: "decode request failed with status 400",
		},
		{
			name:          "decode request with 5xx error",
			statusCode:    500,
			responseBody:  `{"error": "internal server error"}`,
			expectError:   true,
			errorContains: "decode request failed with status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Set headers
				for k, v := range tt.responseHeaders {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			// Create gin context
			w := CreateTestResponseRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			// Create request to test server
			testReq, err := http.NewRequest("POST", server.URL, bytes.NewBuffer([]byte(`{"model": "test"}`)))
			require.NoError(t, err)
			testReq.Header.Set("Content-Type", "application/json")

			_, err = decoderProxy(c, testReq, 0)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
				// Unchanged pre-existing behavior for non-Responses paths: nothing
				// is forwarded to the client on a non-2xx decode response.
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Empty(t, w.Body.String())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDecoderProxyResponsesAPINonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	respBody := `{"id":"resp_1","object":"response","status":"completed",` +
		`"output":[{"type":"message"}],` +
		`"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer server.Close()

	w := CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

	testReq, err := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewBufferString(`{"model":"m","input":"hi"}`))
	require.NoError(t, err)

	outputTokens, err := decoderProxy(c, testReq, 0)
	require.NoError(t, err)

	assert.Equal(t, 7, outputTokens, "output_tokens must be extracted from the Responses usage object")
	assert.Equal(t, respBody, w.Body.String(), "non-streaming Responses body must be forwarded verbatim")
}

func TestDecoderProxyResponsesAPIStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	usage := `"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}`
	sse := func(terminalEvent, terminalPayload string) string {
		return "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			"event: " + terminalEvent + "\n" +
			"data: {\"type\":\"" + terminalEvent + "\",\"response\":{" + terminalPayload + "}}\n\n"
	}

	tests := []struct {
		name       string
		body       string
		wantTokens int
	}{
		{"response.completed with usage", sse("response.completed", usage), 7},
		{"response.incomplete with usage", sse("response.incomplete", usage), 7},
		{"response.failed with usage", sse("response.failed", usage), 7},
		{"terminal event without usage does not fabricate usage", sse("response.completed", `"id":"resp_1"`), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			w := CreateTestResponseRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

			testReq, err := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewBufferString(`{"model":"m","input":"hi","stream":true}`))
			require.NoError(t, err)

			outputTokens, err := decoderProxy(c, testReq, 0)
			require.NoError(t, err)

			assert.Equal(t, tt.wantTokens, outputTokens)
			// SSE forwarded verbatim; no `data: [DONE]` is required or added.
			assert.Equal(t, tt.body, w.Body.String())
			assert.NotContains(t, w.Body.String(), "[DONE]")
		})
	}
}

// TestDecoderProxyResponsesAPINonStreamingErrorDoesNotWritePrematurely covers the review
// concern that decoderProxy wrote a non-2xx Responses response to c.Writer immediately,
// which made c.Writer.Written() true and stopped proxyToPDDisaggregated's retry loop from
// trying another prefill/decode pair on the very first failed attempt. decoderProxy must
// instead return the upstream status/headers/body via DecodeUpstreamError without writing
// anything, leaving the retry decision (and the eventual write) to the caller.
func TestDecoderProxyResponsesAPINonStreamingErrorDoesNotWritePrematurely(t *testing.T) {
	gin.SetMode(gin.TestMode)

	errBody := `{"error":{"message":"invalid input","type":"invalid_request_error"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errBody))
	}))
	defer server.Close()

	w := CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

	testReq, err := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewBufferString(`{"model":"m","input":"hi"}`))
	require.NoError(t, err)

	outputTokens, err := decoderProxy(c, testReq, 0)

	// The error is returned, not written: the PD retry loop must still be able to try
	// another prefill/decode pair (c.Writer.Written() must stay false here).
	require.Error(t, err)
	assert.Equal(t, 0, outputTokens)
	assert.False(t, c.Writer.Written(), "decoderProxy must not write to the client before the retry loop decides")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Body.String())

	var respErr *DecodeUpstreamError
	require.ErrorAs(t, err, &respErr, "a Responses non-2xx must be returned as *DecodeUpstreamError so the caller can retry")
	assert.Equal(t, http.StatusBadRequest, respErr.StatusCode)
	assert.Equal(t, errBody, string(respErr.Body))
	assert.Equal(t, "yes", respErr.Header.Get("X-Upstream"))

	// Once the caller (proxyToPDDisaggregated) decides no further retry will happen, it
	// forwards the captured response via WriteTo — verify that produces the real upstream
	// status/headers/body, not a generic error.
	respErr.WriteTo(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, errBody, w.Body.String())
	assert.Equal(t, "yes", w.Header().Get("X-Upstream"))
}

// TestDecoderProxyResponsesAPIUsesOriginalPathAfterURLRewrite covers the review concern
// that isResponsesPath could be fooled by an HTTPRoute URLRewrite: by the time a request
// reaches decoderProxy, req.URL.Path may already be the rewritten backend path rather than
// the client's original "/v1/responses". AccessLogMiddleware runs before any URLRewrite is
// applied, so its recorded path is used here instead.
func TestDecoderProxyResponsesAPIUsesOriginalPathAfterURLRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	respBody := `{"id":"resp_1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	defer server.Close()

	w := CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	// req.URL.Path no longer looks like "/v1/responses" here, as if an
	// HTTPRoute URLRewrite already rewrote it to a backend-specific path.
	c.Request = httptest.NewRequest("POST", "/rewritten/backend-path", nil)
	accessCtx := accesslog.NewAccessLogContext("req-1", http.MethodPost, "/v1/responses", "HTTP/1.1", "")
	c.Set(accesslog.AccessLogContextKey, accessCtx)

	testReq, err := http.NewRequest("POST", server.URL+"/rewritten/backend-path", bytes.NewBufferString(`{"model":"m","input":"hi"}`))
	require.NoError(t, err)

	outputTokens, err := decoderProxy(c, testReq, 0)
	require.NoError(t, err)

	assert.Equal(t, 7, outputTokens, "Responses usage parsing must apply based on the original "+
		"client path even though req.URL.Path was rewritten away from /v1/responses")
	assert.Equal(t, respBody, w.Body.String())
}

// TestDecoderProxyResponsesAPIRecognizesRewrittenCanonicalPath covers the opposite
// URLRewrite direction: a custom public path (e.g. /llm/v1/responses) rewritten by
// an HTTPRoute to the canonical upstream path "/v1/responses". The original
// client-facing path alone no longer identifies this as a Responses request, so
// decoderProxy must also recognize req.URL.Path once it has been rewritten to
// "/v1/responses".
func TestDecoderProxyResponsesAPIRecognizesRewrittenCanonicalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	respBody := `{"id":"resp_1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	defer server.Close()

	w := CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/llm/v1/responses", nil)
	// The original client-facing path is custom; only the access-log context
	// records it, matching what AccessLogMiddleware captures before URLRewrite.
	accessCtx := accesslog.NewAccessLogContext("req-1", http.MethodPost, "/llm/v1/responses", "HTTP/1.1", "")
	c.Set(accesslog.AccessLogContextKey, accessCtx)

	// req.URL.Path reflects the HTTPRoute URLRewrite to the canonical path.
	testReq, err := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewBufferString(`{"model":"m","input":"hi"}`))
	require.NoError(t, err)

	outputTokens, err := decoderProxy(c, testReq, 0)
	require.NoError(t, err)

	assert.Equal(t, 7, outputTokens, "Responses usage parsing must apply once req.URL.Path is "+
		"rewritten to canonical /v1/responses, even though the original public path was custom")
	assert.Equal(t, respBody, w.Body.String())
}

func TestDecoderProxyChatCompletionsUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("non-streaming uses prompt/completion tokens", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":4,"completion_tokens":9,"total_tokens":13}}`))
		}))
		defer server.Close()

		w := CreateTestResponseRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		testReq, err := http.NewRequest("POST", server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"m"}`))
		require.NoError(t, err)

		outputTokens, err := decoderProxy(c, testReq, 0)
		require.NoError(t, err)
		assert.Equal(t, 9, outputTokens)
	})

	t.Run("streaming still honors data: [DONE]", func(t *testing.T) {
		body := "data: {\"id\":\"x\",\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":9,\"total_tokens\":13}}\n\ndata: [DONE]\n\n"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()

		w := CreateTestResponseRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		testReq, err := http.NewRequest("POST", server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"m","stream":true}`))
		require.NoError(t, err)

		outputTokens, err := decoderProxy(c, testReq, 0)
		require.NoError(t, err)
		assert.Equal(t, 9, outputTokens)
		assert.Contains(t, w.Body.String(), "data: [DONE]")
	})
}

func TestDecoderProxyTimeoutDoesNotTruncateStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	w := CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	require.NoError(t, err)

	_, err = decoderProxy(c, req, 50*time.Millisecond)

	assert.NoError(t, err)
	assert.Contains(t, w.Body.String(), "data: [DONE]")
}
