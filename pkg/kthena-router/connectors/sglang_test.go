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
	if _, err := connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, nil); err != nil {
		t.Fatalf("first Proxy call failed: %v", err)
	}
	// The second call simulates another attempt on the same connector.
	if _, err := connector.Proxy(makeCtx(), reqBody, prefillAddr, decodeAddr, nil); err != nil {
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

	_, _ = connector.Proxy(c, reqBody, "127.0.0.1:1", "127.0.0.1:2", nil)

	for k := range reqBody {
		if _, existed := keysBefore[k]; !existed {
			t.Errorf("Proxy() mutated caller reqBody by adding key %q", k)
		}
	}
}
