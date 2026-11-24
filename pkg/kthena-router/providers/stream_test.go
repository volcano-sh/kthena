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

package providers

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

type streamTestRecorder struct {
	*httptest.ResponseRecorder
}

func (r *streamTestRecorder) CloseNotify() <-chan bool {
	return make(chan bool)
}

// cutOffReader returns data once and then fails, simulating an upstream stream
// that breaks off mid-response.
type cutOffReader struct {
	data []byte
	read bool
}

func (r *cutOffReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.ErrUnexpectedEOF
	}
	r.read = true
	return copy(p, r.data), nil
}

// chatChunk builds a chat completion chunk; a negative completionTokens omits
// usage.
func chatChunk(content string, completionTokens int) string {
	usage := ""
	if completionTokens >= 0 {
		usage = fmt.Sprintf(`,"usage":{"prompt_tokens":10,"completion_tokens":%d,"total_tokens":%d}`, completionTokens, 10+completionTokens)
	}
	return fmt.Sprintf(`data: {"choices":[{"delta":{"content":%q}}]%s}`, content, usage) + "\n\n"
}

const (
	finalUsageChunk = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n"
	doneChunk       = "data: [DONE]\n\n"
)

// ForwardStream reports usage to onUsage once per stream, however many chunks
// carry it, because each copy is a cumulative snapshot rather than an increment.
func TestForwardStreamReportsUsageOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	finalUsage := TokenUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, PromptTokensReported: true}

	tests := []struct {
		name             string
		path             string
		body             string
		cutOff           bool
		routerInjected   bool
		wantUsage        []TokenUsage
		wantReturnTokens int
		wantErr          error
		wantForwarded    []string
		wantNotForwarded []string
	}{
		{
			name:          "usage only on the final chunk",
			path:          "/v1/chat/completions",
			body:          chatChunk("a", -1) + chatChunk("b", -1) + chatChunk("c", -1) + finalUsageChunk + doneChunk,
			wantUsage:     []TokenUsage{finalUsage},
			wantForwarded: []string{`"content":"c"`, `"completion_tokens":3`, "[DONE]"},
		},
		{
			name:          "cumulative usage on every chunk",
			path:          "/v1/chat/completions",
			body:          chatChunk("", 0) + chatChunk("a", 1) + chatChunk("b", 2) + chatChunk("c", 3) + finalUsageChunk + doneChunk,
			wantUsage:     []TokenUsage{finalUsage},
			wantForwarded: []string{`"content":"a"`, `"content":"b"`, `"content":"c"`, "[DONE]"},
		},
		{
			name:          "stream cut off keeps the last usage read",
			path:          "/v1/chat/completions",
			body:          chatChunk("", 0) + chatChunk("a", 1) + chatChunk("b", 2),
			cutOff:        true,
			wantUsage:     []TokenUsage{{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, PromptTokensReported: true}},
			wantErr:       io.ErrUnexpectedEOF,
			wantForwarded: []string{`"content":"b"`},
		},
		{
			name:             "router-injected usage-only chunk is withheld",
			path:             "/v1/chat/completions",
			body:             chatChunk("a", -1) + finalUsageChunk + doneChunk,
			routerInjected:   true,
			wantUsage:        []TokenUsage{finalUsage},
			wantForwarded:    []string{`"content":"a"`, "[DONE]"},
			wantNotForwarded: []string{`"usage"`},
		},
		{
			name:          "no usage",
			path:          "/v1/chat/completions",
			body:          chatChunk("a", -1) + doneChunk,
			wantForwarded: []string{`"content":"a"`, "[DONE]"},
		},
		{
			name: "responses terminal event",
			path: "/v1/responses",
			body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"total_tokens\":13}}}\n\n",
			wantUsage:        []TokenUsage{{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}},
			wantReturnTokens: 3,
			wantForwarded:    []string{"response.completed"},
		},
	}

	adapter := DefaultAdapter()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &streamTestRecorder{httptest.NewRecorder()}
			c, _ := gin.CreateTestContext(w)
			if tt.routerInjected {
				c.Set(common.TokenUsageKey, true)
			}
			var body io.Reader = strings.NewReader(tt.body)
			if tt.cutOff {
				body = &cutOffReader{data: []byte(tt.body)}
			}

			var got []TokenUsage
			tokens, err := ForwardStream(c, body, adapter.ResponseParser(c, tt.path), func(usage TokenUsage) {
				got = append(got, usage)
			})

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantUsage, got)
			assert.Equal(t, tt.wantReturnTokens, tokens)
			for _, s := range tt.wantForwarded {
				assert.Contains(t, w.Body.String(), s)
			}
			for _, s := range tt.wantNotForwarded {
				assert.NotContains(t, w.Body.String(), s)
			}
		})
	}
}
