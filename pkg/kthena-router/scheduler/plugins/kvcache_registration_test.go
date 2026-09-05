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

package plugins

import (
	"testing"
)

func TestRouterSelfEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		podIP   string
		port    int
		want    string
		wantErr bool
	}{
		{
			name:  "IPv4 address",
			podIP: "10.0.0.5",
			port:  9080,
			want:  "http://10.0.0.5:9080",
		},
		{
			name:  "IPv6 address is bracketed",
			podIP: "fd00::1",
			port:  9080,
			want:  "http://[fd00::1]:9080",
		},
		{
			name:    "missing POD_IP",
			podIP:   "",
			port:    9080,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POD_IP", tt.podIP)
			got, err := routerSelfEndpoint(tt.port)
			if (err != nil) != tt.wantErr {
				t.Fatalf("routerSelfEndpoint() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("routerSelfEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewProcessGeneration(t *testing.T) {
	g1 := newProcessGeneration()
	g2 := newProcessGeneration()
	if g1 == "" || g2 == "" {
		t.Fatal("generation must not be empty")
	}
	if g1 == g2 {
		t.Errorf("expected unique generations, got %q twice", g1)
	}
}
