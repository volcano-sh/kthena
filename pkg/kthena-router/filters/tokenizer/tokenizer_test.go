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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSimpleEstimateTokenizer will verify SimpleEstimateTokenizer correctly
// will also estimate token count using the 4 characters per token heuristic
// including the ceiling division for non-multiples of 4
func TestSimpleEstimateTokenizer(t *testing.T) {
	tokenizer := NewSimpleEstimateTokenizer()

	tests := []struct {
		name    string
		prompt  string
		want    int
		wantErr bool
	}{
		{
			name:   "empty string returns 0",
			prompt: "",
			want:   0,
		},
		{
			name:   "exactly 4 characters returns 1 token",
			prompt: "abcd",
			want:   1,
		},
		{
			name:   "5 characters rounds up to 2 tokens",
			prompt: "abcde",
			want:   2,
		},
		{
			name:   "8 characters returns 2 tokens",
			prompt: "abcdefgh",
			want:   2,
		},
		{
			name:   "single character rounds up to 1 token",
			prompt: "a",
			want:   1,
		},
		{
			name:   "whitespace only counts as characters",
			prompt: "    ",
			want:   1,
		},
		{
			name:   "longer prompt",
			prompt: "The quick brown fox jumps over the lazy dog",
			want:   11, // ceil(43/4)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tokenizer.CalculateTokenNum(tt.prompt)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestTickToken verifies that TickToken produces reasonable token counts
// using the cl100k_ base encoding via the offline BPE loader
// will not require network access
func TestTickToken(t *testing.T) {
	tokenizer := &TickToken{}

	tests := []struct {
		name    string
		prompt  string
		wantMin int
		wantMax int
		wantErr bool
	}{
		{
			name:    "empty string returns 0 tokens",
			prompt:  "",
			wantMin: 0,
			wantMax: 0,
		},
		{
			name:    "simple english word",
			prompt:  "hello",
			wantMin: 1,
			wantMax: 3,
		},
		{
			name:    "sentence produces reasonable token count",
			prompt:  "The quick brown fox",
			wantMin: 3,
			wantMax: 8,
		},
		{
			name:    "longer prompt produces more tokens than shorter",
			prompt:  "This is a much longer sentence that should produce more tokens than a single word",
			wantMin: 10,
			wantMax: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tokenizer.CalculateTokenNum(tt.prompt)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.GreaterOrEqual(t, got, tt.wantMin)
				assert.LessOrEqual(t, got, tt.wantMax)
			}
		})
	}
}
