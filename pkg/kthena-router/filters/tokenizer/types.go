package tokenizer

type LoadRequest struct {
	ModelServerID string `json:"model_server_id"`
	ModelrepoID   string `json:"model_repo_id"`
}

type UnloadRequest struct {
	ModelServerID string `json:"model_server_id"`
}

type EncodeRequest struct {
	ModelServerID string `json:"model_server_id"`
	Text          string `json:"text"`
	ReturnTokens  bool   `json:"return_tokens"`
}

type EncodeResponse struct {
	TokenCount int `json:"token_count"`
}
