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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
)

// TestSGLangConnectorRetryIsolation checks that calling Proxy() twice on the
// same connector instance rebuilds both request bodies and assigns a fresh
// bootstrap room to each PD attempt.
func TestSGLangConnectorRetryIsolation(t *testing.T) {
	var prefillCalls, decodeCalls int32
	var prefillBodyLens [2]int64
	var decodeBodyLens [2]int64
	var prefillRooms [2]int64
	var decodeRooms [2]int64

	prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&prefillCalls, 1) - 1
		body, _ := io.ReadAll(r.Body)
		if idx < 2 {
			prefillBodyLens[idx] = int64(len(body))
			var payload struct {
				BootstrapRoom int64 `json:"bootstrap_room"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("failed to decode prefill request: %v", err)
			}
			prefillRooms[idx] = payload.BootstrapRoom
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer prefillServer.Close()

	decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&decodeCalls, 1) - 1
		body, _ := io.ReadAll(r.Body)
		if idx < 2 {
			decodeBodyLens[idx] = int64(len(body))
			var payload struct {
				BootstrapRoom int64 `json:"bootstrap_room"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("failed to decode decode request: %v", err)
			}
			decodeRooms[idx] = payload.BootstrapRoom
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1}}`))
	}))
	defer decodeServer.Close()

	connector := NewSGLangConnector().(*SGLangConnector)
	var nextRoom int64
	connector.newBootstrapRoom = func() int64 {
		return atomic.AddInt64(&nextRoom, 1)
	}

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
	decodeAddr := decodeServer.Listener.Addr().String()

	// First call simulates the initial PD attempt.
	if _, err := connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, 0, nil); err != nil {
		t.Fatalf("first Proxy call failed: %v", err)
	}
	// The second call simulates another attempt on the same connector.
	if _, err := connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, 0, nil); err != nil {
		t.Fatalf("second Proxy call failed: %v", err)
	}

	if prefillBodyLens[0] == 0 {
		t.Error("first Proxy call sent empty body to prefill backend")
	}
	if prefillBodyLens[1] == 0 {
		t.Error("second Proxy call sent empty body to prefill backend — request body was drained and reused")
	}
	if decodeBodyLens[0] == 0 {
		t.Error("first Proxy call sent empty body to decode backend")
	}
	if decodeBodyLens[1] == 0 {
		t.Error("second Proxy call sent empty body to decode backend — request body was drained and reused")
	}
	for i := range prefillRooms {
		if prefillRooms[i] != decodeRooms[i] {
			t.Errorf("attempt %d used different bootstrap rooms: prefill=%d decode=%d", i, prefillRooms[i], decodeRooms[i])
		}
	}
	if prefillRooms[0] == prefillRooms[1] {
		t.Errorf("retry reused bootstrap room %d", prefillRooms[0])
	}
}

func TestSGLangConnectorPrefillTimeoutCancelsDecode(t *testing.T) {
	releasePrefill := make(chan struct{})
	prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releasePrefill:
		}
	}))
	defer func() {
		close(releasePrefill)
		prefillServer.Close()
	}()

	decodeCancelled := make(chan struct{})
	decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(decodeCancelled)
	}))
	defer decodeServer.Close()

	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, "/v1/chat/completions", nil)
	c, _ := gin.CreateTestContext(CreateTestResponseRecorder())
	c.Request = req

	reqBody := map[string]interface{}{
		"model":      "test-model",
		"stream":     true,
		"max_tokens": 100,
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}

	connector := NewSGLangConnector()
	start := time.Now()
	_, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 100*time.Millisecond, nil)
	if err == nil {
		t.Fatal("Proxy() succeeded after prefill timed out")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Proxy() took %v after prefill timed out", elapsed)
	}

	select {
	case <-decodeCancelled:
	case <-time.After(time.Second):
		t.Fatal("decode request was not cancelled after prefill timed out")
	}
}

// TestSGLangConnectorPrefillBodyResponsesAPIURLRewrite covers both HTTPRoute
// URLRewrite directions for SGLang's own prefill-body preparation: the canonical
// public path rewritten to a custom upstream path, and a custom public path
// rewritten to the canonical upstream path. Either way the prefill request must
// be shaped for the Responses API (max_output_tokens), not Chat Completions
// (max_tokens).
func TestSGLangConnectorPrefillBodyResponsesAPIURLRewrite(t *testing.T) {
	tests := []struct {
		name         string
		originalPath string
		currentPath  string
		isResponses  bool
	}{
		{
			name:         "canonical public path rewritten to custom upstream path",
			originalPath: "/v1/responses",
			currentPath:  "/backend/rewritten-path",
			isResponses:  true,
		},
		{
			name:         "custom public path rewritten to canonical upstream path",
			originalPath: "/llm/v1/responses",
			currentPath:  "/v1/responses",
			isResponses:  true,
		},
		{
			name:         "chat completions unaffected",
			originalPath: "/v1/chat/completions",
			currentPath:  "/v1/chat/completions",
			isResponses:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prefillBody map[string]interface{}
			prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &prefillBody)
				w.WriteHeader(http.StatusOK)
			}))
			defer prefillServer.Close()

			decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1}}`))
			}))
			defer decodeServer.Close()

			req, _ := http.NewRequest("POST", tt.currentPath, nil)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			// The original client-facing path is recorded on the access-log context,
			// matching what AccessLogMiddleware captures before URLRewrite runs.
			accessCtx := accesslog.NewAccessLogContext("req-1", http.MethodPost, tt.originalPath, "HTTP/1.1", "")
			c.Set(accesslog.AccessLogContextKey, accessCtx)

			reqBody := map[string]interface{}{
				"model": "test-model",
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "hello"},
				},
			}

			connector := NewSGLangConnector()
			if _, err := connector.Proxy(c, reqBody, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String(), 0, nil); err != nil {
				t.Fatalf("Proxy() failed: %v", err)
			}

			if prefillBody == nil {
				t.Fatal("prefill server never received a request body")
			}
			_, hasMaxOutputTokens := prefillBody["max_output_tokens"]
			_, hasMaxTokens := prefillBody["max_tokens"]
			if hasMaxOutputTokens != tt.isResponses {
				t.Errorf("max_output_tokens present = %v, want %v (body=%v)", hasMaxOutputTokens, tt.isResponses, prefillBody)
			}
			if hasMaxTokens == tt.isResponses {
				t.Errorf("max_tokens present = %v, want %v (body=%v)", hasMaxTokens, !tt.isResponses, prefillBody)
			}
		})
	}
}

// TestSGLangConnectorReqBodyNotMutated checks that Proxy() does not mutate the
// caller's reqBody map. proxyToPDDisaggregated passes the same modelRequest
// across all retry iterations, so mutations would bleed between retries.
func TestSGLangConnectorReqBodyNotMutated(t *testing.T) {
	connector := NewSGLangConnector()

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

	keysBefore := make(map[string]struct{})
	for k := range reqBody {
		keysBefore[k] = struct{}{}
	}

	_, _ = connector.Proxy(c, reqBody, "127.0.0.1:1", "127.0.0.1:2", 0, nil)

	for k := range reqBody {
		if _, existed := keysBefore[k]; !existed {
			t.Errorf("Proxy() mutated caller reqBody by adding key %q", k)
		}
	}
}
