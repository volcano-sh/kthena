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

package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	providerPort        = 8443
	adminPort           = 8080
	maxRequestBodyBytes = 1 << 20
)

type mockConfig struct {
	OpenAIKey    string
	AnthropicKey string
	MaxCaptures  int
	MaxDelay     time.Duration
}

type capturedRequest struct {
	RequestID        string          `json:"request_id"`
	Method           string          `json:"method"`
	Path             string          `json:"path"`
	RawQuery         string          `json:"raw_query,omitempty"`
	Authorized       bool            `json:"authorized"`
	ContentType      string          `json:"content_type,omitempty"`
	AnthropicVersion string          `json:"anthropic_version,omitempty"`
	Body             json.RawMessage `json:"body"`
}

type captureStore struct {
	mu    sync.RWMutex
	max   int
	order []string
	items map[string]capturedRequest
}

func newCaptureStore(maxCaptures int) *captureStore {
	if maxCaptures <= 0 {
		maxCaptures = 100
	}
	return &captureStore{
		max:   maxCaptures,
		items: make(map[string]capturedRequest, maxCaptures),
	}
}

func (s *captureStore) put(capture capturedRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.items[capture.RequestID]; exists {
		for i, requestID := range s.order {
			if requestID == capture.RequestID {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	capture.Body = append(json.RawMessage(nil), capture.Body...)
	s.items[capture.RequestID] = capture
	s.order = append(s.order, capture.RequestID)

	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.items, oldest)
	}
}

func (s *captureStore) get(requestID string) (capturedRequest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	capture, exists := s.items[requestID]
	if !exists {
		return capturedRequest{}, false
	}
	capture.Body = append(json.RawMessage(nil), capture.Body...)
	return capture, true
}

func (s *captureStore) delete(requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.items[requestID]; !exists {
		return false
	}
	delete(s.items, requestID)
	for i, storedID := range s.order {
		if storedID == requestID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

type mockServer struct {
	config   mockConfig
	captures *captureStore
}

type providerProtocol int

const (
	protocolOpenAI providerProtocol = iota
	protocolAnthropic
)

func writeProviderError(w http.ResponseWriter, protocol providerProtocol, status int, errorType, message string) {
	if protocol == protocolAnthropic {
		writeJSON(w, status, fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, errorType, message))
		return
	}
	writeJSON(w, status, fmt.Sprintf(`{"error":{"type":%q,"message":%q}}`, errorType, message))
}

func newMockServer(config mockConfig) *mockServer {
	if config.MaxDelay <= 0 {
		config.MaxDelay = 5 * time.Second
	}
	return &mockServer{
		config:   config,
		captures: newCaptureStore(config.MaxCaptures),
	}
}

func (s *mockServer) providerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleOpenAIChat)
	mux.HandleFunc("/v1/responses", s.handleOpenAIResponses)
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	return mux
}

func (s *mockServer) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/__test/requests/", s.handleCaptureAdmin)
	return mux
}

func (s *mockServer) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeProviderError(w, protocolOpenAI, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	authorized := constantTimeEqual(r.Header.Get("Authorization"), "Bearer "+s.config.OpenAIKey)
	if !authorized {
		writeProviderError(w, protocolOpenAI, http.StatusUnauthorized, "authentication_error", "invalid mock credential")
		return
	}
	capture, ok := s.captureJSONRequest(w, r, true, protocolOpenAI)
	if !ok {
		return
	}
	if hasNonStreamingOpenAIOptions(capture.Body) {
		writeProviderError(w, protocolOpenAI, http.StatusBadRequest, "invalid_request", "include_usage and stream_options are streaming-only options")
		return
	}
	if s.applyControls(w, r, protocolOpenAI) {
		return
	}
	if isStreaming(capture.Body) {
		writeSSE(w,
			`{"id":"chat-mock","object":"chat.completion.chunk","model":"mock-openai-chat","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}`,
			`{"id":"chat-mock","object":"chat.completion.chunk","model":"mock-openai-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"chat-mock","object":"chat.completion.chunk","model":"mock-openai-chat","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`,
			`[DONE]`,
		)
		return
	}
	writeJSON(w, http.StatusOK, `{"id":"chat-mock","object":"chat.completion","model":"mock-openai-chat","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`)
}

func (s *mockServer) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeProviderError(w, protocolOpenAI, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	authorized := constantTimeEqual(r.Header.Get("Authorization"), "Bearer "+s.config.OpenAIKey)
	if !authorized {
		writeProviderError(w, protocolOpenAI, http.StatusUnauthorized, "authentication_error", "invalid mock credential")
		return
	}
	capture, ok := s.captureJSONRequest(w, r, true, protocolOpenAI)
	if !ok || s.applyControls(w, r, protocolOpenAI) {
		return
	}
	if !isStreaming(capture.Body) {
		writeJSON(w, http.StatusOK, `{"id":"resp-mock","object":"response","model":"mock-responses","output":[],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}`)
		return
	}
	writeSSE(w,
		`{"type":"response.output_text.delta","delta":"OK"}`,
		`{"type":"response.completed","response":{"id":"resp-mock","object":"response","model":"mock-responses","output":[],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}}`,
	)
}

func (s *mockServer) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeProviderError(w, protocolAnthropic, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !constantTimeEqual(r.Header.Get("X-Api-Key"), s.config.AnthropicKey) {
		writeProviderError(w, protocolAnthropic, http.StatusUnauthorized, "authentication_error", "invalid mock credential")
		return
	}
	if r.Header.Get("Anthropic-Version") != "2023-06-01" {
		writeProviderError(w, protocolAnthropic, http.StatusBadRequest, "invalid_request_error", "unsupported anthropic-version")
		return
	}
	capture, ok := s.captureJSONRequest(w, r, true, protocolAnthropic)
	if !ok || s.applyControls(w, r, protocolAnthropic) {
		return
	}
	if !isStreaming(capture.Body) {
		writeJSON(w, http.StatusOK, `{"id":"msg-mock","type":"message","role":"assistant","model":"mock-anthropic","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":13,"output_tokens":6}}`)
		return
	}
	writeSSE(w,
		`{"type":"message_start","message":{"id":"msg-mock","type":"message","role":"assistant","model":"mock-anthropic","content":[],"usage":{"input_tokens":13,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`,
		`{"type":"message_stop"}`,
	)
}

func (s *mockServer) applyControls(w http.ResponseWriter, r *http.Request, protocol providerProtocol) bool {
	query := r.URL.Query()
	if status := query.Get("mock_status"); status != "" {
		if status != strconv.Itoa(http.StatusTooManyRequests) {
			writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "unsupported mock_status")
			return true
		}
		writeProviderError(w, protocol, http.StatusTooManyRequests, "rate_limit_error", "mock rate limit")
		return true
	}
	if delayValue := query.Get("mock_delay_ms"); delayValue != "" {
		delayMS, err := strconv.Atoi(delayValue)
		delay := time.Duration(delayMS) * time.Millisecond
		if err != nil || delayMS < 0 || delay > s.config.MaxDelay {
			writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "invalid mock_delay_ms")
			return true
		}
		time.Sleep(delay)
	}
	return false
}

func isStreaming(body json.RawMessage) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

func hasNonStreamingOpenAIOptions(body json.RawMessage) bool {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	if stream, _ := request["stream"].(bool); stream {
		return false
	}
	_, hasIncludeUsage := request["include_usage"]
	_, hasStreamOptions := request["stream_options"]
	return hasIncludeUsage || hasStreamOptions
}

func (s *mockServer) captureJSONRequest(w http.ResponseWriter, r *http.Request, authorized bool, protocol providerProtocol) (capturedRequest, bool) {
	requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if requestID == "" {
		writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "x-request-id is required")
		return capturedRequest{}, false
	}

	limited := http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "request body is invalid or too large")
		return capturedRequest{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
		return capturedRequest{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return capturedRequest{}, false
	}

	compact := bytes.NewBuffer(make([]byte, 0, len(body)))
	if err := json.Compact(compact, body); err != nil {
		writeProviderError(w, protocol, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return capturedRequest{}, false
	}
	capture := capturedRequest{
		RequestID:        requestID,
		Method:           r.Method,
		Path:             r.URL.Path,
		RawQuery:         r.URL.RawQuery,
		Authorized:       authorized,
		ContentType:      r.Header.Get("Content-Type"),
		AnthropicVersion: r.Header.Get("Anthropic-Version"),
		Body:             append(json.RawMessage(nil), compact.Bytes()...),
	}
	s.captures.put(capture)
	return capture, true
}

func (s *mockServer) handleCaptureAdmin(w http.ResponseWriter, r *http.Request) {
	requestID, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/__test/requests/"))
	if err != nil || strings.TrimSpace(requestID) == "" || strings.Contains(requestID, "/") {
		writeJSON(w, http.StatusBadRequest, `{"error":"invalid request id"}`)
		return
	}

	switch r.Method {
	case http.MethodGet:
		capture, exists := s.captures.get(requestID)
		if !exists {
			writeJSON(w, http.StatusNotFound, `{"error":"capture not found"}`)
			return
		}
		body, err := json.Marshal(capture)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, `{"error":"capture encoding failed"}`)
			return
		}
		writeJSON(w, http.StatusOK, string(body))
	case http.MethodDelete:
		if !s.captures.delete(requestID) {
			writeJSON(w, http.StatusNotFound, `{"error":"capture not found"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func constantTimeEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		if flusher != nil {
			flusher.Flush()
		}
	}
}
