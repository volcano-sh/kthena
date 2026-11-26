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
	"sync"
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

// newTestServerWithCfg builds a test server using an explicit Config so
// non-default fields (e.g. RendererMaxInFlight) can be set.
func newTestServerWithCfg(t *testing.T, backend *httptest.Server, cfg Config) *Server {
	t.Helper()
	u, err := url.Parse(backend.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	if cfg.ProxyTimeout == 0 {
		cfg.ProxyTimeout = 5 * time.Second
	}
	m := NewRendererManager(cfg)
	m.renderers["served"] = &renderer{model: "served", port: port, status: StatusReady}
	return NewServer(cfg, m, staticReady(true))
}

// TestServerInFlightLimitRejectsConcurrentRequest verifies that with
// RendererMaxInFlight=1 a second concurrent request is rejected with 503
// while the first is still being proxied.
func TestServerInFlightLimitRejectsConcurrentRequest(t *testing.T) {
	// arrived is signalled by the backend when the first request arrives so
	// we can send the second one only after the in-flight counter is 1.
	arrived := make(chan struct{}, 1)
	// hold gates the backend handler: the first request blocks until released.
	hold := make(chan struct{})
	var releaseHoldOnce sync.Once
	releaseHold := func() { releaseHoldOnce.Do(func() { close(hold) }) }
	// Always release on exit so backend goroutines are never stranded.
	defer releaseHold()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-hold
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tokens": [], "count": 0}`))
	}))
	defer backend.Close()

	cfg := Config{ProxyTimeout: 5 * time.Second, RendererMaxInFlight: 1}
	frontend := httptest.NewServer(newTestServerWithCfg(t, backend, cfg).Handler())
	defer frontend.Close()

	// First request: should be admitted (in-flight = 1).
	firstStatus := make(chan int, 1)
	go func() {
		resp, err := http.Post(frontend.URL+"/tokenize", "application/json",
			strings.NewReader(`{"model": "served", "prompt": "hello"}`))
		if err != nil {
			firstStatus <- -1
			return
		}
		defer resp.Body.Close()
		firstStatus <- resp.StatusCode
	}()

	// Wait until the first request has actually reached the backend and
	// the in-flight counter is 1.
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach the backend within 5s")
	}

	// Second concurrent request must be rejected.
	resp, err := http.Post(frontend.URL+"/tokenize", "application/json",
		strings.NewReader(`{"model": "served", "prompt": "hello"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"second request must be rejected when at in-flight capacity")

	// Release the first request; it must succeed.
	releaseHold()
	select {
	case status := <-firstStatus:
		assert.Equal(t, http.StatusOK, status, "first request must succeed")
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not complete within 5s")
	}
}

// TestServerInFlightLimitReleasedAfterResponse verifies that after a request
// completes, the in-flight slot is released and the next request is admitted.
func TestServerInFlightLimitReleasedAfterResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tokens": [], "count": 0}`))
	}))
	defer backend.Close()

	cfg := Config{ProxyTimeout: 5 * time.Second, RendererMaxInFlight: 1}
	frontend := httptest.NewServer(newTestServerWithCfg(t, backend, cfg).Handler())
	defer frontend.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Post(frontend.URL+"/tokenize", "application/json",
			strings.NewReader(`{"model": "served", "prompt": "hi"}`))
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"request %d should succeed after previous one completed", i+1)
	}
}

// TestServerInFlightLimitZeroMeansUnlimited verifies that with
// RendererMaxInFlight=0 (the default) all concurrent requests are admitted.
func TestServerInFlightLimitZeroMeansUnlimited(t *testing.T) {
	const concurrency = 10
	var wg sync.WaitGroup
	// hold all requests simultaneously until all have arrived at the backend.
	hold := make(chan struct{})
	var releaseHoldOnce sync.Once
	releaseHold := func() { releaseHoldOnce.Do(func() { close(hold) }) }
	defer releaseHold()

	var mu sync.Mutex
	var codes []int

	// arrivedWg counts backend arrivals so we know all requests are truly
	// in-flight before we release the hold.
	var arrivedWg sync.WaitGroup
	arrivedWg.Add(concurrency)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivedWg.Done()
		<-hold
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tokens": [], "count": 0}`))
	}))
	defer backend.Close()

	cfg := Config{ProxyTimeout: 5 * time.Second, RendererMaxInFlight: 0}
	frontend := httptest.NewServer(newTestServerWithCfg(t, backend, cfg).Handler())
	defer frontend.Close()

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(frontend.URL+"/tokenize", "application/json",
				strings.NewReader(`{"model": "served", "prompt": "hi"}`))
			if err != nil {
				mu.Lock()
				codes = append(codes, -1)
				mu.Unlock()
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			codes = append(codes, resp.StatusCode)
			mu.Unlock()
		}()
	}

	// Wait until all goroutines are inside the backend handler, then release.
	done := make(chan struct{})
	go func() { arrivedWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		releaseHold()
		t.Fatal("timed out waiting for all requests to reach the backend")
	}
	releaseHold()
	wg.Wait()

	require.Len(t, codes, concurrency)
	for i, code := range codes {
		assert.Equal(t, http.StatusOK, code, "request %d must succeed with unlimited concurrency", i)
	}
}

// TestAcquireEndpointContention exercises AcquireEndpoint under concurrent
// load: exactly RendererMaxInFlight acquisitions must succeed, all others must
// be rejected, and over-releases must not drive InFlight negative.
func TestAcquireEndpointContention(t *testing.T) {
	const limit = 3
	const goroutines = 10

	cfg := Config{RendererMaxInFlight: limit}
	m := NewRendererManager(cfg)
	m.renderers["m"] = &renderer{model: "m", status: StatusReady}

	// releases collects the release funcs returned by successful acquisitions.
	var mu sync.Mutex
	var releases []func()

	// barrier ensures all goroutines attempt acquisition simultaneously.
	var startWg sync.WaitGroup
	startWg.Add(goroutines)
	var doneWg sync.WaitGroup
	doneWg.Add(goroutines)

	granted := make(chan struct{}, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer doneWg.Done()
			startWg.Done()
			startWg.Wait() // all goroutines start together
			_, rel, ok, _ := m.AcquireEndpoint("m")
			if ok {
				granted <- struct{}{}
				mu.Lock()
				releases = append(releases, rel)
				mu.Unlock()
			}
		}()
	}
	doneWg.Wait()
	close(granted)

	grantCount := 0
	for range granted {
		grantCount++
	}
	assert.Equal(t, limit, grantCount, "exactly RendererMaxInFlight acquisitions must succeed")
	assert.Equal(t, int64(limit), m.renderers["m"].inFlight.Load(), "inFlight must equal granted count")

	// Release all slots and verify InFlight reaches zero (not negative).
	for _, rel := range releases {
		rel()
	}
	assert.Equal(t, int64(0), m.renderers["m"].inFlight.Load(), "inFlight must be zero after all releases")

	// Extra release must not drive InFlight negative.
	releases[0]()
	assert.GreaterOrEqual(t, m.renderers["m"].inFlight.Load(), int64(0), "inFlight must not go negative")
}

// TestManagerAcquireRelease is a unit test for AcquireInFlight and
// ReleaseInFlight that exercises the limit enforcement and CAS semantics
// without spinning up an HTTP server.
func TestManagerAcquireRelease(t *testing.T) {
	tests := []struct {
		name        string
		limit       int
		acquireN    int
		wantGranted int
	}{
		{name: "no_limit_admits_all", limit: 0, acquireN: 5, wantGranted: 5},
		{name: "limit_1_admits_one", limit: 1, acquireN: 3, wantGranted: 1},
		{name: "limit_3_admits_three", limit: 3, acquireN: 5, wantGranted: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{RendererMaxInFlight: tt.limit}
			m := NewRendererManager(cfg)
			m.renderers["m"] = &renderer{model: "m", status: StatusReady}

			granted := 0
			for i := 0; i < tt.acquireN; i++ {
				if m.AcquireInFlight("m") {
					granted++
				}
			}
			assert.Equal(t, tt.wantGranted, granted)

			// After releasing all, the same number should be grantable again.
			for i := 0; i < granted; i++ {
				m.ReleaseInFlight("m")
			}
			regrantable := 0
			for i := 0; i < tt.wantGranted; i++ {
				if m.AcquireInFlight("m") {
					regrantable++
				}
			}
			assert.Equal(t, tt.wantGranted, regrantable, "slots must be regranted after release")
		})
	}
}

// TestManagerAcquireUnknownModel verifies that AcquireInFlight returns false
// for a model that has no registered renderer.
func TestManagerAcquireUnknownModel(t *testing.T) {
	cfg := Config{RendererMaxInFlight: 5}
	m := NewRendererManager(cfg)
	assert.False(t, m.AcquireInFlight("nonexistent"),
		"AcquireInFlight must return false for unregistered model")
}

// TestManagerReleaseUnknownModel verifies that ReleaseInFlight is a no-op for
// a model that has no registered renderer (no panic, no crash).
func TestManagerReleaseUnknownModel(t *testing.T) {
	cfg := Config{}
	m := NewRendererManager(cfg)
	require.NotPanics(t, func() { m.ReleaseInFlight("nonexistent") })
}

// TestManagerSnapshotIncludesInFlight verifies that the InFlight field in
// RendererInfo reflects the current in-flight count.
func TestManagerSnapshotIncludesInFlight(t *testing.T) {
	cfg := Config{RendererMaxInFlight: 10}
	m := NewRendererManager(cfg)
	m.renderers["m"] = &renderer{model: "m", status: StatusReady}

	require.True(t, m.AcquireInFlight("m"))
	require.True(t, m.AcquireInFlight("m"))

	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, int64(2), infos[0].InFlight, "snapshot must report 2 in-flight requests")

	m.ReleaseInFlight("m")
	infos = m.Snapshot()
	assert.Equal(t, int64(1), infos[0].InFlight, "snapshot must report 1 after one release")
}
