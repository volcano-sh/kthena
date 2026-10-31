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

package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	routerpkg "github.com/volcano-sh/kthena/pkg/kthena-router/router"
)

// onFlightRedisKey is the hash the Redis on-flight counter writes to
// (datastore.redisOnFlightHashKey).
const onFlightRedisKey = "kthena:on-flight-requests"

// freeTCPPorts returns n distinct ports that are free on all interfaces and on
// 127.0.0.1, as the router binds ":<port>" and the debug server
// "localhost:<port>". The router binds them only later, so they come from below
// the ephemeral port ranges of Linux (32768-60999) and macOS (49152-65535): an
// outgoing connection cannot take one of them in between, as it can with ports
// from ":0". Every probe stays open until all ports are chosen, so no port is
// returned twice.
func freeTCPPorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for port := 20000 + rand.IntN(10000); len(ports) < n && port < 32768; port++ {
		// On macOS a socket bound to 127.0.0.1 does not stop ":<port>" from
		// binding, so check 127.0.0.1 on its own first.
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // in use
		}
		l.Close()
		if l, err = net.Listen("tcp", fmt.Sprintf(":%d", port)); err != nil {
			continue // in use
		}
		defer l.Close()
		ports = append(ports, port)
	}
	require.Len(t, ports, n, "not enough free ports below the ephemeral port range")
	return ports
}

// onFlightTotal sums the on-flight count of every pod in Redis.
func onFlightTotal(mr *miniredis.Miniredis) (int64, error) {
	if !mr.Exists(onFlightRedisKey) {
		return 0, nil
	}
	fields, err := mr.HKeys(onFlightRedisKey)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, field := range fields {
		n, err := strconv.ParseInt(mr.HGet(onFlightRedisKey, field), 10, 64)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func redisOnFlightTotal(t *testing.T, mr *miniredis.Miniredis) int64 {
	t.Helper()
	total, err := onFlightTotal(mr)
	require.NoError(t, err)
	return total
}

// streamingUpstream serves POST /v1/completions as an SSE stream of chunks
// events, interval apart, followed by [DONE]. Other requests (the store's
// /metrics and /v1/models polls) get an empty 200.
func streamingUpstream(t *testing.T, chunks int, interval time.Duration) *url.URL {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/completions" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 1; i <= chunks; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"text_completion\",\"choices\":[{\"index\":0,\"text\":\"t%d\"}]}\n\n", i)
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	return u
}

// shutdownTestRouter is a Server.Run started with the file config source, one
// ModelServer pointing at upstream and the Redis on-flight counter.
type shutdownTestRouter struct {
	baseURL    string
	metricsURL string
	debugURL   string
	redis      *miniredis.Miniredis
	cancel     context.CancelFunc
	// runReturned receives the Redis on-flight total read right after Run
	// returned, which is when main() exits.
	runReturned chan onFlightResult
}

type onFlightResult struct {
	total int64
	err   error
}

func startShutdownTestRouter(t *testing.T, drainTimeout string, handlerExitTimeout time.Duration, upstream *url.URL) *shutdownTestRouter {
	t.Helper()
	mr := miniredis.RunT(t)
	t.Setenv("REDIS_HOST", mr.Host())
	t.Setenv("REDIS_PORT", mr.Port())
	t.Setenv("DRAIN_TIMEOUT", drainTimeout)

	dir := t.TempDir()
	resourceDir := filepath.Join(dir, "resources")
	require.NoError(t, os.MkdirAll(resourceDir, 0o755))
	manifests := fmt.Sprintf(`apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata: {name: mock, namespace: default}
spec:
  endpoints: [{name: m0, address: 127.0.0.1, port: %s}]
  model: mock-model
  inferenceEngine: vLLM
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata: {name: demo, namespace: default}
spec:
  modelName: demo
  rules: [{name: default, targetModels: [{modelServerName: mock}]}]
`, upstream.Port())
	require.NoError(t, os.WriteFile(filepath.Join(resourceDir, "demo.yaml"), []byte(manifests), 0o600))

	ports := freeTCPPorts(t, 3)
	port, metricsPort, debugPort := ports[0], ports[1], ports[2]
	s := NewServer(strconv.Itoa(port), false, "", "", false, false, debugPort, metricsPort, false, 0, 0)
	s.handlerExitTimeout = handlerExitTimeout
	s.ConfigSource = ConfigSourceFile
	s.ConfigDir = resourceDir
	s.RouterConfigFile = writeRouterConfig(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	tr := &shutdownTestRouter{
		baseURL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		metricsURL:  fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort),
		debugURL:    fmt.Sprintf("http://localhost:%d/debug/config_dump/pods", debugPort),
		redis:       mr,
		cancel:      cancel,
		runReturned: make(chan onFlightResult, 1),
	}
	go func() {
		s.Run(ctx)
		total, err := onFlightTotal(mr)
		tr.runReturned <- onFlightResult{total: total, err: err}
	}()
	t.Cleanup(cancel)

	require.Eventually(t, func() bool {
		resp, err := http.Get(tr.baseURL + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 15*time.Second, 50*time.Millisecond, "router never became ready")
	return tr
}

// writeRouterConfig writes a scheduler configuration that scores pods by
// their in-flight requests and returns its path.
func writeRouterConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "routerConfiguration.yaml")
	require.NoError(t, os.WriteFile(path,
		[]byte("scheduler:\n  plugins:\n    Score:\n      enabled:\n        - name: least-request\n          weight: 1\n"), 0o600))
	return path
}

// startStream sends a streaming completion and returns once the first event
// arrived, so the request is in flight on the backend.
func (tr *shutdownTestRouter) startStream(t *testing.T) (*http.Response, *bufio.Reader) {
	t.Helper()
	resp, err := http.Post(tr.baseURL+"/v1/completions", "application/json",
		strings.NewReader(`{"model":"demo","prompt":"hi","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	reader := bufio.NewReader(resp.Body)
	first := nextEvent(t, reader)
	require.True(t, strings.HasPrefix(first, "data: {"), "unexpected first event %q", first)
	require.Equal(t, int64(1), redisOnFlightTotal(t, tr.redis), "the stream should be counted in Redis")
	return resp, reader
}

// nextEvent reads the stream up to its next "data:" line and returns that line.
func nextEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "data: ") {
			return line
		}
	}
}

func (tr *shutdownTestRouter) waitForRun(t *testing.T, timeout time.Duration) int64 {
	t.Helper()
	select {
	case result := <-tr.runReturned:
		require.NoError(t, result.err)
		return result.total
	case <-time.After(timeout):
		t.Fatalf("Server.Run did not return within %v", timeout)
		return 0
	}
}

func TestServerRunDrainsInFlightRequestsOnShutdown(t *testing.T) {
	const chunks = 5
	upstream := streamingUpstream(t, chunks, 200*time.Millisecond)
	// With a tiny handler budget, only the listener drain can keep Server.Run
	// from returning before the stream has finished.
	tr := startShutdownTestRouter(t, "30s", 20*time.Millisecond, upstream)
	resp, reader := tr.startStream(t)
	defer resp.Body.Close()

	tr.cancel() // what main() does on SIGTERM

	// Read one more event first. By then the drain is well under way, and a
	// management server that stopped on SIGTERM would already be gone.
	require.True(t, strings.HasPrefix(nextEvent(t, reader), "data: {"))
	for _, u := range []string{tr.metricsURL, tr.debugURL} {
		resp, err := http.Get(u)
		require.NoError(t, err, "%s must keep serving while requests drain", u)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, u)
	}

	onFlight := tr.waitForRun(t, 20*time.Second)
	require.Zero(t, onFlight, "Server.Run returned before the in-flight stream released its Redis on-flight count")

	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, chunks-2, strings.Count(string(body), "data: {"), "the client should receive every remaining chunk")
	require.Contains(t, string(body), "data: [DONE]")

	require.Eventually(t, func() bool {
		for _, u := range []string{tr.metricsURL, tr.debugURL} {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "the metrics and debug servers must stop after the drain")
}

func TestServerRunForceClosesRequestsAfterDrainTimeout(t *testing.T) {
	const drainTimeout = 300 * time.Millisecond
	// 50 chunks 100ms apart: the stream outlives the drain timeout.
	upstream := streamingUpstream(t, 50, 100*time.Millisecond)
	tr := startShutdownTestRouter(t, drainTimeout.String(), defaultHandlerExitTimeout, upstream)
	resp, reader := tr.startStream(t)
	defer resp.Body.Close()

	shutdownAt := time.Now()
	tr.cancel()
	onFlight := tr.waitForRun(t, drainTimeout+defaultHandlerExitTimeout+5*time.Second)
	elapsed := time.Since(shutdownAt)

	require.GreaterOrEqual(t, elapsed, drainTimeout, "Server.Run should wait for the drain timeout")
	require.Less(t, elapsed, drainTimeout+defaultHandlerExitTimeout, "the forced close should end the request quickly")
	require.Zero(t, onFlight, "the Redis on-flight count must be released even when the request is force-closed")

	body, _ := io.ReadAll(reader)
	require.NotContains(t, string(body), "data: [DONE]", "the stream should be cut at the drain timeout")
}

// TestServerRunAdmitsQueuedRequestsWhileDraining checks that a request still
// waiting for admission by the fairness or session-boost queue at shutdown is
// served once the in-flight request frees its slot.
func TestServerRunAdmitsQueuedRequestsWhileDraining(t *testing.T) {
	tests := []struct {
		name    string
		enabled *bool
		env     map[string]string
	}{
		{
			name:    "fairness scheduling",
			enabled: &routerpkg.EnableFairnessScheduling,
			env: map[string]string{
				"ENABLE_FAIRNESS_SCHEDULING": "true",
				"FAIRNESS_MAX_CONCURRENT":    "1",
				"FAIRNESS_QUEUE_TIMEOUT":     "10s",
			},
		},
		{
			name:    "session boost without a queue timeout",
			enabled: &routerpkg.EnableSessionBoost,
			env: map[string]string{
				"ENABLE_SESSION_BOOST":           "true",
				"SESSION_BOOST_INFLIGHT_PER_POD": "1",
				"SESSION_BOOST_TIMEOUT":          "0s",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The router reads its scheduling flags once at package init, and the
			// store reads the env vars when Server.Run creates it, so set both.
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			enabled := *tt.enabled
			*tt.enabled = true
			t.Cleanup(func() { *tt.enabled = enabled })

			upstream := streamingUpstream(t, 5, 200*time.Millisecond)
			tr := startShutdownTestRouter(t, "30s", 20*time.Millisecond, upstream)
			// The first stream holds the only slot, so the second request has to
			// wait in the queue until the stream ends.
			resp, _ := tr.startStream(t)
			defer resp.Body.Close()

			type response struct {
				status int
				body   string
				err    error
			}
			queued := make(chan response, 1)
			go func() {
				resp, err := http.Post(tr.baseURL+"/v1/completions", "application/json",
					strings.NewReader(`{"model":"demo","prompt":"hi","stream":true}`))
				if err != nil {
					queued <- response{err: err}
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				queued <- response{status: resp.StatusCode, body: string(body), err: err}
			}()
			require.Eventually(t, func() bool {
				return metrics.DefaultMetrics.ActiveRequestsCount() == 2
			}, 5*time.Second, 10*time.Millisecond, "the second request never reached the router")

			tr.cancel()
			onFlight := tr.waitForRun(t, 20*time.Second)
			require.Zero(t, onFlight, "the Redis on-flight count must be released before Server.Run returns")

			select {
			case got := <-queued:
				require.NoError(t, got.err)
				require.Equal(t, http.StatusOK, got.status, "the queued request was not admitted during the drain: %s", got.body)
				require.Contains(t, got.body, "data: [DONE]")
			case <-time.After(5 * time.Second):
				t.Fatal("the queued request did not finish after Server.Run returned")
			}
		})
	}
}

// TestServerShutdownOrder checks that shutdown waits for the listeners to
// drain, then for the request handlers to return, and only then cancels
// drainCtx.
func TestServerShutdownOrder(t *testing.T) {
	s := &Server{handlerExitTimeout: time.Second}
	require.True(t, s.listeners.add())
	var listenerDrained atomic.Bool
	go func() {
		listenerDrained.Store(true)
		s.listeners.done()
	}()

	running := true
	polls := 0
	activeRequests := func() int64 {
		s.listeners.mu.Lock()
		closed := s.listeners.closed
		s.listeners.mu.Unlock()
		require.True(t, closed && listenerDrained.Load(), "request handlers were polled before the listeners drained")
		polls++
		if polls == 3 {
			running = false // a force-closed handler has released its Redis count
		}
		if running {
			return 1
		}
		return 0
	}
	stopped := false
	s.shutdown(activeRequests, func() {
		require.False(t, running, "drainCtx was cancelled while request handlers were still running")
		stopped = true
	})

	require.Equal(t, 3, polls, "shutdown must wait until the request handlers have returned")
	require.True(t, stopped, "shutdown must cancel drainCtx")
}

// TestServerShutdownDrainsGatewayListeners checks that Gateway listeners stop
// on SIGTERM, like the default listener, and that shutdown waits for them.
func TestServerShutdownDrainsGatewayListeners(t *testing.T) {
	ports := freeTCPPorts(t, 4)
	port, metricsPort, debugPort, gatewayPort := ports[0], ports[1], ports[2], ports[3]
	s := NewServer(strconv.Itoa(port), false, "", "", true, false, debugPort, metricsPort, false, 0, 0)
	store := datastore.New()
	require.NoError(t, store.AddOrUpdateGateway(&gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gw"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{{Name: "http", Port: gatewayv1.PortNumber(gatewayPort), Protocol: gatewayv1.HTTPProtocolType}},
		},
	}))
	s.store = store
	r := NewRouter(store, writeRouterConfig(t, t.TempDir()), common.NewTransportRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drainCtx, stopAfterDrain := context.WithCancel(context.Background())
	defer stopAfterDrain()
	s.startRouter(ctx, drainCtx, r, store)
	gatewayAddr := fmt.Sprintf("127.0.0.1:%d", gatewayPort)
	require.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", gatewayAddr)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 10*time.Millisecond, "the Gateway listener never started")

	cancel() // what main() does on SIGTERM
	stopped := make(chan struct{})
	go func() {
		s.shutdown(r.ActiveRequestCount, stopAfterDrain)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return: the Gateway listener did not drain on SIGTERM")
	}
	if conn, err := net.Dial("tcp", gatewayAddr); err == nil {
		conn.Close()
		t.Fatal("the Gateway listener must be closed when shutdown returns")
	}
}

func TestListenerGroup(t *testing.T) {
	t.Run("closeAndWait waits for registered listeners", func(t *testing.T) {
		var g listenerGroup
		require.True(t, g.add())
		var drained atomic.Bool
		time.AfterFunc(50*time.Millisecond, func() {
			drained.Store(true)
			g.done()
		})
		g.closeAndWait()
		require.True(t, drained.Load(), "closeAndWait returned before the listener drained")
	})

	t.Run("add is refused after close", func(t *testing.T) {
		var g listenerGroup
		g.closeAndWait()
		require.False(t, g.add())
	})

	t.Run("listeners registered concurrently with close are waited for", func(t *testing.T) {
		var g listenerGroup
		var running atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if g.add() {
					running.Add(1)
					time.AfterFunc(10*time.Millisecond, func() {
						running.Add(-1)
						g.done()
					})
				}
			}()
		}
		g.closeAndWait()
		require.Zero(t, running.Load(), "a listener registered before close was still running")
		wg.Wait()
	})
}

func TestStartListenerAfterShutdownStarted(t *testing.T) {
	var g listenerGroup
	g.closeAndWait()
	port := freeTCPPorts(t, 1)[0]

	srv := startListener(context.Background(), listenerConfig{
		addr:          fmt.Sprintf("127.0.0.1:%d", port),
		defaultRouter: &routerpkg.Router{},
		readyCheck:    func() bool { return true },
		group:         &g,
	})
	require.Nil(t, srv)

	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "no listener may be started once the router is shutting down")
	l.Close()
}

func TestGatewayListenerNotStartedWhileShuttingDown(t *testing.T) {
	s := &Server{Port: "8080"}
	s.listeners.closeAndWait()
	lm := NewListenerManager(context.Background(), &routerpkg.Router{}, nil, s)

	port := int32(freeTCPPorts(t, 1)[0])
	lm.StartListenersForGateway(&gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "late"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{{Name: "http", Port: port, Protocol: gatewayv1.HTTPProtocolType}},
		},
	})

	require.NotContains(t, lm.portListeners, port, "a Gateway event during shutdown must not open a new port")
}

func TestWaitForRequestHandlers(t *testing.T) {
	tests := []struct {
		name        string
		active      int64
		finishAfter time.Duration // 0 means the handlers never finish
		timeout     time.Duration
		wantRunning int64
	}{
		{name: "no running handlers", active: 0, timeout: time.Second, wantRunning: 0},
		{name: "handlers finish in time", active: 2, finishAfter: 50 * time.Millisecond, timeout: 5 * time.Second, wantRunning: 0},
		{name: "handlers outlive the timeout", active: 1, timeout: 100 * time.Millisecond, wantRunning: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var active atomic.Int64
			active.Store(tt.active)
			if tt.finishAfter > 0 {
				timer := time.AfterFunc(tt.finishAfter, func() { active.Store(0) })
				defer timer.Stop()
			}
			require.Equal(t, tt.wantRunning, waitForRequestHandlers(active.Load, tt.timeout))
		})
	}
}
