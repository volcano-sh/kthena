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
	defaultEndpoint = "http://localhost:50051"
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

// post sends a JSON POST request and returns the decoded response (if resp
// is non-nil), the raw *http.Response (for callers that need status code,
// headers, etc.), and an error.
//
// Note: httpResp.Body is already closed by the time post returns, so
// callers must not attempt to read httpResp.Body themselves — use the
// decoded resp value instead. httpResp is returned primarily for metadata
// (StatusCode, Header, Request, etc.).
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

// get sends a GET request and returns the decoded response (if resp is
// non-nil), the raw *http.Response, and an error. See post's doc comment
// regarding httpResp.Body already being closed on return.
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

// newHTTPError reads (and bounds) the response body so error messages
// from the sidecar are surfaced to the caller instead of discarded.
func newHTTPError(httpResp *http.Response) error {
	const maxErrBody = 4 << 10 // 4KB cap, avoid huge bodies in error strings
	limited := io.LimitReader(httpResp.Body, maxErrBody)
	b, readErr := io.ReadAll(limited)
	if readErr != nil || len(b) == 0 {
		return fmt.Errorf("tokenizer returned %s", httpResp.Status)
	}
	return fmt.Errorf("tokenizer returned %s: %s", httpResp.Status, bytes.TrimSpace(b))
}
