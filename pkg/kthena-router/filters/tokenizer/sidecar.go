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
