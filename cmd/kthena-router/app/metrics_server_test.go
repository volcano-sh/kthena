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
)

func TestMetricsServerOnlyServesMetrics(t *testing.T) {
	server := newMetricsServer(9090)
	if server.Addr != ":9090" {
		t.Fatalf("metrics server address = %q, want :9090", server.Addr)
	}

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/metrics", wantStatus: http.StatusOK, wantBody: "kthena_router_active_requests"},
		{path: "/healthz", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			server.Handler.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", tt.path, recorder.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Fatalf("GET %s body does not contain %q", tt.path, tt.wantBody)
			}
		})
	}
}

func TestDebugServerBindsToLoopback(t *testing.T) {
	server := newDebugServer(15000, nil)
	if server.Addr != "localhost:15000" {
		t.Fatalf("debug server address = %q, want localhost:15000", server.Addr)
	}
}
