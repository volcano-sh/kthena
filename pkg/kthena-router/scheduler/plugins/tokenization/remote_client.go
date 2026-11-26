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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

const (
	defaultTimeout    = 5 * time.Second
	defaultMaxRetries = 1
)

type httpClient struct {
	client  *retryablehttp.Client
	baseURL string
}

func newRetryableHTTPClient() *retryablehttp.Client {
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = defaultMaxRetries
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 1 * time.Second
	retryClient.HTTPClient.Timeout = defaultTimeout
	retryClient.Logger = nil
	// Do not retry 503 from the tokenizer service: it signals that the
	// per-renderer in-flight limit is reached and the router should fall back
	// to engine-side tokenization immediately rather than sleeping and retrying
	// against a saturated renderer.
	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if resp != nil && resp.StatusCode == http.StatusServiceUnavailable {
			return false, nil
		}
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}
	return retryClient
}

func newHTTPClient(baseURL string, retryClient *retryablehttp.Client) *httpClient {
	return &httpClient{
		client:  retryClient,
		baseURL: baseURL,
	}
}

func (c *httpClient) Post(ctx context.Context, path string, data interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	url := c.baseURL + path

	req, err := retryablehttp.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, ErrHTTPRequest{
			StatusCode: resp.StatusCode,
			Message:    string(body),
		}
	}

	return body, nil
}

func (c *httpClient) Close() {
	if c.client != nil && c.client.HTTPClient != nil {
		c.client.HTTPClient.CloseIdleConnections()
	}
}
