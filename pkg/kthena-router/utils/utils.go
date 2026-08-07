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

// ErrPromptNotFound is returned when the request body carries neither a "prompt"
// nor a "messages" field. It is distinct from a malformed prompt so that callers
// can answer "absent" and "present but invalid" with different status codes.
var ErrPromptNotFound = errors.New("prompt or messages not found in request body")

func GetNamespaceName(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// ParsePrompt extracts the prompt from a chat completions or completions request
// body. Every error other than ErrPromptNotFound means the body is malformed: no
// message is ever dropped silently, because a dropped message yields a prompt that
// looks valid to the caller while no longer matching what the client sent.
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

		msgs := make([]common.Message, 0, len(messageList))
		for i, message := range messageList {
			msgMap, ok := message.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("message at index %d is not an object", i)
			}

			role, ok := msgMap["role"].(string)
			if !ok {
				return nil, fmt.Errorf("message at index %d has no string role field", i)
			}

			content, err := parseMessageContent(msgMap["content"])
			if err != nil {
				return nil, fmt.Errorf("message at index %d: %w", i, err)
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

	return nil, ErrPromptNotFound
}

// parseMessageContent extracts the text of a single chat message. The OpenAI chat
// completions API allows "content" to be a plain string, an array of content parts,
// or null (for assistant messages that only carry tool_calls), so all three forms
// have to be accepted here. Anything else is a malformed request.
func parseMessageContent(content interface{}) (string, error) {
	switch c := content.(type) {
	case nil:
		// A message with null content still occupies a turn in the conversation,
		// so it is kept with an empty content rather than dropped. Note that a
		// missing "content" key also lands here, so a message that omits content
		// entirely is kept the same way; the two are not distinguishable from the
		// decoded map, and neither contributes any text to the prompt.
		return "", nil
	case string:
		return c, nil
	case []interface{}:
		// Text parts are concatenated verbatim, with no separator inserted between
		// them, which is how the server side reassembles them too. Any spacing has
		// to come from the parts themselves.
		var text strings.Builder
		for _, part := range c {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				return "", fmt.Errorf("message content part is not an object")
			}
			// Non-text parts (image_url, input_audio, ...) carry no text and are
			// skipped; only their text siblings contribute to the prompt.
			if partType, ok := partMap["type"].(string); !ok || partType != "text" {
				continue
			}
			partText, ok := partMap["text"].(string)
			if !ok {
				return "", fmt.Errorf("text content part has no string text field")
			}
			text.WriteString(partText)
		}
		return text.String(), nil
	default:
		return "", fmt.Errorf("message content is neither a string nor a list of content parts")
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
