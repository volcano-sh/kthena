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
	"os"
	"reflect"
	"testing"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGetNamespaceName(t *testing.T) {
	obj := &metav1.ObjectMeta{
		Namespace: "default",
		Name:      "test-obj",
	}

	expected := types.NamespacedName{
		Namespace: "default",
		Name:      "test-obj",
	}

	result := GetNamespaceName(obj)
	if result != expected {
		t.Errorf("GetNamespaceName() = %v, want %v", result, expected)
	}
}

func TestParsePrompt(t *testing.T) {
	tests := []struct {
		name    string
		body    map[string]interface{}
		want    *common.ChatMessage
		wantErr bool
	}{
		{
			name: "prompt as string",
			body: map[string]interface{}{
				"prompt": "hello",
			},
			want: &common.ChatMessage{
				Text: "hello",
			},
			wantErr: false,
		},
		{
			name: "prompt not a string",
			body: map[string]interface{}{
				"prompt": 123,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "messages as list",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{
						"role":    "user",
						"content": "hi",
					},
					map[string]interface{}{
						"role":    "assistant",
						"content": "hello",
					},
				},
			},
			want: &common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", Content: "hi"},
					{Role: "assistant", Content: "hello"},
				},
			},
			wantErr: false,
		},
		{
			name: "messages not a list",
			body: map[string]interface{}{
				"messages": "not a list",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "messages content as text blocks",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{
						"role": "user",
						"content": []interface{}{
							map[string]interface{}{
								"type": "text",
								"text": "hello",
							},
							map[string]interface{}{
								"type": "text",
								"text": "world",
							},
						},
					},
				},
			},
			want: &common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", Content: "hello\nworld"},
				},
			},
			wantErr: false,
		},
		{
			name: "chat mixed text and image extracts only text",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "describe"},
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/cat.png"}},
					},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{{Role: "user", Content: "describe"}}},
		},
		{
			name: "chat non-text-only content has an empty schedulable prompt",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": "https://example.com/cat.png"},
					}},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{}},
		},
		{
			name: "assistant tool call without text has an empty schedulable prompt",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "assistant",
					"tool_calls": []interface{}{map[string]interface{}{
						"id":   "call-1",
						"type": "function",
					}},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{}},
		},
		{
			name: "anthropic top-level system with messages",
			body: map[string]interface{}{
				"system": "You are a helpful assistant.",
				"messages": []interface{}{
					map[string]interface{}{
						"role":    "user",
						"content": "hello",
					},
				},
			},
			want: &common.ChatMessage{
				Messages: []common.Message{
					{Role: "system", Content: "You are a helpful assistant."},
					{Role: "user", Content: "hello"},
				},
			},
			wantErr: false,
		},
		{
			name: "anthropic top-level system as text blocks",
			body: map[string]interface{}{
				"system": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "Follow policy.",
					},
					map[string]interface{}{
						"type": "text",
						"text": "Be concise.",
					},
				},
				"messages": []interface{}{
					map[string]interface{}{
						"role":    "user",
						"content": "hello",
					},
				},
			},
			want: &common.ChatMessage{
				Messages: []common.Message{
					{Role: "system", Content: "Follow policy.\nBe concise."},
					{Role: "user", Content: "hello"},
				},
			},
			wantErr: false,
		},
		{
			name: "responses string input with instructions",
			body: map[string]interface{}{
				"instructions": "Be concise.",
				"input":        "Reply OK",
			},
			want: &common.ChatMessage{
				Messages: []common.Message{
					{Role: "developer", Content: "Be concise."},
					{Role: "user", Content: "Reply OK"},
				},
			},
			wantErr: false,
		},
		{
			name: "responses text message items",
			body: map[string]interface{}{
				"input": []interface{}{
					map[string]interface{}{
						"type": "message",
						"role": "user",
						"content": []interface{}{
							map[string]interface{}{"type": "input_text", "text": "hello"},
							map[string]interface{}{"type": "input_text", "text": "world"},
						},
					},
					map[string]interface{}{
						"type":    "message",
						"role":    "assistant",
						"content": "previous answer",
					},
				},
			},
			want: &common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", Content: "hello\nworld"},
					{Role: "assistant", Content: "previous answer"},
				},
			},
			wantErr: false,
		},
		{
			name: "responses non-text input has an empty schedulable prompt",
			body: map[string]interface{}{
				"input": []interface{}{
					map[string]interface{}{
						"type": "message",
						"role": "user",
						"content": []interface{}{
							map[string]interface{}{"type": "input_image", "image_url": "https://example.com/image.png"},
						},
					},
				},
			},
			want:    &common.ChatMessage{},
			wantErr: false,
		},
		{
			name: "responses input has unsupported shape",
			body: map[string]interface{}{
				"input": 123,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "neither prompt nor messages",
			body: map[string]interface{}{
				"foo": "bar",
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePrompt(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePrompt() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParsePrompt() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPromptString(t *testing.T) {
	tests := []struct {
		name        string
		chatMessage common.ChatMessage
		want        string
	}{
		{
			name: "text field present",
			chatMessage: common.ChatMessage{
				Text: "hello",
			},
			want: "hello",
		},
		{
			name: "messages field present",
			chatMessage: common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", Content: "hi"},
					{Role: "assistant", Content: "hello"},
				},
			},
			want: "<|im_start|>user\nhi<|im_end|>\n<|im_start|>assistant\nhello<|im_end|>\n",
		},
		{
			name:        "both empty",
			chatMessage: common.ChatMessage{},
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetPromptString(&tt.chatMessage); got != tt.want {
				t.Errorf("GetPromptString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadEnv(t *testing.T) {
	key := "TEST_ENV_VAR"
	defaultValue := "default"

	// Test default value
	os.Unsetenv(key)
	if got := LoadEnv(key, defaultValue); got != defaultValue {
		t.Errorf("LoadEnv() = %v, want %v", got, defaultValue)
	}

	// Test set value
	expectedValue := "set"
	os.Setenv(key, expectedValue)
	defer os.Unsetenv(key)
	if got := LoadEnv(key, defaultValue); got != expectedValue {
		t.Errorf("LoadEnv() = %v, want %v", got, expectedValue)
	}
}
