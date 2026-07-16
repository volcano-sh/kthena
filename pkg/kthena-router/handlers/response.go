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

// Response handler can be used to handle inference responses and parse usage data to support downstream rate limiting and usage tracking.
package handlers

import (
	"encoding/json"
	"strings"

	"k8s.io/klog/v2"
)

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Define a struct to represent the OpenAI response body
type OpenAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Usage   Usage  `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type openAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type openAIResponsesResponse struct {
	ID     string               `json:"id"`
	Object string               `json:"object"`
	Model  string               `json:"model"`
	Usage  openAIResponsesUsage `json:"usage"`
}

type anthropicResponse struct {
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"`
}

type anthropicStreamResponse struct {
	Message *anthropicResponse `json:"message"`
	Usage   anthropicUsage     `json:"usage"`
}

// Function to parse the OpenAI response body
func ParseOpenAIResponseBody(resp []byte) (*OpenAIResponse, error) {
	// Unmarshal the JSON body into the struct
	var responseBody OpenAIResponse
	err := json.Unmarshal(resp, &responseBody)
	if err != nil {
		return nil, err
	}

	return &responseBody, nil
}

func ParseOpenAIResponsesResponseBody(resp []byte) (*OpenAIResponse, error) {
	var responseBody openAIResponsesResponse
	if err := json.Unmarshal(resp, &responseBody); err != nil {
		return nil, err
	}

	return normalizeOpenAIResponsesResponse(responseBody), nil
}

func ParseAnthropicResponseBody(resp []byte) (*OpenAIResponse, error) {
	var responseBody anthropicResponse
	if err := json.Unmarshal(resp, &responseBody); err != nil {
		return nil, err
	}

	return &OpenAIResponse{
		Model: responseBody.Model,
		Usage: Usage{
			PromptTokens:     responseBody.Usage.InputTokens,
			CompletionTokens: responseBody.Usage.OutputTokens,
			TotalTokens:      responseBody.Usage.InputTokens + responseBody.Usage.OutputTokens,
		},
	}, nil
}

const (
	streamingRespPrefix = "data: "
	streamingEndMsg     = "data: [DONE]"
)

// Example message if "stream_options": {"include_usage": "true"} is included in the request:
// data: {"id":"...","object":"text_completion","created":1739400043,"model":"tweet-summary-0","choices":[],
// "usage":{"prompt_tokens":7,"total_tokens":17,"completion_tokens":10}}
//
// data: [DONE]
//
// Note that vLLM returns usage data in a `data:` entry.
// We strip the `data:` prefix from usage entries and skip `data: [DONE]` markers.
//
// If include_usage is not included in the request, `data: [DONE]` is returned separately, which
// indicates end of streaming.
func ParseStreamRespForUsage(
	responseText string,
) OpenAIResponse {
	var response OpenAIResponse
	if !strings.HasPrefix(responseText, streamingRespPrefix) || strings.HasPrefix(responseText, streamingEndMsg) {
		return response
	}
	content := strings.TrimPrefix(responseText, streamingRespPrefix)

	byteSlice := []byte(content)
	if err := json.Unmarshal(byteSlice, &response); err != nil {
		klog.Error(err, "unmarshaling response body ", content)
		return response
	}

	return response
}

func ParseOpenAIResponsesStreamRespForUsage(responseText string) OpenAIResponse {
	if !strings.HasPrefix(responseText, streamingRespPrefix) || strings.HasPrefix(responseText, streamingEndMsg) {
		return OpenAIResponse{}
	}
	content := strings.TrimPrefix(responseText, streamingRespPrefix)

	var event struct {
		Response *openAIResponsesResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(content), &event); err != nil {
		klog.Error(err, "unmarshaling OpenAI Responses stream body ", content)
		return OpenAIResponse{}
	}
	if event.Response != nil {
		return *normalizeOpenAIResponsesResponse(*event.Response)
	}

	var response openAIResponsesResponse
	if err := json.Unmarshal([]byte(content), &response); err != nil {
		return OpenAIResponse{}
	}
	return *normalizeOpenAIResponsesResponse(response)
}

func normalizeOpenAIResponsesResponse(response openAIResponsesResponse) *OpenAIResponse {
	totalTokens := response.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
	}
	return &OpenAIResponse{
		ID:     response.ID,
		Object: response.Object,
		Model:  response.Model,
		Usage: Usage{
			PromptTokens:     response.Usage.InputTokens,
			CompletionTokens: response.Usage.OutputTokens,
			TotalTokens:      totalTokens,
		},
	}
}

func ParseAnthropicStreamRespForUsage(responseText string) OpenAIResponse {
	var response OpenAIResponse
	if !strings.HasPrefix(responseText, streamingRespPrefix) {
		return response
	}
	content := strings.TrimPrefix(responseText, streamingRespPrefix)

	var streamResponse anthropicStreamResponse
	if err := json.Unmarshal([]byte(content), &streamResponse); err != nil {
		klog.Error(err, "unmarshaling anthropic stream response body ", content)
		return response
	}

	usage := streamResponse.Usage
	if streamResponse.Message != nil {
		response.Model = streamResponse.Message.Model
		usage.InputTokens += streamResponse.Message.Usage.InputTokens
		usage.OutputTokens += streamResponse.Message.Usage.OutputTokens
	}
	response.Usage = Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
	return response
}
