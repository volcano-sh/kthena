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
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultOpenAIKey    = "e2e-openai-key"
	defaultAnthropicKey = "e2e-anthropic-key"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	certificate, err := generateSelfSignedCertificate()
	if err != nil {
		return fmt.Errorf("generate TLS certificate: %w", err)
	}
	mock := newMockServer(mockConfig{
		OpenAIKey:    envOrDefault("MOCK_OPENAI_KEY", defaultOpenAIKey),
		AnthropicKey: envOrDefault("MOCK_ANTHROPIC_KEY", defaultAnthropicKey),
		MaxCaptures:  256,
		MaxDelay:     5 * time.Second,
	})

	adminListener, err := net.Listen("tcp", fmt.Sprintf(":%d", adminPort))
	if err != nil {
		return fmt.Errorf("listen on admin port: %w", err)
	}
	providerListener, err := net.Listen("tcp", fmt.Sprintf(":%d", providerPort))
	if err != nil {
		_ = adminListener.Close()
		return fmt.Errorf("listen on provider port: %w", err)
	}
	providerTLSListener := tls.NewListener(providerListener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})

	adminServer := newHTTPServer(mock.adminHandler())
	providerServer := newHTTPServer(mock.providerHandler())
	errCh := make(chan error, 2)
	go func() {
		log.Printf("external provider mock admin listening on :%d", adminPort)
		errCh <- adminServer.Serve(adminListener)
	}()
	go func() {
		log.Printf("external provider mock HTTPS listening on :%d", providerPort)
		errCh <- providerServer.Serve(providerTLSListener)
	}()

	select {
	case <-ctx.Done():
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	adminErr := adminServer.Shutdown(shutdownCtx)
	providerErr := providerServer.Shutdown(shutdownCtx)
	return errors.Join(adminErr, providerErr)
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
