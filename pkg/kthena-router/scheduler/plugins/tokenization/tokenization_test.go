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

package tokenization

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIntToByteArray(t *testing.T) {
	tests := []struct {
		name     string
		input    []int
		expected []byte
	}{
		{
			name:     "Empty slice",
			input:    []int{},
			expected: []byte{},
		},
		{
			name:     "Single integer",
			input:    []int{1},
			expected: []byte{0, 0, 0, 1},
		},
		{
			name:     "Multiple integers",
			input:    []int{1, 2, 3},
			expected: []byte{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3},
		},
		{
			name:     "Negative integer",
			input:    []int{-1},
			expected: []byte{0xFF, 0xFF, 0xFF, 0xFF},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := intToByteArray(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestTokenizerManager(t *testing.T) {
	config := TokenizerManagerConfig{
		EndpointTemplates: map[string]string{
			"vllm":   "http://%s:8000",
			"sglang": "http://%s:30000",
		},
	}

	manager := NewTokenizerManager(config)
	if manager == nil {
		t.Fatal("Expected non-nil TokenizerManager")
	}

	t.Run("Empty model name", func(t *testing.T) {
		pods := []*datastore.PodInfo{}
		result := manager.GetTokenizer("", pods)
		if result != nil {
			t.Error("Expected nil tokenizer for empty model name")
		}
	})

	t.Run("Empty pods", func(t *testing.T) {
		result := manager.GetTokenizer("test-model", []*datastore.PodInfo{})
		if result != nil {
			t.Error("Expected nil tokenizer for empty pods")
		}
	})

	t.Run("Pod without tokenizer annotation", func(t *testing.T) {
		pods := []*datastore.PodInfo{
			{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
					},
					Status: v1.PodStatus{
						Phase: v1.PodRunning,
						PodIP: "10.0.0.1",
					},
				},
			},
		}

		result := manager.GetTokenizer("test-model", pods)
		t.Logf("GetTokenizer returned: %v", result)
	})
}

func TestErrorTypes(t *testing.T) {
	t.Run("ErrTokenizationFailed", func(t *testing.T) {
		err := ErrTokenizationFailed{
			Message: "test error",
			Cause:   fmt.Errorf("underlying error"),
		}

		expected := "tokenization failed: test error: underlying error"
		if err.Error() != expected {
			t.Errorf("Expected error message '%s', got '%s'", expected, err.Error())
		}
	})

	t.Run("ErrHTTPRequest", func(t *testing.T) {
		err := ErrHTTPRequest{
			StatusCode: 404,
			Message:    "Not Found",
		}

		expected := "HTTP 404: Not Found"
		if err.Error() != expected {
			t.Errorf("Expected error message '%s', got '%s'", expected, err.Error())
		}
	})
}

func TestVLLMAdapter(t *testing.T) {
	adapter := newVLLMAdapter("test-model")

	path := adapter.GetTokenizePath()
	if path == "" {
		t.Error("GetTokenizePath should return a non-empty path")
	}

	t.Run("Completion request", func(t *testing.T) {
		input := TokenizeInput{
			Type:               CompletionInput,
			Text:               "Hello world",
			AddSpecialTokens:   true,
			ReturnTokenStrings: false,
		}

		result, err := adapter.PrepareTokenizeRequest(input)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		req, ok := result.(*vllmTokenizeCompletionRequest)
		if !ok {
			t.Fatalf("Expected *vllmTokenizeCompletionRequest, got %T", result)
		}

		if req.Model != "test-model" {
			t.Errorf("Expected model 'test-model', got %s", req.Model)
		}
		if req.Prompt != "Hello world" {
			t.Errorf("Expected prompt 'Hello world', got %s", req.Prompt)
		}
	})

	t.Run("Chat request", func(t *testing.T) {
		input := TokenizeInput{
			Type: ChatInput,
			Messages: []common.Message{
				{Role: "user", Content: "Hello"},
			},
			AddSpecialTokens:    false,
			ReturnTokenStrings:  true,
			AddGenerationPrompt: true,
		}

		result, err := adapter.PrepareTokenizeRequest(input)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		req, ok := result.(*vllmTokenizeChatRequest)
		if !ok {
			t.Fatalf("Expected *vllmTokenizeChatRequest, got %T", result)
		}

		if req.Model != "test-model" {
			t.Errorf("Expected model 'test-model', got %s", req.Model)
		}
		if len(req.Messages) != 1 {
			t.Errorf("Expected 1 message, got %d", len(req.Messages))
		}
	})

	t.Run("Parse response", func(t *testing.T) {
		jsonData := `{"count":3,"max_model_len":2048,"tokens":[1,2,3],"token_strs":["Hello","world","!"]}`

		result, err := adapter.ParseTokenizeResponse([]byte(jsonData))
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Count != 3 {
			t.Errorf("Expected count 3, got %d", result.Count)
		}
		if result.MaxModelLen != 2048 {
			t.Errorf("Expected max_model_len 2048, got %d", result.MaxModelLen)
		}
		if len(result.Tokens) != 3 {
			t.Errorf("Expected 3 tokens, got %d", len(result.Tokens))
		}
		if len(result.TokenStrings) != 3 {
			t.Errorf("Expected 3 token strings, got %d", len(result.TokenStrings))
		}
	})
}

func TestRemoteTokenizerCreation(t *testing.T) {
	t.Run("Valid configuration", func(t *testing.T) {
		config := RemoteTokenizerConfig{
			Engine:   "vllm",
			Endpoint: "http://localhost:8000",
			Model:    "test-model",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err != nil {
			t.Fatalf("Failed to create tokenizer: %v", err)
		}
		if tokenizer == nil {
			t.Fatal("Expected non-nil tokenizer")
		}
	})

	t.Run("Unsupported engine", func(t *testing.T) {
		config := RemoteTokenizerConfig{
			Engine:   "unsupported",
			Endpoint: "http://localhost:8000",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err == nil {
			t.Fatal("Expected error for unsupported engine")
		}
		if tokenizer != nil {
			t.Error("Expected nil tokenizer for unsupported engine")
		}
		if _, ok := err.(ErrInvalidConfig); !ok {
			t.Errorf("Expected ErrInvalidConfig, got %T: %v", err, err)
		}
	})

	t.Run("SGLang engine", func(t *testing.T) {
		config := RemoteTokenizerConfig{
			Engine:   "sglang",
			Endpoint: "http://localhost:30000",
			Model:    "test-model",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err != nil {
			t.Fatalf("Failed to create SGLang tokenizer: %v", err)
		}
		if tokenizer == nil {
			t.Fatal("Expected non-nil tokenizer for SGLang engine")
		}
	})

	t.Run("Empty endpoint", func(t *testing.T) {
		config := RemoteTokenizerConfig{
			Engine:   "vllm",
			Endpoint: "",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		t.Logf("NewRemoteTokenizer with empty endpoint: tokenizer=%v, err=%v", tokenizer != nil, err)
	})
}

func TestRemoteTokenizerWithMockServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			http.NotFound(w, r)
			return
		}

		response := map[string]interface{}{
			"count":         3,
			"max_model_len": 2048,
			"tokens":        []int{1, 2, 3},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	config := RemoteTokenizerConfig{
		Engine:   "vllm",
		Endpoint: server.URL,
		Model:    "test-model",
	}

	tokenizer, err := NewRemoteTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	t.Run("TokenizeInputText", func(t *testing.T) {
		result, err := tokenizer.TokenizeInputText("Hello world test")
		if err != nil {
			t.Fatalf("TokenizeInputText failed: %v", err)
		}

		expectedLen := 3 * 4
		if len(result) != expectedLen {
			t.Errorf("Expected %d bytes, got %d", expectedLen, len(result))
		}
	})
}

func TestErrorScenarios(t *testing.T) {
	t.Run("HTTP 500 error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}))
		defer server.Close()

		config := RemoteTokenizerConfig{
			Engine:   "vllm",
			Endpoint: server.URL,
			Model:    "test-model",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err != nil {
			t.Fatalf("Failed to create tokenizer: %v", err)
		}

		_, err = tokenizer.TokenizeInputText("test")
		if err == nil {
			t.Error("Expected error for HTTP 500")
		}

		if httpErr, ok := err.(ErrHTTPRequest); ok {
			if httpErr.StatusCode != 500 {
				t.Errorf("Expected status code 500, got %d", httpErr.StatusCode)
			}
		}
	})

	t.Run("Invalid JSON response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		config := RemoteTokenizerConfig{
			Engine:   "vllm",
			Endpoint: server.URL,
			Model:    "test-model",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err != nil {
			t.Fatalf("Failed to create tokenizer: %v", err)
		}

		_, err = tokenizer.TokenizeInputText("test")
		if err == nil {
			t.Error("Expected error for invalid JSON")
		}
	})
}

func TestJSONSerialization(t *testing.T) {
	t.Run("TokenizeResult", func(t *testing.T) {
		original := TokenizeResult{
			Count:        3,
			MaxModelLen:  2048,
			Tokens:       []int{1, 2, 3},
			TokenStrings: []string{"Hello", "world", "!"},
		}

		jsonData, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var unmarshaled TokenizeResult
		err = json.Unmarshal(jsonData, &unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if !reflect.DeepEqual(original, unmarshaled) {
			t.Errorf("Serialization round-trip failed")
		}
	})

	t.Run("vLLM completion request", func(t *testing.T) {
		addSpecialTokens := true
		returnTokenStrs := false

		req := vllmTokenizeCompletionRequest{
			Model:            "test-model",
			Prompt:           "Hello world",
			AddSpecialTokens: &addSpecialTokens,
			ReturnTokenStrs:  &returnTokenStrs,
		}

		jsonData, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var unmarshaled map[string]interface{}
		err = json.Unmarshal(jsonData, &unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if unmarshaled["model"] != "test-model" {
			t.Errorf("Expected model 'test-model', got %v", unmarshaled["model"])
		}
		if unmarshaled["prompt"] != "Hello world" {
			t.Errorf("Expected prompt 'Hello world', got %v", unmarshaled["prompt"])
		}
	})
}

func TestConcurrentAccess(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		time.Sleep(10 * time.Millisecond)
		response := map[string]interface{}{
			"count":         1,
			"max_model_len": 1024,
			"tokens":        []int{requestCount},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	config := RemoteTokenizerConfig{
		Engine:   "vllm",
		Endpoint: server.URL,
		Model:    "test-model",
	}

	tokenizer, err := NewRemoteTokenizer(config)
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	const numRequests = 5
	results := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			_, err := tokenizer.TokenizeInputText(fmt.Sprintf("test request %d", id))
			results <- err
		}(i)
	}

	for i := 0; i < numRequests; i++ {
		err := <-results
		if err != nil {
			t.Errorf("Request %d failed: %v", i, err)
		}
	}

	if requestCount != numRequests {
		t.Errorf("Expected %d requests, got %d", numRequests, requestCount)
	}
}

func BenchmarkIntToByteArray(b *testing.B) {
	input := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = intToByteArray(input)
	}
}

func BenchmarkTokenizeInputText(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"count":         5,
			"max_model_len": 2048,
			"tokens":        []int{1, 2, 3, 4, 5},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	config := RemoteTokenizerConfig{
		Engine:   "vllm",
		Endpoint: server.URL,
		Model:    "test-model",
	}

	tokenizer, err := NewRemoteTokenizer(config)
	if err != nil {
		b.Fatalf("Failed to create tokenizer: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = tokenizer.TokenizeInputText("Hello world test benchmark")
	}
}

func TestSGLangAdapter(t *testing.T) {
	adapter := newSGLangAdapter("test-model")

	t.Run("GetTokenizePath", func(t *testing.T) {
		if got := adapter.GetTokenizePath(); got != "/tokenize" {
			t.Errorf("Expected '/tokenize', got %q", got)
		}
	})

	t.Run("Completion request", func(t *testing.T) {
		input := TokenizeInput{
			Type:               CompletionInput,
			Text:               "Hello SGLang",
			AddSpecialTokens:   true,
			ReturnTokenStrings: false,
		}

		result, err := adapter.PrepareTokenizeRequest(input)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		req, ok := result.(*sglangTokenizeCompletionRequest)
		if !ok {
			t.Fatalf("Expected *sglangTokenizeCompletionRequest, got %T", result)
		}

		if req.Model != "test-model" {
			t.Errorf("Expected model 'test-model', got %s", req.Model)
		}
		if req.Prompt != "Hello SGLang" {
			t.Errorf("Expected prompt 'Hello SGLang', got %s", req.Prompt)
		}
		if req.AddSpecialTokens == nil || *req.AddSpecialTokens != true {
			t.Errorf("Expected AddSpecialTokens=true, got %v", req.AddSpecialTokens)
		}
	})

	t.Run("Chat request", func(t *testing.T) {
		input := TokenizeInput{
			Type: ChatInput,
			Messages: []common.Message{
				{Role: "user", Content: "Hello"},
				{Role: "assistant", Content: "Hi!"},
			},
			AddSpecialTokens: false,
		}

		result, err := adapter.PrepareTokenizeRequest(input)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		req, ok := result.(*sglangTokenizeChatRequest)
		if !ok {
			t.Fatalf("Expected *sglangTokenizeChatRequest, got %T", result)
		}

		if req.Model != "test-model" {
			t.Errorf("Expected model 'test-model', got %s", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Errorf("Expected 2 messages, got %d", len(req.Messages))
		}
		if req.AddSpecialTokens == nil || *req.AddSpecialTokens != false {
			t.Errorf("Expected AddSpecialTokens=false, got %v", req.AddSpecialTokens)
		}
	})

	t.Run("Empty model omits model in JSON", func(t *testing.T) {
		emptyAdapter := newSGLangAdapter("")
		result, err := emptyAdapter.PrepareTokenizeRequest(TokenizeInput{
			Type: CompletionInput,
			Text: "x",
		})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}
		if _, ok := m["model"]; ok {
			t.Errorf("Expected 'model' field to be omitted in JSON when empty, got %v", m["model"])
		}
	})

	t.Run("Unsupported input type", func(t *testing.T) {
		_, err := adapter.PrepareTokenizeRequest(TokenizeInput{Type: "unknown"})
		if err == nil {
			t.Error("Expected error for unsupported input type")
		}
	})

	t.Run("Parse response", func(t *testing.T) {
		jsonData := `{"count":3,"max_model_len":2048,"tokens":[10,20,30]}`
		result, err := adapter.ParseTokenizeResponse([]byte(jsonData))
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if result.Count != 3 {
			t.Errorf("Expected count 3, got %d", result.Count)
		}
		if result.MaxModelLen != 2048 {
			t.Errorf("Expected max_model_len 2048, got %d", result.MaxModelLen)
		}
		if !reflect.DeepEqual(result.Tokens, []int{10, 20, 30}) {
			t.Errorf("Expected tokens [10,20,30], got %v", result.Tokens)
		}
	})

	t.Run("Parse invalid JSON", func(t *testing.T) {
		_, err := adapter.ParseTokenizeResponse([]byte("not json"))
		if err == nil {
			t.Error("Expected error for invalid JSON")
		}
	})

	t.Run("Parse empty response", func(t *testing.T) {
		result, err := adapter.ParseTokenizeResponse([]byte(`{}`))
		if err != nil {
			t.Fatalf("Unexpected error for empty JSON object: %v", err)
		}
		if result.Count != 0 || len(result.Tokens) != 0 {
			t.Errorf("Expected zero-valued result, got %+v", result)
		}
	})
}

func TestNewEngineAdapter(t *testing.T) {
	cases := []struct {
		name      string
		engine    string
		wantType  engineAdapter
		wantError bool
	}{
		{"vllm", "vllm", &vllmAdapter{}, false},
		{"sglang", "sglang", &sglangAdapter{}, false},
		{"empty defaults to vllm", "", &vllmAdapter{}, false},
		{"vllm uppercase", "VLLM", &vllmAdapter{}, false},
		{"sglang mixed case", "SGLang", &sglangAdapter{}, false},
		{"sglang with whitespace", "  sglang  ", &sglangAdapter{}, false},
		{"unsupported", "unsupported", nil, true},
		{"random string", "openai", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := newEngineAdapter(tc.engine, "test-model")
			if tc.wantError {
				if err == nil {
					t.Fatalf("Expected error for engine %q", tc.engine)
				}
				if _, ok := err.(ErrInvalidConfig); !ok {
					t.Errorf("Expected ErrInvalidConfig, got %T: %v", err, err)
				}
				if adapter != nil {
					t.Errorf("Expected nil adapter on error, got %T", adapter)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if reflect.TypeOf(adapter) != reflect.TypeOf(tc.wantType) {
				t.Errorf("Expected %T, got %T", tc.wantType, adapter)
			}
		})
	}
}

func TestRemoteTokenizerSGLangWithMockServer(t *testing.T) {
	t.Run("Standard tokens response", func(t *testing.T) {
		var capturedBody map[string]interface{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/tokenize" {
				http.NotFound(w, r)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				t.Errorf("Failed to decode request body: %v", err)
			}

			response := map[string]interface{}{
				"count":         3,
				"max_model_len": 4096,
				"tokens":        []int{100, 200, 300},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		}))
		defer server.Close()

		config := RemoteTokenizerConfig{
			Engine:   "sglang",
			Endpoint: server.URL,
			Model:    "sglang-model",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err != nil {
			t.Fatalf("Failed to create tokenizer: %v", err)
		}

		result, err := tokenizer.TokenizeInputText("Hello SGLang")
		if err != nil {
			t.Fatalf("TokenizeInputText failed: %v", err)
		}

		expectedLen := 3 * 4
		if len(result) != expectedLen {
			t.Errorf("Expected %d bytes, got %d", expectedLen, len(result))
		}

		if capturedBody["model"] != "sglang-model" {
			t.Errorf("Expected request model 'sglang-model', got %v", capturedBody["model"])
		}
		if capturedBody["prompt"] != "Hello SGLang" {
			t.Errorf("Expected request prompt 'Hello SGLang', got %v", capturedBody["prompt"])
		}
	})

	t.Run("HTTP 500 propagates error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}))
		defer server.Close()

		config := RemoteTokenizerConfig{
			Engine:   "sglang",
			Endpoint: server.URL,
			Model:    "sglang-model",
		}

		tokenizer, err := NewRemoteTokenizer(config)
		if err != nil {
			t.Fatalf("Failed to create tokenizer: %v", err)
		}

		_, err = tokenizer.TokenizeInputText("test")
		if err == nil {
			t.Error("Expected error for HTTP 500")
		}
	})
}

func TestSGLangJSONSerialization(t *testing.T) {
	t.Run("Completion request omits empty model", func(t *testing.T) {
		addSpecialTokens := true
		req := sglangTokenizeCompletionRequest{
			Prompt:           "Hello",
			AddSpecialTokens: &addSpecialTokens,
		}

		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if _, ok := m["model"]; ok {
			t.Error("Expected 'model' field to be omitted when empty")
		}
		if m["prompt"] != "Hello" {
			t.Errorf("Expected prompt 'Hello', got %v", m["prompt"])
		}
		if m["add_special_tokens"] != true {
			t.Errorf("Expected add_special_tokens=true, got %v", m["add_special_tokens"])
		}
	})

	t.Run("Chat request includes messages", func(t *testing.T) {
		addSpecialTokens := true
		req := sglangTokenizeChatRequest{
			Model: "m",
			Messages: []common.Message{
				{Role: "user", Content: "hi"},
			},
			AddSpecialTokens: &addSpecialTokens,
		}

		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		msgs, ok := m["messages"].([]interface{})
		if !ok {
			t.Fatalf("Expected messages array, got %T", m["messages"])
		}
		if len(msgs) != 1 {
			t.Errorf("Expected 1 message, got %d", len(msgs))
		}
		if m["add_special_tokens"] != true {
			t.Errorf("Expected add_special_tokens=true, got %v", m["add_special_tokens"])
		}
	})
}

func TestTokenizerManagerTokenizePrompt(t *testing.T) {
	config := TokenizerManagerConfig{
		EndpointTemplates: map[string]string{
			"vllm":   "http://%s:8000",
			"sglang": "http://%s:30000",
		},
	}

	manager := NewTokenizerManager(config)
	if manager == nil {
		t.Fatal("Expected non-nil TokenizerManager")
	}

	t.Run("Empty pods", func(t *testing.T) {
		prompt := common.ChatMessage{Text: "Hello world"}

		_, err := manager.TokenizePrompt("test-model", prompt, []*datastore.PodInfo{})
		if err == nil {
			t.Error("Expected error for empty pods")
		}
	})

	t.Run("Empty prompt", func(t *testing.T) {
		prompt := common.ChatMessage{}

		_, err := manager.TokenizePrompt("test-model", prompt, []*datastore.PodInfo{})
		if err == nil {
			t.Error("Expected error for empty prompt")
		}
	})
}

func TestNormalizeEngineName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"vLLM canonical casing", "vLLM", "vllm"},
		{"SGLang canonical casing", "SGLang", "sglang"},
		{"already lowercase vllm", "vllm", "vllm"},
		{"already lowercase sglang", "sglang", "sglang"},
		{"mixed case sglang", "SgLaNg", "sglang"},
		{"surrounding whitespace", "  vllm  ", "vllm"},
		{"empty string defaults to vllm", "", "vllm"},
		{"whitespace only defaults to vllm", "   ", "vllm"},
		{"unknown engine passes through normalized", "TensorRT", "tensorrt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeEngineName(tt.input)
			if got != tt.expected {
				t.Errorf("normalizeEngineName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestTokenizerManagerEndpointTemplateLookup(t *testing.T) {
	pod := &datastore.PodInfo{
		Pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				PodIP: "10.0.0.1",
			},
		},
	}

	t.Run("vllm template matches default engine", func(t *testing.T) {
		mgr := NewTokenizerManager(TokenizerManagerConfig{
			EndpointTemplates: map[string]string{
				"vllm": "http://%s:8000",
			},
		})
		tok := mgr.GetTokenizer("test-model", []*datastore.PodInfo{pod})
		if tok == nil {
			t.Fatal("expected tokenizer for vllm-templated pod")
		}
		rt, ok := tok.(remoteTokenizer)
		if !ok {
			t.Fatalf("expected tokenizer to satisfy remoteTokenizer, got %T", tok)
		}
		if got, want := rt.GetEndpoint(), "http://10.0.0.1:8000"; got != want {
			t.Errorf("endpoint = %q, want %q", got, want)
		}
	})

	t.Run("only sglang template skips empty-engine pod", func(t *testing.T) {
		mgr := NewTokenizerManager(TokenizerManagerConfig{
			EndpointTemplates: map[string]string{
				"sglang": "http://%s:30000",
			},
		})
		tok := mgr.GetTokenizer("test-model", []*datastore.PodInfo{pod})
		if tok != nil {
			t.Errorf("expected nil tokenizer (no template for vllm), got %T", tok)
		}
	})

	t.Run("both templates configured prefers pod's engine", func(t *testing.T) {
		mgr := NewTokenizerManager(TokenizerManagerConfig{
			EndpointTemplates: map[string]string{
				"vllm":   "http://%s:8000",
				"sglang": "http://%s:30000",
			},
		})
		tok := mgr.GetTokenizer("test-model", []*datastore.PodInfo{pod})
		if tok == nil {
			t.Fatal("expected tokenizer when both templates configured")
		}
		rt, ok := tok.(remoteTokenizer)
		if !ok {
			t.Fatalf("expected tokenizer to satisfy remoteTokenizer, got %T", tok)
		}
		if got, want := rt.GetEndpoint(), "http://10.0.0.1:8000"; got != want {
			t.Errorf("endpoint = %q, want %q (empty engine should resolve to vllm)", got, want)
		}
	})

	t.Run("nil EndpointTemplates skips all pods", func(t *testing.T) {
		mgr := NewTokenizerManager(TokenizerManagerConfig{})
		tok := mgr.GetTokenizer("test-model", []*datastore.PodInfo{pod})
		if tok != nil {
			t.Errorf("expected nil tokenizer with nil templates, got %T", tok)
		}
	})
}
