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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

func TestNIXLConnectorProxy(t *testing.T) {
	// Test non-streaming request
	t.Run("NonStreamingRequest", func(t *testing.T) {
		connector := NewNIXLConnector()

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

		// The NIXL connector will fail due to network issues (prefill request)
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001")
		if err == nil {
			t.Error("Expected NIXL connector Proxy to return error due to network/connection issues")
		}

		// Verify that prefill request was built
		nixlConn := connector.(*NIXLConnector)
		if nixlConn.prefillRequest == nil {
			t.Error("Expected prefill request to be built")
		}

		// Verify prefill request body
		if nixlConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(nixlConn.prefillRequest)
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

				// Should have kv_transfer_params for NIXL
				if kvTransferParams, ok := prefillBody["kv_transfer_params"]; !ok {
					t.Error("Expected prefill request to have kv_transfer_params field")
				} else if params, isMap := kvTransferParams.(map[string]interface{}); !isMap {
					t.Error("Expected kv_transfer_params to be a map")
				} else {
					// Verify NIXL-specific kv_transfer_params
					if doRemoteDecode, ok := params["do_remote_decode"]; !ok || doRemoteDecode != true {
						t.Errorf("Expected do_remote_decode to be true, got %v", doRemoteDecode)
					}
					if doRemotePrefill, ok := params["do_remote_prefill"]; !ok || doRemotePrefill != false {
						t.Errorf("Expected do_remote_prefill to be false, got %v", doRemotePrefill)
					}
				}

				// Should still have model and messages
				if model, ok := prefillBody["model"]; !ok || model != "test-model" {
					t.Errorf("Expected prefill request to have model 'test-model', got %v", model)
				}
			}
		}

		// Verify decode request body was prepared (but not executed due to prefill failure)
		if nixlConn.decodeRequestBody == nil {
			t.Error("Expected decode request body to be prepared")
		} else {
			fmt.Println("Decode request body:", nixlConn.decodeRequestBody)
			// Decode request should have include_usage set for non-streaming requests
			if includeUsage, ok := nixlConn.decodeRequestBody["include_usage"]; !ok || includeUsage != true {
				t.Errorf("Expected decode request body include_usage to be true, got %v", includeUsage)
			}
			// Should preserve original max_tokens
			if maxTokens, ok := nixlConn.decodeRequestBody["max_tokens"]; !ok {
				t.Error("Expected decode request body to have max_tokens field")
			} else if maxTokens, ok := maxTokens.(int); !ok || maxTokens != 100 {
				t.Errorf("Expected decode request body max_tokens to be 100, got %v", maxTokens)
			}
		}
	})

	// Test streaming request
	t.Run("StreamingRequest", func(t *testing.T) {
		connector := NewNIXLConnector()

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

		// The NIXL connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001")
		if err == nil {
			t.Error("Expected NIXL connector Proxy to return error due to network/connection issues")
		}

		// Verify that prefill request was built
		nixlConn := connector.(*NIXLConnector)
		if nixlConn.prefillRequest == nil {
			t.Error("Expected prefill request to be built")
		}

		// For streaming requests, verify that token usage context was set
		if val, exists := c.Get(common.TokenUsageKey); !exists || val != true {
			t.Error("Expected token usage to be set in context for streaming request")
		}

		// Verify prefill request body for streaming request
		if nixlConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(nixlConn.prefillRequest)
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

				// Should have NIXL-specific kv_transfer_params
				if kvTransferParams, ok := prefillBody["kv_transfer_params"]; !ok {
					t.Error("Expected prefill request to have kv_transfer_params field")
				} else if params, isMap := kvTransferParams.(map[string]interface{}); !isMap {
					t.Error("Expected kv_transfer_params to be a map")
				} else {
					if doRemoteDecode, ok := params["do_remote_decode"]; !ok || doRemoteDecode != true {
						t.Errorf("Expected do_remote_decode to be true, got %v", doRemoteDecode)
					}
				}
			}
		}

		// Verify decode request body
		if nixlConn.decodeRequestBody != nil {
			// Decode request should preserve stream field
			if stream, ok := nixlConn.decodeRequestBody["stream"]; !ok || stream != true {
				t.Errorf("Expected decode request body stream to be true, got %v", stream)
			}
			// Decode request should have stream_options with include_usage added
			if streamOptions, ok := nixlConn.decodeRequestBody["stream_options"]; !ok {
				t.Error("Expected decode request body to have stream_options")
			} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
				t.Error("Expected stream_options to be a map")
			} else if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
				t.Errorf("Expected stream_options include_usage to be true, got %v", includeUsage)
			}
		}
	})

	// Test streaming request with existing stream_options
	t.Run("StreamingRequestWithStreamOptions", func(t *testing.T) {
		connector := NewNIXLConnector()

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

		// The NIXL connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001")
		if err == nil {
			t.Error("Expected NIXL connector Proxy to return error due to network/connection issues")
		}

		// For streaming requests with existing stream_options, token usage should not be added to context
		if val, exists := c.Get(common.TokenUsageKey); exists && val == true {
			t.Error("Did not expect token usage to be set in context when stream_options already exists")
		}

		// Verify decode request body preserves existing stream_options
		nixlConn := connector.(*NIXLConnector)
		if nixlConn.decodeRequestBody != nil {
			if streamOptions, ok := nixlConn.decodeRequestBody["stream_options"]; !ok {
				t.Error("Expected decode request body to preserve existing stream_options")
			} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
				t.Error("Expected stream_options to be a map")
			} else if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
				t.Errorf("Expected existing stream_options include_usage to be preserved as true, got %v", includeUsage)
			}
		}
	})

	// Test max_completion_tokens handling
	t.Run("MaxCompletionTokensHandling", func(t *testing.T) {
		connector := NewNIXLConnector()

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

		// The NIXL connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001")
		if err == nil {
			t.Error("Expected NIXL connector Proxy to return error due to network/connection issues")
		}

		// Verify prefill request handling of max_completion_tokens
		nixlConn := connector.(*NIXLConnector)
		if nixlConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(nixlConn.prefillRequest)
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

		// Verify decode request preserves original max_completion_tokens
		if nixlConn.decodeRequestBody != nil {
			if maxCompletionTokens, ok := nixlConn.decodeRequestBody["max_completion_tokens"]; !ok {
				t.Error("Expected decode request body to have max_completion_tokens field")
			} else if maxCompletionTokens, ok := maxCompletionTokens.(int); !ok || maxCompletionTokens != 50 {
				t.Errorf("Expected decode request body max_completion_tokens to be 50, got %v", maxCompletionTokens)
			}
		}
	})

	// Test NIXL-specific kv_transfer_params structure
	t.Run("KVTransferParamsStructure", func(t *testing.T) {
		connector := NewNIXLConnector()

		// Create a proper test context with a valid HTTP request
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		reqBody := map[string]interface{}{
			"model": "test-model",
			"messages": []interface{}{
				map[string]interface{}{
					"role":    "user",
					"content": "test message",
				},
			},
		}

		// The NIXL connector will fail due to network issues
		_, err := connector.Proxy(c, reqBody, "localhost:8000", "localhost:8001")
		if err == nil {
			t.Error("Expected NIXL connector Proxy to return error due to network/connection issues")
		}

		// Verify detailed kv_transfer_params structure in prefill request
		nixlConn := connector.(*NIXLConnector)
		if nixlConn.prefillRequest != nil {
			prefillBody, err := parseRequestBody(nixlConn.prefillRequest)
			if err != nil {
				t.Errorf("Failed to parse prefill request body: %v", err)
			} else {
				if kvTransferParams, ok := prefillBody["kv_transfer_params"]; !ok {
					t.Error("Expected prefill request to have kv_transfer_params field")
				} else if params, isMap := kvTransferParams.(map[string]interface{}); !isMap {
					t.Error("Expected kv_transfer_params to be a map")
				} else {
					// Verify all expected NIXL kv_transfer_params fields
					expectedFields := map[string]interface{}{
						"do_remote_decode":  true,
						"do_remote_prefill": false,
					}

					for field, expectedValue := range expectedFields {
						if actualValue, ok := params[field]; !ok {
							t.Errorf("Expected kv_transfer_params to have field '%s'", field)
						} else if actualValue != expectedValue {
							t.Errorf("Expected kv_transfer_params['%s'] to be %v, got %v", field, expectedValue, actualValue)
						}
					}
				}
			}
		}
	})
}

// TestNIXLConnectorRetryBodyNotDrained checks that calling Proxy() twice on the
// same connector instance (as proxyToPDDisaggregated does during retries) sends
// a non-empty body to the prefill backend on both attempts.
func TestNIXLConnectorRetryBodyNotDrained(t *testing.T) {
	var callCount int32
	var bodyLengths [2]int64

	// prefill server records body size for each call and returns valid kv_transfer_params
	prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&callCount, 1) - 1
		body, _ := io.ReadAll(r.Body)
		if idx < 2 {
			bodyLengths[idx] = int64(len(body))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"kv_transfer_params": nil})
	}))
	defer prefillServer.Close()

	connector := NewNIXLConnector()

	reqBody := map[string]interface{}{
		"model":      "test-model",
		"max_tokens": 100,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	makeCtx := func() *gin.Context {
		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req
		return c
	}

	prefillAddr := prefillServer.Listener.Addr().String()
	decodeAddr := "127.0.0.1:1" // nothing listening here; decode will fail

	// First call — simulates retry iteration 0
	connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr)
	// Second call — simulates retry iteration 1 on the same connector instance
	connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr)

	if bodyLengths[0] == 0 {
		t.Error("first Proxy call sent empty body to prefill backend")
	}
	if bodyLengths[1] == 0 {
		t.Error("second Proxy call sent empty body to prefill backend — request body was drained and reused")
	}
}

// TestNIXLConnectorReqBodyNotMutated checks that Proxy() does not mutate the
// caller's reqBody map. proxyToPDDisaggregated passes the same modelRequest
// across all retry iterations, so mutations would bleed between retries.
func TestNIXLConnectorReqBodyNotMutated(t *testing.T) {
	connector := NewNIXLConnector()

	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	reqBody := map[string]interface{}{
		"model":      "test-model",
		"max_tokens": 100,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	// snapshot keys present before
	keysBefore := make(map[string]struct{})
	for k := range reqBody {
		keysBefore[k] = struct{}{}
	}

	connector.Proxy(c, reqBody, "127.0.0.1:1", "127.0.0.1:2")

	for k := range reqBody {
		if _, existed := keysBefore[k]; !existed {
			t.Errorf("Proxy() mutated caller reqBody by adding key %q", k)
		}
	}
}
