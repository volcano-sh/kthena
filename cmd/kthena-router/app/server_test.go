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
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewServerDebugPortDefault tests that NewServer accepts different debug port values
func TestNewServerDebugPortDefault(t *testing.T) {
	testCases := []struct {
		name      string
		debugPort int
	}{
		{"default port", 15000},
		{"custom port", 16000},
		{"another custom port", 17000},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServer("8080", false, "", "", false, false, tc.debugPort, 0, 0)
			assert.Equal(t, tc.debugPort, server.DebugPort, "DebugPort should match the provided value")
		})
	}
}

// TestParseServerLimits tests that listener bounds are read from the
// environment and that invalid values never leave a listener unbounded.
func TestParseServerLimits(t *testing.T) {
	testCases := []struct {
		name              string
		readHeaderTimeout string
		idleTimeout       string
		maxHeaderBytes    string
		want              serverLimits
	}{
		{
			name: "defaults when unset",
			want: serverLimits{
				readHeaderTimeout: defaultReadHeaderTimeout,
				idleTimeout:       defaultIdleTimeout,
				maxHeaderBytes:    defaultMaxHeaderBytes,
			},
		},
		{
			name:              "overrides are honoured",
			readHeaderTimeout: "3s",
			idleTimeout:       "45s",
			maxHeaderBytes:    "8192",
			want: serverLimits{
				readHeaderTimeout: 3 * time.Second,
				idleTimeout:       45 * time.Second,
				maxHeaderBytes:    8192,
			},
		},
		{
			name:              "unparsable values fall back to defaults",
			readHeaderTimeout: "ten-seconds",
			idleTimeout:       "120",
			maxHeaderBytes:    "1MiB",
			want: serverLimits{
				readHeaderTimeout: defaultReadHeaderTimeout,
				idleTimeout:       defaultIdleTimeout,
				maxHeaderBytes:    defaultMaxHeaderBytes,
			},
		},
		{
			name:              "non-positive values fall back to defaults",
			readHeaderTimeout: "0s",
			idleTimeout:       "-1s",
			maxHeaderBytes:    "0",
			want: serverLimits{
				readHeaderTimeout: defaultReadHeaderTimeout,
				idleTimeout:       defaultIdleTimeout,
				maxHeaderBytes:    defaultMaxHeaderBytes,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("READ_HEADER_TIMEOUT", tc.readHeaderTimeout)
			t.Setenv("IDLE_TIMEOUT", tc.idleTimeout)
			t.Setenv("MAX_HEADER_BYTES", tc.maxHeaderBytes)

			assert.Equal(t, tc.want, parseServerLimits())
		})
	}
}
