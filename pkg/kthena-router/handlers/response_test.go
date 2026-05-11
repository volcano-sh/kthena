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
	"reflect"
	"testing"
)

func TestParseOpenAIResponseBody(t *testing.T) {
	tests := []struct {
		name    string
		resp    []byte
		want    *OpenAIResponse
		wantErr bool
	}{
		{
			name: "valid response",
			resp: []byte(`{"id":"test-id","object":"text_completion","created":123456789,"model":"test-model","usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`),
			want: &OpenAIResponse{
				ID:      "test-id",
				Object:  "text_completion",
				Created: 123456789,
				Model:   "test-model",
				Usage: Usage{
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
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseOpenAIResponseBody() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseStreamRespForUsage(t *testing.T) {
	tests := []struct {
		name         string
		responseText string
		want         OpenAIResponse
	}{
		{
			name:         "valid stream with usage",
			responseText: `data: {"id":"test-id","object":"text_completion","created":1739400043,"model":"tweet-summary-0","choices":[],"usage":{"prompt_tokens":7,"total_tokens":17,"completion_tokens":10}}`,
			want: OpenAIResponse{
				ID:      "test-id",
				Object:  "text_completion",
				Created: 1739400043,
				Model:   "tweet-summary-0",
				Usage: Usage{
					PromptTokens:     7,
					CompletionTokens: 10,
					TotalTokens:      17,
				},
			},
		},
		{
			name:         "stream [DONE]",
			responseText: `data: [DONE]`,
			want:         OpenAIResponse{},
		},
		{
			name:         "no data: prefix",
			responseText: `{"id":"test-id"}`,
			want:         OpenAIResponse{},
		},
		{
			name:         "invalid json",
			responseText: `data: {"id":"test-id",`,
			want:         OpenAIResponse{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseStreamRespForUsage(tt.responseText)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseStreamRespForUsage() = %v, want %v", got, tt.want)
			}
		})
	}
}
