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
	"errors"
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

// ErrPromptNotFound is returned when the request body carries none of the
// "prompt", "messages" or "input" fields. It is distinct from a malformed prompt so that callers
// can answer "absent" and "present but invalid" with different status codes.
var ErrPromptNotFound = errors.New("prompt or messages not found in request body")

func GetNamespaceName(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// ParsePrompt extracts the prompt from a completions, chat completions or responses
// request body. Every error other than ErrPromptNotFound means the body is malformed:
// a malformed chat message is never dropped silently, because a dropped message
// yields a prompt that looks valid to the caller while no longer matching what the
// client sent.
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
		if len(messageList) == 0 {
			return nil, fmt.Errorf("messages list is empty")
		}

		msgs := make([]common.Message, 0, len(messageList)+1)
		if systemContent, ok := parseMessageContent(body["system"]); ok {
			msgs = append(msgs, common.Message{
				Role:    "system",
				Content: systemContent,
			})
		}
		for i, message := range messageList {
			msgMap, ok := message.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("message at index %d is not an object", i)
			}

			role, ok := msgMap["role"].(string)
			if !ok {
				return nil, fmt.Errorf("message at index %d has no string role field", i)
			}

			content, ok, err := parseChatMessageContent(msgMap["content"])
			if err != nil {
				return nil, fmt.Errorf("message at index %d: %w", i, err)
			}
			if !ok {
				continue
			}

			msgs = append(msgs, common.Message{
				Role:    role,
				Content: content,
			})
		}

		return &common.ChatMessage{
			Messages: msgs,
		}, nil
	}

	if input, ok := body["input"]; ok {
		return parseResponsesPrompt(body["instructions"], input)
	}

	return nil, ErrPromptNotFound
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
			content, ok := parseMessageContent(itemMap["content"])
			if !ok {
				continue
			}
			msgs = append(msgs, common.Message{Role: role, Content: content})
		}
	default:
		return nil, fmt.Errorf("input is not a string or list")
	}
	if !parsedInput {
		return nil, fmt.Errorf("input does not contain text")
	}
	return &common.ChatMessage{Messages: msgs}, nil
}

func parseMessageContent(content any) (string, bool) {
	if contentStr, ok := content.(string); ok {
		return contentStr, true
	}

	contentList, ok := content.([]interface{})
	if !ok {
		return "", false
	}

	parts := make([]string, 0, len(contentList))
	for _, item := range contentList {
		contentMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if contentType, ok := contentMap["type"].(string); ok {
			switch contentType {
			case "text", "input_text", "output_text":
			default:
				continue
			}
		}
		text, ok := contentMap["text"].(string)
		if !ok {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// parseChatMessageContent extracts the text of a single chat message. The chat
// APIs allow "content" to be a plain string, a list of content parts, or null (for
// assistant turns that only carry tool_calls). ok is false when the message carries
// no text at all, in which case it contributes nothing to the prompt. Any other
// shape is a malformed request and is reported instead of being dropped silently.
func parseChatMessageContent(content interface{}) (string, bool, error) {
	switch c := content.(type) {
	case nil:
		return "", false, nil
	case string:
		return c, true, nil
	case []interface{}:
		for _, part := range c {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				return "", false, fmt.Errorf("message content part is not an object")
			}
			switch partType, _ := partMap["type"].(string); partType {
			case "text", "input_text", "output_text":
				if _, ok := partMap["text"].(string); !ok {
					return "", false, fmt.Errorf("text content part has no string text field")
				}
			}
		}
		text, ok := parseMessageContent(c)
		return text, ok, nil
	default:
		return "", false, fmt.Errorf("message content is neither a string nor a list of content parts")
	}
}

func GetPromptString(chatMessage *common.ChatMessage) string {
	// If Text field is present, return text directly (for prompt format)
	if chatMessage.Text != "" {
		return chatMessage.Text
	}

	// For chat messages, convert to ChatML format
	var result strings.Builder
	for _, msg := range chatMessage.Messages {
		fmt.Fprintf(&result, "<|im_start|>%s\n%s<|im_end|>\n", msg.Role, msg.Content)
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
