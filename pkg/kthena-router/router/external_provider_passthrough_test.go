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

package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestExternalProviderForwardsOpaqueRequestContent(t *testing.T) {
	tests := []struct {
		name         string
		providerType aiv1alpha1.ExternalProviderType
		path         string
		requestBody  string
		responseBody string
		wantFields   []string
	}{
		{
			name:         "openai chat tools and image",
			providerType: aiv1alpha1.OpenAI,
			path:         "/v1/chat/completions",
			requestBody:  `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]},{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
			responseBody: `{"id":"chat-pass","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			wantFields:   []string{"messages", "tools"},
		},
		{
			name:         "openai responses tools image and file",
			providerType: aiv1alpha1.OpenAI,
			path:         "/v1/responses",
			requestBody:  `{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/cat.png"},{"type":"input_file","file_id":"file-1"}]},{"type":"function_call_output","call_id":"call-1","output":{"ok":true}},{"type":"additional_tools","tools":[{"type":"custom","name":"shell"}]},{"type":"custom_tool_call_output","call_id":"call-2","output":"ok"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			responseBody: `{"id":"resp-pass","object":"response","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			wantFields:   []string{"input", "tools"},
		},
		{
			name:         "anthropic tools image and document",
			providerType: aiv1alpha1.Anthropic,
			path:         "/v1/messages",
			requestBody:  `{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"hello"}}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"q":"x"}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
			responseBody: `{"id":"msg-pass","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantFields:   []string{"messages", "tools"},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls int
			var got map[string]interface{}
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.responseBody)
			}))
			defer upstream.Close()

			fixture := newExternalObservabilityFixture(t, fmt.Sprintf("passthrough-%d", i), tt.providerType, upstream.URL)
			body := addModelToRequestBody(tt.requestBody, fixture.clientModel)
			var original map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(body), &original))

			w, accessCtx := executeExternalObservabilityRequest(t, fixture, tt.path, body)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, 1, upstreamCalls)
			assert.Equal(t, fixture.providerModel, got["model"])
			for _, field := range tt.wantFields {
				assert.Equal(t, original[field], got[field], field)
			}
			assert.Nil(t, accessCtx.Error)
		})
	}
}
