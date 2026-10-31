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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// TestTokenizeProxiesToStub sends a tokenize request and asserts that the response
// contains the stub renderer's fixed output (count=3, tokens=[1,2,3]).
func TestTokenizeProxiesToStub(t *testing.T) {
	// The sidecar test (alphabetically before this file) reinstalls kthena, which
	// replaces the tokenizer pod and breaks the TestMain port-forward tunnel.
	require.NoError(t, testCtx.RefreshPortForward(), "refresh tokenizer port-forward")

	pod := utils.GetTokenizerPod(t, testCtx.KubeClient, kthenaNamespace)
	utils.WaitForTokenizerModelReady(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)

	status, resp, err := utils.SendTokenizeRequest(t, testCtx.TokenizerBaseURL,
		tokenizercontext.ModelServerName, "Hello, tokenizer!")
	require.NoError(t, err, "tokenize request should not error")
	assert.Equal(t, http.StatusOK, status)

	// The stub returns count=3 and tokens=[1,2,3] — this is the fingerprint of the stub.
	assert.Equal(t, 3, resp.Count, "stub response should have count=3")
	assert.Equal(t, 4096, resp.MaxModelLen, "stub response should have max_model_len=4096")
	assert.Equal(t, []int{1, 2, 3}, resp.Tokens, "stub response should have tokens=[1,2,3]")

	// Determinism: a second call with different prompt text must return the same token IDs.
	_, resp2, err2 := utils.SendTokenizeRequest(t, testCtx.TokenizerBaseURL,
		tokenizercontext.ModelServerName, "A completely different prompt")
	require.NoError(t, err2, "second tokenize request should not error")
	assert.Equal(t, resp.Tokens, resp2.Tokens, "stub must return deterministic tokens across calls")
}

// TestTokenizeUnknownModelReturns503 verifies that a model not configured in the tokenizer
// service causes the service to return HTTP 503.
func TestTokenizeUnknownModelReturns503(t *testing.T) {
	require.NoError(t, testCtx.RefreshPortForward(), "refresh tokenizer port-forward")
	utils.WaitForTokenizerHealthy(t, testCtx.TokenizerBaseURL, 2*time.Minute)

	status, _, err := utils.SendTokenizeRequest(t, testCtx.TokenizerBaseURL, "no-such-model", "prompt")
	// SendTokenizeRequest returns an error on non-200; check the status code.
	if err == nil {
		t.Fatal("expected a non-200 response for unknown model")
	}
	assert.Equal(t, http.StatusServiceUnavailable, status,
		"tokenizer service should return 503 for unconfigured model")
}
