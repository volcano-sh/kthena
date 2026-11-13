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

package standalone

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// TestTokenizeProxiesToStub verifies that the tokenizer service correctly proxies
// tokenize requests to the renderer subprocess and returns its output verbatim.
// The stub renderer returns one token per whitespace-delimited word, so a two-word
// prompt yields count=2 and tokens=[1,2]. A longer prompt must yield a larger count,
// proving the proxy threads the prompt through to the renderer rather than caching.
func TestTokenizeProxiesToStub(t *testing.T) {
	utils.WaitForTokenizerModelReady(t, testCtx.TokenizerBaseURL,
		tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)

	// "Hello, tokenizer!" is two whitespace-delimited words → count=2, tokens=[1,2].
	status, resp, err := utils.SendTokenizeRequest(t, testCtx.TokenizerBaseURL,
		tokenizercontext.ModelServerName, "Hello, tokenizer!")
	require.NoError(t, err, "tokenize request should not error")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, 2, resp.Count, "renderer must return one token per whitespace-delimited word")
	assert.Equal(t, 4096, resp.MaxModelLen, "renderer max_model_len must be 4096")
	assert.Equal(t, []int{1, 2}, resp.Tokens, "renderer must return sequential token IDs")

	// A four-word prompt must yield a larger count, proving the proxy passes the
	// prompt to the renderer rather than returning a fixed cached response.
	_, resp2, err2 := utils.SendTokenizeRequest(t, testCtx.TokenizerBaseURL,
		tokenizercontext.ModelServerName, "A completely different prompt")
	require.NoError(t, err2, "longer prompt request should not error")
	assert.Equal(t, 4, resp2.Count, "four-word prompt must yield four tokens")
	assert.Greater(t, resp2.Count, resp.Count,
		"longer prompt must produce more tokens: proxy must pass prompt through to renderer")
}
