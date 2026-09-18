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

package tokenizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"k8s.io/klog/v2"
)

// maxRequestBody bounds proxied request bodies (prompts are small).
const maxRequestBody = 10 << 20 // 10 MiB

// readyChecker reports whether the ModelServer watcher completed its initial
// sync.
type readyChecker interface {
	HasSynced() bool
}

// Server is the HTTP frontend of the tokenizer service. It exposes the
// vLLM-compatible /tokenize and /detokenize endpoints and proxies each request
// to the renderer subprocess serving the requested model. Requests for models
// without a ready renderer get 503 so that the router falls back to
// engine-side tokenization.
type Server struct {
	config  Config
	manager *RendererManager
	ready   readyChecker
	client  *http.Client
}

func NewServer(config Config, manager *RendererManager, ready readyChecker) *Server {
	return &Server{
		config:  config,
		manager: manager,
		ready:   ready,
		client:  &http.Client{Timeout: config.ProxyTimeout},
	}
}

// Handler returns the HTTP handler of the frontend.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/tokenize", s.proxy("/tokenize"))
	mux.HandleFunc("/detokenize", s.proxy("/detokenize"))
	mux.HandleFunc("/models", s.models)
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

// Run serves the frontend until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port)),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// proxy forwards a tokenize/detokenize request to the renderer that serves
// the model named in the request body.
func (s *Server) proxy(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read request body")
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if payload.Model == "" {
			writeError(w, http.StatusBadRequest, "missing 'model' field")
			return
		}

		endpoint, ok := s.manager.EndpointFor(payload.Model)
		if !ok {
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("no ready tokenizer for model %q", payload.Model))
			return
		}

		resp, err := s.client.Post(endpoint+path, "application/json", bytes.NewReader(body))
		if err != nil {
			klog.Warningf("Proxy to renderer for model %s failed: %v", payload.Model, err)
			writeError(w, http.StatusBadGateway,
				fmt.Sprintf("tokenizer backend for model %q unreachable", payload.Model))
			return
		}
		defer resp.Body.Close()

		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": s.manager.Snapshot()})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reflects only watcher sync: a not-yet-loaded model falls back on the
// router side instead of failing the whole service's readiness.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.ready != nil && s.ready.HasSynced() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
