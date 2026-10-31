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

// tokenizer-engine-mock is a lightweight LLM inference engine mock for E2E tests.
// It simulates vLLM's HTTP API: chat completions and the /tokenize endpoint.
// Tests use it as the ModelServer backend when they need the router fallback path.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

type tokenizeResp struct {
	Count       int   `json:"count"`
	MaxModelLen int   `json:"max_model_len"`
	Tokens      []int `json:"tokens"`
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	var tokenizeCount int64

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		model, _ := req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-mock","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"mock response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`, model)
	})

	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&tokenizeCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenizeResp{
			Count:       5,
			MaxModelLen: 4096,
			Tokens:      []int{1, 2, 3, 4, 5},
		})
	})

	mux.HandleFunc("/tokenize-stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := atomic.LoadInt64(&tokenizeCount)
		_ = json.NewEncoder(w).Encode(map[string]int64{"requests": n})
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("tokenizer-engine-mock listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("listen error: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
