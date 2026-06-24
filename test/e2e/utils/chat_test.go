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

import "testing"

func TestContainsError(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{
			name:     "chat completion with error word in content",
			response: `{"id":"chatcmpl-1","choices":[{"message":{"content":"this answer mentions error in text","role":"assistant"}}]}`,
			want:     false,
		},
		{
			name:     "openai style error object",
			response: `{"error":{"message":"route not found","type":"invalid_request_error"}}`,
			want:     true,
		},
		{
			name:     "null error field",
			response: `{"id":"chatcmpl-2","error":null}`,
			want:     false,
		},
		{
			name:     "non json response",
			response: "error: model unavailable",
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := containsError(tc.response)
			if got != tc.want {
				t.Fatalf("containsError() = %v, want %v", got, tc.want)
			}
		})
	}
}
