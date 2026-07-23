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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRouterAccessLogLine(t *testing.T) {
	tests := []struct {
		name             string
		line             string
		requestID        string
		wantError        string
		wantErrorMessage string
	}{
		{
			name:      "text success",
			line:      `[2026-07-15T17:54:19.928200669Z] "POST /v1/chat/completions HTTP/1.1" 200 model_name=e2e-openai-chat model_route=test/external-openai-chat request_id=req-success backend_type=external_provider backend_name=test/external-openai-chat upstream_model=mock-openai-chat upstream_status_code=200 upstream_attempts=1 tokens=21/5 timings=1502ms(0+1501+1)`,
			requestID: "req-success",
		},
		{
			name:             "text upstream error",
			line:             `[2026-07-15T17:54:20Z] "POST /v1/chat/completions HTTP/1.1" 429 error=upstream_response:provider test/external-openai-chat returned HTTP 429 model_name=e2e-openai-chat model_route=test/external-openai-chat request_id=req-error backend_type=external_provider backend_name=test/external-openai-chat upstream_model=mock-openai-chat upstream_status_code=429 upstream_attempts=1 error_origin=upstream tokens=8/0 timings=2ms(0+2+0)`,
			requestID:        "req-error",
			wantError:        "upstream_response",
			wantErrorMessage: "provider test/external-openai-chat returned HTTP 429",
		},
		{
			name:      "json",
			line:      `{"method":"POST","path":"/v1/responses","protocol":"HTTP/1.1","status_code":200,"model_name":"e2e-responses","request_id":"req-json","input_tokens":12,"output_tokens":4,"duration_total":1}`,
			requestID: "req-json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := ParseRouterAccessLogLine([]byte(tt.line))
			require.True(t, ok)
			assert.Equal(t, tt.requestID, entry.RequestID)
			assert.Equal(t, "POST", entry.Method)
			if tt.wantError == "" {
				assert.Nil(t, entry.Error)
			} else {
				require.NotNil(t, entry.Error)
				assert.Equal(t, tt.wantError, entry.Error.Type)
				assert.Equal(t, tt.wantErrorMessage, entry.Error.Message)
			}
			if tt.name == "text success" {
				assert.Equal(t, 21, entry.InputTokens)
				assert.Equal(t, 5, entry.OutputTokens)
				assert.Equal(t, int64(1502), entry.DurationTotal)
				assert.Equal(t, int64(1501), entry.DurationUpstreamProcessing)
			}
		})
	}
}

func TestParseRouterAccessLogLineRejectsOtherJSON(t *testing.T) {
	_, ok := ParseRouterAccessLogLine([]byte(`{"request_id":"not-an-access-log"}`))
	assert.False(t, ok)
}
