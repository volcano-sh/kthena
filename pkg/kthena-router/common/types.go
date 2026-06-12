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
	UserIdKey     = "user_id"
	JWTTokenKey   = "jwt_token"
	TokenUsageKey = "token_usage"
)

// Message represents a single message in a chat conversation
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatMessage represents either a direct text prompt or structured chat messages
type ChatMessage struct {
	// Text is used for direct prompt input (completion mode)
	Text string `json:"text,omitempty"`

	// Messages is used for chat conversation input (chat mode)
	Messages []Message `json:"messages,omitempty"`
}
