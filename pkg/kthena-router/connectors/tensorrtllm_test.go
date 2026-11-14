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
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTensorRTLLMConnectorProxyChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var prefillBody map[string]interface{}
	var decodeBody map[string]interface{}
	var prefillHits, decodeHits atomic.Int32

	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefillHits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &prefillBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":    "cmpl-ctx",
			"model": "test-model",
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"text":          "",
					"finish_reason": "length",
					"disaggregated_params": map[string]interface{}{
						"request_type":         "context_only",
						"ctx_request_id":       42,
						"first_gen_tokens":     []int{7},
						"encoded_opaque_state": "AQID",
					},
				},
			},
			"prompt_token_ids": []int{1, 2, 3},
			"usage":            map[string]interface{}{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
		})
	}))
	defer prefillSrv.Close()

	decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeHits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &decodeBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-gen",
			"object":  "chat.completion",
			"model":   "test-model",
			"choices": []interface{}{map[string]interface{}{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]interface{}{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
		})
	}))
	defer decodeSrv.Close()

	req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	original := map[string]interface{}{
		"model":      "test-model",
		"max_tokens": 16,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
		"stream": true,
	}
	reqBody := cloneReqBody(original)

	connector := NewTensorRTLLMConnector()
	if _, err := connector.Proxy(c, reqBody, prefillSrv.Listener.Addr().String(), decodeSrv.Listener.Addr().String(), 0, nil); err != nil {
		t.Fatalf("Proxy failed: %v", err)
	}

	if prefillHits.Load() != 1 || decodeHits.Load() != 1 {
		t.Fatalf("expected 1 prefill and 1 decode hit, got %d/%d", prefillHits.Load(), decodeHits.Load())
	}

	params, ok := prefillBody["disaggregated_params"].(map[string]interface{})
	if !ok {
		t.Fatalf("prefill missing disaggregated_params: %#v", prefillBody)
	}
	if params["request_type"] != "context_only" {
		t.Errorf("prefill request_type=%v", params["request_type"])
	}
	if _, hasStream := prefillBody["stream"]; hasStream {
		t.Error("prefill must not stream")
	}
	if prefillBody["max_tokens"].(float64) != 1 {
		t.Errorf("prefill max_tokens=%v", prefillBody["max_tokens"])
	}

	decodeParams, ok := decodeBody["disaggregated_params"].(map[string]interface{})
	if !ok {
		t.Fatalf("decode missing disaggregated_params: %#v", decodeBody)
	}
	if decodeParams["request_type"] != "generation_only" {
		t.Errorf("decode request_type=%v", decodeParams["request_type"])
	}
	if decodeParams["ctx_request_id"].(float64) != 42 {
		t.Errorf("decode ctx_request_id=%v", decodeParams["ctx_request_id"])
	}
	if decodeParams["encoded_opaque_state"] != "AQID" {
		t.Errorf("decode encoded_opaque_state=%v", decodeParams["encoded_opaque_state"])
	}
	ids, ok := decodeBody["prompt_token_ids"].([]interface{})
	if !ok || len(ids) != 3 {
		t.Errorf("decode prompt_token_ids=%v", decodeBody["prompt_token_ids"])
	}

	// Original body must not be mutated.
	if _, ok := original["disaggregated_params"]; ok {
		t.Error("original reqBody must not gain disaggregated_params")
	}
	if original["stream"] != true {
		t.Error("original stream flag mutated")
	}
}

func TestTensorRTLLMConnectorProxyCompletionsPromptRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var decodeBody map[string]interface{}
	prefillSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"finish_reason": "length",
					"disaggregated_params": map[string]interface{}{
						"request_type":   "context_only",
						"ctx_request_id": 9,
					},
				},
			},
			"prompt_token_ids": []int{11, 22},
		})
	}))
	defer prefillSrv.Close()

	decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &decodeBody)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{"index": 0, "text": "x", "finish_reason": "stop"}},
		})
	}))
	defer decodeSrv.Close()

	req, _ := http.NewRequest("POST", "/v1/completions", nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	reqBody := map[string]interface{}{
		"model":      "m",
		"prompt":     "hello",
		"max_tokens": 8,
	}
	if _, err := NewTensorRTLLMConnector().Proxy(c, reqBody, prefillSrv.Listener.Addr().String(), decodeSrv.Listener.Addr().String(), 0, nil); err != nil {
		t.Fatalf("Proxy failed: %v", err)
	}
	prompt, ok := decodeBody["prompt"].([]interface{})
	if !ok || len(prompt) != 2 {
		t.Fatalf("expected prompt token ids, got %#v", decodeBody["prompt"])
	}
}

func TestTensorRTLLMConnectorEarlyReturnOnStop(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newPrefill := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "cmpl-early",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"text":          "done",
						"finish_reason": "stop",
						"disaggregated_params": map[string]interface{}{
							"request_type":   "context_only",
							"ctx_request_id": 1,
						},
					},
				},
			})
		}))
	}

	t.Run("non-stream", func(t *testing.T) {
		var decodeHits atomic.Int32
		prefillSrv := newPrefill()
		defer prefillSrv.Close()
		decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			decodeHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer decodeSrv.Close()

		req, _ := http.NewRequest("POST", "/v1/completions", nil)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = req

		if _, err := NewTensorRTLLMConnector().Proxy(c, map[string]interface{}{"model": "m", "prompt": "x", "max_tokens": 4},
			prefillSrv.Listener.Addr().String(), decodeSrv.Listener.Addr().String(), 0, nil); err != nil {
			t.Fatalf("Proxy failed: %v", err)
		}
		if decodeHits.Load() != 0 {
			t.Fatalf("decode should be skipped on early stop, hits=%d", decodeHits.Load())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type=%q, want application/json", ct)
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("parse response: %v", err)
		}
		choice := out["choices"].([]interface{})[0].(map[string]interface{})
		if _, ok := choice["disaggregated_params"]; ok {
			t.Error("early-return response must strip disaggregated_params")
		}
	})

	t.Run("stream", func(t *testing.T) {
		var decodeHits atomic.Int32
		prefillSrv := newPrefill()
		defer prefillSrv.Close()
		decodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			decodeHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer decodeSrv.Close()

		req, _ := http.NewRequest("POST", "/v1/completions", nil)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = req

		if _, err := NewTensorRTLLMConnector().Proxy(c, map[string]interface{}{
			"model": "m", "prompt": "x", "max_tokens": 4, "stream": true,
		}, prefillSrv.Listener.Addr().String(), decodeSrv.Listener.Addr().String(), 0, nil); err != nil {
			t.Fatalf("Proxy failed: %v", err)
		}
		if decodeHits.Load() != 0 {
			t.Fatalf("decode should be skipped on early stop, hits=%d", decodeHits.Load())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type=%q, want text/event-stream", ct)
		}
		if got := rec.Body.String(); got != "data: [DONE]\n\n" {
			t.Fatalf("body=%q, want SSE DONE only", got)
		}
	})
}

func TestNeedsGeneration(t *testing.T) {
	if !needsGeneration("length") || !needsGeneration("") || !needsGeneration("not_finished") {
		t.Fatal("expected generation for length/empty/not_finished")
	}
	if needsGeneration("stop") || needsGeneration("content_filter") {
		t.Fatal("expected no generation for terminal reasons")
	}
}
