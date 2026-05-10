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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

func TestNIXLConnectorProxy(t *testing.T) {
	t.Run("BuildsPrefillRequest", func(t *testing.T) {
		connector := NewNIXLConnector().(*NIXLConnector)
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

		prefillReq, err := connector.buildPrefillRequest(req, map[string]interface{}{
			"model":                 "test-model",
			"stream":                true,
			"stream_options":        map[string]interface{}{"include_usage": true},
			"max_tokens":            100,
			"max_completion_tokens": 50,
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "test message"},
			},
		})
		if err != nil {
			t.Fatalf("buildPrefillRequest returned error: %v", err)
		}

		prefillBody, err := parseRequestBody(prefillReq)
		if err != nil {
			t.Fatalf("Failed to parse prefill request body: %v", err)
		}

		if maxTokens, ok := prefillBody["max_tokens"].(float64); !ok || maxTokens != 1 {
			t.Errorf("Expected prefill max_tokens to be 1, got %v", prefillBody["max_tokens"])
		}
		if maxCompletionTokens, ok := prefillBody["max_completion_tokens"].(float64); !ok || maxCompletionTokens != 1 {
			t.Errorf("Expected prefill max_completion_tokens to be 1, got %v", prefillBody["max_completion_tokens"])
		}
		if _, hasStream := prefillBody["stream"]; hasStream {
			t.Error("Expected prefill request to remove stream")
		}
		if _, hasStreamOptions := prefillBody["stream_options"]; hasStreamOptions {
			t.Error("Expected prefill request to remove stream_options")
		}
		if model := prefillBody["model"]; model != "test-model" {
			t.Errorf("Expected prefill model to be preserved, got %v", model)
		}

		params, ok := prefillBody["kv_transfer_params"].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected kv_transfer_params to be a map, got %T", prefillBody["kv_transfer_params"])
		}
		if params["do_remote_decode"] != true {
			t.Errorf("Expected do_remote_decode to be true, got %v", params["do_remote_decode"])
		}
		if params["do_remote_prefill"] != false {
			t.Errorf("Expected do_remote_prefill to be false, got %v", params["do_remote_prefill"])
		}
	})

	t.Run("BuildsNonStreamingDecodeRequest", func(t *testing.T) {
		connector := NewNIXLConnector().(*NIXLConnector)
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		decodeBody := addTokenUsage(c, map[string]interface{}{
			"model":      "test-model",
			"max_tokens": 100,
		})

		decodeReq, err := connector.buildDecodeRequest(c, decodeBody, map[string]interface{}{"cache": "ready"})
		if err != nil {
			t.Fatalf("buildDecodeRequest returned error: %v", err)
		}

		parsedBody, err := parseRequestBody(decodeReq)
		if err != nil {
			t.Fatalf("Failed to parse decode request body: %v", err)
		}
		if parsedBody["include_usage"] != true {
			t.Errorf("Expected non-streaming decode request to include usage, got %v", parsedBody["include_usage"])
		}
		if maxTokens, ok := parsedBody["max_tokens"].(float64); !ok || maxTokens != 100 {
			t.Errorf("Expected decode max_tokens to stay 100, got %v", parsedBody["max_tokens"])
		}
		if params, ok := parsedBody["kv_transfer_params"].(map[string]interface{}); !ok || params["cache"] != "ready" {
			t.Errorf("Expected decode kv_transfer_params to be attached, got %v", parsedBody["kv_transfer_params"])
		}
	})

	t.Run("BuildsStreamingDecodeRequest", func(t *testing.T) {
		connector := NewNIXLConnector().(*NIXLConnector)
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		decodeBody := addTokenUsage(c, map[string]interface{}{
			"model":      "test-model",
			"stream":     true,
			"max_tokens": 100,
		})
		if val, exists := c.Get(common.TokenUsageKey); !exists || val != true {
			t.Error("Expected token usage to be set in context for streaming request")
		}

		decodeReq, err := connector.buildDecodeRequest(c, decodeBody, nil)
		if err != nil {
			t.Fatalf("buildDecodeRequest returned error: %v", err)
		}
		parsedBody, err := parseRequestBody(decodeReq)
		if err != nil {
			t.Fatalf("Failed to parse decode request body: %v", err)
		}
		if parsedBody["stream"] != true {
			t.Errorf("Expected decode stream to stay true, got %v", parsedBody["stream"])
		}
		streamOptions, ok := parsedBody["stream_options"].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected stream_options to be a map, got %T", parsedBody["stream_options"])
		}
		if streamOptions["include_usage"] != true {
			t.Errorf("Expected stream_options include_usage to be true, got %v", streamOptions["include_usage"])
		}
	})

	t.Run("KeepsExistingStreamOptions", func(t *testing.T) {
		connector := NewNIXLConnector().(*NIXLConnector)
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		decodeBody := addTokenUsage(c, map[string]interface{}{
			"model":          "test-model",
			"stream":         true,
			"stream_options": map[string]interface{}{"include_usage": true},
		})
		if val, exists := c.Get(common.TokenUsageKey); exists && val == true {
			t.Error("Did not expect token usage to be set when stream_options already requests usage")
		}

		decodeReq, err := connector.buildDecodeRequest(c, decodeBody, nil)
		if err != nil {
			t.Fatalf("buildDecodeRequest returned error: %v", err)
		}
		parsedBody, err := parseRequestBody(decodeReq)
		if err != nil {
			t.Fatalf("Failed to parse decode request body: %v", err)
		}
		streamOptions, ok := parsedBody["stream_options"].(map[string]interface{})
		if !ok {
			t.Fatalf("Expected stream_options to be a map, got %T", parsedBody["stream_options"])
		}
		if streamOptions["include_usage"] != true {
			t.Errorf("Expected existing stream_options include_usage to be preserved, got %v", streamOptions["include_usage"])
		}
	})
}

func TestNIXLConnectorBuildPrefillRequestMarshalError(t *testing.T) {
	connector := NewNIXLConnector().(*NIXLConnector)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	result, err := connector.buildPrefillRequest(req, map[string]interface{}{
		"model":       "test-model",
		"bad_payload": make(chan struct{}),
	})

	if err == nil {
		t.Fatal("Expected marshal error")
	}
	if result != nil {
		t.Fatal("Expected nil request on marshal error")
	}
	if got := err.Error(); !strings.Contains(got, "failed to marshal prefill request body") {
		t.Fatalf("Expected prefill marshal error, got %q", got)
	}
}

func TestNIXLConnectorBuildDecodeRequestMarshalError(t *testing.T) {
	connector := NewNIXLConnector().(*NIXLConnector)
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	result, err := connector.buildDecodeRequest(c, map[string]interface{}{
		"model":       "test-model",
		"bad_payload": func() {},
	}, nil)

	if err == nil {
		t.Fatal("Expected marshal error")
	}
	if result != nil {
		t.Fatal("Expected nil request on marshal error")
	}
	if got := err.Error(); !strings.Contains(got, "failed to marshal decode request body") {
		t.Fatalf("Expected decode marshal error, got %q", got)
	}
}

func TestNIXLConnectorProxyReturnsMarshalError(t *testing.T) {
	connector := NewNIXLConnector()
	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	_, err := connector.Proxy(c, map[string]interface{}{
		"model":       "test-model",
		"bad_payload": make(chan struct{}),
	}, "127.0.0.1:1", "127.0.0.1:2")

	if err == nil {
		t.Fatal("Expected marshal error")
	}
	if got := err.Error(); !strings.Contains(got, "failed to marshal prefill request body") {
		t.Fatalf("Expected prefill marshal error, got %q", got)
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
