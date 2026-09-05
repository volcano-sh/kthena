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
	"fmt"
	"math"
	"unicode/utf8"
)

type SimpleEstimateTokenizer struct {
	CharactersPerToken float64
}

func NewSimpleEstimateTokenizer() Tokenizer {
	return &SimpleEstimateTokenizer{
		CharactersPerToken: 4.0,
	}
}

// Load is a no-op for the estimator.
func (s *SimpleEstimateTokenizer) Load(modelServerID, modelRepoID string) error {
	return nil
}

// Unload is a no-op for the estimator.
func (s *SimpleEstimateTokenizer) Unload(modelServerID string) error {
	return nil
}

func (s *SimpleEstimateTokenizer) Encode(modelServerID, prompt string) ([]uint32, int, error) {
	if s.CharactersPerToken <= 0 {
		return []uint32{0}, 0, fmt.Errorf("CharactersPerToken must be positive, got %f", s.CharactersPerToken)
	}

	if prompt == "" {
		return []uint32{0}, 0, nil
	}

	characterCount := utf8.RuneCountInString(prompt)
	return []uint32{0}, int(math.Ceil(float64(characterCount) / s.CharactersPerToken)), nil
}
