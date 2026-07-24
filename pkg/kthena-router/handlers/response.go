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

	"github.com/openai/openai-go/v3"
	"k8s.io/klog/v2"
)

// ParseOpenAIResponseBody parses an OpenAI-compatible non-streaming response using the official openai-go SDK.
func ParseOpenAIResponseBody(resp []byte) (*openai.ChatCompletion, error) {
	var responseBody openai.ChatCompletion
	err := json.Unmarshal(resp, &responseBody)
	if err != nil {
		return nil, err
	}

	return &responseBody, nil
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
) openai.ChatCompletionChunk {
	var response openai.ChatCompletionChunk
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
