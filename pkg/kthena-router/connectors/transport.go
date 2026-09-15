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

package connectors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/handlers"
	"github.com/volcano-sh/kthena/pkg/kthena-router/providers"
	"k8s.io/klog/v2"
)

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func roundTrip(req *http.Request, timeout time.Duration) (*http.Response, error) {
	// Stop the timeout after response headers so long-running streams can finish.
	cancel := context.CancelFunc(func() {})
	if timeout > 0 {
		var ctx context.Context
		ctx, cancel = context.WithCancel(req.Context())
		timer := time.AfterFunc(timeout, cancel)
		defer timer.Stop()
		req = req.WithContext(ctx)
	}

	resp, err := upstreamTransport.RoundTrip(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if timeout > 0 {
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

func prefillerProxy(_ *gin.Context, req *http.Request, timeout time.Duration) error {
	resp, err := roundTrip(req, timeout)
	if err != nil {
		return fmt.Errorf("prefill request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("prefill request failed with status %d", resp.StatusCode)
	}

	klog.V(4).Infof("Prefill request completed successfully")
	return nil
}

// DecodeUpstreamError carries a non-2xx decode-request upstream response without
// writing it to the client. decoderProxy returns this instead of writing directly, so the PD
// retry loop in proxyToPDDisaggregated can still try another prefill/decode pair: writing to
// c.Writer immediately would make c.Writer.Written() true and stop the retry loop on the very
// first failed attempt. The caller decides when to call WriteTo — only once retries are
// exhausted (or not retryable) and this is the response that will actually reach the client.
// Currently only constructed for the OpenAI Responses API path; see decoderProxy.
type DecodeUpstreamError struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (e *DecodeUpstreamError) Error() string {
	return fmt.Sprintf("decode request failed with status %d", e.StatusCode)
}

// WriteTo forwards the captured upstream status, headers, and body to the client.
func (e *DecodeUpstreamError) WriteTo(c *gin.Context) {
	copyResponseHeaders(c, e.Header)
	c.Status(e.StatusCode)
	_, _ = c.Writer.Write(e.Body)
}

func decoderProxy(c *gin.Context, req *http.Request, timeout time.Duration) (int, error) {
	resp, err := roundTrip(req, timeout)
	if err != nil {
		return 0, fmt.Errorf("decode request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if isResponsesRequest(c, req) {
			// Chat Completions falls through without forwarding anything here
			// (unchanged, pre-existing behavior). For Responses, capture the
			// upstream status/headers/body instead of writing them now: the PD
			// retry loop (proxyToPDDisaggregated) still needs the chance to try
			// another prefill/decode pair, which it can only do while c.Writer
			// stays unwritten. The caller forwards this via WriteTo once it
			// decides no further retry will happen.
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				return 0, fmt.Errorf("failed to read decode error response with status %d: %w", resp.StatusCode, readErr)
			}
			return 0, &DecodeUpstreamError{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: body}
		}
		return 0, fmt.Errorf("decode request failed with status %d", resp.StatusCode)
	}

	copyResponseHeaders(c, resp.Header)
	c.Status(resp.StatusCode)

	// Determine if this is a streaming response
	stream := isStreamingResponse(resp)

	// The OpenAI Responses API uses a different usage shape
	// (input_tokens/output_tokens) and streaming terminal events
	// (response.completed/incomplete/failed, no `data: [DONE]`). Route it through
	// the shared provider response parser instead of the Chat Completions helpers.
	if isResponsesRequest(c, req) {
		parser := providers.DefaultAdapter().ResponseParser(c, "/v1/responses")
		if stream {
			outputTokens, err := providers.ForwardStream(c, resp.Body, parser, nil)
			if err != nil {
				return outputTokens, fmt.Errorf("streaming decode interrupted: %w", err)
			}
			return outputTokens, nil
		}
		outputTokens, err := providers.ForwardBody(c, resp.Body, parser, nil)
		if err != nil {
			return 0, fmt.Errorf("non-streaming decode interrupted: %w", err)
		}
		return outputTokens, nil
	}

	if stream {
		// Handle streaming response
		outputTokens, err := handleStreamingResponse(c, resp)
		if err != nil {
			return outputTokens, fmt.Errorf("streaming decode interrupted: %w", err)
		}
		return outputTokens, nil
	} else {
		// Handle non-streaming response
		outputTokens, err := handleNonStreamingResponse(c, resp)
		if err != nil {
			return 0, fmt.Errorf("non-streaming decode interrupted: %w", err)
		}
		return outputTokens, nil
	}
}

// preparePrefillBody modifies a request body for a PD-disaggregated prefill
// request: it disables streaming and caps the prefill output to a single token.
// The output-cap field is protocol-specific: the OpenAI Responses API uses
// max_output_tokens, while Chat Completions uses max_tokens (and
// max_completion_tokens when the client already set it).
func preparePrefillBody(reqBody map[string]interface{}, path string) {
	delete(reqBody, "stream")
	delete(reqBody, "stream_options")

	if isResponsesPath(path) {
		reqBody["max_output_tokens"] = 1
		return
	}

	reqBody["max_tokens"] = 1
	if reqBody["max_completion_tokens"] != nil {
		reqBody["max_completion_tokens"] = 1
	}
}

func buildPrefillRequest(c *gin.Context, req *http.Request, modelRequest map[string]interface{}) *http.Request {
	// In PD disaggregated mode, we need to send a prefill request to the prefill pod with non stream mode.
	preparePrefillBody(modelRequest, responsesPrefillPath(c, req))

	body, err := json.Marshal(modelRequest)
	if err != nil {
		return nil
	}

	// build request
	reqCopy := req.Clone(req.Context())
	reqCopy.URL.Scheme = "http"
	reqCopy.Body = io.NopCloser(bytes.NewBuffer(body))
	reqCopy.ContentLength = int64(len(body))

	return reqCopy
}

func BuildDecodeRequest(c *gin.Context, req *http.Request, modelRequest map[string]interface{}) *http.Request {
	var body []byte
	if isResponsesRequest(c, req) {
		// Chat Completions can opt into usage reporting via stream_options.include_usage.
		// The Responses API has no equivalent request-side flag: it always returns
		// usage in the response body (non-streaming) or in whichever terminal event
		// (response.completed/incomplete/failed) ends the stream, so that field must
		// not be injected here. When the parsed request still matches the original
		// body (no model rewrite) replay that body verbatim so opaque Responses
		// fields are preserved byte-for-byte; otherwise re-marshal the parsed map,
		// which changes only the model.
		if raw, ok := unmutatedResponsesBody(c, modelRequest); ok {
			body = raw
		} else {
			marshaled, err := json.Marshal(modelRequest)
			if err != nil {
				return nil
			}
			body = marshaled
		}
	} else {
		modelRequest = AddTokenUsage(c, modelRequest)
		marshaled, err := json.Marshal(modelRequest)
		if err != nil {
			return nil
		}
		body = marshaled
	}

	reqCopy := req.Clone(req.Context())
	reqCopy.URL.Scheme = "http"
	reqCopy.Body = io.NopCloser(bytes.NewBuffer(body))
	reqCopy.ContentLength = int64(len(body))

	return reqCopy
}

// isResponsesPath reports whether p targets the OpenAI Responses API endpoint.
// It mirrors the exact-match convention used by the provider adapters.
func isResponsesPath(p string) bool {
	return p == "/v1/responses"
}

// isResponsesRequest reports whether req targets the OpenAI Responses API.
// It checks both the original client-facing path and the current (possibly
// URLRewrite-mutated) request path, so a canonical client using /v1/responses
// directly and an HTTPRoute that rewrites a custom public path (e.g.
// /llm/v1/responses) to canonical /v1/responses are both recognized: by the
// time this runs, req.URL.Path reflects any URLRewrite already applied, while
// originalRequestPath still reflects the original public path.
func isResponsesRequest(c *gin.Context, req *http.Request) bool {
	return isResponsesPath(originalRequestPath(c, req)) || isResponsesPath(req.URL.Path)
}

// responsesPrefillPath resolves the path preparePrefillBody uses to choose between
// Responses (max_output_tokens) and Chat Completions (max_tokens) prefill-body
// shaping. It defers to isResponsesRequest so every prefill-body call site — the
// default HTTP connector via buildPrefillRequest, and NIXL/SGLang's own
// connector-specific prefill construction — recognizes a Responses request from
// either the original client path or a URLRewrite-mutated req.URL.Path, instead of
// each connector re-implementing that check independently.
func responsesPrefillPath(c *gin.Context, req *http.Request) string {
	if isResponsesRequest(c, req) {
		return "/v1/responses"
	}
	return originalRequestPath(c, req)
}

// originalRequestPath returns the client-facing request path used for wire
// protocol detection (e.g. isResponsesPath). It prefers the path recorded by
// the access-log middleware, which always runs before HTTPRoute matching and
// so captures the path before any URLRewrite filter can mutate req.URL.Path
// in place; it falls back to req.URL.Path when no access-log context is set
// (e.g. connector unit tests that call these helpers directly).
func originalRequestPath(c *gin.Context, req *http.Request) string {
	if c != nil {
		if accessCtx := accesslog.GetAccessLogContext(c); accessCtx != nil && accessCtx.Path != "" {
			return accessCtx.Path
		}
	}
	return req.URL.Path
}

// copyResponseHeaders copies all upstream response headers onto the client response.
func copyResponseHeaders(c *gin.Context, headers http.Header) {
	for k, vv := range headers {
		for _, v := range vv {
			c.Header(k, v)
		}
	}
}

// unmutatedResponsesBody returns the original raw request body when it is
// available on the gin context and still consistent with modelRequest (i.e. the
// model was not rewritten). It lets BuildDecodeRequest forward a Responses
// request byte-for-byte instead of re-marshalling the parsed map. It reports
// false whenever the raw body is missing or no longer matches, so the caller
// falls back to marshalling modelRequest.
func unmutatedResponsesBody(c *gin.Context, modelRequest map[string]interface{}) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	raw, exists := c.Get(common.RawRequestBodyKey)
	if !exists {
		return nil, false
	}
	rawBody, ok := raw.([]byte)
	if !ok || len(rawBody) == 0 {
		return nil, false
	}
	var original struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rawBody, &original); err != nil {
		return nil, false
	}
	model, _ := modelRequest["model"].(string)
	if model != original.Model {
		return nil, false
	}
	return rawBody, true
}

// AddTokenUsage adds token usage to the request body if it is not already present
// should be used for decode requests or non PD disaggregated mode
func AddTokenUsage(c *gin.Context, reqBody map[string]interface{}) map[string]interface{} {
	// Chat Completions can opt into usage reporting via stream_options.include_usage
	// (streaming) or include_usage (non-streaming), injected below. The Responses
	// API has no equivalent request-side flag and always returns usage in the
	// response body or terminal stream event, so neither field must be injected
	// here. This guard covers the nixl/sglang PD decode paths, which call
	// AddTokenUsage directly.
	if c != nil && c.Request != nil && isResponsesRequest(c, c.Request) {
		return reqBody
	}
	// Check if streaming is enabled
	if isStreamingRequest(reqBody) {
		if !isTokenUsageEnabled(reqBody) {
			// For streaming requests, add stream_options to include token usage
			reqBody["stream_options"] = map[string]interface{}{
				"include_usage": true,
			}
			// add stream token usage to context
			c.Set(common.TokenUsageKey, true)
		}
	} else {
		// For non-streaming requests, ensure we request usage information
		// Most OpenAI-compatible APIs return usage by default for non-streaming,
		// but we can be explicit about it
		reqBody["include_usage"] = true
	}
	return reqBody
}

// isStreaming checks if the given model request has streaming enabled
func isStreamingRequest(modelRequest map[string]interface{}) bool {
	if v, ok := modelRequest["stream"]; ok {
		if stream, isBool := v.(bool); isBool && stream {
			return true
		}
	}
	return false
}

func isTokenUsageEnabled(modelRequest map[string]interface{}) bool {
	// Check if token usage is enabled in the model request
	if v, ok := modelRequest["stream_options"]; ok {
		if streamOptions, isMap := v.(map[string]interface{}); isMap {
			if includeUsage, isBool := streamOptions["include_usage"].(bool); isBool && includeUsage {
				return true
			}
		}
	}
	return false
}

// isStreamingResponse checks if the response is a streaming response.
// It uses mime.ParseMediaType so that parameters such as charset are ignored,
// e.g. "text/event-stream; charset=utf-8" is correctly recognised as streaming.
func isStreamingResponse(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "text/event-stream" || mediaType == "application/x-ndjson"
}

// handleStreamingResponse handles streaming responses
func handleStreamingResponse(c *gin.Context, resp *http.Response) (int, error) {
	totalOutputTokens := 0
	reader := bufio.NewReader(resp.Body)
	var streamErr error
	c.Stream(func(w io.Writer) bool {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Try to parse usage from this line
			parsed := handlers.ParseStreamRespForUsage(string(line))
			if parsed.Usage.CompletionTokens > 0 {
				klog.V(4).Infof("Parsed usage: %+v", parsed.Usage)
				// Accumulate output tokens
				totalOutputTokens += parsed.Usage.CompletionTokens
				// Check if token usage should be filtered
				if v, ok := c.Get(common.TokenUsageKey); ok && v.(bool) {
					return true
				}
			}
			// Forward to downstream
			_, _ = w.Write(line)
		}
		if err != nil {
			if err != io.EOF {
				klog.Errorf("error reading stream body: %v", err)
				streamErr = err
			}
			return false
		}
		return true
	})
	return totalOutputTokens, streamErr
}

// handleNonStreamingResponse handles non-streaming responses
func handleNonStreamingResponse(c *gin.Context, resp *http.Response) (int, error) {
	var buf bytes.Buffer
	teeReader := io.TeeReader(resp.Body, &buf)

	_, err := io.Copy(c.Writer, teeReader)
	if err != nil {
		klog.Errorf("copy response to downstream failed: %v", err)
		return 0, err
	}

	// Parse usage if present
	parsed, err := handlers.ParseOpenAIResponseBody(buf.Bytes())
	if err != nil {
		klog.V(4).Infof("failed to parse non-streaming response usage: %v", err)
		return 0, nil
	}

	if parsed != nil && parsed.Usage.CompletionTokens > 0 {
		klog.V(4).Infof("Parsed usage: %+v", parsed.Usage)
		return parsed.Usage.CompletionTokens, nil
	}

	return 0, nil
}
