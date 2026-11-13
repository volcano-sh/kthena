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

// kthena-tokenizer-stub is a minimal vllm-render stand-in for E2E tests.
// It accepts the same args that RendererManager passes:
//
//	kthena-tokenizer-stub <source> --served-model-name <model> --host <h> --port <p>
//
// and serves a tiny HTTP API so tests can assert tokenizer lifecycle and traffic.
//
// This binary is intentionally kept under test/e2e/ and is only compiled into
// the E2E test image (Dockerfile.kthena-tokenizer-e2e). It must not be included
// in the production image.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	port := "8100"
	args := os.Args[1:]
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			port = args[i+1]
			break
		}
	}

	var requestCount int64

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		words := strings.Fields(req.Prompt)
		n := len(words)
		if n == 0 {
			n = 1
		}
		tokens := make([]int, n)
		for i := range tokens {
			tokens[i] = i + 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Count       int   `json:"count"`
			MaxModelLen int   `json:"max_model_len"`
			Tokens      []int `json:"tokens"`
		}{Count: n, MaxModelLen: 4096, Tokens: tokens})
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := atomic.LoadInt64(&requestCount)
		_ = json.NewEncoder(w).Encode(map[string]int64{"requests": n})
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("kthena-tokenizer-stub listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("listen error: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}
