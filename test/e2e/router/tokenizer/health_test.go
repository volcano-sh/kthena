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

package tokenizer

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// TestTokenizerHealthEndpoints verifies that /healthz returns 200 {"status":"ok"}
// and /readyz returns 200 {"status":"ready"} once the service is up.
func TestTokenizerHealthEndpoints(t *testing.T) {
	utils.WaitForTokenizerHealthy(t, testCtx.TokenizerBaseURL, 2*time.Minute)

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(testCtx.TokenizerBaseURL + "/healthz") //nolint:gosec
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body map[string]string
		raw, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		assert.Equal(t, "ok", body["status"])
	})

	t.Run("readyz_ready", func(t *testing.T) {
		// Wait for the stub renderer for tokenizer-test-model to register as ready.
		pod := utils.GetTokenizerPod(t, testCtx.KubeClient, kthenaNamespace)
		utils.WaitForTokenizerModelReady(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
			tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)

		resp, err := http.Get(testCtx.TokenizerBaseURL + "/readyz") //nolint:gosec
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body map[string]string
		raw, _ := io.ReadAll(resp.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		assert.Equal(t, "ready", body["status"])
	})
}

// TestTokenizerModelsEndpoint verifies that /models lists the configured model.
func TestTokenizerModelsEndpoint(t *testing.T) {
	utils.WaitForTokenizerHealthy(t, testCtx.TokenizerBaseURL, 2*time.Minute)
	pod := utils.GetTokenizerPod(t, testCtx.KubeClient, kthenaNamespace)
	utils.WaitForTokenizerModelReady(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)

	models, err := utils.QueryTokenizerModels(t, testCtx.TokenizerBaseURL)
	require.NoError(t, err, "GET /models should succeed")

	modelList, ok := models["models"].([]interface{})
	require.True(t, ok, "/models response should contain a 'models' array")
	require.NotEmpty(t, modelList, "expected at least one model to be registered")

	found := false
	for _, m := range modelList {
		entry, _ := m.(map[string]interface{})
		if entry["model"] == tokenizercontext.ModelServerName {
			found = true
			assert.Equal(t, "ready", entry["status"], "model status should be ready")
		}
	}
	assert.True(t, found, "tokenizer-test-model should appear in /models")
}
