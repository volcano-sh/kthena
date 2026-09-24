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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
