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
	"time"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"k8s.io/klog/v2"
)

type TokenizerManagerConfig struct {
	EnableVLLMRemote bool
	EndpointTemplate string
}

type TokenizerManager struct {
	config TokenizerManagerConfig
}

func NewTokenizerManager(config TokenizerManagerConfig) *TokenizerManager {
	return &TokenizerManager{
		config: config,
	}
}

// GetTokenizer creates a tokenizer by randomly selecting from the provided pods
func (m *TokenizerManager) GetTokenizer(model string, pods []*datastore.PodInfo) Tokenizer {
	return m.createTokenizerFromPods(model, pods)
}

func (m *TokenizerManager) createTokenizerFromPods(model string, pods []*datastore.PodInfo) Tokenizer {
	if len(pods) == 0 {
		klog.Warningf("No pods provided for model %s", model)
		return nil
	}

	// Randomly select a pod to start with
	startIdx := rand.Intn(len(pods))

	// Try pods starting from random index, wrapping around if needed
	for i := 0; i < len(pods); i++ {
		podIdx := (startIdx + i) % len(pods)
		podInfo := pods[podIdx]

		endpoint := fmt.Sprintf(m.config.EndpointTemplate, podInfo.Pod.Status.PodIP)

		config := RemoteTokenizerConfig{
			Engine:             "vllm",
			Endpoint:           endpoint,
			Model:              model,
			AddSpecialTokens:   true,
			ReturnTokenStrings: false,
		}

		tok, err := NewRemoteTokenizer(config)
		if err != nil {
			klog.Warningf("Failed to create vLLM tokenizer for model %s at endpoint %s: %v", model, endpoint, err)
			continue
		}

		klog.V(4).Infof("TokenizerManager: successfully created tokenizer for model %s at endpoint %s", model, endpoint)
		return tok
	}

	klog.Warningf("Failed to create tokenizer for model %s after trying %d pods", model, len(pods))
	return nil
}

// TokenizePrompt tokenizes a prompt (text or chat messages) and returns uint32 tokens
func (m *TokenizerManager) TokenizePrompt(
	model string,
	prompt *common.ChatMessage,
	pods []*datastore.PodInfo,
) ([]uint32, error) {
	tokenizer := m.GetTokenizer(model, pods)
	if tokenizer == nil {
		return nil, fmt.Errorf("no tokenizer available for model %s", model)
	}

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
