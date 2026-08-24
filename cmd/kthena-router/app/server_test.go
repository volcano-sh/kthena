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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewServerManagementConfiguration(t *testing.T) {
	testCases := []struct {
		name               string
		debugPort          int
		metricsPort        int
		exposeOnRouterPort bool
	}{
		{"default ports", 15000, 9090, false},
		{"custom ports with compatibility endpoint", 16000, 9191, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServer("8080", false, "", "", false, false, tc.debugPort, tc.metricsPort, tc.exposeOnRouterPort, 0, 0)
			assert.Equal(t, tc.debugPort, server.DebugPort, "DebugPort should match the provided value")
			assert.Equal(t, tc.metricsPort, server.MetricsPort, "MetricsPort should match the provided value")
			assert.Equal(t, tc.exposeOnRouterPort, server.ExposeMetricsOnRouterPort)
		})
	}
}
