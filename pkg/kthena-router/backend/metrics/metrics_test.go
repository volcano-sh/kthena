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

package metrics

import "testing"

func TestPodEndpointURL(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		port uint32
		path string
		want string
	}{
		{
			name: "ipv4",
			ip:   "10.244.0.12",
			port: 8000,
			path: "/metrics",
			want: "http://10.244.0.12:8000/metrics",
		},
		{
			name: "ipv6",
			ip:   "fd00:10:244::12",
			port: 8000,
			path: "/v1/models",
			want: "http://[fd00:10:244::12]:8000/v1/models",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodEndpointURL(tt.ip, tt.port, tt.path)
			if got != tt.want {
				t.Fatalf("PodEndpointURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
