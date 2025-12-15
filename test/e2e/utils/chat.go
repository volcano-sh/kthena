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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// DefaultRouterURL is the default URL for the router service via port-forward
	// Use 127.0.0.1 instead of localhost to avoid IPv6 resolution issues in CI environments
	DefaultRouterURL = "http://127.0.0.1:8080/v1/chat/completions"
)

// ChatMessage represents a chat message in the request
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionsRequest represents a chat completions API request
type ChatCompletionsRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// ChatCompletionsResponse represents the response from chat completions API
type ChatCompletionsResponse struct {
	StatusCode int
	Body       string
}

// CheckChatCompletions sends a chat completions request to the router service and verifies the response.
// It uses the port-forwarded router service at localhost:8080.
func CheckChatCompletions(t *testing.T, modelName string, messages []ChatMessage) *ChatCompletionsResponse {
	return CheckChatCompletionsWithURL(t, DefaultRouterURL, modelName, messages)
}

// CheckChatCompletionsWithURL sends a chat completions request to the specified URL and verifies the response.
// It retries with exponential backoff if the request fails or returns a non-200 status code.
func CheckChatCompletionsWithURL(t *testing.T, url string, modelName string, messages []ChatMessage) *ChatCompletionsResponse {
	requestBody := ChatCompletionsRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
	}

	jsonData, err := json.Marshal(requestBody)
	require.NoError(t, err, "Failed to marshal request body")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Retry configuration
	maxRetries := 10
	initialBackoff := 1 * time.Second
	maxBackoff := 10 * time.Second
	backoff := initialBackoff

	var resp *http.Response
	var responseStr string

	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		require.NoError(t, err, "Failed to create HTTP request")
		req.Header.Set("Content-Type", "application/json")

		resp, err = client.Do(req)
		if err != nil {
			if attempt < maxRetries-1 {
				t.Logf("Attempt %d/%d failed: %v, retrying in %v...", attempt+1, maxRetries, err, backoff)
				time.Sleep(backoff)
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			require.NoError(t, err, "Failed to send HTTP request after retries")
		}

		// Read response body
		responseBody, err := io.ReadAll(resp.Body)
		resp.Body.Close() // Close immediately after reading
		if err != nil {
			if attempt < maxRetries-1 {
				t.Logf("Attempt %d/%d failed to read response: %v, retrying in %v...", attempt+1, maxRetries, err, backoff)
				time.Sleep(backoff)
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			require.NoError(t, err, "Failed to read response body after retries")
		}

		responseStr = string(responseBody)

		// Check if response is successful
		if resp.StatusCode == http.StatusOK && responseStr != "" && !containsError(responseStr) {
			t.Logf("Chat response status: %d", resp.StatusCode)
			t.Logf("Chat response: %s", responseStr)
			break
		}

		// If not successful and we have retries left, retry
		if attempt < maxRetries-1 {
			t.Logf("Attempt %d/%d returned status %d or error response, retrying in %v...", attempt+1, maxRetries, resp.StatusCode, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Last attempt, verify response
		t.Logf("Chat response status: %d", resp.StatusCode)
		t.Logf("Chat response: %s", responseStr)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected HTTP 200 status code")
		assert.NotEmpty(t, responseStr, "Chat response is empty")
		assert.NotContains(t, responseStr, "error", "Chat response contains error")
		break
	}

	return &ChatCompletionsResponse{
		StatusCode: resp.StatusCode,
		Body:       responseStr,
	}
}

// containsError checks if the response string contains error indicators
func containsError(response string) bool {
	responseLower := strings.ToLower(response)
	return strings.Contains(responseLower, "error")
}

// min returns the minimum of two time.Duration values
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// NewChatMessage creates a new chat message
func NewChatMessage(role, content string) ChatMessage {
	return ChatMessage{
		Role:    role,
		Content: content,
	}
}
