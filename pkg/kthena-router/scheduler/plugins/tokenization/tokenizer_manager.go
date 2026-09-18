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
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"k8s.io/klog/v2"
)

const maxEndpointPort = 65535

// TokenizerServiceConfig configures the optional dedicated tokenizer service.
// When enabled, tokenization requests are sent to the service endpoint first,
// optionally falling back to the inference engine pods on failure.
type TokenizerServiceConfig struct {
	// Enabled turns on the dedicated tokenizer service. Disabled by default.
	Enabled bool
	// Endpoint is the base URL of the tokenizer service,
	// e.g. "http://127.0.0.1:8100" (sidecar) or
	// "http://kthena-tokenizer.kthena-system.svc:8100" (standalone).
	Endpoint string
	// FallbackToEngine falls back to engine-side tokenization when the
	// tokenizer service fails or has no tokenizer for the model.
	FallbackToEngine bool
}

// TokenizerManagerConfig configures how prompts are tokenized: via the
// backend engines' /tokenize endpoints and, optionally, via the dedicated
// tokenizer service.
type TokenizerManagerConfig struct {
	EndpointPorts map[string]int
	// Service optionally routes tokenization to a dedicated tokenizer service.
	Service TokenizerServiceConfig
}

type TokenizerManager struct {
	config TokenizerManagerConfig
	// client owns the connection pool shared by the short-lived tokenizer
	// wrappers created for individual scheduling requests.
	client *retryablehttp.Client

	// mu guards serviceTokenizers. Service-backed tokenizers are created once
	// per model and then reused for every prompt.
	mu                sync.Mutex
	serviceTokenizers map[string]Tokenizer
}

func NewTokenizerManager(config TokenizerManagerConfig) *TokenizerManager {
	return &TokenizerManager{
		config:            config,
		client:            newRetryableHTTPClient(),
		serviceTokenizers: make(map[string]Tokenizer),
	}
}

// serviceTokenizerFor returns the cached tokenizer backed by the dedicated
// tokenizer service for the model, creating it on first use. The service
// exposes the vLLM-compatible /tokenize API.
func (m *TokenizerManager) serviceTokenizerFor(model string) Tokenizer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tok, ok := m.serviceTokenizers[model]; ok {
		return tok
	}

	config := RemoteTokenizerConfig{
		Engine:             EngineVLLM,
		Endpoint:           m.config.Service.Endpoint,
		Model:              model,
		AddSpecialTokens:   true,
		ReturnTokenStrings: false,
	}

	// newHTTPClient (remote_client.go) wraps the shared retryable client with
	// the per-endpoint base URL.
	client := newHTTPClient(m.config.Service.Endpoint, m.client)
	tok, err := newRemoteTokenizer(config, client, false)
	if err != nil {
		klog.Warningf("TokenizerManager: failed to create service tokenizer for model %s at %s: %v",
			model, m.config.Service.Endpoint, err)
		return nil
	}
	m.serviceTokenizers[model] = tok
	return tok
}

// newEngineTokenizer creates a tokenizer backed by the /tokenize endpoint of a
// randomly selected backend inference engine pod, as opposed to the dedicated
// tokenizer service.
func (m *TokenizerManager) newEngineTokenizer(model string, pods []*datastore.PodInfo) Tokenizer {
	if len(pods) == 0 {
		klog.Warningf("No pods provided for model %s", model)
		return nil
	}

	// Randomly select a pod to start with
	startIdx := rand.Intn(len(pods))

	// Track unsupported engines so we can emit a metric if ALL pods fail
	// due to incompatible engines
	unsupportedEngines := make(map[string]struct{})

	// Try pods starting from random index, wrapping around if needed
	for i := 0; i < len(pods); i++ {
		podIdx := (startIdx + i) % len(pods)
		podInfo := pods[podIdx]
		pod := podInfo.GetPod()
		if pod == nil {
			continue
		}

		engine, err := normalizeEngine(podInfo.GetEngine())
		if err != nil {
			klog.Warningf("TokenizerManager: invalid engine for pod %s: %v", pod.Name, err)
			unsupportedEngines[podInfo.GetEngine()] = struct{}{}
			continue
		}
		port, ok := m.config.EndpointPorts[engine]
		if !ok || port < 1 || port > maxEndpointPort {
			klog.Warningf("TokenizerManager: no valid endpoint port for engine %q, skipping pod %s", engine, pod.Name)
			continue
		}
		endpoint := buildTokenizerEndpoint(pod.Status.PodIP, port)

		config := RemoteTokenizerConfig{
			Engine:             engine,
			Endpoint:           endpoint,
			Model:              model,
			AddSpecialTokens:   true,
			ReturnTokenStrings: false,
		}

		client := newHTTPClient(endpoint, m.client)
		tok, err := newRemoteTokenizer(config, client, false)
		if err != nil {
			klog.Warningf("Failed to create %s tokenizer for model %s at %s: %v", engine, model, endpoint, err)
			continue
		}

		klog.V(4).Infof("TokenizerManager: created %s tokenizer for model %s at %s", engine, model, endpoint)
		return tok
	}

	// All pods exhausted
	// record a failure metric per unsupported engine
	for engine := range unsupportedEngines {
		metrics.DefaultMetrics.RecordTokenizerUnsupportedEngine(model, engine)
	}
	klog.Warningf("Failed to create tokenizer for model %s after trying %d pods", model, len(pods))
	return nil
}

func buildTokenizerEndpoint(podIP string, port int) string {
	return "http://" + net.JoinHostPort(podIP, strconv.Itoa(port))
}

func normalizeEngine(engine string) (string, error) {
	switch strings.ToLower(engine) {
	case EngineSGLang:
		return EngineSGLang, nil
	case EngineVLLM:
		return EngineVLLM, nil
	case "":
		return "", ErrInvalidConfig{Message: "empty engine string"}
	default:
		return "", ErrInvalidConfig{Message: fmt.Sprintf("unsupported engine: %q", engine)}
	}
}

// TokenizePrompt tokenizes a prompt (text or chat messages) and returns uint32 tokens.
// When the dedicated tokenizer service is enabled, it is tried first; on failure the
// manager falls back to engine-side tokenization if configured to do so.
func (m *TokenizerManager) TokenizePrompt(
	model string,
	prompt *common.ChatMessage,
	pods []*datastore.PodInfo,
) ([]uint32, error) {
	if m.config.Service.Enabled {
		if tokenizer := m.serviceTokenizerFor(model); tokenizer != nil {
			tokens, err := m.tokenizeWith(tokenizer, prompt)
			if err == nil {
				return tokens, nil
			}
			if !m.config.Service.FallbackToEngine {
				return nil, fmt.Errorf("tokenizer service failed for model %s: %w", model, err)
			}
			klog.V(2).Infof("TokenizerManager: tokenizer service failed for model %s, falling back to engine: %v", model, err)
		} else if !m.config.Service.FallbackToEngine {
			return nil, fmt.Errorf("tokenizer service unavailable for model %s", model)
		}
	}

	tokenizer := m.newEngineTokenizer(model, pods)
	if tokenizer == nil {
		return nil, fmt.Errorf("no tokenizer available for model %s", model)
	}
	return m.tokenizeWith(tokenizer, prompt)
}

// tokenizeWith tokenizes a prompt using the given tokenizer.
func (m *TokenizerManager) tokenizeWith(tokenizer Tokenizer, prompt *common.ChatMessage) ([]uint32, error) {
	// Handle text prompts directly
	if prompt.Text != "" {
		tokens, err := tokenizer.TokenizeInputText(prompt.Text)
		if err != nil {
			return nil, fmt.Errorf("text tokenization failed: %w", err)
		}

		// Convert byte array to uint32 tokens
		tokens32 := make([]uint32, len(tokens)/4)
		for i := 0; i < len(tokens32); i++ {
			tokens32[i] = binary.BigEndian.Uint32(tokens[i*4 : (i+1)*4])
		}
		return tokens32, nil
	}

	// Handle chat messages with extended tokenizer
	if len(prompt.Messages) > 0 {
		extendedTok, ok := tokenizer.(ExtendedTokenizer)
		if !ok {
			return nil, fmt.Errorf("tokenizer does not support chat template processing")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		input := TokenizeInput{
			Type:                ChatInput,
			Messages:            prompt.Messages,
			AddSpecialTokens:    false,
			AddGenerationPrompt: true,
			ReturnTokenStrings:  false,
		}

		result, err := extendedTok.TokenizeWithOptions(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("chat template tokenization failed: %w", err)
		}

		// Convert int tokens to uint32
		tokens32 := make([]uint32, len(result.Tokens))
		for i, token := range result.Tokens {
			tokens32[i] = uint32(token)
		}
		return tokens32, nil
	}

	return nil, fmt.Errorf("empty prompt provided")
}
