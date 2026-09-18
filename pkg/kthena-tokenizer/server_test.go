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

package tokenizer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticReady bool

func (r staticReady) HasSynced() bool { return bool(r) }

// newTestServer returns a frontend whose manager routes "served" to the given
// fake renderer backend.
func newTestServer(t *testing.T, backend *httptest.Server) *Server {
	t.Helper()
	u, err := url.Parse(backend.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	cfg := Config{ProxyTimeout: 5 * time.Second}
	m := NewRendererManager(cfg)
	m.renderers["served"] = &renderer{model: "served", port: port, status: StatusReady}
	return NewServer(cfg, m, staticReady(true))
}

func TestServerProxiesTokenize(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/tokenize", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"served"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count": 3, "tokens": [1, 2, 3], "max_model_len": 4096}`))
	}))
	defer backend.Close()

	frontend := httptest.NewServer(newTestServer(t, backend).Handler())
	defer frontend.Close()

	resp, err := http.Post(frontend.URL+"/tokenize", "application/json",
		strings.NewReader(`{"model": "served", "prompt": "hello"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var parsed struct {
		Count  int   `json:"count"`
		Tokens []int `json:"tokens"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
	assert.Equal(t, 3, parsed.Count)
	assert.Equal(t, []int{1, 2, 3}, parsed.Tokens)
}

func TestServerRejectsBadRequests(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	frontend := httptest.NewServer(newTestServer(t, backend).Handler())
	defer frontend.Close()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"invalid JSON", "not-json", http.StatusBadRequest},
		{"missing model", `{"prompt": "hello"}`, http.StatusBadRequest},
		{"unknown model", `{"model": "other", "prompt": "hi"}`, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(frontend.URL+"/tokenize", "application/json", strings.NewReader(tt.body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, tt.want, resp.StatusCode)
		})
	}

	t.Run("method not allowed", func(t *testing.T) {
		resp, err := http.Get(frontend.URL + "/tokenize")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

func TestServerReportsBackendFailure(t *testing.T) {
	backend := httptest.NewServer(nil)
	frontend := httptest.NewServer(newTestServer(t, backend).Handler())
	defer frontend.Close()
	backend.Close() // renderer marked ready but unreachable

	resp, err := http.Post(frontend.URL+"/tokenize", "application/json",
		strings.NewReader(`{"model": "served", "prompt": "hello"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestServerModelsAndHealth(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	frontend := httptest.NewServer(newTestServer(t, backend).Handler())
	defer frontend.Close()

	resp, err := http.Get(frontend.URL + "/models")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var parsed struct {
		Models []RendererInfo `json:"models"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
	require.Len(t, parsed.Models, 1)
	assert.Equal(t, "served", parsed.Models[0].Model)
	assert.Equal(t, string(StatusReady), parsed.Models[0].Status)

	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK} {
		resp, err := http.Get(frontend.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, want, resp.StatusCode, path)
	}
}

func TestServerReadyzWaitsForSync(t *testing.T) {
	cfg := Config{ProxyTimeout: time.Second}
	server := NewServer(cfg, NewRendererManager(cfg), staticReady(false))
	frontend := httptest.NewServer(server.Handler())
	defer frontend.Close()

	resp, err := http.Get(frontend.URL + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
