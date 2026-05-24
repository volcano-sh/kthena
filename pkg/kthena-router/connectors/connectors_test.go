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
	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

func TestHTTPConnector(t *testing.T) {
	connector := NewHTTPConnector()

	if connector.Name() != "default" {
		t.Errorf("Expected HTTP connector name 'default', got '%s'", connector.Name())
	}
}

func TestNIXLConnector(t *testing.T) {
	connector := NewNIXLConnector()

	if connector.Name() != "nixl" {
		t.Errorf("Expected NIXL connector name 'nixl', got '%s'", connector.Name())
	}
}

func TestFactory(t *testing.T) {
	factory := NewDefaultFactory()

	// Test HTTP connector
	httpConnector := factory.GetConnector(v1alpha1.ConnectorTypeHTTP)
	if httpConnector == nil {
		t.Error("Expected HTTP connector to be registered")
	}
	if httpConnector != nil && httpConnector.Name() != "default" {
		t.Errorf("Expected HTTP connector name 'default', got '%s'", httpConnector.Name())
	}

	// Test NIXL connector
	nixlConnector := factory.GetConnector(v1alpha1.ConnectorTypeNIXL)
	if nixlConnector == nil {
		t.Error("Expected NIXL connector to be registered")
	}
	if nixlConnector != nil && nixlConnector.Name() != "nixl" {
		t.Errorf("Expected NIXL connector name 'nixl', got '%s'", nixlConnector.Name())
	}

	// Test LMCache connector (currently uses HTTP implementation)
	lmcacheConnector := factory.GetConnector(v1alpha1.ConnectorTypeLMCache)
	if lmcacheConnector == nil {
		t.Error("Expected LMCache connector to be registered")
	}
	if lmcacheConnector != nil && lmcacheConnector.Name() != "default" {
		t.Errorf("Expected LMCache connector name 'default' (using HTTP implementation), got '%s'", lmcacheConnector.Name())
	}

	// Test unknown connector type
	unknownConnector := factory.GetConnector("unknown")
	if unknownConnector == nil {
		t.Error("Expected LMCache connector to be registered")
	}
	if unknownConnector != nil && unknownConnector.Name() != "default" {
		t.Errorf("Expected unknown connector name 'default' (using HTTP implementation), got '%s'", unknownConnector.Name())
	}
}

// Helper function to parse request body
func parseRequestBody(req *http.Request) (map[string]interface{}, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	// Reset the body for potential future reads
	req.Body = io.NopCloser(bytes.NewBuffer(body))

	var result map[string]interface{}
	err = json.Unmarshal(body, &result)
	return result, err
}

func TestHTTPConnectorProxy(t *testing.T) {
	// Test non-streaming request
	t.Run("NonStreamingRequest", func(t *testing.T) {
		connector := NewHTTPConnector()

		// Create a proper test context with a valid HTTP request
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		reqBody := map[string]interface{}{
			"model":      "test-model",
			"max_tokens": 100,
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "test message",
				},
			},
		}

		// The HTTP connector does support the Proxy method, but it will fail
		// because the test addresses don't exist and the prefill/decode calls will fail
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001", nil)
		if err == nil {
			t.Error("Expected HTTP connector Proxy to return error due to network/connection issues")
		}

		// Verify that prefill and decode requests were built
		httpConn := connector.(*HTTPConnector)
		if httpConn.prefillRequest == nil {
			t.Error("Expected prefill request to be built")
		}
		if httpConn.decodeRequest == nil {
			t.Error("Expected decode request to be built")
		}

		// Verify request body fields
		if httpConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(httpConn.prefillRequest)
			if err != nil {
				t.Errorf("Failed to parse prefill request body: %v", err)
			} else {
				// Prefill request should have max_tokens set to 1
				if maxTokens, ok := prefillBody["max_tokens"]; !ok {
					t.Error("Expected prefill request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
					t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
				}
				// Prefill request should not have stream field
				if _, hasStream := prefillBody["stream"]; hasStream {
					t.Error("Expected prefill request to not have stream field")
				}
				// Prefill request should not have stream_options field
				if _, hasStreamOptions := prefillBody["stream_options"]; hasStreamOptions {
					t.Error("Expected prefill request to not have stream_options field")
				}
				// Should still have model and messages
				if model, ok := prefillBody["model"]; !ok || model != "test-model" {
					t.Errorf("Expected prefill request to have model 'test-model', got %v", model)
				}
			}
		}

		if httpConn.decodeRequest != nil {
			decodeBody, err := parseRequestBody(httpConn.decodeRequest)
			if err != nil {
				t.Errorf("Failed to parse decode request body: %v", err)
			} else {
				// Decode request should have include_usage set for non-streaming requests
				if includeUsage, ok := decodeBody["include_usage"]; !ok || includeUsage != true {
					t.Errorf("Expected decode request include_usage to be true, got %v", includeUsage)
				}
				// Should preserve original max_tokens
				if maxTokens, ok := decodeBody["max_tokens"]; !ok {
					t.Error("Expected decode request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 100.0 {
					t.Errorf("Expected decode request max_tokens to be 100, got %v", maxTokens)
				}
				// Should still have model and messages
				if model, ok := decodeBody["model"]; !ok || model != "test-model" {
					t.Errorf("Expected decode request to have model 'test-model', got %v", model)
				}
			}
		}
	})

	// Test streaming request
	t.Run("StreamingRequest", func(t *testing.T) {
		connector := NewHTTPConnector()

		// Create a proper test context with a valid HTTP request
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		reqBody := map[string]interface{}{
			"model":      "test-model",
			"stream":     true,
			"max_tokens": 100,
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "test message",
				},
			},
		}

		// The HTTP connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001", nil)
		if err == nil {
			t.Error("Expected HTTP connector Proxy to return error due to network/connection issues")
		}

		// Verify that prefill and decode requests were built
		httpConn := connector.(*HTTPConnector)
		if httpConn.prefillRequest == nil {
			t.Error("Expected prefill request to be built")
		}
		if httpConn.decodeRequest == nil {
			t.Error("Expected decode request to be built")
		}

		// For streaming requests, verify that token usage context was set
		if val, exists := c.Get(common.TokenUsageKey); !exists || val != true {
			t.Error("Expected token usage to be set in context for streaming request")
		}

		// Verify request body fields for streaming request
		if httpConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(httpConn.prefillRequest)
			if err != nil {
				t.Errorf("Failed to parse prefill request body: %v", err)
			} else {
				// Prefill request should have max_tokens set to 1
				if maxTokens, ok := prefillBody["max_tokens"]; !ok {
					t.Error("Expected prefill request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
					t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
				}
				// Prefill request should not have stream field (removed for prefill)
				if _, hasStream := prefillBody["stream"]; hasStream {
					t.Error("Expected prefill request to not have stream field")
				}
				// Prefill request should not have stream_options field (removed for prefill)
				if _, hasStreamOptions := prefillBody["stream_options"]; hasStreamOptions {
					t.Error("Expected prefill request to not have stream_options field")
				}
			}
		}

		if httpConn.decodeRequest != nil {
			decodeBody, err := parseRequestBody(httpConn.decodeRequest)
			if err != nil {
				t.Errorf("Failed to parse decode request body: %v", err)
			} else {
				// Decode request should preserve stream field
				if stream, ok := decodeBody["stream"]; !ok || stream != true {
					t.Errorf("Expected decode request stream to be true, got %v", stream)
				}
				// Decode request should have stream_options with include_usage added
				if streamOptions, ok := decodeBody["stream_options"]; !ok {
					t.Error("Expected decode request to have stream_options")
				} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
					t.Error("Expected stream_options to be a map")
				} else if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
					t.Errorf("Expected stream_options include_usage to be true, got %v", includeUsage)
				}
				// Should preserve original max_tokens
				if maxTokens, ok := decodeBody["max_tokens"]; !ok {
					t.Error("Expected decode request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 100.0 {
					t.Errorf("Expected decode request max_tokens to be 100, got %v", maxTokens)
				}
			}
		}
	})

	// Test streaming request with existing stream_options
	t.Run("StreamingRequestWithStreamOptions", func(t *testing.T) {
		connector := NewHTTPConnector()

		// Create a proper test context with a valid HTTP request
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		reqBody := map[string]interface{}{
			"model":  "test-model",
			"stream": true,
			"stream_options": map[string]interface{}{
				"include_usage": true,
			},
			"max_tokens": 100,
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "test message",
				},
			},
		}

		// The HTTP connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001", nil)
		if err == nil {
			t.Error("Expected HTTP connector Proxy to return error due to network/connection issues")
		}

		// Verify that prefill and decode requests were built
		httpConn := connector.(*HTTPConnector)
		if httpConn.prefillRequest == nil {
			t.Error("Expected prefill request to be built")
		}
		if httpConn.decodeRequest == nil {
			t.Error("Expected decode request to be built")
		}

		// For streaming requests with existing stream_options, token usage should not be added to context
		if val, exists := c.Get(common.TokenUsageKey); exists && val == true {
			t.Error("Did not expect token usage to be set in context when stream_options already exists")
		}

		// Verify request body fields when stream_options already exists
		if httpConn.decodeRequest != nil {
			decodeBody, err := parseRequestBody(httpConn.decodeRequest)
			if err != nil {
				t.Errorf("Failed to parse decode request body: %v", err)
			} else {
				// Decode request should preserve existing stream_options
				if streamOptions, ok := decodeBody["stream_options"]; !ok {
					t.Error("Expected decode request to preserve existing stream_options")
				} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
					t.Error("Expected stream_options to be a map")
				} else if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
					t.Errorf("Expected existing stream_options include_usage to be preserved as true, got %v", includeUsage)
				}
			}
		}
	})

	// Test max_completion_tokens handling
	t.Run("MaxCompletionTokensHandling", func(t *testing.T) {
		connector := NewHTTPConnector()

		// Create a proper test context with a valid HTTP request
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		reqBody := map[string]interface{}{
			"model":                 "test-model",
			"max_completion_tokens": 50,
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "test message",
				},
			},
		}

		// The HTTP connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001", nil)
		if err == nil {
			t.Error("Expected HTTP connector Proxy to return error due to network/connection issues")
		}

		// Verify that both prefill and decode requests were built
		httpConn := connector.(*HTTPConnector)
		if httpConn.prefillRequest == nil {
			t.Error("Expected prefill request to be built")
		}
		if httpConn.decodeRequest == nil {
			t.Error("Expected decode request to be built")
		}

		// Both requests should have content
		if httpConn.prefillRequest.ContentLength == 0 {
			t.Error("Expected prefill request to have a body")
		}
		if httpConn.decodeRequest.ContentLength == 0 {
			t.Error("Expected decode request to have a body")
		}

		// Verify request body fields for max_completion_tokens handling
		if httpConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(httpConn.prefillRequest)
			if err != nil {
				t.Errorf("Failed to parse prefill request body: %v", err)
			} else {
				// Prefill request should have max_tokens set to 1
				if maxTokens, ok := prefillBody["max_tokens"]; !ok {
					t.Error("Expected prefill request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
					t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
				}
				// Prefill request should have max_completion_tokens set to 1
				if maxCompletionTokens, ok := prefillBody["max_completion_tokens"]; !ok {
					t.Error("Expected prefill request to have max_completion_tokens field")
				} else if maxCompletionTokensFloat, ok := maxCompletionTokens.(float64); !ok || maxCompletionTokensFloat != 1.0 {
					t.Errorf("Expected prefill request max_completion_tokens to be 1, got %v", maxCompletionTokens)
				}
			}
		}

		if httpConn.decodeRequest != nil {
			decodeBody, err := parseRequestBody(httpConn.decodeRequest)
			if err != nil {
				t.Errorf("Failed to parse decode request body: %v", err)
			} else {
				// Decode request should preserve original max_completion_tokens
				if maxCompletionTokens, ok := decodeBody["max_completion_tokens"]; !ok {
					t.Error("Expected decode request to have max_completion_tokens field")
				} else if maxCompletionTokensFloat, ok := maxCompletionTokens.(float64); !ok || maxCompletionTokensFloat != 50.0 {
					t.Errorf("Expected decode request max_completion_tokens to be 50, got %v", maxCompletionTokens)
				}
				// Decode request should have include_usage for non-streaming
				if includeUsage, ok := decodeBody["include_usage"]; !ok || includeUsage != true {
					t.Errorf("Expected decode request include_usage to be true, got %v", includeUsage)
				}
			}
		}
	})

	// Test request body modifications in detail
	t.Run("RequestBodyModifications", func(t *testing.T) {
		connector := NewHTTPConnector()

		// Create a proper test context with a valid HTTP request
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		originalReqBody := map[string]interface{}{
			"model":      "test-model",
			"stream":     true,
			"max_tokens": 200,
			"stream_options": map[string]interface{}{
				"some_other_option": "value",
			},
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "test message",
				},
			},
		}

		// Make a copy of the original to verify it gets modified
		reqBodyCopy := make(map[string]interface{})
		for k, v := range originalReqBody {
			reqBodyCopy[k] = v
		}

		// The HTTP connector will fail due to network issues
		_, err := connector.Proxy(c, reqBodyCopy, "localhost:8000", "localhost:8001", nil)
		if err == nil {
			t.Error("Expected HTTP connector Proxy to return error due to network/connection issues")
		}

		// Verify that the original request body was modified correctly
		httpConn := connector.(*HTTPConnector)

		// Check prefill request body
		if httpConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(httpConn.prefillRequest)
			if err != nil {
				t.Errorf("Failed to parse prefill request body: %v", err)
			} else {
				// Should have modified max_tokens to 1
				if maxTokens, ok := prefillBody["max_tokens"]; !ok {
					t.Error("Expected prefill request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
					t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
				}

				// Should have removed stream field
				if _, hasStream := prefillBody["stream"]; hasStream {
					t.Error("Expected prefill request to not have stream field")
				}

				// Should have removed stream_options field
				if _, hasStreamOptions := prefillBody["stream_options"]; hasStreamOptions {
					t.Error("Expected prefill request to not have stream_options field")
				}
			}
		}

		// Check decode request body
		if httpConn.decodeRequest != nil {
			decodeBody, err := parseRequestBody(httpConn.decodeRequest)
			if err != nil {
				t.Errorf("Failed to parse decode request body: %v", err)
			} else {
				// Should preserve stream field
				if stream, ok := decodeBody["stream"]; !ok || stream != true {
					t.Errorf("Expected decode request stream to be true, got %v", stream)
				}

				// Should preserve original max_tokens
				if maxTokens, ok := decodeBody["max_tokens"]; !ok {
					t.Error("Expected decode request to have max_tokens field")
				} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 200.0 {
					t.Errorf("Expected decode request max_tokens to be 200, got %v", maxTokens)
				}

				// Should have stream_options with include_usage added
				if streamOptions, ok := decodeBody["stream_options"]; !ok {
					t.Error("Expected decode request to have stream_options")
				} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
					t.Error("Expected stream_options to be a map")
				} else {
					// Should have include_usage added
					if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
						t.Errorf("Expected stream_options include_usage to be true, got %v", includeUsage)
					}
					// Note: Original stream_options are replaced, not preserved
					// This is the current behavior of buildDecodeRequest
					if len(opts) != 1 {
						t.Errorf("Expected stream_options to only contain include_usage, got %+v", opts)
					}
				}
			}
		}
	})
}
