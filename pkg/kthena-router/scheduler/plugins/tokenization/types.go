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

import "github.com/volcano-sh/kthena/pkg/kthena-router/common"

const (
	EngineVLLM   = "vllm"
	EngineSGLang = "sglang"
)

type TokenizeInputType string

const (
	CompletionInput TokenizeInputType = "completion"
	ChatInput       TokenizeInputType = "chat"
)

type TokenizeInput struct {
	Type                TokenizeInputType
	Text                string
	Messages            []common.Message
	AddSpecialTokens    bool
	ReturnTokenStrings  bool
	AddGenerationPrompt bool
}

// TokenStrings is only populated by vLLM. SGLang's /tokenize response does not
// include token_strs, so this field will always be nil for SGLang-backed pods.
type TokenizeResult struct {
	Count        int      `json:"count"`
	MaxModelLen  int      `json:"max_model_len"`
	Tokens       []int    `json:"tokens"`
	TokenStrings []string `json:"token_strs,omitempty"`
}

type RemoteTokenizerConfig struct {
	Engine             string
	Endpoint           string
	Model              string
	AddSpecialTokens   bool
	ReturnTokenStrings bool
}

type vllmTokenizeCompletionRequest struct {
	Model            string `json:"model,omitempty"`
	Prompt           string `json:"prompt"`
	AddSpecialTokens *bool  `json:"add_special_tokens,omitempty"`
	ReturnTokenStrs  *bool  `json:"return_token_strs,omitempty"`
}

type vllmTokenizeChatRequest struct {
	Model                string                 `json:"model,omitempty"`
	Messages             []common.Message       `json:"messages"`
	AddSpecialTokens     *bool                  `json:"add_special_tokens,omitempty"`
	AddGenerationPrompt  *bool                  `json:"add_generation_prompt,omitempty"`
	ContinueFinalMessage *bool                  `json:"continue_final_message,omitempty"`
	ReturnTokenStrs      *bool                  `json:"return_token_strs,omitempty"`
	ChatTemplate         *string                `json:"chat_template,omitempty"`
	ChatTemplateKwargs   map[string]interface{} `json:"chat_template_kwargs,omitempty"`
	Tools                []interface{}          `json:"tools,omitempty"`
	MMProcessorKwargs    map[string]interface{} `json:"mm_processor_kwargs,omitempty"`
}

type vllmTokenizeResponse struct {
	Count       int      `json:"count"`
	MaxModelLen int      `json:"max_model_len"`
	Tokens      []int    `json:"tokens"`
	TokenStrs   []string `json:"token_strs,omitempty"`
}

type sglangTokenizeCompletionRequest struct {
	Model            string `json:"model,omitempty"`
	Prompt           string `json:"prompt"`
	AddSpecialTokens *bool  `json:"add_special_tokens,omitempty"`
}

type sglangTokenizeChatRequest struct {
	Model            string           `json:"model,omitempty"`
	Messages         []common.Message `json:"messages"`
	AddSpecialTokens *bool            `json:"add_special_tokens,omitempty"`
	// Reserved — passed through from request when supported.
	Tools                []interface{}          `json:"tools,omitempty"`
	ToolChoice           interface{}            `json:"tool_choice,omitempty"`
	ReasoningEffort      *string                `json:"reasoning_effort,omitempty"`
	ContinueFinalMessage *bool                  `json:"continue_final_message,omitempty"`
	ChatTemplateKwargs   map[string]interface{} `json:"chat_template_kwargs,omitempty"`
}

type sglangTokenizeResponse struct {
	Count       int   `json:"count"`
	MaxModelLen int   `json:"max_model_len"`
	Tokens      []int `json:"tokens"`
}
