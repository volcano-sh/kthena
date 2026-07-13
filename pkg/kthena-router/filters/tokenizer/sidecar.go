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
	"context"

	"k8s.io/klog/v2"
)

type localTokenizer struct {
	client *Client
}

func NewlocalTokenizer() Tokenizer {
	return &localTokenizer{
		client: NewClient("http://localhost:8000"),
	}
}

func (s *localTokenizer) Load(modelServerID, modelID string) error {
	req := LoadRequest{
		ModelServerID: modelServerID,
		ModelrepoID:   modelID,
	}
	_, err := s.client.post(
		context.Background(),
		"/v1/load",
		req,
		nil,
	)
	return err
}

func (s *localTokenizer) Unload(modelServerID string) error {
	req := UnloadRequest{
		ModelServerID: modelServerID,
	}
	_, err := s.client.post(
		context.Background(),
		"/v1/unload",
		req,
		nil,
	)
	return err
}
func (s *localTokenizer) CountTokens(modelServerID, prompt string) (int, error) {
	req := EncodeRequest{
		ModelServerID: modelServerID,
		Text:          prompt,
		ReturnTokens:  false,
	}

	var resp EncodeResponse
	_, err := s.client.post(
		context.Background(),
		"/v1/encode",
		req,
		&resp,
	)
	if err == nil {
		return resp.TokenCount, nil
	}

	klog.Warningf("Local tokenizer unavailable, using heuristic token estimation: %v", err)

	estimator := &SimpleEstimateTokenizer{
		CharactersPerToken: 4,
	}
	return estimator.CountTokens(modelServerID, prompt)
}

func (s *localTokenizer) Encode(modelServerID, prompt string) ([]uint32, error) {
	req := EncodeRequest{
		ModelServerID: modelServerID,
		Text:          prompt,
		ReturnTokens:  true,
	}
	var resp EncodeResponse
	_, err := s.client.post(
		context.Background(),
		"/v1/encode",
		req,
		&resp,
	)
	if err != nil {
		return nil, err
	}
	ids := make([]uint32, len(resp.TokenIds))
	for i, id := range resp.TokenIds {
		{
			ids[i] = uint32(id)
		}
	}
	return ids, nil
}
