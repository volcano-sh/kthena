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
	"fmt"
	"io"
	"net/http"
)

const (
	defaultEndpoint = "http://localhost:8000"
)

type Client struct {
	endpoint string
	client   *http.Client
}

func NewClient(endpoint string) *Client {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Client{
		endpoint: endpoint,
		client:   &http.Client{},
	}
}

func (c *Client) post(ctx context.Context, path string, req any, resp any) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.endpoint+path,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 300 {
		return httpResp, newHTTPError(httpResp)
	}

	if resp != nil {
		if err := json.NewDecoder(httpResp.Body).Decode(resp); err != nil {
			return httpResp, err
		}
	}
	return httpResp, nil
}

func (c *Client) get(ctx context.Context, path string, resp any) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.endpoint+path,
		nil,
	)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 300 {
		return httpResp, newHTTPError(httpResp)
	}

	if resp != nil {
		if err := json.NewDecoder(httpResp.Body).Decode(resp); err != nil {
			return httpResp, err
		}
	}
	return httpResp, nil
}

func newHTTPError(httpResp *http.Response) error {
	const maxErrBody = 4 << 10 // 4KB cap, avoid huge bodies in error strings
	limited := io.LimitReader(httpResp.Body, maxErrBody)
	b, readErr := io.ReadAll(limited)
	if readErr != nil || len(b) == 0 {
		return fmt.Errorf("tokenizer returned %s", httpResp.Status)
	}
	return fmt.Errorf("tokenizer returned %s: %s", httpResp.Status, bytes.TrimSpace(b))
}
