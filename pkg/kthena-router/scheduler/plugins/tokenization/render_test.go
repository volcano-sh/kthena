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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/testutil"
)

func TestNewVLLMChatRenderRequest(t *testing.T) {
	image := common.MultiModalItem{Modality: common.ModalityImage, Hash: 1}
	parts := []map[string]interface{}{
		{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/cat.png"}},
		{"type": "text", "text": "describe"},
	}

	t.Run("messages keep their content parts", func(t *testing.T) {
		request, err := newVLLMChatRenderRequest("model", &common.ChatMessage{Messages: []common.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "describe", MultiModal: []common.MultiModalItem{image}, ContentParts: parts},
		}})
		require.NoError(t, err)
		data, err := json.Marshal(request)
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"model": "model",
			"messages": [
				{"role": "system", "content": "be brief"},
				{"role": "user", "content": [
					{"type": "image_url", "image_url": {"url": "https://example.com/cat.png"}},
					{"type": "text", "text": "describe"}
				]}
			],
			"add_generation_prompt": true,
			"mm_payload": "none"
		}`, string(data))
	})

	t.Run("text prompt is not renderable", func(t *testing.T) {
		_, err := newVLLMChatRenderRequest("model", &common.ChatMessage{Text: "hello"})
		assert.ErrorIs(t, err, errNotRenderable)
	})

	t.Run("media without content parts is not renderable", func(t *testing.T) {
		_, err := newVLLMChatRenderRequest("model", &common.ChatMessage{Messages: []common.Message{
			{Role: "user", MultiModal: []common.MultiModalItem{image}},
		}})
		assert.ErrorIs(t, err, errNotRenderable)
	})

	t.Run("chat completions request is rendered with its own fields", func(t *testing.T) {
		chatRequest := map[string]interface{}{
			"model":                "route-model",
			"messages":             []interface{}{map[string]interface{}{"role": "user", "content": []interface{}{parts[0], parts[1]}}},
			"tools":                []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "f"}}},
			"chat_template_kwargs": map[string]interface{}{"enable_thinking": false},
			"mm_processor_kwargs":  map[string]interface{}{"max_pixels": 1024},
			"stream":               true,
			"stream_options":       map[string]interface{}{"include_usage": true},
		}
		request, err := newVLLMChatRenderRequest("model", &common.ChatMessage{
			Messages: []common.Message{
				{Role: "user", Content: "describe", MultiModal: []common.MultiModalItem{image}, ContentParts: parts},
			},
			ChatRequest: chatRequest,
		})
		require.NoError(t, err)
		data, err := json.Marshal(request)
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"model": "model",
			"messages": [{"role": "user", "content": [
				{"type": "image_url", "image_url": {"url": "https://example.com/cat.png"}},
				{"type": "text", "text": "describe"}
			]}],
			"tools": [{"type": "function", "function": {"name": "f"}}],
			"chat_template_kwargs": {"enable_thinking": false},
			"mm_processor_kwargs": {"max_pixels": 1024},
			"mm_payload": "none"
		}`, string(data))
		assert.Equal(t, "route-model", chatRequest["model"], "the inference request must not change")
		assert.Contains(t, chatRequest, "stream")
	})
}

func TestReferencesRemoteMedia(t *testing.T) {
	tests := []struct {
		name string
		part map[string]interface{}
		want bool
	}{
		{"image by URL", map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://127.0.0.1:15000/debug/pprof/"}}, true},
		{"bare video URL", map[string]interface{}{"type": "video_url", "video_url": "https://example.com/v.mp4"}, true},
		{"image without URL", map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{}}, true},
		{"inline image", map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}}, false},
		{"bare inline audio URL", map[string]interface{}{"type": "audio_url", "audio_url": "DATA:audio/wav;base64,AAAA"}, false},
		{"input audio", map[string]interface{}{"type": "input_audio", "input_audio": map[string]interface{}{"data": "AAAA", "format": "wav"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := &common.ChatMessage{Messages: []common.Message{{
				Role:         "user",
				ContentParts: []map[string]interface{}{{"type": "text", "text": "hi"}, tt.part},
			}}}
			assert.Equal(t, tt.want, referencesRemoteMedia(prompt))
		})
	}
}

func TestParseVLLMChatRenderResponse(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    *RenderResult
		wantErr bool
	}{
		{
			name: "media of every modality ordered by offset",
			data: `{"token_ids": [1, 8, 8, 2, 9, 9, 9, 3], "features": {
				"mm_hashes": {"image": ["img"], "audio": ["aud"]},
				"mm_placeholders": {"image": [{"offset": 4, "length": 3, "is_embed": null}], "audio": [{"offset": 1, "length": 2}]},
				"kwargs_data": null}}`,
			want: &RenderResult{
				Tokens: []uint32{1, 8, 8, 2, 9, 9, 9, 3},
				MultiModal: []MultiModalPlaceholder{
					{Identifier: "aud", Offset: 1, Length: 2},
					{Identifier: "img", Offset: 4, Length: 3},
				},
			},
		},
		{
			name: "text-only render has no features",
			data: `{"token_ids": [1, 2], "features": null}`,
			want: &RenderResult{Tokens: []uint32{1, 2}},
		},
		{
			name:    "no token IDs",
			data:    `{"token_ids": []}`,
			wantErr: true,
		},
		{
			name: "hashes and placeholders differ in number",
			data: `{"token_ids": [1, 2, 3], "features": {
				"mm_hashes": {"image": ["a", "b"]}, "mm_placeholders": {"image": [{"offset": 0, "length": 1}]}}}`,
			wantErr: true,
		},
		{
			name: "placeholders without hashes",
			data: `{"token_ids": [1, 2, 3], "features": {
				"mm_hashes": {}, "mm_placeholders": {"image": [{"offset": 0, "length": 1}]}}}`,
			wantErr: true,
		},
		{
			name: "placeholder beyond the tokens",
			data: `{"token_ids": [1, 2, 3], "features": {
				"mm_hashes": {"image": ["a"]}, "mm_placeholders": {"image": [{"offset": 2, "length": 2}]}}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVLLMChatRenderResponse([]byte(tt.data))
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// newRenderTestServer returns an httptest server that renders every request
// into the given tokens and counts the requests it received.
func newRenderTestServer(t *testing.T, tokens []uint32, statusCode int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != vllmChatRenderPath {
			http.NotFound(w, r)
			return
		}
		if statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"token_ids": tokens})
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestTokenizerManagerRenderPrompt(t *testing.T) {
	newPrompt := func(url string) *common.ChatMessage {
		return &common.ChatMessage{Messages: []common.Message{{
			Role:         "user",
			MultiModal:   []common.MultiModalItem{{Modality: common.ModalityImage, Hash: 1}},
			ContentParts: []map[string]interface{}{{"type": "image_url", "image_url": map[string]interface{}{"url": url}}},
		}}}
	}
	prompt := newPrompt("data:image/png;base64,AAAA")
	remotePrompt := newPrompt("https://example.com/cat.png")
	newManager := func(service string, fallback bool, enginePort int) *TokenizerManager {
		return NewTokenizerManager(TokenizerManagerConfig{
			EndpointPorts: map[string]int{EngineVLLM: enginePort, EngineSGLang: enginePort},
			Service: TokenizerServiceConfig{
				Enabled:          service != "",
				Endpoint:         service,
				FallbackToEngine: fallback,
			},
		})
	}

	t.Run("service success skips engine pods", func(t *testing.T) {
		service, _ := newRenderTestServer(t, []uint32{1, 2}, http.StatusOK)
		engine, engineRequests := newRenderTestServer(t, []uint32{9}, http.StatusOK)
		engineAddr := engine.Listener.Addr().(*net.TCPAddr)
		pod := testutil.PodInfoWithEngine("pod-vllm", "default", engineAddr.IP.String(), EngineVLLM)

		result, err := newManager(service.URL, true, engineAddr.Port).RenderPrompt("model", prompt, []*datastore.PodInfo{pod})
		require.NoError(t, err)
		assert.Equal(t, []uint32{1, 2}, result.Tokens)
		assert.Zero(t, engineRequests.Load())
	})

	t.Run("service failure falls back to a vLLM pod", func(t *testing.T) {
		service, _ := newRenderTestServer(t, nil, http.StatusNotFound)
		engine, engineRequests := newRenderTestServer(t, []uint32{9}, http.StatusOK)
		engineAddr := engine.Listener.Addr().(*net.TCPAddr)
		pods := []*datastore.PodInfo{
			testutil.PodInfoWithEngine("pod-sglang", "default", "192.0.2.1", EngineSGLang),
			testutil.PodInfoWithEngine("pod-vllm", "default", engineAddr.IP.String(), EngineVLLM),
		}

		for i := 0; i < 5; i++ {
			result, err := newManager(service.URL, true, engineAddr.Port).RenderPrompt("model", prompt, pods)
			require.NoError(t, err)
			assert.Equal(t, []uint32{9}, result.Tokens)
		}
		assert.Equal(t, int32(5), engineRequests.Load())
	})

	t.Run("service failure without fallback returns error", func(t *testing.T) {
		service, _ := newRenderTestServer(t, nil, http.StatusNotFound)
		engine, engineRequests := newRenderTestServer(t, []uint32{9}, http.StatusOK)
		engineAddr := engine.Listener.Addr().(*net.TCPAddr)
		pod := testutil.PodInfoWithEngine("pod-vllm", "default", engineAddr.IP.String(), EngineVLLM)

		_, err := newManager(service.URL, false, engineAddr.Port).RenderPrompt("model", prompt, []*datastore.PodInfo{pod})
		assert.Error(t, err)
		assert.Zero(t, engineRequests.Load())
	})

	t.Run("no vLLM pod returns error", func(t *testing.T) {
		pod := testutil.PodInfoWithEngine("pod-sglang", "default", "127.0.0.1", EngineSGLang)
		_, err := newManager("", true, 8000).RenderPrompt("model", prompt, []*datastore.PodInfo{pod})
		assert.Error(t, err)
	})

	t.Run("media passed by URL is rendered on a vLLM pod only", func(t *testing.T) {
		service, serviceRequests := newRenderTestServer(t, []uint32{1, 2}, http.StatusOK)
		engine, engineRequests := newRenderTestServer(t, []uint32{9}, http.StatusOK)
		engineAddr := engine.Listener.Addr().(*net.TCPAddr)
		pod := testutil.PodInfoWithEngine("pod-vllm", "default", engineAddr.IP.String(), EngineVLLM)

		result, err := newManager(service.URL, true, engineAddr.Port).RenderPrompt("model", remotePrompt, []*datastore.PodInfo{pod})
		require.NoError(t, err)
		assert.Equal(t, []uint32{9}, result.Tokens)

		_, err = newManager(service.URL, false, engineAddr.Port).RenderPrompt("model", remotePrompt, []*datastore.PodInfo{pod})
		assert.Error(t, err)

		assert.Zero(t, serviceRequests.Load())
		assert.Equal(t, int32(1), engineRequests.Load())
	})

	t.Run("text prompt is not rendered", func(t *testing.T) {
		engine, engineRequests := newRenderTestServer(t, []uint32{9}, http.StatusOK)
		engineAddr := engine.Listener.Addr().(*net.TCPAddr)
		pod := testutil.PodInfoWithEngine("pod-vllm", "default", engineAddr.IP.String(), EngineVLLM)

		_, err := newManager("", true, engineAddr.Port).RenderPrompt("model", &common.ChatMessage{Text: "hi"}, []*datastore.PodInfo{pod})
		assert.ErrorIs(t, err, errNotRenderable)
		assert.Zero(t, engineRequests.Load())
	})
}
