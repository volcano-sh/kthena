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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// DefaultRouterURL is the default URL for the router service via port-forward
	// Use 127.0.0.1 instead of localhost to avoid IPv6 resolution issues in CI environments
	DefaultRouterURL = "http://127.0.0.1:8080/v1/chat/completions"
	// DefaultChatMaxTokens matches OpenAI-style e2e backends (e.g. Dynamo mocker) that require max_tokens.
	DefaultChatMaxTokens = 16
)

// ChatMessage represents a chat message in the request
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionsRequest represents a chat completions API request
type ChatCompletionsRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens"`
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
		Model:     modelName,
		Messages:  messages,
		Stream:    false,
		MaxTokens: DefaultChatMaxTokens,
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
	requestBody := ChatCompletionsRequest{
		Model:     modelName,
		Messages:  messages,
		Stream:    false,
		MaxTokens: DefaultChatMaxTokens,
	}
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

// SendRouterChatRequests sends count chat requests to the router and requires HTTP 200 for each.
func SendRouterChatRequests(t *testing.T, routerChatURL, modelName, prompt string, count int) {
	t.Helper()
	messages := []ChatMessage{NewChatMessage("user", prompt)}
	for i := 0; i < count; i++ {
		resp := SendChatRequestWithRetryQuiet(t, routerChatURL, modelName, messages, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

// DirectChatToPod sends count streaming chat requests directly to a pod via port-forward.
func DirectChatToPod(t *testing.T, pod corev1.Pod, model, prompt string, count, maxTokens int) {
	t.Helper()
	localPort := AllocateLocalPort(t)
	pf, err := SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err)
	defer pf.Close()

	url := fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	body, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     true,
	})
	require.NoError(t, err)
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 0; i < count; i++ {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	t.Logf("sent %d streaming chat warmup requests to pod %s (max_tokens=%d)", count, pod.Name, maxTokens)
}

// StartSustainedLongRequestsToPod keeps concurrent long requests on one pod via port-forward.
// Traffic bypasses the router and raises engine waiting for scheduler Filter plugins.
func StartSustainedLongRequestsToPod(t *testing.T, pod corev1.Pod, model, prompt string, concurrency, maxTokens int) func() {
	t.Helper()
	localPort := AllocateLocalPort(t)
	pf, err := SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err)

	url := fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	body, err := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     true,
	})
	require.NoError(t, err)

	client := &http.Client{Timeout: 2 * time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < concurrency; i++ {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if err != nil && ctx.Err() != nil {
					return
				}
			}
		}()
	}

	return func() {
		cancel()
		pf.Close()
	}
}

func SendChatRequestWithURL(t *testing.T, url string, modelName string, messages []ChatMessage) *http.Response {
	requestBody := ChatCompletionsRequest{
		Model:     modelName,
		Messages:  messages,
		Stream:    false,
		MaxTokens: DefaultChatMaxTokens,
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

// SendChatRequestUntilRouterProgrammed polls until the Kthena Router has programmed the route (stops returning 404).
// We don't retry on 503 because it only occurs when the client disconnects before receiving a response.
func SendChatRequestUntilRouterProgrammed(t *testing.T, modelName string, messages []ChatMessage) *http.Response {
	var finalResp *http.Response
	ctx := context.Background()

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		resp := SendChatRequest(t, modelName, messages)
		if resp.StatusCode == http.StatusNotFound {
			t.Logf("Route not yet programmed (404), retrying...")
			resp.Body.Close()
			return false, nil
		}
		finalResp = resp
		return true, nil
	})

	require.NoError(t, err, "Router never programmed the route for model %q", modelName)
	return finalResp
}
