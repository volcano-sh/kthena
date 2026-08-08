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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

func TestOpenAIAdapterBuildRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=true", nil)
	req.Header.Set("Authorization", "Bearer downstream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	req.Header.Set("X-Should-Not-Forward", "no")
	req.Header.Set("x-api-key", "downstream-key")
	req.Header.Set("Anthropic-Version", "must-not-leak")
	providerModel := "gpt-4o-mini"
	provider := &networkingv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "openai-provider",
		},
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.openai.com",
			Model:        &providerModel,
			Auth: &networkingv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
					Key:                  "api-key",
				},
			},
			Headers: map[string]string{
				"OpenAI-Organization": "org-test",
			},
		},
	}
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"api-key": []byte("provider-key"),
		},
	}
	body := map[string]interface{}{
		"model":  "client-model",
		"stream": true,
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]interface{}{"url": "https://example.com/cat.png"},
			}},
		}},
		"tools": []interface{}{map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": "lookup"},
		}},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, secret, body)
	if !assert.NoError(t, err) {
		return
	}

	assert.Equal(t, "https", upstream.URL.Scheme)
	assert.Equal(t, "api.openai.com", upstream.URL.Host)
	assert.Equal(t, "/v1/chat/completions", upstream.URL.Path)
	assert.Equal(t, "trace=true", upstream.URL.RawQuery)
	assert.Equal(t, "Bearer provider-key", upstream.Header.Get("Authorization"))
	assert.Equal(t, "", upstream.Header.Get("Cookie"))
	assert.Equal(t, "", upstream.Header.Get("x-api-key"))
	assert.Equal(t, "", upstream.Header.Get("Anthropic-Version"))
	assert.Equal(t, "", upstream.Header.Get("X-Should-Not-Forward"))
	assert.Equal(t, "text/event-stream", upstream.Header.Get("Accept"))
	assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", upstream.Header.Get("Traceparent"))
	assert.Equal(t, "org-test", upstream.Header.Get("OpenAI-Organization"))

	var got map[string]interface{}
	assert.NoError(t, json.NewDecoder(upstream.Body).Decode(&got))
	assert.Equal(t, providerModel, got["model"])
	assert.Equal(t, body["messages"], got["messages"])
	assert.Equal(t, body["tools"], got["tools"])
	streamOptions, ok := got["stream_options"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, true, streamOptions["include_usage"])
	tokenUsageInjected, _ := c.Get(common.TokenUsageKey)
	assert.Equal(t, true, tokenUsageInjected)
}

func TestOpenAIAdapterBuildRequestDoesNotInjectUsageForNonStreamingRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.example.com",
		},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{
		"model":  "m",
		"stream": false,
	})
	assert.NoError(t, err)

	var got map[string]interface{}
	assert.NoError(t, json.NewDecoder(upstream.Body).Decode(&got))
	assert.NotContains(t, got, "include_usage")
	assert.NotContains(t, got, "stream_options")
	_, tokenUsageInjected := c.Get(common.TokenUsageKey)
	assert.False(t, tokenUsageInjected)
}

func TestOpenAIAdapterBuildRequestMergesStreamingUsageOption(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.example.com",
		},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{
		"model":  "m",
		"stream": true,
		"stream_options": map[string]interface{}{
			"include_usage": false,
			"vendor_option": "preserve-me",
		},
	})
	assert.NoError(t, err)

	var got map[string]interface{}
	assert.NoError(t, json.NewDecoder(upstream.Body).Decode(&got))
	assert.Equal(t, map[string]interface{}{
		"include_usage": true,
		"vendor_option": "preserve-me",
	}, got["stream_options"])
	tokenUsageInjected, exists := c.Get(common.TokenUsageKey)
	assert.True(t, exists)
	assert.Equal(t, true, tokenUsageInjected)
}

func TestOpenAIAdapterBuildRequestPreservesInvalidStreamingUsageOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.example.com",
		},
	}
	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)

	tests := []struct {
		name          string
		streamOptions interface{}
	}{
		{
			name:          "stream options is not an object",
			streamOptions: "invalid",
		},
		{
			name: "include usage is not a boolean",
			streamOptions: map[string]interface{}{
				"include_usage": "invalid",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{
				"model":          "m",
				"stream":         true,
				"stream_options": tt.streamOptions,
			})
			assert.NoError(t, err)

			var got map[string]interface{}
			assert.NoError(t, json.NewDecoder(upstream.Body).Decode(&got))
			assert.Equal(t, tt.streamOptions, got["stream_options"])
			_, tokenUsageInjected := c.Get(common.TokenUsageKey)
			assert.False(t, tokenUsageInjected)
		})
	}
}

func TestOpenAIAdapterBuildRequestDoesNotDuplicateV1WhenBaseURLHasPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	tests := []struct {
		name    string
		baseURL string
		wantURL string
	}{
		{
			name:    "openai compatible base url already includes v1",
			baseURL: "https://api.example.com/v1",
			wantURL: "https://api.example.com/v1/chat/completions?trace=true",
		},
		{
			name:    "openai compatible base url includes provider-specific prefix",
			baseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			wantURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions?trace=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=true", nil)
			provider := &networkingv1alpha1.ExternalModelProvider{
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      tt.baseURL,
				},
			}

			adapter, err := NewAdapter(provider.Spec.ProviderType)
			assert.NoError(t, err)
			upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
			assert.NoError(t, err)

			assert.Equal(t, tt.wantURL, upstream.URL.String())
		})
	}
}

func TestOpenAIResponsesAdapterBuildRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?trace=true", nil)
	providerModel := "gpt-5.6-sol"
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.example.com/v1",
			Model:        &providerModel,
			Auth: &networkingv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
					Key:                  "api-key",
				},
			},
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"api-key": []byte("provider-key")}}
	input := []interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "input_image", "image_url": "https://example.com/cat.png"},
				map[string]interface{}{"type": "input_file", "file_id": "file-1"},
			},
		},
		map[string]interface{}{"type": "function_call_output", "call_id": "call-1", "output": map[string]interface{}{"ok": true}},
	}
	tools := []interface{}{map[string]interface{}{
		"type":       "function",
		"name":       "lookup",
		"parameters": map[string]interface{}{"type": "object"},
	}}
	body := map[string]interface{}{
		"model":  "route-model",
		"input":  input,
		"stream": true,
		"tools":  tools,
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, secret, body)
	if !assert.NoError(t, err) {
		return
	}

	assert.Equal(t, "https://api.example.com/v1/responses?trace=true", upstream.URL.String())
	assert.Equal(t, "Bearer provider-key", upstream.Header.Get("Authorization"))

	var got map[string]interface{}
	assert.NoError(t, json.NewDecoder(upstream.Body).Decode(&got))
	assert.Equal(t, providerModel, got["model"])
	assert.Equal(t, input, got["input"])
	assert.Equal(t, tools, got["tools"])
	assert.Equal(t, true, got["stream"])
	assert.NotContains(t, got, "include_usage")
	assert.NotContains(t, got, "stream_options")
	_, tokenUsageInjected := c.Get(common.TokenUsageKey)
	assert.False(t, tokenUsageInjected)
}

func TestAnthropicAdapterBuildRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer downstream")
	providerModel := "claude-3-5-sonnet-latest"
	provider := &networkingv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "anthropic-provider",
		},
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.Anthropic,
			BaseURL:      "https://api.anthropic.com",
			Model:        &providerModel,
			Auth: &networkingv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
					Key:                  "api-key",
				},
			},
			Headers: map[string]string{
				"anthropic-version": "2023-06-01",
			},
		},
	}
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"api-key": []byte("anthropic-key"),
		},
	}
	messages := []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": "AA=="}},
			map[string]interface{}{"type": "tool_use", "id": "tool-1", "name": "lookup", "input": map[string]interface{}{"q": "x"}},
		},
	}}
	tools := []interface{}{map[string]interface{}{
		"name":         "lookup",
		"input_schema": map[string]interface{}{"type": "object"},
	}}
	body := map[string]interface{}{
		"model":    "client-model",
		"stream":   true,
		"messages": messages,
		"tools":    tools,
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, secret, body)
	assert.NoError(t, err)

	assert.Equal(t, "https://api.anthropic.com/v1/messages", upstream.URL.String())
	assert.Equal(t, "anthropic-key", upstream.Header.Get("x-api-key"))
	assert.Equal(t, "", upstream.Header.Get("Authorization"))
	assert.Equal(t, "2023-06-01", upstream.Header.Get("anthropic-version"))

	var got map[string]interface{}
	assert.NoError(t, json.NewDecoder(upstream.Body).Decode(&got))
	assert.Equal(t, providerModel, got["model"])
	assert.Equal(t, messages, got["messages"])
	assert.Equal(t, tools, got["tools"])
	assert.NotContains(t, got, "stream_options")
	assert.NotContains(t, got, "include_usage")
	_, tokenUsageInjected := c.Get(common.TokenUsageKey)
	assert.False(t, tokenUsageInjected)
}

func TestAnthropicAdapterBuildRequestPreservesV1WhenBaseURLHasPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?trace=true", nil)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.Anthropic,
			BaseURL:      "https://api.example.com/anthropic",
		},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
	assert.NoError(t, err)

	assert.Equal(t, "https://api.example.com/anthropic/v1/messages?trace=true", upstream.URL.String())
}

func TestAnthropicAdapterBuildRequestDoesNotDuplicateV1(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?trace=true", nil)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.Anthropic,
			BaseURL:      "https://api.example.com/v1",
		},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
	assert.NoError(t, err)

	assert.Equal(t, "https://api.example.com/v1/messages?trace=true", upstream.URL.String())
}

func TestAnthropicAdapterForwardsProtocolVersionHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.Anthropic,
			BaseURL:      "https://api.anthropic.com",
		},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	upstream, err := adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
	assert.NoError(t, err)
	assert.Equal(t, "2023-06-01", upstream.Header.Get("Anthropic-Version"))
	assert.Equal(t, "context-1m-2025-08-07", upstream.Header.Get("Anthropic-Beta"))
}

func TestProviderAuthSchemeOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	secret := &corev1.Secret{Data: map[string][]byte{"api-key": []byte("provider-key")}}

	tests := []struct {
		name           string
		providerType   networkingv1alpha1.ExternalProviderType
		path           string
		scheme         networkingv1alpha1.ProviderAuthScheme
		wantAuthHeader string
		wantAPIKey     string
	}{
		{
			name:           "anthropic gateway uses bearer",
			providerType:   networkingv1alpha1.Anthropic,
			path:           "/v1/messages",
			scheme:         networkingv1alpha1.ProviderAuthSchemeBearer,
			wantAuthHeader: "Bearer provider-key",
		},
		{
			name:         "openai compatible gateway uses api key header",
			providerType: networkingv1alpha1.OpenAI,
			path:         "/v1/responses",
			scheme:       networkingv1alpha1.ProviderAuthSchemeAPIKey,
			wantAPIKey:   "provider-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req.Header.Set("Authorization", "Bearer downstream")
			req.Header.Set("x-api-key", "downstream-key")
			provider := &networkingv1alpha1.ExternalModelProvider{
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: tt.providerType,
					BaseURL:      "https://api.example.com",
					Auth: &networkingv1alpha1.ProviderAuth{
						Scheme: tt.scheme,
						SecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
							Key:                  "api-key",
						},
					},
				},
			}
			adapter, err := NewAdapter(tt.providerType)
			assert.NoError(t, err)
			upstream, err := adapter.BuildRequest(c, req, provider, secret, map[string]interface{}{"model": "m"})
			assert.NoError(t, err)
			assert.Equal(t, tt.wantAuthHeader, upstream.Header.Get("Authorization"))
			assert.Equal(t, tt.wantAPIKey, upstream.Header.Get("x-api-key"))
		})
	}
}

func TestOpenAIAdapterResponseParser(t *testing.T) {
	adapter, err := NewAdapter(networkingv1alpha1.OpenAI)
	assert.NoError(t, err)

	t.Run("chat completions", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		injected, _ := gin.CreateTestContext(httptest.NewRecorder())
		injected.Set(common.TokenUsageKey, true)
		parser := adapter.ResponseParser(injected, "/v1/chat/completions")

		result := parser.ParseStreamLine(`data: {"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)
		assert.True(t, result.HasUsage)
		assert.True(t, result.SuppressLine, "router-injected usage-only chunk must be withheld")
		assert.Equal(t, TokenUsage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33}, result.Usage)

		result = parser.ParseStreamLine(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)
		assert.True(t, result.HasUsage)
		assert.True(t, result.SuppressLine)

		result = parser.ParseStreamLine(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)
		assert.True(t, result.HasUsage)
		assert.False(t, result.SuppressLine, "content-bearing chunk must always be forwarded")

		clientOpted, _ := gin.CreateTestContext(httptest.NewRecorder())
		clientParser := adapter.ResponseParser(clientOpted, "/v1/chat/completions")
		result = clientParser.ParseStreamLine(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)
		assert.True(t, result.HasUsage)
		assert.False(t, result.SuppressLine, "client-requested usage chunk must be forwarded")

		usage, ok := parser.ParseBody([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`))
		assert.True(t, ok)
		assert.Equal(t, TokenUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12}, usage)

		usage, ok = parser.ParseBody([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":0,"total_tokens":7}}`))
		assert.True(t, ok)
		assert.Equal(t, TokenUsage{PromptTokens: 7, TotalTokens: 7}, usage)

		parser.RecordStreamLineWritten("data: [DONE]\n")
		assert.True(t, parser.StreamCompleted())
	})

	t.Run("responses", func(t *testing.T) {
		parser := adapter.ResponseParser(nil, "/v1/responses")

		result := parser.ParseStreamLine(`data: {"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}}`)
		assert.False(t, result.HasUsage)
		_, ok := parser.FinalStreamUsage()
		assert.False(t, ok, "usage from an incomplete stream must not be recorded")
		parser.RecordStreamLineWritten(`data: {"type":"response.completed"}`)
		assert.True(t, parser.StreamCompleted())

		usage, ok := parser.FinalStreamUsage()
		assert.True(t, ok)
		assert.Equal(t, TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}, usage)

		usage, ok = adapter.ResponseParser(nil, "/v1/responses").ParseBody([]byte(`{"usage":{"input_tokens":8,"output_tokens":2}}`))
		assert.True(t, ok)
		assert.Equal(t, TokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}, usage)
	})
}

func TestAnthropicAdapterResponseParser(t *testing.T) {
	adapter, err := NewAdapter(networkingv1alpha1.Anthropic)
	assert.NoError(t, err)
	parser := adapter.ResponseParser(nil, "/v1/messages")

	result := parser.ParseStreamLine(`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"cache_creation_input_tokens":7,"cache_read_input_tokens":5,"output_tokens":1}}}`)
	assert.False(t, result.HasUsage)
	result = parser.ParseStreamLine(`data: {"type":"message_delta","usage":{"output_tokens":22}}`)
	assert.False(t, result.HasUsage)
	_, ok := parser.FinalStreamUsage()
	assert.False(t, ok, "usage from an incomplete stream must not be recorded")
	parser.RecordStreamLineWritten(`data: {"type":"message_stop"}`)
	assert.True(t, parser.StreamCompleted())

	usage, ok := parser.FinalStreamUsage()
	assert.True(t, ok)
	assert.Equal(t, TokenUsage{PromptTokens: 23, CompletionTokens: 22, TotalTokens: 45}, usage)

	usage, ok = adapter.ResponseParser(nil, "/v1/messages").ParseBody([]byte(`{"usage":{"input_tokens":9,"cache_creation_input_tokens":6,"cache_read_input_tokens":3,"output_tokens":4}}`))
	assert.True(t, ok)
	assert.Equal(t, TokenUsage{PromptTokens: 18, CompletionTokens: 4, TotalTokens: 22}, usage)

	usage, ok = adapter.ResponseParser(nil, "/v1/messages").ParseBody([]byte(`{"usage":{"input_tokens":9,"output_tokens":0}}`))
	assert.True(t, ok)
	assert.Equal(t, TokenUsage{PromptTokens: 9, TotalTokens: 9}, usage)
}

func TestBuildRequestRequiresConfiguredSecretKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.openai.com",
			Auth: &networkingv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
					Key:                  "api-key",
				},
			},
		},
	}
	secret := &corev1.Secret{Data: map[string][]byte{"other": []byte("value")}}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	_, err = adapter.BuildRequest(c, req, provider, secret, map[string]interface{}{"model": "m"})
	assert.Error(t, err)
	var configurationError *ConfigurationError
	assert.ErrorAs(t, err, &configurationError)
}

func TestBuildRequestRejectsProtocolPathMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	t.Run("openai adapter rejects anthropic path", func(t *testing.T) {
		provider := &networkingv1alpha1.ExternalModelProvider{
			Spec: networkingv1alpha1.ExternalModelProviderSpec{
				ProviderType: networkingv1alpha1.OpenAI,
				BaseURL:      "https://api.openai.com",
			},
		}
		adapter, err := NewAdapter(provider.Spec.ProviderType)
		assert.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		_, err = adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
		var pathErr *UnsupportedPathError
		assert.ErrorAs(t, err, &pathErr)
	})

	t.Run("anthropic adapter rejects openai path", func(t *testing.T) {
		provider := &networkingv1alpha1.ExternalModelProvider{
			Spec: networkingv1alpha1.ExternalModelProviderSpec{
				ProviderType: networkingv1alpha1.Anthropic,
				BaseURL:      "https://api.anthropic.com",
			},
		}
		adapter, err := NewAdapter(provider.Spec.ProviderType)
		assert.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		_, err = adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
		var pathErr *UnsupportedPathError
		assert.ErrorAs(t, err, &pathErr)
	})
}

func TestBuildRequestRejectsReservedStaticHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.OpenAI,
			BaseURL:      "https://api.openai.com",
			Headers: map[string]string{
				"x-api-key": "must-use-auth",
			},
		},
	}

	adapter, err := NewAdapter(provider.Spec.ProviderType)
	assert.NoError(t, err)
	_, err = adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
	assert.Error(t, err)
}

func TestBuildRequestRejectsInvalidRuntimeConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name    string
		baseURL string
		headers map[string]string
	}{
		{
			name:    "plain HTTP URL",
			baseURL: "http://api.example.com",
		},
		{
			name:    "URL without host",
			baseURL: "https:///v1",
		},
		{
			name:    "URL with userinfo",
			baseURL: "https://user@example.com",
		},
		{
			name:    "URL with query",
			baseURL: "https://api.example.com?debug=true",
		},
		{
			name:    "invalid header name",
			baseURL: "https://api.example.com",
			headers: map[string]string{"Bad Header": "value"},
		},
		{
			name:    "invalid header value",
			baseURL: "https://api.example.com",
			headers: map[string]string{"X-Tenant": "tenant-a\r\nX-Injected: true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			provider := &networkingv1alpha1.ExternalModelProvider{
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      tt.baseURL,
					Headers:      tt.headers,
				},
			}
			adapter, err := NewAdapter(provider.Spec.ProviderType)
			assert.NoError(t, err)

			_, err = adapter.BuildRequest(c, req, provider, nil, map[string]interface{}{"model": "m"})
			var configurationError *ConfigurationError
			assert.ErrorAs(t, err, &configurationError)
		})
	}
}

func TestValidateConfigurationRejectsUnsupportedAuthScheme(t *testing.T) {
	provider := &networkingv1alpha1.ExternalModelProvider{
		Spec: networkingv1alpha1.ExternalModelProviderSpec{
			ProviderType: networkingv1alpha1.Anthropic,
			BaseURL:      "https://api.example.com",
			Auth: &networkingv1alpha1.ProviderAuth{
				Scheme: "Basic",
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
					Key:                  "api-key",
				},
			},
		},
	}

	err := ValidateConfiguration(provider)
	var configurationError *ConfigurationError
	assert.ErrorAs(t, err, &configurationError)
	assert.Contains(t, err.Error(), "unsupported provider auth scheme")
}

func TestDoTLSVerificationPolicy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	t.Run("secure client rejects self signed certificate", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.RequestURI = ""

		resp, err := Do(req, false)
		if resp != nil {
			resp.Body.Close()
		}

		assert.Error(t, err)
	})

	t.Run("insecure client allows self signed certificate", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.RequestURI = ""

		resp, err := Do(req, true)
		assert.NoError(t, err)
		if assert.NotNil(t, resp) {
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			assert.NoError(t, readErr)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "ok", string(body))
		}
	})
}

func TestProviderTransportHandlesCustomDefaultTransport(t *testing.T) {
	custom := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	})

	_, preservesCustomTransport := providerTransport(custom, false).(roundTripperFunc)
	assert.True(t, preservesCustomTransport)
	isolatedTransport, isolatesInsecureTransport := providerTransport(custom, true).(*http.Transport)
	if assert.True(t, isolatesInsecureTransport) {
		assert.NotNil(t, isolatedTransport.DialContext)
		assert.Equal(t, providerTransportBaseline.TLSHandshakeTimeout, isolatedTransport.TLSHandshakeTimeout)
		assert.Equal(t, providerTransportBaseline.IdleConnTimeout, isolatedTransport.IdleConnTimeout)
		assert.Equal(t, providerTransportBaseline.ExpectContinueTimeout, isolatedTransport.ExpectContinueTimeout)
		assert.Equal(t, providerTransportBaseline.MaxIdleConns, isolatedTransport.MaxIdleConns)
		assert.Zero(t, isolatedTransport.ResponseHeaderTimeout)
		if assert.NotNil(t, isolatedTransport.TLSClientConfig) {
			assert.True(t, isolatedTransport.TLSClientConfig.InsecureSkipVerify)
		}
	}
	assert.NotPanics(t, func() {
		_ = providerTransport(custom, true)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
