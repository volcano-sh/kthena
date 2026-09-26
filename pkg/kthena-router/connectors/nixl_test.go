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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

func TestNIXLConnectorProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Test non-streaming request
	t.Run("NonStreamingRequest", func(t *testing.T) {
		connector := NewNIXLConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kv_transfer_params": {"buffer_id": 123}}`))
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":10}}`))
		}))
		defer decodeServer.Close()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
		c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
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

		tokens, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
		assert.NoError(t, err)
		assert.Equal(t, 10, tokens)

		// Prefill request should have max_tokens set to 1 and stream removed
		assert.Equal(t, float64(1), prefillBody["max_tokens"])
		assert.Nil(t, prefillBody["stream"])
		assert.Nil(t, prefillBody["stream_options"])
		assert.Equal(t, "test-model", prefillBody["model"])

		// Prefill request should have kv_transfer_params
		params, ok := prefillBody["kv_transfer_params"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, true, params["do_remote_decode"])
		assert.Equal(t, false, params["do_remote_prefill"])

		// Decode request should have include_usage and kv_transfer_params from prefill response
		assert.Equal(t, true, decodeBody["include_usage"])
		assert.Equal(t, float64(100), decodeBody["max_tokens"])
		assert.Equal(t, "test-model", decodeBody["model"])
		decodeParams, ok := decodeBody["kv_transfer_params"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, float64(123), decodeParams["buffer_id"])
	})

	// Test streaming request
	t.Run("StreamingRequest", func(t *testing.T) {
		connector := NewNIXLConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kv_transfer_params": {"buffer_id": 456}}`))
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeBody)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"))
		}))
		defer decodeServer.Close()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
		c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
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

		tokens, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
		assert.NoError(t, err)
		assert.Equal(t, 5, tokens)

		// Prefill request checks
		assert.Equal(t, float64(1), prefillBody["max_tokens"])
		assert.Nil(t, prefillBody["stream"])
		assert.Nil(t, prefillBody["stream_options"])

		// For streaming requests, verify that token usage context was set
		val, exists := c.Get(common.TokenUsageKey)
		assert.True(t, exists)
		assert.Equal(t, true, val)

		// Decode request should preserve stream: true and have stream_options with include_usage
		assert.Equal(t, true, decodeBody["stream"])
		streamOpts, ok := decodeBody["stream_options"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, true, streamOpts["include_usage"])
	})

	// Test streaming request with existing stream_options
	t.Run("StreamingRequestWithStreamOptions", func(t *testing.T) {
		connector := NewNIXLConnector()

		var decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kv_transfer_params": {}}`))
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeBody)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer decodeServer.Close()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
		c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
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

		_, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
		assert.NoError(t, err)

		// For streaming requests with existing stream_options, token usage should not be added to context
		val, exists := c.Get(common.TokenUsageKey)
		assert.False(t, exists && val == true)

		// Verify decode request body preserves existing stream_options
		streamOpts, ok := decodeBody["stream_options"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, true, streamOpts["include_usage"])
	})

	// Test max_completion_tokens handling
	t.Run("MaxCompletionTokensHandling", func(t *testing.T) {
		connector := NewNIXLConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kv_transfer_params": {}}`))
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &decodeBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":10}}`))
		}))
		defer decodeServer.Close()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
		c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
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

		_, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
		assert.NoError(t, err)

		// Prefill request handling of max_completion_tokens
		assert.Equal(t, float64(1), prefillBody["max_tokens"])
		assert.Equal(t, float64(1), prefillBody["max_completion_tokens"])

		// Decode request preserves original max_completion_tokens
		assert.Equal(t, float64(50), decodeBody["max_completion_tokens"])
		assert.Equal(t, true, decodeBody["include_usage"])
	})

	// Test NIXL-specific kv_transfer_params structure
	t.Run("KVTransferParamsStructure", func(t *testing.T) {
		connector := NewNIXLConnector()

		var prefillBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kv_transfer_params": {}}`))
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":10}}`))
		}))
		defer decodeServer.Close()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
		c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
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

		_, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
		assert.NoError(t, err)

		// Verify detailed kv_transfer_params structure in prefill request
		params, ok := prefillBody["kv_transfer_params"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, true, params["do_remote_decode"])
		assert.Equal(t, false, params["do_remote_prefill"])
	})

	// Test concurrent requests
	t.Run("ConcurrentRequests", func(t *testing.T) {
		connector := NewNIXLConnector()

		const concurrency = 20
		var (
			mu            sync.Mutex
			prefillBodies = make(map[string]map[string]interface{})
			decodeBodies  = make(map[string]map[string]interface{})
		)

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			_ = json.Unmarshal(b, &body)
			model, _ := body["model"].(string)

			mu.Lock()
			prefillBodies[model] = body
			mu.Unlock()

			var idx int
			_, _ = fmt.Sscanf(model, "model-%d", &idx)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"kv_transfer_params": map[string]interface{}{
					"buffer_id": idx,
				},
			})
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			_ = json.Unmarshal(b, &body)
			model, _ := body["model"].(string)

			mu.Lock()
			decodeBodies[model] = body
			mu.Unlock()

			var idx int
			_, _ = fmt.Sscanf(model, "model-%d", &idx)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp := map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"message": map[string]interface{}{
							"content": fmt.Sprintf("reply-%d", idx),
						},
					},
				},
				"usage": map[string]interface{}{
					"completion_tokens": 10 + idx,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer decodeServer.Close()

		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				recorder := CreateTestResponseRecorder()
				c, _ := gin.CreateTestContext(recorder)
				req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
				c.Request = req

				modelName := fmt.Sprintf("model-%d", idx)
				reqBody := map[string]interface{}{
					"model":      modelName,
					"max_tokens": float64(100 + idx),
					"messages": []interface{}{
						map[string]interface{}{"role": "user", "content": fmt.Sprintf("msg-%d", idx)},
					},
				}
				tokens, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
				assert.NoError(t, err)
				assert.Equal(t, 10+idx, tokens)
				assert.Contains(t, recorder.Body.String(), fmt.Sprintf("reply-%d", idx))
			}(i)
		}
		wg.Wait()

		// Verify that all concurrent requests were received with their distinct payloads and kv_transfer_params
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, concurrency, len(prefillBodies))
		assert.Equal(t, concurrency, len(decodeBodies))

		for i := 0; i < concurrency; i++ {
			modelName := fmt.Sprintf("model-%d", i)

			prefillBody, ok := prefillBodies[modelName]
			assert.True(t, ok, "prefill body for %s missing", modelName)
			assert.Equal(t, float64(1), prefillBody["max_tokens"])
			params, ok := prefillBody["kv_transfer_params"].(map[string]interface{})
			assert.True(t, ok)
			assert.Equal(t, true, params["do_remote_decode"])
			assert.Equal(t, false, params["do_remote_prefill"])

			decodeBody, ok := decodeBodies[modelName]
			assert.True(t, ok, "decode body for %s missing", modelName)
			assert.Equal(t, float64(100+i), decodeBody["max_tokens"])
			assert.Equal(t, true, decodeBody["include_usage"])
			decodeParams, ok := decodeBody["kv_transfer_params"].(map[string]interface{})
			assert.True(t, ok)
			assert.Equal(t, float64(i), decodeParams["buffer_id"])
		}
	})
}

func TestNIXLPrefillTimeoutIncludesResponseBody(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"kv_transfer_params":`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	connector := NewNIXLConnector().(*NIXLConnector)
	start := time.Now()
	_, err = connector.prefill(req, server.Listener.Addr().String(), 100*time.Millisecond, upstreamTransport)
	if err == nil {
		t.Fatal("prefill succeeded after response body stalled")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("prefill took %v after response body stalled", elapsed)
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
	connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, 0, nil)
	// Second call — simulates retry iteration 1 on the same connector instance
	connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, 0, nil)

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

	connector.Proxy(c, reqBody, "127.0.0.1:1", "127.0.0.1:2", 0, nil)

	for k := range reqBody {
		if _, existed := keysBefore[k]; !existed {
			t.Errorf("Proxy() mutated caller reqBody by adding key %q", k)
		}
	}
}
