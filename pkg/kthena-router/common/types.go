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
}

// ChatMessage represents either a direct text prompt or structured chat message
type ChatMessage struct {
	// Text is used for direct prompt input (completion mode)
	Text string `json:"text,omitempty"`

	// Messages is used for chat conversation input (chat mode)
	Messages []Message `json:"messages,omitempty"`
}

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
