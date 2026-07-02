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

package tokenization

import (
	"encoding/json"
	"fmt"
)

const sglangTokenizePath = "/tokenize"

type sglangAdapter struct {
	model string
}

func newSGLangAdapter(model string) *sglangAdapter {
	return &sglangAdapter{model: model}
}

func (sa *sglangAdapter) GetTokenizePath() string {
	return sglangTokenizePath
}

func (sa *sglangAdapter) PrepareTokenizeRequest(input TokenizeInput) (interface{}, error) {
	addSpecial := input.AddSpecialTokens

	switch input.Type {
	case CompletionInput:
		req := &sglangTokenizeCompletionRequest{
			Prompt:           input.Text,
			AddSpecialTokens: &addSpecial,
		}
		if sa.model != "" {
			req.Model = sa.model
		}
		return req, nil

	case ChatInput:
		req := &sglangTokenizeChatRequest{
			Messages:         input.Messages,
			AddSpecialTokens: &addSpecial,
		}
		if sa.model != "" {
			req.Model = sa.model
		}
		return req, nil

	default:
		return nil, fmt.Errorf("unsupported input type: %s", input.Type)
	}
}

func (sa *sglangAdapter) ParseTokenizeResponse(data []byte) (*TokenizeResult, error) {
	var resp sglangTokenizeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse sglang tokenize response: %w", err)
	}

	return &TokenizeResult{
		Count:       resp.Count,
		MaxModelLen: resp.MaxModelLen,
		Tokens:      resp.Tokens,
	}, nil
}
