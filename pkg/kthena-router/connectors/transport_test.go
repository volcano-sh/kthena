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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

			result := buildPrefillRequest(originalReq, tt.modelRequest)

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

			err = prefillerProxy(c, testReq)

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

			_, err = decoderProxy(c, testReq)

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
