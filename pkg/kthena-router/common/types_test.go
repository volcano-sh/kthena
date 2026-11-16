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

import (
	"encoding/json"
	"testing"
)

func TestChatMessageHasMultiModal(t *testing.T) {
	image := []MultiModalItem{{Modality: ModalityImage, Hash: 1}}
	tests := []struct {
		name string
		msg  *ChatMessage
		want bool
	}{
		{name: "nil", msg: nil, want: false},
		{name: "text prompt", msg: &ChatMessage{Text: "hello"}, want: false},
		{name: "text messages", msg: &ChatMessage{Messages: []Message{{Role: "user", Content: "hi"}}}, want: false},
		{
			name: "media in a later message",
			msg: &ChatMessage{Messages: []Message{
				{Role: "system", Content: "be brief"},
				{Role: "user", MultiModal: image},
			}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.msg.HasMultiModal(); got != tt.want {
				t.Errorf("HasMultiModal() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Tokenizer requests marshal Message; media must not leak into them.
func TestMessageJSONOmitsMultiModal(t *testing.T) {
	data, err := json.Marshal(Message{Role: "user", Content: "hi", MultiModal: []MultiModalItem{{Modality: ModalityImage, Hash: 1}}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if want := `{"role":"user","content":"hi"}`; string(data) != want {
		t.Errorf("Marshal() = %s, want %s", data, want)
	}
}
