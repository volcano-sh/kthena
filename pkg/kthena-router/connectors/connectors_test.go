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

func TestHTTPConnectorProxy(t *testing.T) {
	// Test non-streaming request
	t.Run("NonStreamingRequest", func(t *testing.T) {
		var prefillReceivedBody map[string]interface{}
		var decodeReceivedBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillReceivedBody)
			w.WriteHeader(http.StatusOK)
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeReceivedBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":5}}`))
		}))
		defer decodeServer.Close()

		connector := NewHTTPConnector()

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
		if tokens != 5 {
			t.Errorf("Expected 5 output tokens, got %d", tokens)
		}

		// Verify prefill request body fields
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
		if model, ok := prefillReceivedBody["model"]; !ok || model != "test-model" {
			t.Errorf("Expected prefill request to have model 'test-model', got %v", model)
		}

		// Verify decode request body fields
		if includeUsage, ok := decodeReceivedBody["include_usage"]; !ok || includeUsage != true {
			t.Errorf("Expected decode request include_usage to be true, got %v", includeUsage)
		}
		if maxTokens, ok := decodeReceivedBody["max_tokens"]; !ok {
			t.Error("Expected decode request to have max_tokens field")
		} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 100.0 {
			t.Errorf("Expected decode request max_tokens to be 100, got %v", maxTokens)
		}
		if model, ok := decodeReceivedBody["model"]; !ok || model != "test-model" {
			t.Errorf("Expected decode request to have model 'test-model', got %v", model)
		}
	})

	// Test streaming request
	t.Run("StreamingRequest", func(t *testing.T) {
		var prefillReceivedBody map[string]interface{}
		var decodeReceivedBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillReceivedBody)
			w.WriteHeader(http.StatusOK)
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

		connector := NewHTTPConnector()

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

		// Verify prefill request body fields
		if maxTokens, ok := prefillReceivedBody["max_tokens"]; !ok {
			t.Error("Expected prefill request to have max_tokens field")
		} else if maxTokensFloat, ok := maxTokens.(float64); !ok || maxTokensFloat != 1.0 {
			t.Errorf("Expected prefill request max_tokens to be 1, got %v", maxTokens)
		}
		if _, hasStream := prefillReceivedBody["stream"]; hasStream {
			t.Error("Expected prefill request to not have stream field")
		}

		// Verify decode request body fields
		if stream, ok := decodeReceivedBody["stream"]; !ok || stream != true {
			t.Errorf("Expected decode request stream to be true, got %v", stream)
		}
		if streamOptions, ok := decodeReceivedBody["stream_options"]; !ok {
			t.Error("Expected decode request to have stream_options")
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
			w.WriteHeader(http.StatusOK)
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

		connector := NewHTTPConnector()

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
			t.Error("Expected decode request to preserve existing stream_options")
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
			w.WriteHeader(http.StatusOK)
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

		connector := NewHTTPConnector()

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
			t.Error("Expected decode request to have max_completion_tokens field")
		} else if maxCompletionTokensFloat, ok := maxCompletionTokens.(float64); !ok || maxCompletionTokensFloat != 50.0 {
			t.Errorf("Expected decode request max_completion_tokens to be 50, got %v", maxCompletionTokens)
		}
	})
}

// TestHTTPConnector_ConcurrentThreadSafety verifies that concurrent requests through
// the same HTTPConnector instance do not race or corrupt each other's payloads.
func TestHTTPConnector_ConcurrentThreadSafety(t *testing.T) {
	prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer prefillServer.Close()

	decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)

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

	connector := NewHTTPConnector()
	prefillAddr := prefillServer.Listener.Addr().String()
	decodeAddr := decodeServer.Listener.Addr().String()

	const concurrency = 10
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			modelName := fmt.Sprintf("model-%d", id)
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
