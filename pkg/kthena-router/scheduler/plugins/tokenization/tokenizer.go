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
)

type remoteTokenizerImpl struct {
	config  RemoteTokenizerConfig
	client  *httpClient
	adapter engineAdapter
}

func NewRemoteTokenizer(config RemoteTokenizerConfig) (Tokenizer, error) {
	engine, err := normalizeEngine(config.Engine)
	if err != nil {
		return nil, err
	}

	var adapter engineAdapter
	switch engine {
	case EngineSGLang:
		adapter = newSGLangAdapter(config.Model)
	case EngineVLLM:
		adapter = newVLLMAdapter(config.Model)
	}

	client := newHTTPClient(config.Endpoint)
	return &remoteTokenizerImpl{
		config:  config,
		client:  client,
		adapter: adapter,
	}, nil
}

func (t *remoteTokenizerImpl) TokenizeInputText(text string) ([]byte, error) {
	ctx := context.Background()
	input := TokenizeInput{
		Type:             CompletionInput,
		Text:             text,
		AddSpecialTokens: t.config.AddSpecialTokens,
	}

	result, err := t.TokenizeWithOptions(ctx, input)
	if err != nil {
		return nil, err
	}

	return intToByteArray(result.Tokens), nil
}

func (t *remoteTokenizerImpl) TokenizeWithOptions(ctx context.Context, input TokenizeInput) (*TokenizeResult, error) {
	request, err := t.adapter.PrepareTokenizeRequest(input)
	if err != nil {
		return nil, ErrTokenizationFailed{
			Message: "failed to prepare request",
			Cause:   err,
		}
	}

	path := t.adapter.GetTokenizePath()
	respData, err := t.client.Post(ctx, path, request)
	if err != nil {
		return nil, ErrTokenizationFailed{
			Message: "request failed",
			Cause:   err,
		}
	}

	result, err := t.adapter.ParseTokenizeResponse(respData)
	if err != nil {
		return nil, ErrTokenizationFailed{
			Message: "failed to parse response",
			Cause:   err,
		}
	}

	return result, nil
}

func (t *remoteTokenizerImpl) GetEndpoint() string {
	return t.config.Endpoint
}

func (t *remoteTokenizerImpl) Close() error {
	if t.client != nil {
		t.client.Close()
	}
	return nil
}

var _ remoteTokenizer = (*remoteTokenizerImpl)(nil)
