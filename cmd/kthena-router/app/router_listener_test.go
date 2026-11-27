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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	routerpkg "github.com/volcano-sh/kthena/pkg/kthena-router/router"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestInferenceListenerMetricsExposure(t *testing.T) {
	tests := []struct {
		name          string
		exposeMetrics bool
		wantStatus    int
	}{
		{name: "disabled by default", exposeMetrics: false, wantStatus: http.StatusNotFound},
		{name: "explicit compatibility endpoint", exposeMetrics: true, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newListenerHandler(listenerConfig{
				defaultRouter: &routerpkg.Router{},
				readyCheck:    func() bool { return true },
				exposeMetrics: tt.exposeMetrics,
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("GET /metrics status = %d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
}

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
			name:     "multi label subdomain match",
			pattern:  "*.example.com",
			hostname: "a.b.example.com",
			want:     true,
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

// A Gateway listener serves every path, so authentication must cover every
// path, not only the ones under /v1/.
func TestGatewayListenerAuthenticatesEveryPath(t *testing.T) {
	quietGin(t)
	t.Setenv("ACCESS_LOG_ENABLED", "false")
	issuer := newTokenIssuer(t)
	r := NewRouter(datastore.New(), issuer.routerConfig(t), common.NewTransportRegistry())
	lm := &ListenerManager{
		router: r,
		server: &Server{Port: "80"},
		portListeners: map[int32]*PortListenerInfo{
			80: {Listeners: []ListenerConfig{{GatewayKey: "default/gateway", ListenerName: "http", Port: 80}}},
		},
	}
	valid := issuer.token(t, "https://issuer.example", "alice")
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`

	tests := []struct {
		name          string
		method        string
		path          string
		token         string
		exposeMetrics bool
		wantStatus    int
	}{
		{name: "canonical path without token", method: http.MethodPost, path: "/v1/chat/completions", wantStatus: http.StatusUnauthorized},
		{name: "rewritten prefix without token", method: http.MethodPost, path: "/llm/v1/chat/completions", wantStatus: http.StatusUnauthorized},
		{name: "rewritten prefix with forged token", method: http.MethodPost, path: "/llm/v1/chat/completions", token: "forged.x.y", wantStatus: http.StatusUnauthorized},
		{name: "backend endpoint outside /v1 without token", method: http.MethodPost, path: "/tokenize", wantStatus: http.StatusUnauthorized},
		{name: "uncleaned path without token", method: http.MethodPost, path: "//v1/chat/completions", wantStatus: http.StatusUnauthorized},
		// Without --expose-metrics-on-router-port the router does not serve
		// /metrics itself, so the request is proxied to the backend.
		{name: "backend metrics without token", method: http.MethodGet, path: "/metrics", wantStatus: http.StatusUnauthorized},
		{name: "router metrics need no token", method: http.MethodGet, path: "/metrics", exposeMetrics: true, wantStatus: http.StatusOK},
		{name: "model list without token", method: http.MethodGet, path: "/models", wantStatus: http.StatusUnauthorized},
		{name: "model list with valid token", method: http.MethodGet, path: "/models", token: valid, wantStatus: http.StatusOK},
		{name: "health check needs no token", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newListenerHandler(listenerConfig{
				gateway:       &listenerGatewayConfig{lm: lm, port: 80},
				exposeMetrics: tt.exposeMetrics,
			})
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("%s %s status = %d, want %d: %s", tt.method, tt.path, recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}
