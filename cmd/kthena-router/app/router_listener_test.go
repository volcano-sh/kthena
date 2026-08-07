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
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestWildcardHostnameMatch(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		hostname string
		want     bool
	}{
		{
			name:     "single label wildcard match",
			pattern:  "*.example.com",
			hostname: "api.example.com",
			want:     true,
		},
		{
			name:     "case insensitive match",
			pattern:  "*.Example.com",
			hostname: "API.example.COM",
			want:     true,
		},
		{
			name:     "multi label subdomain should not match",
			pattern:  "*.example.com",
			hostname: "a.b.example.com",
			want:     false,
		},
		{
			name:     "apex hostname should not match",
			pattern:  "*.example.com",
			hostname: "example.com",
			want:     false,
		},
		{
			name:     "non wildcard pattern should not match",
			pattern:  "example.com",
			hostname: "example.com",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wildcardHostnameMatch(tt.pattern, tt.hostname); got != tt.want {
				t.Fatalf("wildcardHostnameMatch(%q, %q) = %v, want %v", tt.pattern, tt.hostname, got, tt.want)
			}
		})
	}
}

func TestFindBestMatchingListener(t *testing.T) {
	exactHost := "api.example.com"
	wildcardHost := "*.example.com"

	lm := &ListenerManager{
		portListeners: map[int32]*PortListenerInfo{
			80: {
				Listeners: []ListenerConfig{
					{
						GatewayKey:   "wildcard-gw",
						ListenerName: "wildcard-listener",
						Port:         80,
						Hostname:     &wildcardHost,
					},
					{
						GatewayKey:   "exact-gw",
						ListenerName: "exact-listener",
						Port:         80,
						Hostname:     &exactHost,
					},
					{
						GatewayKey:   "default-gw",
						ListenerName: "default-listener",
						Port:         80,
						Hostname:     nil,
					},
				},
			},
		},
	}

	t.Run("exact match has highest priority", func(t *testing.T) {
		listener, found := lm.findBestMatchingListener(80, "api.example.com")
		if !found {
			t.Fatalf("expected listener to be found")
		}
		if listener.GatewayKey != "exact-gw" {
			t.Fatalf("expected exact-gw, got %s", listener.GatewayKey)
		}
	})

	t.Run("wildcard match has second priority", func(t *testing.T) {
		listener, found := lm.findBestMatchingListener(80, "foo.example.com")
		if !found {
			t.Fatalf("expected listener to be found")
		}
		if listener.GatewayKey != "wildcard-gw" {
			t.Fatalf("expected wildcard-gw, got %s", listener.GatewayKey)
		}
	})

	t.Run("listener without hostname is fallback", func(t *testing.T) {
		listener, found := lm.findBestMatchingListener(80, "other.test.com")
		if !found {
			t.Fatalf("expected listener to be found")
		}
		if listener.GatewayKey != "default-gw" {
			t.Fatalf("expected default-gw, got %s", listener.GatewayKey)
		}
	})
}

func TestMatchedListenerIsStableAfterGatewayUpdate(t *testing.T) {
	requestHost := "request.example.com"
	otherHost := "other.example.com"
	target := ListenerConfig{
		GatewayKey:   "default/target",
		ListenerName: "http",
		Port:         80,
		Hostname:     &requestHost,
		Protocol:     string(gatewayv1.HTTPProtocolType),
	}
	other := ListenerConfig{
		GatewayKey:   "default/other",
		ListenerName: "http",
		Port:         80,
		Hostname:     &otherHost,
		Protocol:     string(gatewayv1.HTTPProtocolType),
	}
	lm := &ListenerManager{
		server: &Server{Port: "8080"},
		portListeners: map[int32]*PortListenerInfo{
			80: {Listeners: []ListenerConfig{target, other}},
		},
		gatewayListeners: map[string][]ListenerConfig{
			target.GatewayKey: {target},
		},
	}

	matched, found := lm.findBestMatchingListener(80, requestHost)
	if !found {
		t.Fatalf("expected listener to be found")
	}

	updatedHost := gatewayv1.Hostname("updated.example.com")
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "target",
		},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					Hostname: &updatedHost,
				},
			},
		},
	}
	lm.StartListenersForGateway(gateway)

	if matched.GatewayKey != target.GatewayKey {
		t.Fatalf("matched listener changed to %q after update", matched.GatewayKey)
	}
}

// serveWithLimits starts a listener-equivalent HTTP server on a random local
// port and returns its address.
func serveWithLimits(t *testing.T, limits serverLimits, handler http.Handler) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	srv := newHTTPServer(ln.Addr().String(), handler, limits)
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})
	return ln.Addr().String()
}

func TestNewHTTPServerAppliesLimits(t *testing.T) {
	limits := serverLimits{
		readHeaderTimeout: 3 * time.Second,
		idleTimeout:       45 * time.Second,
		maxHeaderBytes:    8192,
	}

	srv := newHTTPServer(":8080", http.NotFoundHandler(), limits)

	if srv.ReadHeaderTimeout != limits.readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, limits.readHeaderTimeout)
	}
	if srv.IdleTimeout != limits.idleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, limits.idleTimeout)
	}
	if srv.MaxHeaderBytes != limits.maxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, limits.maxHeaderBytes)
	}
	// A global write deadline would truncate streaming inference responses.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 so that streaming responses are not truncated", srv.WriteTimeout)
	}
	// ReadTimeout would bound the upload of a large inference request body;
	// the body size limit handles that instead.
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0", srv.ReadTimeout)
	}
}

func TestNewHTTPServerClosesSlowHeaderConnections(t *testing.T) {
	const readHeaderTimeout = 200 * time.Millisecond

	addr := serveWithLimits(t, serverLimits{
		readHeaderTimeout: readHeaderTimeout,
		idleTimeout:       time.Minute,
		maxHeaderBytes:    defaultMaxHeaderBytes,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached for an incomplete request")
	}))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send the request line and one header, then stall without the blank line
	// that terminates the header block.
	if _, err := conn.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	start := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(10 * readHeaderTimeout)); err != nil {
		t.Fatalf("set read deadline failed: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the server to close the connection, got a response")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("connection still open after %v, want it closed after %v", time.Since(start), readHeaderTimeout)
	}
	if elapsed := time.Since(start); elapsed < readHeaderTimeout {
		t.Errorf("connection closed after %v, want at least %v", elapsed, readHeaderTimeout)
	}
}

func TestNewHTTPServerRejectsOversizedHeaders(t *testing.T) {
	const maxHeaderBytes = 1024

	var handlerCalled atomic.Bool
	addr := serveWithLimits(t, serverLimits{
		readHeaderTimeout: 10 * time.Second,
		idleTimeout:       time.Minute,
		maxHeaderBytes:    maxHeaderBytes,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled.Store(true)
	}))

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	// net/http allows a few KiB of slack above MaxHeaderBytes, so overshoot it
	// by a wide margin.
	req.Header.Set("X-Oversized", strings.Repeat("a", 16*maxHeaderBytes))

	// The server may reply 431 or close the connection while the client is
	// still writing; either way the request must not reach the handler.
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusRequestHeaderFieldsTooLarge)
		}
	}
	if handlerCalled.Load() {
		t.Error("handler was invoked for a request with oversized headers")
	}
}

func TestNewHTTPServerAllowsLongStreamingResponses(t *testing.T) {
	const (
		chunks        = 5
		chunkInterval = 100 * time.Millisecond
	)

	// Both bounds are shorter than the response duration; neither may truncate it.
	addr := serveWithLimits(t, serverLimits{
		readHeaderTimeout: 100 * time.Millisecond,
		idleTimeout:       100 * time.Millisecond,
		maxHeaderBytes:    defaultMaxHeaderBytes,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < chunks; i++ {
			if _, err := io.WriteString(w, "data: chunk\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(chunkInterval)
		}
	}))

	resp, err := http.Get("http://" + addr + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the streamed body failed: %v", err)
	}
	if got := strings.Count(string(body), "data: chunk"); got != chunks {
		t.Errorf("received %d chunks, want %d", got, chunks)
	}
}
