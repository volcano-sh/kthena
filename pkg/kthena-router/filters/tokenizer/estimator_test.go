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

import "testing"

func TestSimpleEstimateTokenizerCalculateTokenNum(t *testing.T) {
	tests := []struct {
		name      string
		tokenizer *SimpleEstimateTokenizer
		prompt    string
		want      int
		wantErr   bool
	}{
		{
			name:      "valid case: empty prompt",
			tokenizer: &SimpleEstimateTokenizer{CharactersPerToken: 4},
			prompt:    "",
			want:      0,
			wantErr:   false,
		},
		{
			name:      "valid case: ascii prompt",
			tokenizer: &SimpleEstimateTokenizer{CharactersPerToken: 4},
			prompt:    "hello world",
			want:      3, // ceil(11/4)
			wantErr:   false,
		},
		{
			name:      "valid case: unicode prompt uses rune count",
			tokenizer: &SimpleEstimateTokenizer{CharactersPerToken: 4},
			prompt:    "你好世界",
			want:      1, // 4 runes, not 12 bytes
			wantErr:   false,
		},
		{
			name:      "error case: zero CharactersPerToken",
			tokenizer: &SimpleEstimateTokenizer{CharactersPerToken: 0},
			prompt:    "test",
			wantErr:   true,
		},
		{
			name:      "error case: negative CharactersPerToken",
			tokenizer: &SimpleEstimateTokenizer{CharactersPerToken: -1},
			prompt:    "test",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.tokenizer.CalculateTokenNum(tt.prompt)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CalculateTokenNum() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("CalculateTokenNum() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("CalculateTokenNum() = %d, want %d", got, tt.want)
			}
		})
	}
}
