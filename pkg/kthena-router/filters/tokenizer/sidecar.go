package tokenizer

import "context"

type localTokenizer struct {
	client *Client
}

func NewlocalTokenizer(endpoint string) Tokenizer {
	return &localTokenizer{
		client: NewClient(endpoint),
	}
}

func (s *localTokenizer) Load(modelServerID, modelRepoID string) error {
	req := LoadRequest{
		ModelServerID: modelServerID,
		ModelrepoID:   modelRepoID,
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
	if err != nil {
		return 0, err
	}
	return resp.TokenCount, nil
}
