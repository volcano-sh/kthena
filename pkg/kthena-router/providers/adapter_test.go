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
