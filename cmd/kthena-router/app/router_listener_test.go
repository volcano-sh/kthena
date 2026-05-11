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

import "testing"

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
