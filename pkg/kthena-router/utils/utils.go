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
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
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
		promptStr, err := parseCompletionPrompt(prompt)
		if err != nil {
			return nil, err
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
		if systemContent, ok := parseMessageContent(body["system"]); ok {
			msgs = append(msgs, common.Message{
				Role:    "system",
				Content: systemContent,
			})
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

			content, ok := parseMessageContent(msgMap["content"])
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

	return nil, fmt.Errorf("prompt or messages not found in request body")
}

// parseCompletionPrompt returns the text the router uses for token estimation and
// prefix hashing of a Completions prompt. The Completions API accepts a string, a
// list of strings, a list of token IDs or a list of token-ID lists. A batch is
// joined with newlines, and token IDs are written as space-separated numbers so
// that identical token prefixes hash identically. The request body itself is
// forwarded unchanged.
func parseCompletionPrompt(prompt any) (string, error) {
	switch p := prompt.(type) {
	case string:
		return p, nil
	case []interface{}:
		if len(p) == 0 {
			return "", fmt.Errorf("prompt is an empty list")
		}
		if ids, ok := tokenIDsText(p); ok {
			return ids, nil
		}
		parts := make([]string, 0, len(p))
		_, stringBatch := p[0].(string)
		for _, item := range p {
			if stringBatch {
				text, ok := item.(string)
				if !ok {
					return "", fmt.Errorf("prompt list mixes strings with other values")
				}
				parts = append(parts, text)
				continue
			}
			ids, ok := item.([]interface{})
			if !ok || len(ids) == 0 {
				return "", fmt.Errorf("prompt list item is neither a string nor a list of token IDs")
			}
			text, ok := tokenIDsText(ids)
			if !ok {
				return "", fmt.Errorf("prompt list item is neither a string nor a list of token IDs")
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("prompt is neither a string nor a list")
	}
}

// tokenIDsText formats a list of token IDs as space-separated numbers. It reports
// false if any item is not a non-negative integer that fits in an int64.
func tokenIDsText(items []interface{}) (string, bool) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case json.Number:
			id, err := v.Int64()
			if err != nil || id < 0 {
				return "", false
			}
			ids = append(ids, strconv.FormatInt(id, 10))
		case float64:
			// float64(math.MaxInt64) rounds up to 2^63, so ">=" keeps 2^63 out
			// of the int64 conversion below.
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.MaxInt64 || v != math.Trunc(v) {
				return "", false
			}
			ids = append(ids, strconv.FormatInt(int64(v), 10))
		default:
			return "", false
		}
	}
	return strings.Join(ids, " "), true
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
