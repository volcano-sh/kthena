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
	"strings"
	"time"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"k8s.io/klog/v2"
)

// vllmChatRenderPath is the vLLM endpoint that renders a Chat Completions
// request into the token IDs the engine would compute for it, with the media of
// the request expanded into placeholder tokens.
const vllmChatRenderPath = "/v1/chat/completions/render"

// vllmRenderPaths are the vLLM render endpoints of the APIs whose requests are
// rendered as sent. Each endpoint prepares a request the way the engine does
// for inference on that API, for example the Responses endpoint turns
// instructions into a system message, and returns the same token IDs and
// media features as the Chat Completions one.
var vllmRenderPaths = map[common.API]string{
	common.APIChatCompletions: vllmChatRenderPath,
	common.APIResponses:       "/v1/responses/render",
	common.APIMessages:        "/v1/messages/render",
}

// MultiModalPlaceholder is a media item of a rendered prompt, as the engine
// places it in the tokens and keys it in its prefix cache. vLLM keys every KV
// block that overlaps the item by Identifier and by Offset relative to the
// start of the block, in addition to the token IDs of the block.
type MultiModalPlaceholder struct {
	// Identifier is what the engine identifies the item by: a hash vLLM
	// computes over the media, the model and the processor arguments, or one
	// derived from the uuid the client set on the content part. The same media
	// sent again with the same arguments gets the same identifier, so it
	// matches the KV blocks cached for it.
	Identifier string
	// Offset is the index in RenderResult.Tokens of the first placeholder
	// token of the item.
	Offset int
	// Length is the number of tokens of the item, starting at Offset: the
	// placeholder range vLLM reports for it, which for some models also
	// includes tokens between the media embeddings, such as row separators.
	Length int
}

// RenderResult is a chat prompt rendered by the engine.
type RenderResult struct {
	// Tokens are the token IDs of the prompt, media placeholders included.
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

type vllmRenderResponse struct {
	TokenIDs []uint32 `json:"token_ids"`
	Features *struct {
		MMHashes       map[string][]string               `json:"mm_hashes"`
		MMPlaceholders map[string][]vllmPlaceholderRange `json:"mm_placeholders"`
	} `json:"features"`
}

// errNotRenderable is returned for prompts the engine cannot render: text
// prompts, messages holding media that has no Chat Completions form, and
// Responses requests that continue a stored response.
var errNotRenderable = errors.New("prompt cannot be rendered by the engine")

// newVLLMRenderRequest builds the render request of a prompt and returns it
// with the vLLM endpoint it is sent to. A prompt parsed from a request of an
// API with a render endpoint is rendered from that request, through the
// endpoint of its API, so that every field that shapes the prompt, such as
// tools, instructions, chat_template_kwargs, mm_processor_kwargs or the tool
// calls of a message, is rendered as for inference. A prompt of another API
// is rendered from its messages in the Chat Completions format.
func newVLLMRenderRequest(model string, prompt *common.ChatMessage) (string, interface{}, error) {
	if prompt.Text != "" || len(prompt.Messages) == 0 {
		return "", nil, errNotRenderable
	}
	for _, msg := range prompt.Messages {
		if msg.ContentParts == nil && len(msg.MultiModal) > 0 {
			return "", nil, errNotRenderable
		}
	}
	if path, ok := vllmRenderPaths[prompt.API]; ok && prompt.Request != nil {
		// The render endpoint is stateless and rejects a response that
		// continues a stored one.
		if prompt.API == common.APIResponses && prompt.Request["previous_response_id"] != nil {
			return "", nil, errNotRenderable
		}
		request := make(map[string]interface{}, len(prompt.Request)+1)
		for key, value := range prompt.Request {
			request[key] = value
		}
		request["model"] = model
		request["mm_payload"] = "none"
		// The render response is not streamed, and vLLM rejects stream_options
		// without stream.
		delete(request, "stream")
		delete(request, "stream_options")
		return path, request, nil
	}
	messages := make([]vllmChatRenderMessage, 0, len(prompt.Messages))
	for _, msg := range prompt.Messages {
		var content interface{} = msg.Content
		if msg.ContentParts != nil {
			content = msg.ContentParts
		}
		messages = append(messages, vllmChatRenderMessage{Role: msg.Role, Content: content})
	}
	return vllmChatRenderPath, &vllmChatRenderRequest{
		Model:               model,
		Messages:            messages,
		AddGenerationPrompt: true,
		MMPayload:           "none",
	}, nil
}

func parseVLLMRenderResponse(data []byte) (*RenderResult, error) {
	var resp vllmRenderResponse
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
// since the render endpoints are specific to vLLM.
//
// Rendering fetches the media passed by URL. The tokenizer service can run in
// the router pod, where a client-provided URL could reach endpoints that are
// only meant to be reachable from within the pod, so a prompt holding such
// media is rendered on a vLLM pod, which fetches the media for inference
// anyway.
func (m *TokenizerManager) RenderPrompt(
	model string,
	prompt *common.ChatMessage,
	pods []*datastore.PodInfo,
) (*RenderResult, error) {
	path, request, err := newVLLMRenderRequest(model, prompt)
	if err != nil {
		return nil, err
	}

	if m.config.Service.Enabled {
		if referencesRemoteMedia(prompt) {
			if !m.config.Service.FallbackToEngine {
				return nil, fmt.Errorf("prompt for model %s holds media passed by URL, which the tokenizer service does not render", model)
			}
		} else {
			result, err := m.renderWith(m.config.Service.Endpoint, path, request)
			if err == nil {
				return result, nil
			}
			if !m.config.Service.FallbackToEngine {
				return nil, fmt.Errorf("tokenizer service failed to render prompt for model %s: %w", model, err)
			}
			klog.V(2).Infof("TokenizerManager: tokenizer service failed to render prompt for model %s, falling back to engine: %v", model, err)
		}
	}

	endpoint, ok := m.vllmEndpoint(pods)
	if !ok {
		return nil, fmt.Errorf("no vLLM pod to render prompt for model %s", model)
	}
	return m.renderWith(endpoint, path, request)
}

// mediaURLFields are the content part fields vLLM fetches media from. vLLM
// reads them from parts without a type, and from typed parts that carry a
// uuid, so they are checked whatever the type of a part says.
var mediaURLFields = []string{"image_url", "audio_url", "video_url"}

// promptFields are the fields of a request, per API, that hold the prompt and
// so the media the engine fetches to render it.
var promptFields = map[common.API][]string{
	common.APIChatCompletions: {"messages"},
	common.APIResponses:       {"input"},
	common.APIMessages:        {"system", "messages"},
}

// referencesRemoteMedia reports whether the render request of a prompt holds
// media passed by URL rather than inline in a data URL. A request rendered as
// sent is checked itself, at any depth of its prompt fields: it may hold parts
// that the router does not parse, such as media parts without a type, or the
// media of a tool output.
func referencesRemoteMedia(prompt *common.ChatMessage) bool {
	if fields, ok := promptFields[prompt.API]; ok && prompt.Request != nil {
		for _, field := range fields {
			if valueReferencesRemoteMedia(prompt.Request[field]) {
				return true
			}
		}
		return false
	}
	for _, msg := range prompt.Messages {
		for _, part := range msg.ContentParts {
			if partReferencesRemoteMedia(part) {
				return true
			}
		}
	}
	return false
}

// valueReferencesRemoteMedia reports whether a JSON value holds, at any depth,
// a content part with media passed by URL.
func valueReferencesRemoteMedia(value interface{}) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		if partReferencesRemoteMedia(v) {
			return true
		}
		for _, field := range v {
			if valueReferencesRemoteMedia(field) {
				return true
			}
		}
	case []interface{}:
		for _, item := range v {
			if valueReferencesRemoteMedia(item) {
				return true
			}
		}
	}
	return false
}

// partReferencesRemoteMedia reports whether a content part holds media passed
// by URL. A media part whose URL cannot be read counts as remote.
func partReferencesRemoteMedia(part map[string]interface{}) bool {
	hasURLField := false
	for _, field := range mediaURLFields {
		value, ok := part[field]
		if !ok {
			continue
		}
		hasURLField = true
		// The URL is held either directly or in the url field of an object.
		url, _ := value.(string)
		if source, ok := value.(map[string]interface{}); ok {
			url, _ = source["url"].(string)
		}
		if !isDataURL(url) {
			return true
		}
	}
	partType, _ := part["type"].(string)
	switch partType {
	case "image_url", "audio_url", "video_url", "input_image":
		return !hasURLField
	case "image":
		// An Anthropic image is fetched when its source is a URL, and is
		// otherwise turned into a data URL from its inline data. Its source
		// is used whatever other fields the part carries.
		source, _ := part["source"].(map[string]interface{})
		if source["type"] != "url" {
			return false
		}
		url, _ := source["url"].(string)
		return !isDataURL(url)
	}
	return false
}

func isDataURL(url string) bool {
	return len(url) >= len("data:") && strings.EqualFold(url[:len("data:")], "data:")
}

func (m *TokenizerManager) renderWith(endpoint, path string, request interface{}) (*RenderResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := newHTTPClient(endpoint, m.client).Post(ctx, path, request)
	if err != nil {
		return nil, fmt.Errorf("render request to %s failed: %w", endpoint, err)
	}
	return parseVLLMRenderResponse(data)
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
