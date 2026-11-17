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

package utils

import (
	"fmt"
	"os"
	"strings"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

var (
	KVCacheUsage      = "kv_cache_usage"
	RequestWaitingNum = "request_waiting_num"
	RequestRunningNum = "request_running_num"
	TPOT              = "TPOT"
	TTFT              = "TTFT"
)

func GetNamespaceName(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

func ParsePrompt(body map[string]interface{}) (*common.ChatMessage, error) {
	if prompt, ok := body["prompt"]; ok {
		promptStr, ok := prompt.(string)
		if !ok {
			return nil, fmt.Errorf("prompt is not a string")
		}
		return &common.ChatMessage{
			Text: promptStr,
		}, nil
	}

	if messages, ok := body["messages"]; ok {
		messageList, ok := messages.([]interface{})
		if !ok {
			return nil, fmt.Errorf("messages is not a list")
		}

		msgs := make([]common.Message, 0, len(messageList)+1)
		if systemMsg, ok := parseMessage("system", body["system"]); ok {
			msgs = append(msgs, systemMsg)
		}
		for _, message := range messageList {
			msgMap, ok := message.(map[string]interface{})
			if !ok {
				continue
			}

			role, ok := msgMap["role"].(string)
			if !ok {
				continue
			}

			msg, ok := parseMessage(role, msgMap["content"])
			if !ok {
				continue
			}

			msgs = append(msgs, msg)
		}

		return &common.ChatMessage{
			Messages: msgs,
		}, nil
	}

	if input, ok := body["input"]; ok {
		return parseResponsesPrompt(body["instructions"], input)
	}

	return nil, fmt.Errorf("prompt or messages not found in request body")
}

func parseResponsesPrompt(instructions, input any) (*common.ChatMessage, error) {
	var msgs []common.Message
	if instructionText, ok := instructions.(string); ok && instructionText != "" {
		msgs = append(msgs, common.Message{Role: "developer", Content: instructionText})
	}

	parsedInput := false
	switch value := input.(type) {
	case string:
		msgs = append(msgs, common.Message{Role: "user", Content: value})
		parsedInput = true
	case []interface{}:
		// Responses input may contain only non-text content. Keep an empty
		// schedulable prompt so protocol-specific validation or the upstream
		// model can decide whether that content is supported.
		parsedInput = true
		for _, item := range value {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if itemType, ok := itemMap["type"].(string); ok && itemType != "" && itemType != "message" {
				continue
			}
			role, ok := itemMap["role"].(string)
			if !ok || role == "" {
				continue
			}
			msg, ok := parseMessage(role, itemMap["content"])
			if !ok {
				continue
			}
			msgs = append(msgs, msg)
		}
	default:
		return nil, fmt.Errorf("input is not a string or list")
	}
	if !parsedInput {
		return nil, fmt.Errorf("input does not contain text")
	}
	return &common.ChatMessage{Messages: msgs}, nil
}

// parseMessage builds a message from its role and content. The content is
// either a string or a list of content parts, whose text parts are joined into
// Content and whose media parts are kept in MultiModal. It returns false when
// the content has neither text nor media.
func parseMessage(role string, content any) (common.Message, bool) {
	if contentStr, ok := content.(string); ok {
		return common.Message{Role: role, Content: contentStr}, true
	}

	contentList, ok := content.([]interface{})
	if !ok {
		return common.Message{}, false
	}

	parts := make([]string, 0, len(contentList))
	var media []common.MultiModalItem
	var chatParts []map[string]interface{}
	renderable := true
	// textLen is the length of the text parts joined so far, which is where a
	// media part read now sits in Content.
	textLen := 0
	for _, item := range contentList {
		contentMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if contentType, ok := contentMap["type"].(string); ok {
			switch contentType {
			case "text", "input_text", "output_text":
			default:
				if mediaItem, ok := parseMediaPart(contentType, contentMap); ok {
					mediaItem.Offset = textLen
					media = append(media, mediaItem)
					if chatPart, ok := chatMediaPart(contentType, contentMap); ok {
						chatParts = append(chatParts, chatPart)
					} else {
						renderable = false
					}
				}
				continue
			}
		}
		text, ok := contentMap["text"].(string)
		if !ok {
			continue
		}
		if len(parts) > 0 {
			textLen++ // the "\n" joining this part to the previous one
		}
		textLen += len(text)
		parts = append(parts, text)
		chatParts = append(chatParts, map[string]interface{}{"type": "text", "text": text})
	}
	if len(parts) == 0 && len(media) == 0 {
		return common.Message{}, false
	}
	if !renderable {
		chatParts = nil
	}
	return common.Message{
		Role:         role,
		Content:      strings.Join(parts, "\n"),
		MultiModal:   media,
		ContentParts: chatParts,
	}, true
}

// GetPromptString renders the text of a prompt. Media is not rendered, and
// messages that only hold media are skipped.
func GetPromptString(chatMessage *common.ChatMessage) string {
	// If Text field is present, return text directly (for prompt format)
	if chatMessage.Text != "" {
		return chatMessage.Text
	}

	// For chat messages, convert to ChatML format
	var result strings.Builder
	for _, msg := range chatMessage.Messages {
		if msg.Content == "" && len(msg.MultiModal) > 0 {
			continue
		}
		fmt.Fprintf(&result, "<|im_start|>%s\n", msg.Role)
		result.WriteString(msg.Content)
		result.WriteString("<|im_end|>\n")
	}
	return result.String()
}

// PromptSegment is a part of a rendered prompt: either a run of text, or a
// single media item rendered as a placeholder holding its modality and hash.
type PromptSegment struct {
	Text  string
	Media bool
}

// GetPromptSegments renders a prompt like GetPromptString, but keeps messages
// that only hold media and splits the result at every media item, which gets
// a segment of its own at its position in its message. Prompts that differ
// only in their media, or in the order of their media and text, therefore
// render differently. A text-only prompt renders as one segment holding
// exactly what GetPromptString returns.
func GetPromptSegments(chatMessage *common.ChatMessage) []PromptSegment {
	if chatMessage.Text != "" {
		return []PromptSegment{{Text: chatMessage.Text}}
	}

	var segments []PromptSegment
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			segments = append(segments, PromptSegment{Text: text.String()})
			text.Reset()
		}
	}
	for _, msg := range chatMessage.Messages {
		fmt.Fprintf(&text, "<|im_start|>%s\n", msg.Role)
		// Offsets out of order or beyond the content are clamped, so every
		// item is still rendered.
		pos := 0
		for _, item := range msg.MultiModal {
			offset := min(max(item.Offset, pos), len(msg.Content))
			text.WriteString(msg.Content[pos:offset])
			flush()
			segments = append(segments, PromptSegment{
				Text:  fmt.Sprintf("<|%s:%016x|>", item.Modality, item.Hash),
				Media: true,
			})
			pos = offset
		}
		text.WriteString(msg.Content[pos:])
		text.WriteString("<|im_end|>\n")
	}
	flush()
	return segments
}

func LoadEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		klog.Warningf("environment variable %s is not set, using default value: %s", key, defaultValue)
		return defaultValue
	}
	return value
}
