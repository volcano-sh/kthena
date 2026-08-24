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
)

type anthropicAdapter struct{}

var anthropicForwardHeaders = []string{
	"Anthropic-Version",
	"Anthropic-Beta",
}

func (anthropicAdapter) BuildRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}) (*http.Request, error) {
	if req.URL.Path != "/v1/messages" {
		return nil, &UnsupportedPathError{ProviderType: networkingv1alpha1.Anthropic, Path: req.URL.Path}
	}
	_, rewriteBody := modelOverride(provider)
	upstreamURL, err := anthropicProviderURL(provider.Spec.BaseURL, req.URL.Path, req.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	upstream, err := buildProviderRequest(c, req, provider, modelRequest, upstreamURL, rewriteBody, anthropicForwardHeaders...)
	if err != nil {
		return nil, err
	}
	if err := applyProviderAuth(upstream.Header, provider, secret, networkingv1alpha1.ProviderAuthSchemeAPIKey); err != nil {
		return nil, err
	}
	return upstream, nil
}

func (anthropicAdapter) ResponseParser(*gin.Context, string) ResponseUsageParser {
	return &anthropicUsageParser{}
}

func anthropicProviderURL(baseURL, requestPath, rawQuery string) (*url.URL, error) {
	parsed, err := parseProviderBaseURL(baseURL)
	if err != nil {
		return nil, newConfigurationError("invalid provider base URL: %w", err)
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/v1") {
		requestPath = trimAPIVersionPrefix(requestPath)
	}
	return appendProviderPath(parsed, requestPath, rawQuery), nil
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type anthropicResponse struct {
	Usage anthropicUsage `json:"usage"`
}

type anthropicStreamResponse struct {
	Message *anthropicResponse `json:"message"`
	Usage   anthropicUsage     `json:"usage"`
}

type anthropicUsageParser struct {
	latest    TokenUsage
	completed bool
}

func (p *anthropicUsageParser) ParseStreamLine(line string) StreamUsageParseResult {
	payload, ok := streamDataPayload(line)
	if !ok {
		return StreamUsageParseResult{}
	}
	var response anthropicStreamResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return StreamUsageParseResult{}
	}

	usage := response.Usage
	if response.Message != nil {
		usage.InputTokens += response.Message.Usage.InputTokens
		usage.CacheCreationInputTokens += response.Message.Usage.CacheCreationInputTokens
		usage.CacheReadInputTokens += response.Message.Usage.CacheReadInputTokens
		usage.OutputTokens += response.Message.Usage.OutputTokens
	}
	inputTokens := anthropicInputTokens(usage)
	if inputTokens > 0 {
		p.latest.PromptTokens = inputTokens
	}
	if usage.OutputTokens > 0 {
		p.latest.CompletionTokens = usage.OutputTokens
	}
	p.latest.TotalTokens = p.latest.PromptTokens + p.latest.CompletionTokens
	return StreamUsageParseResult{}
}

func (p *anthropicUsageParser) ParseBody(body []byte) (TokenUsage, bool) {
	var response anthropicResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return TokenUsage{}, false
	}
	usage := TokenUsage{
		PromptTokens:     anthropicInputTokens(response.Usage),
		CompletionTokens: response.Usage.OutputTokens,
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage, usage.TotalTokens > 0
}

func anthropicInputTokens(usage anthropicUsage) int {
	return usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
}

func (p *anthropicUsageParser) FinalStreamUsage() (TokenUsage, bool) {
	return p.latest, p.completed && p.latest.TotalTokens > 0
}

func (p *anthropicUsageParser) RecordStreamLineWritten(line string) {
	p.completed = p.completed || isJSONStreamEvent(line, "message_stop")
}

func (p *anthropicUsageParser) StreamCompleted() bool {
	return p.completed
}
