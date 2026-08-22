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
		var prefillReceivedBody map[string]interface{}
		var decodeReceivedBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillReceivedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"kv_transfer_params": map[string]interface{}{"remote_host": "10.0.0.1"},
			})
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeReceivedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":3}}`))
		}))
		defer decodeServer.Close()

		connector := NewNIXLConnector()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
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

		prefillHost := prefillServer.Listener.Addr().String()
		decodeHost := decodeServer.Listener.Addr().String()

		tokens, err := connector.Proxy(c, reqBody, prefillHost, decodeHost, nil)
		if err != nil {
			t.Fatalf("Unexpected error from Proxy: %v", err)
		}
		if tokens != 3 {
			t.Errorf("Expected 3 output tokens, got %d", tokens)
		}

		// Verify prefill request body
		if maxTokens, ok := prefillReceivedBody["max_tokens"]; !ok {
			t.Error("Expected prefill request to have max_tokens field")
		} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
			t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
		}
		if _, hasStream := prefillReceivedBody["stream"]; hasStream {
			t.Error("Expected prefill request to not have stream field")
		}
		if _, hasStreamOptions := prefillReceivedBody["stream_options"]; hasStreamOptions {
			t.Error("Expected prefill request to not have stream_options field")
		}

		// Should have kv_transfer_params for NIXL in prefill request
		if kvTransferParams, ok := prefillReceivedBody["kv_transfer_params"]; !ok {
			t.Error("Expected prefill request to have kv_transfer_params field")
		} else if params, isMap := kvTransferParams.(map[string]interface{}); !isMap {
			t.Error("Expected kv_transfer_params to be a map")
		} else {
			if doRemoteDecode, ok := params["do_remote_decode"]; !ok || doRemoteDecode != true {
				t.Errorf("Expected do_remote_decode to be true, got %v", doRemoteDecode)
			}
			if doRemotePrefill, ok := params["do_remote_prefill"]; !ok || doRemotePrefill != false {
				t.Errorf("Expected do_remote_prefill to be false, got %v", doRemotePrefill)
			}
		}

		if model, ok := prefillReceivedBody["model"]; !ok || model != "test-model" {
			t.Errorf("Expected prefill request to have model 'test-model', got %v", model)
		}

		// Verify decode request body
		if includeUsage, ok := decodeReceivedBody["include_usage"]; !ok || includeUsage != true {
			t.Errorf("Expected decode request body include_usage to be true, got %v", includeUsage)
		}
		if maxTokens, ok := decodeReceivedBody["max_tokens"]; !ok {
			t.Error("Expected decode request body to have max_tokens field")
		} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 100.0 {
			t.Errorf("Expected decode request body max_tokens to be 100, got %v", maxTokens)
		}
		// Verify kv_transfer_params was relayed to decode
		if kvParams, ok := decodeReceivedBody["kv_transfer_params"]; !ok {
			t.Error("Expected decode request body to have relayed kv_transfer_params")
		} else if m, ok := kvParams.(map[string]interface{}); !ok || m["remote_host"] != "10.0.0.1" {
			t.Errorf("Expected remote_host 10.0.0.1, got %v", kvParams)
		}
	})

	// Test streaming request
	t.Run("StreamingRequest", func(t *testing.T) {
		var prefillReceivedBody map[string]interface{}
		var decodeReceivedBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillReceivedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"kv_transfer_params": map[string]interface{}{"remote_host": "10.0.0.2"},
			})
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeReceivedBody)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"completion_tokens\":1}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}))
		defer decodeServer.Close()

		connector := NewNIXLConnector()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		rec := CreateTestResponseRecorder()
		c, _ := gin.CreateTestContext(rec)
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

		prefillHost := prefillServer.Listener.Addr().String()
		decodeHost := decodeServer.Listener.Addr().String()

		_, err := connector.Proxy(c, reqBody, prefillHost, decodeHost, nil)
		if err != nil {
			t.Fatalf("Unexpected error from Proxy: %v", err)
		}

		// For streaming requests, verify that token usage context was set
		if val, exists := c.Get(common.TokenUsageKey); !exists || val != true {
			t.Error("Expected token usage to be set in context for streaming request")
		}

		// Verify prefill request body
		if maxTokens, ok := prefillReceivedBody["max_tokens"]; !ok {
			t.Error("Expected prefill request to have max_tokens field")
		} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
			t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
		}
		if _, hasStream := prefillReceivedBody["stream"]; hasStream {
			t.Error("Expected prefill request to not have stream field")
		}

		// Verify decode request body
		if stream, ok := decodeReceivedBody["stream"]; !ok || stream != true {
			t.Errorf("Expected decode request body stream to be true, got %v", stream)
		}
		if streamOptions, ok := decodeReceivedBody["stream_options"]; !ok {
			t.Error("Expected decode request body to have stream_options")
		} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
			t.Error("Expected stream_options to be a map")
		} else if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
			t.Errorf("Expected stream_options include_usage to be true, got %v", includeUsage)
		}
	})

	// Test streaming request with existing stream_options
	t.Run("StreamingRequestWithStreamOptions", func(t *testing.T) {
		var decodeReceivedBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"kv_transfer_params": nil})
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeReceivedBody)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}))
		defer decodeServer.Close()

		connector := NewNIXLConnector()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		rec := CreateTestResponseRecorder()
		c, _ := gin.CreateTestContext(rec)
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

		prefillHost := prefillServer.Listener.Addr().String()
		decodeHost := decodeServer.Listener.Addr().String()

		_, err := connector.Proxy(c, reqBody, prefillHost, decodeHost, nil)
		if err != nil {
			t.Fatalf("Unexpected error from Proxy: %v", err)
		}

		// For streaming requests with existing stream_options, token usage should not be added to context
		if val, exists := c.Get(common.TokenUsageKey); exists && val == true {
			t.Error("Did not expect token usage to be set in context when stream_options already exists")
		}

		// Verify decode request body preserves existing stream_options
		if streamOptions, ok := decodeReceivedBody["stream_options"]; !ok {
			t.Error("Expected decode request body to preserve existing stream_options")
		} else if opts, isMap := streamOptions.(map[string]interface{}); !isMap {
			t.Error("Expected stream_options to be a map")
		} else if includeUsage, ok := opts["include_usage"]; !ok || includeUsage != true {
			t.Errorf("Expected existing stream_options include_usage to be preserved as true, got %v", includeUsage)
		}
	})

	// Test max_completion_tokens handling
	t.Run("MaxCompletionTokensHandling", func(t *testing.T) {
		var prefillReceivedBody map[string]interface{}
		var decodeReceivedBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillReceivedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"kv_transfer_params": nil})
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeReceivedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer decodeServer.Close()

		connector := NewNIXLConnector()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
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

		prefillHost := prefillServer.Listener.Addr().String()
		decodeHost := decodeServer.Listener.Addr().String()

		_, err := connector.Proxy(c, reqBody, prefillHost, decodeHost, nil)
		if err != nil {
			t.Fatalf("Unexpected error from Proxy: %v", err)
		}

		// Verify prefill request handling of max_completion_tokens
		if maxTokens, ok := prefillReceivedBody["max_tokens"]; !ok {
			t.Error("Expected prefill request to have max_tokens field")
		} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
			t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
		}
		if maxCompletionTokens, ok := prefillReceivedBody["max_completion_tokens"]; !ok {
			t.Error("Expected prefill request to have max_completion_tokens field")
		} else if maxCompletionTokensFloat, ok := maxCompletionTokens.(float64); !ok || maxCompletionTokensFloat != 1.0 {
			t.Errorf("Expected prefill request max_completion_tokens to be 1, got %v", maxCompletionTokens)
		}

		// Verify decode request preserves original max_completion_tokens
		if maxCompletionTokens, ok := decodeReceivedBody["max_completion_tokens"]; !ok {
			t.Error("Expected decode request body to have max_completion_tokens field")
		} else if maxCompletionTokensFloat, ok := maxCompletionTokens.(float64); !ok || maxCompletionTokensFloat != 50.0 {
			t.Errorf("Expected decode request body max_completion_tokens to be 50, got %v", maxCompletionTokens)
		}
	})
}

// TestNIXLConnector_ConcurrentThreadSafety verifies that concurrent requests through
// the same NIXLConnector instance do not race or corrupt each other's payloads.
func TestNIXLConnector_ConcurrentThreadSafety(t *testing.T) {
	prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"kv_transfer_params": map[string]interface{}{"assigned_model": model},
		})
	}))
	defer prefillServer.Close()

	decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)

		// Verify relayed kv_transfer_params matches the request model
		kvParams, _ := body["kv_transfer_params"].(map[string]interface{})
		if kvParams == nil || kvParams["assigned_model"] != model {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"model mismatch: req=%s, kv=%v"}`, model, kvParams)))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]interface{}{
			"model": model,
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": "response for " + model}},
			},
			"usage": map[string]int{"completion_tokens": 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer decodeServer.Close()

	connector := NewNIXLConnector()
	prefillAddr := prefillServer.Listener.Addr().String()
	decodeAddr := decodeServer.Listener.Addr().String()

	const concurrency = 10
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			modelName := fmt.Sprintf("nixl-model-%d", id)
			req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = req

			reqBody := map[string]interface{}{
				"model":      modelName,
				"max_tokens": 50 + id,
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": fmt.Sprintf("msg-%d", id)},
				},
			}

			_, err := connector.Proxy(c, reqBody, prefillAddr, decodeAddr, nil)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d failed: %w", id, err)
				return
			}

			var respBody map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
				errCh <- fmt.Errorf("goroutine %d unmarshal error: %w", id, err)
				return
			}
			if respBody["model"] != modelName {
				errCh <- fmt.Errorf("goroutine %d received response for model %v, expected %s", id, respBody["model"], modelName)
				return
			}
			errCh <- nil
		}(i)
	}

	for i := 0; i < concurrency; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("Concurrent request error: %v", err)
		}
	}
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
	connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, nil)
	// Second call — simulates retry iteration 1 on the same connector instance
	connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, nil)

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

	connector.Proxy(c, reqBody, "127.0.0.1:1", "127.0.0.1:2", nil)

	for k := range reqBody {
		if _, existed := keysBefore[k]; !existed {
			t.Errorf("Proxy() mutated caller reqBody by adding key %q", k)
		}
	}
}
