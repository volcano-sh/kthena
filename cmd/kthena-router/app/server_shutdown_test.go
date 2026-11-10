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
	"testing"
	"testing/synctest"
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

// freeTCPPorts returns n distinct ports for the router's listeners, which bind
// ":<port>", and its debug server, which binds "localhost:<port>". The router
// binds them only later, so they come from below the default ephemeral port
// ranges of Linux (32768-60999) and macOS (49152-65535), where an outgoing
// connection does not take them in the meantime as it can with ports from
// ":0". Every probe stays open until all ports are chosen, so no port is
// returned twice.
func freeTCPPorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, 0, n)
	for port := 20000 + rand.IntN(10000); len(ports) < n && port < 32768; port++ {
		if loopbackInUse(port) {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue // in use
		}
		defer l.Close()
		ports = append(ports, port)
	}
	require.Len(t, ports, n, "not enough free ports below the ephemeral port range")
	return ports
}

// loopbackInUse reports whether another socket already holds port on a
// loopback address. On macOS ":<port>" can still be bound in that case, but the
// test's requests to localhost would reach the other socket.
func loopbackInUse(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second); err == nil {
			conn.Close()
			return true
		}
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	l.Close()
	return false
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
		// Read the whole request, so that the server notices when the router
		// cancels it.
		_, _ = io.Copy(io.Discard, r.Body)
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
	baseURL string
	redis   *miniredis.Miniredis
	cancel  context.CancelFunc
	// runReturned receives the Redis on-flight total read right after Run
	// returned, which is when main() exits.
	runReturned chan onFlightResult
}

type onFlightResult struct {
	total int64
	err   error
}

func startShutdownTestRouter(t *testing.T, drainTimeout string, upstream *url.URL) *shutdownTestRouter {
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
	s.ConfigSource = ConfigSourceFile
	s.ConfigDir = resourceDir
	s.RouterConfigFile = writeRouterConfig(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	tr := &shutdownTestRouter{
		baseURL:     fmt.Sprintf("http://127.0.0.1:%d", port),
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

// goWait calls wait in a new goroutine and returns a channel that is closed
// once wait returns.
func goWait(wait func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	return done
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestServerRunDrainsInFlightRequestsOnShutdown(t *testing.T) {
	const chunks = 5
	upstream := streamingUpstream(t, chunks, 200*time.Millisecond)
	tr := startShutdownTestRouter(t, "30s", upstream)
	resp, reader := tr.startStream(t)
	defer resp.Body.Close()

	tr.cancel() // what main() does on SIGTERM
	onFlight := tr.waitForRun(t, 20*time.Second)
	require.Zero(t, onFlight, "Server.Run returned before the in-flight stream released its Redis on-flight count")

	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, chunks-1, strings.Count(string(body), "data: {"), "the client should receive every remaining chunk")
	require.Contains(t, string(body), "data: [DONE]")
}

func TestServerRunForceClosesRequestsAfterDrainTimeout(t *testing.T) {
	const drainTimeout = 300 * time.Millisecond
	// 50 chunks 100ms apart: the stream would last 5s, well past the drain timeout.
	upstream := streamingUpstream(t, 50, 100*time.Millisecond)
	tr := startShutdownTestRouter(t, drainTimeout.String(), upstream)
	resp, reader := tr.startStream(t)
	defer resp.Body.Close()

	shutdownAt := time.Now()
	tr.cancel()
	onFlight := tr.waitForRun(t, 10*time.Second)
	elapsed := time.Since(shutdownAt)

	require.GreaterOrEqual(t, elapsed, drainTimeout, "Server.Run should wait for the drain timeout")
	require.Less(t, elapsed, drainTimeout+2*time.Second, "the handler should return right after the forced close, not when the stream would have ended")
	require.Zero(t, onFlight, "the Redis on-flight count must be released even when the request is force-closed")

	body, _ := io.ReadAll(reader)
	require.NotContains(t, string(body), "data: [DONE]", "the stream should be cut at the drain timeout")
}

// TestServerRunForceClosesPipelinedRequest checks that the forced close at the
// drain timeout also ends a request whose client has pipelined more bytes.
// net/http stops watching such a connection, so closing it does not cancel the
// request by itself.
func TestServerRunForceClosesPipelinedRequest(t *testing.T) {
	const drainTimeout = 300 * time.Millisecond
	// One chunk, then nothing for a minute: only cancelling the request ends it.
	upstream := streamingUpstream(t, 1, time.Minute)
	tr := startShutdownTestRouter(t, drainTimeout.String(), upstream)
	conn, err := net.Dial("tcp", strings.TrimPrefix(tr.baseURL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	body := `{"model":"demo","prompt":"hi","stream":true}`
	_, err = fmt.Fprintf(conn, "POST /v1/completions HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return redisOnFlightTotal(t, tr.redis) == 1 },
		5*time.Second, 10*time.Millisecond, "the request never reached the upstream")
	// The handler has read the body, so this byte starts a pipelined request.
	_, err = conn.Write([]byte("G"))
	require.NoError(t, err)

	tr.cancel()
	onFlight := tr.waitForRun(t, 10*time.Second)
	require.Zero(t, onFlight, "the Redis on-flight count must be released before Server.Run returns")
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
			// The router reads its scheduling flags once at package init, and
			// Server.Run reads the env vars when it creates the store and the
			// router, so set both.
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			enabled := *tt.enabled
			*tt.enabled = true
			t.Cleanup(func() { *tt.enabled = enabled })

			upstream := streamingUpstream(t, 5, 200*time.Millisecond)
			tr := startShutdownTestRouter(t, "30s", upstream)
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
// drain and then for the request handlers to return, and that requests are
// refused only once the listeners have stopped. It runs in a synctest bubble,
// so synctest.Wait returns once shutdown is blocked.
func TestServerShutdownOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s Server
		require.True(t, s.listeners.add()) // a listener that is still draining
		require.True(t, s.requests.add())  // the handler of a force-closed connection
		returned := goWait(s.shutdown)

		synctest.Wait()
		require.False(t, isClosed(returned), "shutdown returned while a listener was draining")
		require.True(t, s.requests.add(), "a request was refused while the listeners were draining")
		s.requests.done()

		s.listeners.done()
		synctest.Wait()
		require.False(t, isClosed(returned), "shutdown returned while a request handler was running")
		require.False(t, s.requests.add(), "a request that arrives once the listeners have stopped must not join the wait")

		s.requests.done()
		<-returned
	})
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

// slowModel is the model name for which blockingStore holds requests.
const slowModel = "slow"

// blockingStore holds HasModel calls for slowModel until release is closed, so
// a request stays in the router's handler for as long as a test needs.
type blockingStore struct {
	datastore.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStore) HasModel(name string) bool {
	if name == slowModel {
		s.entered <- struct{}{}
		<-s.release
	}
	return s.Store.HasModel(name)
}

// TestInferenceListenersJoinDrainGroups checks that the default listener and
// Gateway listeners stay in Server.listeners until they have drained, that each
// request they serve is in Server.requests while its handler runs, and that
// they refuse requests once that group is closed.
func TestInferenceListenersJoinDrainGroups(t *testing.T) {
	tests := []struct {
		name    string
		gateway bool
	}{
		{name: "default listener"},
		{name: "Gateway listener", gateway: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ports := freeTCPPorts(t, 4)
			port, metricsPort, debugPort := ports[0], ports[1], ports[2]
			s := NewServer(strconv.Itoa(port), false, "", "", tt.gateway, false, debugPort, metricsPort, false, 0, 0)
			store := datastore.New()
			if tt.gateway {
				port = ports[3] // Gateway mode starts no listener on the router's own port
				require.NoError(t, store.AddOrUpdateGateway(&gatewayv1.Gateway{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gw"},
					Spec: gatewayv1.GatewaySpec{
						Listeners: []gatewayv1.Listener{{Name: "http", Port: gatewayv1.PortNumber(port), Protocol: gatewayv1.HTTPProtocolType}},
					},
				}))
			}
			s.store = store
			blocking := &blockingStore{Store: store, entered: make(chan struct{}, 1), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(blocking.release) })
			r := NewRouter(blocking, writeRouterConfig(t, t.TempDir()), common.NewTransportRegistry())

			ctx, cancel := context.WithCancel(context.Background())
			s.startRouter(ctx, r, store)
			t.Cleanup(func() {
				release()
				cancel()
				// The router's active-request count is shared by all tests, so
				// the handler must have returned before the next test starts.
				select {
				case <-goWait(s.shutdown):
				case <-time.After(10 * time.Second):
					t.Error("the router did not stop")
				}
			})

			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			require.Eventually(t, func() bool {
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					return false
				}
				conn.Close()
				return true
			}, 5*time.Second, 10*time.Millisecond, "the listener never started")

			listenersDone := goWait(s.listeners.closeAndWait)
			require.Never(t, func() bool { return isClosed(listenersDone) }, 100*time.Millisecond, 10*time.Millisecond,
				"the listener is not tracked, so shutdown would not wait for it")

			slowDone := make(chan error, 1)
			go func() {
				resp, err := http.Post("http://"+addr+"/v1/completions", "application/json",
					strings.NewReader(`{"model":"`+slowModel+`","prompt":"hi"}`))
				if err == nil {
					resp.Body.Close()
				}
				slowDone <- err
			}()
			select {
			case <-blocking.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the request never reached the router's handler")
			}

			// The handler is held in HasModel, so a wait for the requests cannot finish.
			requestsDone := goWait(s.requests.closeAndWait)
			require.Never(t, func() bool { return isClosed(requestsDone) }, 100*time.Millisecond, 10*time.Millisecond,
				"the running request is not tracked, so shutdown would not wait for it")
			require.Eventually(t, func() bool {
				resp, err := http.Get("http://" + addr + "/v1/models")
				if err != nil {
					return false
				}
				resp.Body.Close()
				return resp.StatusCode == http.StatusServiceUnavailable
			}, 5*time.Second, 10*time.Millisecond, "a request that arrives once the group is closed must be refused")

			// On SIGTERM the listener starts to drain, but the running request
			// keeps its connection open.
			cancel()
			require.Never(t, func() bool { return isClosed(listenersDone) }, 100*time.Millisecond, 10*time.Millisecond,
				"the listener left the group before it had drained")

			release()
			select {
			case err := <-slowDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the request did not finish after its handler was released")
			}
			require.Eventually(t, func() bool { return isClosed(requestsDone) && isClosed(listenersDone) },
				5*time.Second, 10*time.Millisecond, "the waits did not finish once the request had returned and the listener had drained")
		})
	}
}
