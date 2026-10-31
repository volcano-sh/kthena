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
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"count":3,"max_model_len":4096,"tokens":[1,2,3]}`)
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
