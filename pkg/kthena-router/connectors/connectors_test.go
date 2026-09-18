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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
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
	gin.SetMode(gin.TestMode)

	// Test non-streaming request
	t.Run("NonStreamingRequest", func(t *testing.T) {
		connector := NewHTTPConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
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

		// Decode request should have include_usage set for non-streaming requests
		assert.Equal(t, true, decodeBody["include_usage"])
		assert.Equal(t, float64(100), decodeBody["max_tokens"])
		assert.Equal(t, "test-model", decodeBody["model"])
	})

	// Test streaming request
	t.Run("StreamingRequest", func(t *testing.T) {
		connector := NewHTTPConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
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

		// Prefill request should have max_tokens set to 1 and stream removed
		assert.Equal(t, float64(1), prefillBody["max_tokens"])
		assert.Nil(t, prefillBody["stream"])

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

	// Test stream_options handling
	t.Run("StreamOptionsHandling", func(t *testing.T) {
		connector := NewHTTPConnector()

		var decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
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

		// Decode request should preserve existing stream_options
		streamOpts, ok := decodeBody["stream_options"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, true, streamOpts["include_usage"])
	})

	// Test max_completion_tokens handling
	t.Run("MaxCompletionTokensHandling", func(t *testing.T) {
		connector := NewHTTPConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
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

		// Prefill request should have max_tokens set to 1 and max_completion_tokens set to 1
		assert.Equal(t, float64(1), prefillBody["max_tokens"])
		assert.Equal(t, float64(1), prefillBody["max_completion_tokens"])

		// Decode request should preserve original max_completion_tokens
		assert.Equal(t, float64(50), decodeBody["max_completion_tokens"])
		assert.Equal(t, true, decodeBody["include_usage"])
	})

	// Test request body modifications in detail
	t.Run("RequestBodyModifications", func(t *testing.T) {
		connector := NewHTTPConnector()

		var prefillBody, decodeBody map[string]interface{}
		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prefillBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
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

		reqBodyCopy := make(map[string]interface{})
		for k, v := range originalReqBody {
			reqBodyCopy[k] = v
		}

		_, err := connector.Proxy(c, reqBodyCopy, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
		assert.NoError(t, err)

		// Prefill assertions
		assert.Equal(t, float64(1), prefillBody["max_tokens"])
		assert.Nil(t, prefillBody["stream"])
		assert.Nil(t, prefillBody["stream_options"])

		// Decode assertions
		assert.Equal(t, true, decodeBody["stream"])
		assert.Equal(t, float64(200), decodeBody["max_tokens"])
		opts, ok := decodeBody["stream_options"].(map[string]interface{})
		assert.True(t, ok)
		assert.Equal(t, true, opts["include_usage"])
	})

	// Test concurrent requests
	t.Run("ConcurrentRequests", func(t *testing.T) {
		connector := NewHTTPConnector()

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":5}}`))
		}))
		defer decodeServer.Close()

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
				req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{}`))
				c.Request = req

				reqBody := map[string]interface{}{
					"model":      fmt.Sprintf("model-%d", idx),
					"max_tokens": float64(100 + idx),
					"messages": []interface{}{
						map[string]interface{}{"role": "user", "content": fmt.Sprintf("msg-%d", idx)},
					},
				}
				tokens, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil)
				assert.NoError(t, err)
				assert.Equal(t, 5, tokens)
			}(i)
		}
		wg.Wait()
	})
}
