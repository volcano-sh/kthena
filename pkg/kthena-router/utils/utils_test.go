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
	"reflect"
	"testing"

	"github.com/cespare/xxhash"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func image(source string, offset int) common.MultiModalItem {
	return common.MultiModalItem{Modality: common.ModalityImage, Hash: xxhash.Sum64String(source), Offset: offset}
}

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
			name: "chat mixed text and image keeps text and image",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "text", "text": "describe"},
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/cat.png"}},
					},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{{
				Role:       "user",
				Content:    "describe",
				MultiModal: []common.MultiModalItem{image("https://example.com/cat.png", 8)},
			}}},
		},
		{
			name: "chat image-only content keeps the message",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": "https://example.com/cat.png"},
					}},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{{
				Role:       "user",
				MultiModal: []common.MultiModalItem{image("https://example.com/cat.png", 0)},
			}}},
		},
		{
			name: "chat content without text or media has an empty schedulable prompt",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "file", "file": map[string]interface{}{"file_id": "file-1"}},
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{}},
					},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{}},
		},
		{
			name: "chat media parts of every supported type keep request order",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,iVBORw0KGgo="}},
						map[string]interface{}{"type": "text", "text": "compare"},
						map[string]interface{}{"type": "image_url", "image_url": "https://example.com/dog.png"},
						map[string]interface{}{"type": "input_audio", "input_audio": map[string]interface{}{"data": "UklGRg==", "format": "wav"}},
						map[string]interface{}{"type": "audio_url", "audio_url": map[string]interface{}{"url": "https://example.com/a.mp3"}},
						map[string]interface{}{"type": "video_url", "video_url": map[string]interface{}{"url": "https://example.com/v.mp4"}},
					},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{{
				Role:    "user",
				Content: "compare",
				MultiModal: []common.MultiModalItem{
					image("data:image/png;base64,iVBORw0KGgo=", 0),
					image("https://example.com/dog.png", 7),
					{Modality: common.ModalityAudio, Hash: xxhash.Sum64String("UklGRg=="), Offset: 7},
					{Modality: common.ModalityAudio, Hash: xxhash.Sum64String("https://example.com/a.mp3"), Offset: 7},
					{Modality: common.ModalityVideo, Hash: xxhash.Sum64String("https://example.com/v.mp4"), Offset: 7},
				},
			}}},
		},
		{
			name: "anthropic image blocks",
			body: map[string]interface{}{
				"messages": []interface{}{map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
						map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": "https://example.com/cat.png"}},
						map[string]interface{}{"type": "text", "text": "describe"},
					},
				}},
			},
			want: &common.ChatMessage{Messages: []common.Message{{
				Role:       "user",
				Content:    "describe",
				MultiModal: []common.MultiModalItem{image("iVBORw0KGgo=", 0), image("https://example.com/cat.png", 0)},
			}}},
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
			name: "responses image input",
			body: map[string]interface{}{
				"input": []interface{}{
					map[string]interface{}{
						"type": "message",
						"role": "user",
						"content": []interface{}{
							map[string]interface{}{"type": "input_image", "image_url": "https://example.com/image.png"},
							map[string]interface{}{"type": "input_image", "file_id": "file-1"},
						},
					},
				},
			},
			want: &common.ChatMessage{Messages: []common.Message{{
				Role:       "user",
				MultiModal: []common.MultiModalItem{image("https://example.com/image.png", 0), image("file-1", 0)},
			}}},
			wantErr: false,
		},
		{
			name: "responses input without text or media has an empty schedulable prompt",
			body: map[string]interface{}{
				"input": []interface{}{
					map[string]interface{}{
						"type": "message",
						"role": "user",
						"content": []interface{}{
							map[string]interface{}{"type": "input_file", "file_id": "file-1"},
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
		{
			name: "media not rendered and media-only messages skipped",
			chatMessage: common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", MultiModal: []common.MultiModalItem{{Modality: common.ModalityImage, Hash: 0x1}}},
					{Role: "user", Content: "describe", MultiModal: []common.MultiModalItem{{Modality: common.ModalityImage, Hash: 0x2}}},
					{Role: "assistant", Content: ""},
				},
			},
			want: "<|im_start|>user\ndescribe<|im_end|>\n<|im_start|>assistant\n<|im_end|>\n",
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

func TestGetPromptStringWithMedia(t *testing.T) {
	tests := []struct {
		name        string
		chatMessage common.ChatMessage
		want        string
	}{
		{
			name:        "text prompt",
			chatMessage: common.ChatMessage{Text: "hello"},
			want:        "hello",
		},
		{
			name: "text messages render like GetPromptString",
			chatMessage: common.ChatMessage{
				Messages: []common.Message{{Role: "user", Content: "hi"}},
			},
			want: "<|im_start|>user\nhi<|im_end|>\n",
		},
		{
			name: "media rendered as placeholders before text",
			chatMessage: common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", MultiModal: []common.MultiModalItem{{Modality: common.ModalityAudio, Hash: 0xff}}},
					{Role: "user", Content: "describe", MultiModal: []common.MultiModalItem{
						{Modality: common.ModalityImage, Hash: 0x1},
						{Modality: common.ModalityVideo, Hash: 0xabc},
					}},
				},
			},
			want: "<|im_start|>user\n<|audio:00000000000000ff|><|im_end|>\n" +
				"<|im_start|>user\n<|image:0000000000000001|><|video:0000000000000abc|>describe<|im_end|>\n",
		},
		{
			name: "media rendered at their offsets in the text",
			chatMessage: common.ChatMessage{
				Messages: []common.Message{
					{Role: "user", Content: "first\nsecond", MultiModal: []common.MultiModalItem{
						{Modality: common.ModalityImage, Hash: 0x1, Offset: 0},
						{Modality: common.ModalityImage, Hash: 0x2, Offset: 5},
						{Modality: common.ModalityAudio, Hash: 0x3, Offset: 12},
					}},
				},
			},
			want: "<|im_start|>user\n<|image:0000000000000001|>first<|image:0000000000000002|>\nsecond<|audio:0000000000000003|><|im_end|>\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetPromptStringWithMedia(&tt.chatMessage); got != tt.want {
				t.Errorf("GetPromptStringWithMedia() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPromptStringWithMediaKeepsPartOrder(t *testing.T) {
	text := map[string]interface{}{"type": "text", "text": "describe"}
	img := map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/cat.png"}}
	render := func(parts ...interface{}) string {
		t.Helper()
		prompt, err := ParsePrompt(map[string]interface{}{
			"messages": []interface{}{map[string]interface{}{"role": "user", "content": parts}},
		})
		if err != nil {
			t.Fatalf("ParsePrompt() error = %v", err)
		}
		return GetPromptStringWithMedia(prompt)
	}

	placeholder := fmt.Sprintf("<|image:%016x|>", xxhash.Sum64String("https://example.com/cat.png"))
	textFirst := render(text, img)
	imageFirst := render(img, text)
	if want := "<|im_start|>user\ndescribe" + placeholder + "<|im_end|>\n"; textFirst != want {
		t.Errorf("text then image rendered %q, want %q", textFirst, want)
	}
	if want := "<|im_start|>user\n" + placeholder + "describe<|im_end|>\n"; imageFirst != want {
		t.Errorf("image then text rendered %q, want %q", imageFirst, want)
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
