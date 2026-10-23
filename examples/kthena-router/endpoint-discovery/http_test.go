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

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestHTTPSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
		count  int
		fail   bool
	}{
		{"snapshot", `[{"name":"instance-a","address":"127.0.0.1","port":18081}]`, 200, 1, false},
		{"empty", `[]`, 200, 0, false},
		{"unavailable", `[]`, 503, 0, true},
		{"null", `null`, 200, 0, true},
		{"malformed", `[`, 200, 0, true},
		{"trailing data", `[] []`, 200, 0, true},
		{"oversized", `[]` + strings.Repeat(" ", maxSnapshotBytes), 200, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer registry.Close()
			source, err := newHTTPSource(types.NamespacedName{}, map[string]interface{}{"url": registry.URL}, nil)
			require.NoError(t, err)
			defer source.Close()
			snapshot, err := source.Snapshot(context.Background())
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Len(t, snapshot, tc.count)
				if tc.count > 0 {
					require.Equal(t, "instance-a", snapshot[0].Name)
					require.Equal(t, "127.0.0.1", snapshot[0].Address)
					require.EqualValues(t, 18081, *snapshot[0].Port)
				}
			}
		})
	}
}

func TestHTTPSourceCancellation(t *testing.T) {
	started := make(chan struct{})
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer registry.Close()
	source, err := newHTTPSource(types.NamespacedName{}, map[string]interface{}{"url": registry.URL}, nil)
	require.NoError(t, err)
	defer source.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := source.Snapshot(ctx)
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestHTTPSourceParameters(t *testing.T) {
	for _, parameters := range []map[string]interface{}{
		nil, {"url": 42}, {"url": "relative"}, {"url": "file:///tmp/endpoints"},
		{"url": "http://127.0.0.1", "typo": true},
	} {
		source, err := newHTTPSource(types.NamespacedName{}, parameters, nil)
		require.Error(t, err)
		require.Nil(t, source)
	}
}
