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

package providers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

type openAIAdapter struct{}

func (openAIAdapter) BuildRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}) (*http.Request, error) {
	if !isOpenAIPath(req.URL.Path) {
		return nil, &UnsupportedPathError{ProviderType: networkingv1alpha1.OpenAI, Path: req.URL.Path}
	}
	rewriteBody := false
	if _, ok := modelOverride(provider); ok {
		rewriteBody = true
	}
	if req.URL.Path != "/v1/responses" && addOpenAIStreamingTokenUsage(c, modelRequest) {
		rewriteBody = true
	}
	upstreamURL, err := openAIProviderURL(provider.Spec.BaseURL, req.URL.Path, req.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	upstream, err := buildProviderRequest(c, req, provider, modelRequest, upstreamURL, rewriteBody)
	if err != nil {
		return nil, err
	}
	if err := applyProviderAuth(upstream.Header, provider, secret, networkingv1alpha1.ProviderAuthSchemeBearer); err != nil {
		return nil, err
	}
	return upstream, nil
}

func (openAIAdapter) ResponseParser(c *gin.Context, path string) ResponseUsageParser {
	if path == "/v1/responses" {
		return &openAIResponsesUsageParser{}
	}
	return &openAIUsageParser{suppressUsageOnly: routerInjectedUsage(c)}
}

func addOpenAIStreamingTokenUsage(c *gin.Context, modelRequest map[string]interface{}) bool {
	streaming, _ := modelRequest["stream"].(bool)
	if !streaming {
		return false
	}

	streamOptionsValue, exists := modelRequest["stream_options"]
	streamOptions, ok := streamOptionsValue.(map[string]interface{})
	if exists && !ok {
		return false
	}
	if !exists {
		streamOptions = map[string]interface{}{}
		modelRequest["stream_options"] = streamOptions
	}
	if includeUsageValue, exists := streamOptions["include_usage"]; exists {
		includeUsage, ok := includeUsageValue.(bool)
		if !ok || includeUsage {
			return false
		}
	}

	streamOptions["include_usage"] = true
	c.Set(common.TokenUsageKey, true)
	return true
}

func isOpenAIPath(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/completions" || path == "/v1/responses"
}

func openAIProviderURL(baseURL, requestPath, rawQuery string) (*url.URL, error) {
	parsed, err := parseProviderBaseURL(baseURL)
	if err != nil {
		return nil, newConfigurationError("invalid provider base URL: %w", err)
	}
	if strings.TrimRight(parsed.Path, "/") != "" {
		requestPath = trimAPIVersionPrefix(requestPath)
	}
	return appendProviderPath(parsed, requestPath, rawQuery), nil
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIResponse struct {
	Choices []json.RawMessage `json:"choices"`
	Usage   openAIUsage       `json:"usage"`
}

type openAIUsageParser struct {
	// suppressUsageOnly is set when the router injected include_usage into the
	// request; the resulting usage-only chunk is then withheld from the client.
	suppressUsageOnly bool
	completed         bool
}

func (p *openAIUsageParser) ParseStreamLine(line string) StreamUsageParseResult {
	payload, ok := streamDataPayload(line)
	if !ok {
		return StreamUsageParseResult{}
	}
	var response openAIResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return StreamUsageParseResult{}
	}
	usage := tokenUsageFromOpenAIResponse(response)
	if usage.TotalTokens <= 0 {
		return StreamUsageParseResult{}
	}
	return StreamUsageParseResult{
		Usage:        usage,
		HasUsage:     true,
		SuppressLine: p.suppressUsageOnly && len(response.Choices) == 0,
	}
}

func (*openAIUsageParser) ParseBody(body []byte) (TokenUsage, bool) {
	var response openAIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return TokenUsage{}, false
	}
	usage := tokenUsageFromOpenAIResponse(response)
	return usage, usage.TotalTokens > 0
}

func (*openAIUsageParser) FinalStreamUsage() (TokenUsage, bool) {
	return TokenUsage{}, false
}

func (p *openAIUsageParser) RecordStreamLineWritten(line string) {
	p.completed = p.completed || strings.TrimSpace(line) == "data: [DONE]"
}

func (p *openAIUsageParser) StreamCompleted() bool {
	return p.completed
}

type openAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type openAIResponsesResponse struct {
	Usage openAIResponsesUsage `json:"usage"`
}

type openAIResponsesUsageParser struct {
	latest    TokenUsage
	completed bool
}

func (p *openAIResponsesUsageParser) ParseStreamLine(line string) StreamUsageParseResult {
	response, ok := parseOpenAIResponsesStreamLine(line)
	if !ok {
		return StreamUsageParseResult{}
	}
	usage := tokenUsageFromOpenAIResponses(response)
	if usage.TotalTokens > 0 {
		p.latest = usage
	}
	return StreamUsageParseResult{}
}

func (p *openAIResponsesUsageParser) ParseBody(body []byte) (TokenUsage, bool) {
	var response openAIResponsesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return TokenUsage{}, false
	}
	usage := tokenUsageFromOpenAIResponses(response)
	return usage, usage.TotalTokens > 0
}

func (p *openAIResponsesUsageParser) FinalStreamUsage() (TokenUsage, bool) {
	return p.latest, p.completed && p.latest.TotalTokens > 0
}

func (p *openAIResponsesUsageParser) RecordStreamLineWritten(line string) {
	p.completed = p.completed || isJSONStreamEvent(line, "response.completed")
}

func (p *openAIResponsesUsageParser) StreamCompleted() bool {
	return p.completed
}

func parseOpenAIResponsesStreamLine(line string) (openAIResponsesResponse, bool) {
	payload, ok := streamDataPayload(line)
	if !ok {
		return openAIResponsesResponse{}, false
	}
	var event struct {
		Response *openAIResponsesResponse `json:"response"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return openAIResponsesResponse{}, false
	}
	if event.Response != nil {
		return *event.Response, true
	}
	var response openAIResponsesResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return openAIResponsesResponse{}, false
	}
	return response, true
}

func tokenUsageFromOpenAIResponses(response openAIResponsesResponse) TokenUsage {
	usage := TokenUsage{
		PromptTokens:     response.Usage.InputTokens,
		CompletionTokens: response.Usage.OutputTokens,
		TotalTokens:      response.Usage.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func tokenUsageFromOpenAIResponse(response openAIResponse) TokenUsage {
	usage := TokenUsage{
		PromptTokens:     response.Usage.PromptTokens,
		CompletionTokens: response.Usage.CompletionTokens,
		TotalTokens:      response.Usage.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}
