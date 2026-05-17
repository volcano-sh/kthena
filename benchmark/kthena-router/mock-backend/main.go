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

// OpenAI-compatible HTTP surface for router benchmarking without a real LLM.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type server struct {
	model      string
	tokenDelay time.Duration
}

func main() {
	listen := flag.String("listen", ":8000", "Listen address (matches vLLM default in sglang bench_serving)")
	model := flag.String("model", "mock-llm", "Model id for /v1/models and responses")
	tokenDelay := flag.Duration("token-delay", 0, "Optional sleep between streamed tokens")
	flag.Parse()

	s := &server{model: *model, tokenDelay: *tokenDelay}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/completions", s.handleCompletions)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

	log.Printf("mock inference on %s (model=%q)", *listen, *model)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{
				"id":       s.model,
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "kthena-mock",
			},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.Body.Close(); err != nil {
		log.Printf("close body: %v", err)
	}

	maxTok := intFromAny(body["max_tokens"], 16)
	stream := boolFromAny(body["stream"], true)
	model := stringFromAny(body["model"], s.model)

	if stream {
		s.streamCompletions(w, model, maxTok)
		return
	}
	s.completeCompletions(w, model, maxTok)
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.Body.Close(); err != nil {
		log.Printf("close body: %v", err)
	}

	maxTok := intFromAny(body["max_completion_tokens"], intFromAny(body["max_tokens"], 16))
	stream := boolFromAny(body["stream"], true)
	model := stringFromAny(body["model"], s.model)

	if stream {
		s.streamChat(w, model, maxTok)
		return
	}
	s.completeChat(w, model, maxTok)
}

func (s *server) streamCompletions(w http.ResponseWriter, model string, maxTok int) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	for i := 0; i < maxTok; i++ {
		if s.tokenDelay > 0 {
			time.Sleep(s.tokenDelay)
		}
		chunk := map[string]any{
			"id":      fmt.Sprintf("cmpl-mock-%d", time.Now().UnixNano()),
			"object":  "text_completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{
				{"text": "t", "index": 0, "finish_reason": nil},
			},
		}
		if !writeSSE(w, chunk) {
			return
		}
		fl.Flush()
	}

	usage := map[string]any{
		"id":      fmt.Sprintf("cmpl-mock-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{"text": "", "index": 0, "finish_reason": "stop"},
		},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": maxTok,
			"total_tokens":      maxTok + 1,
		},
	}
	if !writeSSE(w, usage) {
		return
	}
	fl.Flush()
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	fl.Flush()
}

func (s *server) completeCompletions(w http.ResponseWriter, model string, maxTok int) {
	text := strings.Repeat("t", maxTok)
	resp := map[string]any{
		"id":      fmt.Sprintf("cmpl-mock-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"text":          text,
				"index":         0,
				"finish_reason": "stop",
				"logprobs":      nil,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": maxTok,
			"total_tokens":      maxTok + 1,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) streamChat(w http.ResponseWriter, model string, maxTok int) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	for i := 0; i < maxTok; i++ {
		if s.tokenDelay > 0 {
			time.Sleep(s.tokenDelay)
		}
		chunk := map[string]any{
			"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{"content": "t"},
					"finish_reason": nil,
				},
			},
		}
		if !writeSSE(w, chunk) {
			return
		}
		fl.Flush()
	}

	final := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	if !writeSSE(w, final) {
		return
	}
	fl.Flush()

	usage := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": maxTok,
			"total_tokens":      maxTok + 1,
		},
	}
	if !writeSSE(w, usage) {
		return
	}
	fl.Flush()
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	fl.Flush()
}

func (s *server) completeChat(w http.ResponseWriter, model string, maxTok int) {
	text := strings.Repeat("t", maxTok)
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": maxTok,
			"total_tokens":      maxTok + 1,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		log.Printf("encode json: %v", err)
	}
}

func writeSSE(w http.ResponseWriter, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal sse: %v", err)
		return false
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	if err != nil {
		log.Printf("write sse: %v", err)
		return false
	}
	return true
}

func intFromAny(v any, def int) int {
	if v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return def
		}
		return int(n)
	default:
		return def
	}
}

func stringFromAny(v any, def string) string {
	if v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	return s
}

func boolFromAny(v any, def bool) bool {
	if v == nil {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}
