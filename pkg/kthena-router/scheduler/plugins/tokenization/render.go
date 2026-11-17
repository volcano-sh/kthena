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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"k8s.io/klog/v2"
)

// vllmChatRenderPath is the vLLM endpoint that renders a chat request into the
// token IDs the engine would compute for it, with the media of the request
// expanded into placeholder tokens.
const vllmChatRenderPath = "/v1/chat/completions/render"

// MultiModalPlaceholder is the span of placeholder tokens a media item of a
// rendered prompt is expanded into, and the identifier the engine keys the
// item by in its prefix cache.
type MultiModalPlaceholder struct {
	Identifier string
	Offset     int
	Length     int
}

// RenderResult is a chat prompt rendered by the engine.
type RenderResult struct {
	Tokens []uint32
	// MultiModal holds the placeholders of the media of the prompt, ordered by
	// offset.
	MultiModal []MultiModalPlaceholder
}

type vllmChatRenderMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type vllmChatRenderRequest struct {
	Model               string                  `json:"model,omitempty"`
	Messages            []vllmChatRenderMessage `json:"messages"`
	AddGenerationPrompt bool                    `json:"add_generation_prompt"`
	// MMPayload "none" leaves the processed media tensors out of the response,
	// since only the token layout and the item identifiers are needed.
	MMPayload string `json:"mm_payload"`
}

type vllmPlaceholderRange struct {
	Offset int `json:"offset"`
	Length int `json:"length"`
}

type vllmChatRenderResponse struct {
	TokenIDs []uint32 `json:"token_ids"`
	Features *struct {
		MMHashes       map[string][]string               `json:"mm_hashes"`
		MMPlaceholders map[string][]vllmPlaceholderRange `json:"mm_placeholders"`
	} `json:"features"`
}

// errNotRenderable is returned for prompts that cannot be sent to the engine
// as the chat request it renders: text prompts, and messages holding media
// that has no Chat Completions form.
var errNotRenderable = errors.New("prompt cannot be rendered as a chat request")

func newVLLMChatRenderRequest(model string, prompt *common.ChatMessage) (*vllmChatRenderRequest, error) {
	if prompt.Text != "" || len(prompt.Messages) == 0 {
		return nil, errNotRenderable
	}
	messages := make([]vllmChatRenderMessage, 0, len(prompt.Messages))
	for _, msg := range prompt.Messages {
		var content interface{} = msg.Content
		if msg.ContentParts != nil {
			content = msg.ContentParts
		} else if len(msg.MultiModal) > 0 {
			return nil, errNotRenderable
		}
		messages = append(messages, vllmChatRenderMessage{Role: msg.Role, Content: content})
	}
	return &vllmChatRenderRequest{
		Model:               model,
		Messages:            messages,
		AddGenerationPrompt: true,
		MMPayload:           "none",
	}, nil
}

func parseVLLMChatRenderResponse(data []byte) (*RenderResult, error) {
	var resp vllmChatRenderResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse render response: %w", err)
	}
	if len(resp.TokenIDs) == 0 {
		return nil, fmt.Errorf("render response has no token IDs")
	}
	result := &RenderResult{Tokens: resp.TokenIDs}
	if resp.Features == nil {
		return result, nil
	}
	for modality, hashes := range resp.Features.MMHashes {
		placeholders := resp.Features.MMPlaceholders[modality]
		if len(placeholders) != len(hashes) {
			return nil, fmt.Errorf("render response has %d %s hashes but %d placeholders",
				len(hashes), modality, len(placeholders))
		}
		for i, placeholder := range placeholders {
			if placeholder.Offset < 0 || placeholder.Length <= 0 ||
				placeholder.Offset+placeholder.Length > len(resp.TokenIDs) {
				return nil, fmt.Errorf("render response has %s placeholder [%d, +%d) out of %d tokens",
					modality, placeholder.Offset, placeholder.Length, len(resp.TokenIDs))
			}
			result.MultiModal = append(result.MultiModal, MultiModalPlaceholder{
				Identifier: hashes[i],
				Offset:     placeholder.Offset,
				Length:     placeholder.Length,
			})
		}
	}
	for modality := range resp.Features.MMPlaceholders {
		if _, ok := resp.Features.MMHashes[modality]; !ok {
			return nil, fmt.Errorf("render response has %s placeholders but no hashes", modality)
		}
	}
	sort.Slice(result.MultiModal, func(i, j int) bool {
		return result.MultiModal[i].Offset < result.MultiModal[j].Offset
	})
	return result, nil
}

// RenderPrompt renders a chat prompt the way a vLLM engine renders the request
// it came from, media included. It uses the tokenizer service when it is
// enabled, falling back to a vLLM pod as configured. Only vLLM pods are used,
// since the render endpoint is specific to vLLM.
func (m *TokenizerManager) RenderPrompt(
	model string,
	prompt *common.ChatMessage,
	pods []*datastore.PodInfo,
) (*RenderResult, error) {
	request, err := newVLLMChatRenderRequest(model, prompt)
	if err != nil {
		return nil, err
	}

	if m.config.Service.Enabled {
		result, err := m.renderWith(m.config.Service.Endpoint, request)
		if err == nil {
			return result, nil
		}
		if !m.config.Service.FallbackToEngine {
			return nil, fmt.Errorf("tokenizer service failed to render prompt for model %s: %w", model, err)
		}
		klog.V(2).Infof("TokenizerManager: tokenizer service failed to render prompt for model %s, falling back to engine: %v", model, err)
	}

	endpoint, ok := m.vllmEndpoint(pods)
	if !ok {
		return nil, fmt.Errorf("no vLLM pod to render prompt for model %s", model)
	}
	return m.renderWith(endpoint, request)
}

func (m *TokenizerManager) renderWith(endpoint string, request *vllmChatRenderRequest) (*RenderResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := newHTTPClient(endpoint, m.client).Post(ctx, vllmChatRenderPath, request)
	if err != nil {
		return nil, fmt.Errorf("render request to %s failed: %w", endpoint, err)
	}
	return parseVLLMChatRenderResponse(data)
}

// vllmEndpoint returns the endpoint of a vLLM pod picked at random.
func (m *TokenizerManager) vllmEndpoint(pods []*datastore.PodInfo) (string, bool) {
	port, ok := m.config.EndpointPorts[EngineVLLM]
	if !ok || port < 1 || port > maxEndpointPort || len(pods) == 0 {
		return "", false
	}
	start := rand.Intn(len(pods))
	for i := range pods {
		podInfo := pods[(start+i)%len(pods)]
		pod := podInfo.GetPod()
		if pod == nil || pod.Status.PodIP == "" {
			continue
		}
		if engine, err := normalizeEngine(podInfo.GetEngine()); err != nil || engine != EngineVLLM {
			continue
		}
		return buildTokenizerEndpoint(pod.Status.PodIP, port), true
	}
	return "", false
}
