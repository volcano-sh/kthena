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

package handlers

import (
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestParseOpenAIResponseBody(t *testing.T) {
	tests := []struct {
		name    string
		resp    []byte
		want    *openai.ChatCompletion
		wantErr bool
	}{
		{
			name: "valid response",
			resp: []byte(`{"id":"test-id","object":"text_completion","created":123456789,"model":"test-model","usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`),
			want: &openai.ChatCompletion{
				ID:      "test-id",
				Object:  "text_completion",
				Created: 123456789,
				Model:   "test-model",
				Usage: openai.CompletionUsage{
					PromptTokens:     10,
					CompletionTokens: 20,
					TotalTokens:      30,
				},
			},
			wantErr: false,
		},
		{
			name:    "invalid json",
			resp:    []byte(`{"id":"test-id",`),
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOpenAIResponseBody(tt.resp)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOpenAIResponseBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.ID != tt.want.ID {
					t.Errorf("ID got = %v, want %v", got.ID, tt.want.ID)
				}
				if got.Usage.PromptTokens != tt.want.Usage.PromptTokens {
					t.Errorf("PromptTokens got = %v, want %v", got.Usage.PromptTokens, tt.want.Usage.PromptTokens)
				}
				if got.Usage.CompletionTokens != tt.want.Usage.CompletionTokens {
					t.Errorf("CompletionTokens got = %v, want %v", got.Usage.CompletionTokens, tt.want.Usage.CompletionTokens)
				}
				if got.Usage.TotalTokens != tt.want.Usage.TotalTokens {
					t.Errorf("TotalTokens got = %v, want %v", got.Usage.TotalTokens, tt.want.Usage.TotalTokens)
				}
			}
		})
	}
}

func TestParseStreamRespForUsage(t *testing.T) {
	tests := []struct {
		name         string
		responseText string
		want         openai.ChatCompletionChunk
	}{
		{
			name:         "valid stream with usage",
			responseText: `data: {"id":"test-id","object":"text_completion","created":1739400043,"model":"tweet-summary-0","choices":[],"usage":{"prompt_tokens":7,"total_tokens":17,"completion_tokens":10}}`,
			want: openai.ChatCompletionChunk{
				ID:      "test-id",
				Object:  "text_completion",
				Created: 1739400043,
				Model:   "tweet-summary-0",
				Usage: openai.CompletionUsage{
					PromptTokens:     7,
					CompletionTokens: 10,
					TotalTokens:      17,
				},
			},
		},
		{
			name:         "stream [DONE]",
			responseText: `data: [DONE]`,
			want:         openai.ChatCompletionChunk{},
		},
		{
			name:         "no data: prefix",
			responseText: `{"id":"test-id"}`,
			want:         openai.ChatCompletionChunk{},
		},
		{
			name:         "invalid json",
			responseText: `data: {"id":"test-id",`,
			want:         openai.ChatCompletionChunk{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseStreamRespForUsage(tt.responseText)
			if got.ID != tt.want.ID {
				t.Errorf("ID got = %v, want %v", got.ID, tt.want.ID)
			}
			if got.Usage.PromptTokens != tt.want.Usage.PromptTokens {
				t.Errorf("PromptTokens got = %v, want %v", got.Usage.PromptTokens, tt.want.Usage.PromptTokens)
			}
			if got.Usage.CompletionTokens != tt.want.Usage.CompletionTokens {
				t.Errorf("CompletionTokens got = %v, want %v", got.Usage.CompletionTokens, tt.want.Usage.CompletionTokens)
			}
			if got.Usage.TotalTokens != tt.want.Usage.TotalTokens {
				t.Errorf("TotalTokens got = %v, want %v", got.Usage.TotalTokens, tt.want.Usage.TotalTokens)
			}
		})
	}
}
