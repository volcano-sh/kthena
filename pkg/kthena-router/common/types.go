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

package common

const (
	UserIdKey            = "user_id"
	JWTTokenKey          = "jwt_token"
	TokenUsageKey        = "token_usage"
	RawRequestBodyKey    = "raw_request_body"
	UpstreamTransportKey = "upstream_transport"
)

// Modality is the kind of a non-text input carried by a request.
type Modality string

const (
	ModalityImage Modality = "image"
	ModalityAudio Modality = "audio"
	ModalityVideo Modality = "video"
)

// MultiModalItem is a non-text input (image, audio or video) of a request.
// The router never fetches or decodes media; it only records what is needed
// to tell items apart.
type MultiModalItem struct {
	Modality Modality
	// Hash identifies the item by what the client sent: the URL for media
	// passed by reference, or the encoded payload for inline media.
	Hash uint64
	// Offset is the byte offset in Message.Content at which the item appears,
	// so the item keeps its position relative to the text of its message.
	Offset int
}

// Message represents a single message in a chat conversation
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// MultiModal lists the non-text items of the message in request order.
	// It is not serialized, so tokenizer requests built from Message stay text-only.
	MultiModal []MultiModalItem `json:"-"`
	// ContentParts holds the content parts of a message given as a list, in
	// request order and in the Chat Completions format, so that the engine can
	// render a multimodal prompt exactly as it renders the request. It is nil
	// for string content, and for content holding media that has no Chat
	// Completions form, such as a file referenced by its ID. It is not
	// serialized.
	ContentParts []map[string]interface{} `json:"-"`
}

// ChatMessage represents either a direct text prompt or structured chat message
type ChatMessage struct {
	// Text is used for direct prompt input (completion mode)
	Text string `json:"text,omitempty"`

	// Messages is used for chat conversation input (chat mode)
	Messages []Message `json:"messages,omitempty"`

	// Request is the body of the request the prompt was parsed from, and API
	// the API of that request. The engine renders a multimodal prompt from
	// the request itself, through the render endpoint of its API, so that the
	// request fields that shape the prompt, such as tools, instructions,
	// chat_template_kwargs or mm_processor_kwargs, are rendered as for
	// inference. Request is nil for APIs without a render endpoint. Neither is
	// serialized.
	Request map[string]interface{} `json:"-"`
	API     API                    `json:"-"`
}

// API is an inference API whose requests the engine can render.
type API string

const (
	APIChatCompletions API = "chat_completions"
	APIResponses       API = "responses"
	APIMessages        API = "messages"
)

// HasMultiModal reports whether any message carries a non-text input.
func (m *ChatMessage) HasMultiModal() bool {
	if m == nil {
		return false
	}
	for _, msg := range m.Messages {
		if len(msg.MultiModal) > 0 {
			return true
		}
	}
	return false
}
