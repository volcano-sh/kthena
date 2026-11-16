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
					media = append(media, mediaItem)
				}
				continue
			}
		}
		text, ok := contentMap["text"].(string)
		if !ok {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 && len(media) == 0 {
		return common.Message{}, false
	}
	return common.Message{Role: role, Content: strings.Join(parts, "\n"), MultiModal: media}, true
}

// GetPromptString renders the text of a prompt. Media is not rendered, and
// messages that only hold media are skipped.
func GetPromptString(chatMessage *common.ChatMessage) string {
	return renderPrompt(chatMessage, false)
}

// GetPromptStringWithMedia renders a prompt like GetPromptString, and renders
// each media item as a placeholder holding its hash at the start of its
// message, so prompts that differ only in their media do not render the same.
func GetPromptStringWithMedia(chatMessage *common.ChatMessage) string {
	return renderPrompt(chatMessage, true)
}

func renderPrompt(chatMessage *common.ChatMessage, withMedia bool) string {
	// If Text field is present, return text directly (for prompt format)
	if chatMessage.Text != "" {
		return chatMessage.Text
	}

	// For chat messages, convert to ChatML format
	var result strings.Builder
	for _, msg := range chatMessage.Messages {
		if !withMedia && msg.Content == "" && len(msg.MultiModal) > 0 {
			continue
		}
		fmt.Fprintf(&result, "<|im_start|>%s\n", msg.Role)
		if withMedia {
			for _, item := range msg.MultiModal {
				fmt.Fprintf(&result, "<|%s:%016x|>", item.Modality, item.Hash)
			}
		}
		fmt.Fprintf(&result, "%s<|im_end|>\n", msg.Content)
	}
	return result.String()
}

func LoadEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		klog.Warningf("environment variable %s is not set, using default value: %s", key, defaultValue)
		return defaultValue
	}
	return value
}
