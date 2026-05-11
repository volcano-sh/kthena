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
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseOpenAIRequestBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name:    "valid json with model",
			body:    `{"model": "test-model", "prompt": "hello"}`,
			want:    "test-model",
			wantErr: false,
		},
		{
			name:    "invalid json",
			body:    `{"model": "test-model",`,
			want:    "",
			wantErr: true,
		},
		{
			name:    "empty body",
			body:    "",
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(tt.body))
			got, err := ParseOpenAIRequestBody(req)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOpenAIRequestBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseOpenAIRequestBody() = %v, want %v", got, tt.want)
			}
		})
	}
}
