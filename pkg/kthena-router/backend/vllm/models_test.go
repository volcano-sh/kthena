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

package vllm

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchPodModels(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       []string
		wantErr    string
	}{
		{
			name:       "returns model IDs",
			statusCode: http.StatusOK,
			body:       `{"data":[{"id":"model-a"},{"id":"model-b"}]}`,
			want:       []string{"model-a", "model-b"},
		},
		{
			name:       "empty model list",
			statusCode: http.StatusOK,
			body:       `{"data":[]}`,
			want:       []string{},
		},
		{
			name:       "non-200 status",
			statusCode: http.StatusInternalServerError,
			body:       `internal error`,
			wantErr:    "HTTP 500",
		},
		{
			name:       "invalid JSON",
			statusCode: http.StatusOK,
			body:       `not json`,
			wantErr:    "invalid character",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/models", r.URL.Path)
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			u, err := url.Parse(server.URL)
			require.NoError(t, err)
			port, err := strconv.ParseUint(u.Port(), 10, 32)
			require.NoError(t, err)

			models, err := FetchPodModels(u.Hostname(), uint32(port), "")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, models)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, models)
		})
	}
}

func TestFetchPodModelsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.ParseUint(u.Port(), 10, 32)
	require.NoError(t, err)

	server.Close()

	models, err := FetchPodModels(u.Hostname(), uint32(port), "")
	require.Error(t, err)
	assert.Nil(t, models)
}

// Mimics vLLM started with --api-key.
func apiKeyProtectedBackend(t *testing.T, apiKey string) (string, uint32) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
	}))
	t.Cleanup(server.Close)

	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	parsed, err := strconv.ParseUint(port, 10, 32)
	require.NoError(t, err)
	return host, uint32(parsed)
}

func TestFetchPodModelsWithAPIKey(t *testing.T) {
	host, port := apiKeyProtectedBackend(t, "s3cret")

	models, err := FetchPodModels(host, port, "s3cret")
	require.NoError(t, err)
	assert.Equal(t, []string{"qwen"}, models)
}

func TestFetchPodModelsWithoutAPIKeyIsUnauthorized(t *testing.T) {
	host, port := apiKeyProtectedBackend(t, "s3cret")

	_, err := FetchPodModels(host, port, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
}

func TestFetchPodModelsReportsUnauthorizedDistinctly(t *testing.T) {
	host, port := apiKeyProtectedBackend(t, "s3cret")

	_, err := FetchPodModels(host, port, "wrong-key")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized, "a rejected key must be distinguishable from any other failure")
}
