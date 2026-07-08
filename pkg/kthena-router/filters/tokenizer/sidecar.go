package tokenizer

import "context"

type SidecarTokenizer struct {
	client *Client
}

func NewSidecarTokenizer(endpoint string) LocalTokenizer {
	return &SidecarTokenizer{
		client: NewClient(endpoint),
	}
}

func (s *SidecarTokenizer) Load(modelServerID, modelID string) error {
	req := LoadRequest{
		ModelServerID: modelServerID,
		ModelID:       modelID,
	}
	_, err := s.client.post(
		context.Background(),
		"/v1/load",
		req,
		nil,
	)
	return err
}

func (s *SidecarTokenizer) Unload(modelServerID string) error {
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

func (s *SidecarTokenizer) CountTokens(modelServerID, prompt string) (int, error) {
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
