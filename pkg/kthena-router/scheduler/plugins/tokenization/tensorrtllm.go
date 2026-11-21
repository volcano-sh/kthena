/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
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

// trtllm-serve expose render endpoints (prototype) when
// TRTLLM_ENABLE_RENDER_ENDPOINTS=1. They accept the same body as the OpenAI
// chat/completions APIs and return token_ids without running the model.
const (
	tensorrtllmChatRenderPath       = "/v1/chat/completions/render"
	tensorrtllmCompletionRenderPath = "/v1/completions/render"
)

type tensorrtllmAdapter struct {
	model string
}

func newTensorRTLLMAdapter(model string) *tensorrtllmAdapter {
	return &tensorrtllmAdapter{model: model}
}

func (ta *tensorrtllmAdapter) GetTokenizePath(inputType TokenizeInputType) string {
	if inputType == CompletionInput {
		return tensorrtllmCompletionRenderPath
	}
	return tensorrtllmChatRenderPath
}

func (ta *tensorrtllmAdapter) PrepareTokenizeRequest(input TokenizeInput) (interface{}, error) {
	switch input.Type {
	case CompletionInput:
		req := map[string]interface{}{
			"prompt": input.Text,
		}
		if ta.model != "" {
			req["model"] = ta.model
		}
		return req, nil

	case ChatInput:
		req := map[string]interface{}{
			"messages": input.Messages,
		}
		if ta.model != "" {
			req["model"] = ta.model
		}
		return req, nil

	default:
		return nil, fmt.Errorf("unsupported input type: %s", input.Type)
	}
}

type tensorrtllmRenderResponse struct {
	TokenIDs []int `json:"token_ids"`
	// Some builds nest ids under prompt_token_ids; accept both.
	PromptTokenIDs []int `json:"prompt_token_ids"`
}

func (ta *tensorrtllmAdapter) ParseTokenizeResponse(data []byte) (*TokenizeResult, error) {
	var resp tensorrtllmRenderResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse tensorrt-llm render response: %w", err)
	}

	tokens := resp.TokenIDs
	if len(tokens) == 0 {
		tokens = resp.PromptTokenIDs
	}
	if tokens == nil {
		tokens = []int{}
	}

	return &TokenizeResult{
		Count:  len(tokens),
		Tokens: tokens,
	}, nil
}
