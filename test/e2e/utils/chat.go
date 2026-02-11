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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
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
	Attempts   int
}

// CheckChatCompletions sends a chat completions request to the router service and verifies the response.
// It uses the port-forwarded router service at localhost:8080.
func CheckChatCompletions(t *testing.T, modelName string, messages []ChatMessage) *ChatCompletionsResponse {
	return CheckChatCompletionsWithURLAndHeaders(t, DefaultRouterURL, modelName, messages, nil)
}

// CheckChatCompletionsWithURL sends a chat completions request to the specified URL and verifies the response.
// It retries with exponential backoff if the request fails or returns a non-200 status code.
func CheckChatCompletionsWithURL(t *testing.T, url string, modelName string, messages []ChatMessage) *ChatCompletionsResponse {
	return CheckChatCompletionsWithURLAndHeaders(t, url, modelName, messages, nil)
}

// CheckChatCompletionsWithHeaders sends a chat completions request with custom headers to the default router URL.
func CheckChatCompletionsWithHeaders(t *testing.T, modelName string, messages []ChatMessage, headers map[string]string) *ChatCompletionsResponse {
	return CheckChatCompletionsWithURLAndHeaders(t, DefaultRouterURL, modelName, messages, headers)
}

// SendChatRequestWithRetry sends a chat completions request with retry logic but without assertions.
// It returns the final response regardless of status code.
func SendChatRequestWithRetry(t *testing.T, url string, modelName string, messages []ChatMessage, headers map[string]string) *ChatCompletionsResponse {
	return sendChatRequestWithRetry(t, url, modelName, messages, headers, false)
}

// SendChatRequestWithRetryQuiet is like SendChatRequestWithRetry but does not log response status/body on success.
// Use it in loops or high-volume checks to avoid log flood (e.g. TestModelRouteSubsetShared weighted distribution).
func SendChatRequestWithRetryQuiet(t *testing.T, url string, modelName string, messages []ChatMessage, headers map[string]string) *ChatCompletionsResponse {
	return sendChatRequestWithRetry(t, url, modelName, messages, headers, true)
}

func sendChatRequestWithRetry(t *testing.T, url string, modelName string, messages []ChatMessage, headers map[string]string, quiet bool) *ChatCompletionsResponse {
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
	var attempt int

	for attempt = 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		require.NoError(t, err, "Failed to create HTTP request")
		req.Header.Set("Content-Type", "application/json")

		// Add custom headers if provided
		for key, value := range headers {
			req.Header.Set(key, value)
		}

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
		resp.Body.Close()
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
			if !quiet {
				t.Logf("Chat response status: %d", resp.StatusCode)
				t.Logf("Chat response: %s", responseStr)
			}
			break
		}

		if attempt < maxRetries-1 {
			t.Logf("Attempt %d/%d returned status %d or error response, retrying in %v...", attempt+1, maxRetries, resp.StatusCode, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		if !quiet {
			t.Logf("Chat response status: %d", resp.StatusCode)
			t.Logf("Chat response: %s", responseStr)
		}
		break
	}

	return &ChatCompletionsResponse{
		StatusCode: resp.StatusCode,
		Body:       responseStr,
		Attempts:   attempt + 1,
	}
}

func CheckChatCompletionsWithURLAndHeaders(t *testing.T, url string, modelName string, messages []ChatMessage, headers map[string]string) *ChatCompletionsResponse {
	resp := SendChatRequestWithRetry(t, url, modelName, messages, headers)

	// Assert successful response
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected HTTP 200 status code")
	assert.NotEmpty(t, resp.Body, "Chat response is empty")
	assert.NotContains(t, resp.Body, "error", "Chat response contains error")

	return resp
}

// CheckChatCompletionsQuiet is like CheckChatCompletions but does not log response status/body on success.
// Use it in high-volume loops (e.g. weighted distribution tests) to avoid log flood.
func CheckChatCompletionsQuiet(t *testing.T, modelName string, messages []ChatMessage) *ChatCompletionsResponse {
	resp := SendChatRequestWithRetryQuiet(t, DefaultRouterURL, modelName, messages, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected HTTP 200 status code")
	assert.NotEmpty(t, resp.Body, "Chat response is empty")
	assert.NotContains(t, resp.Body, "error", "Chat response contains error")
	return resp
}

// WaitForChatModelReady polls until the chat model is routable (returns 200).
// Use before assertions when the router may need time to discover new models.
func WaitForChatModelReady(t *testing.T, url, modelName string, messages []ChatMessage, timeout time.Duration) {
	t.Helper()
	requestBody := ChatCompletionsRequest{Model: modelName, Messages: messages, Stream: false}
	jsonData, err := json.Marshal(requestBody)
	require.NoError(t, err)
	client := &http.Client{Timeout: 30 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			return false, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("WaitForChatModelReady(%s): %v, retrying...", modelName, err)
			return false, nil
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && !containsError(string(body)) {
			return true, nil
		}
		return false, nil
	})
	require.NoError(t, err, "Model %s did not become ready within %v", modelName, timeout)
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
func SendChatRequest(t *testing.T, modelName string, messages []ChatMessage) *http.Response {
	return SendChatRequestWithURL(t, DefaultRouterURL, modelName, messages)
}

func SendChatRequestWithURL(t *testing.T, url string, modelName string, messages []ChatMessage) *http.Response {
	requestBody := ChatCompletionsRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
	}

	jsonData, err := json.Marshal(requestBody)
	require.NoError(t, err, "Failed to marshal request body")

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	require.NoError(t, err, "Failed to create HTTP request")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err, "Failed to send HTTP request")

	return resp
}
