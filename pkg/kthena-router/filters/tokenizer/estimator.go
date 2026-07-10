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

func (s *SimpleEstimateTokenizer) CountTokens(modelServerID, prompt string) (int, error) {
	if s.CharactersPerToken <= 0 {
		return 0, fmt.Errorf("CharactersPerToken must be positive, got %f", s.CharactersPerToken)
	}

	if prompt == "" {
		return 0, nil
	}

	characterCount := utf8.RuneCountInString(prompt)
	return int(math.Ceil(float64(characterCount) / s.CharactersPerToken)), nil
}
