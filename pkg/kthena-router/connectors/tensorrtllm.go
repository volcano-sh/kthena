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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// ConnectorTypeTensorRTLLM is the KVConnectorType for TensorRT-LLM native PD.
// It is defined here (not in API types) so it does not appear in CRD enums —
// selection is automatic from InferenceEngine=TensorRTLLM, like SGLang.
const ConnectorTypeTensorRTLLM v1alpha1.KVConnectorType = "tensorrtllm"

// TensorRTLLMConnector implements context-first PD for TensorRT-LLM workers
// that expose cache_transceiver_config.backend=NIXL.
//
// Protocol mirrors NVIDIA trtllm-serve disaggregated (openai_disagg_server):
//  1. Prefill: disaggregated_params.request_type=context_only (non-stream)
//  2. Read choices[0].disaggregated_params (+ top-level prompt_token_ids)
//  3. Decode: same params with request_type=generation_only; completions replace
//     prompt with prompt_token_ids; chat sets prompt_token_ids
//
// See: https://nvidia.github.io/TensorRT-LLM/latest/features/disagg-serving.html
type TensorRTLLMConnector struct{}

// NewTensorRTLLMConnector creates a TensorRT-LLM PD connector.
func NewTensorRTLLMConnector() KVConnector {
	return &TensorRTLLMConnector{}
}

// Name returns the connector type name.
func (t *TensorRTLLMConnector) Name() string {
	return "tensorrtllm"
}

// Proxy runs context-first prefill then decode with disaggregated_params handoff.
func (t *TensorRTLLMConnector) Proxy(c *gin.Context, reqBody map[string]interface{}, prefillAddr, decodeAddr string, timeout time.Duration, hooks *OnFlightHooks) (int, error) {
	rt := upstreamRoundTripper(c)

	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	prefillBody := cloneReqBody(reqBody)
	preparePrefillBody(prefillBody, responsesPrefillPath(c, c.Request))
	prefillBody["disaggregated_params"] = map[string]interface{}{
		"request_type": "context_only",
	}
	prefillReq, err := buildRequest(c.Request, prefillBody)
	if err != nil {
		return 0, &NonRetryableError{Err: fmt.Errorf("tensorrtllm: build prefill request: %w", err)}
	}

	if metricsRecorder != nil {
		metricsRecorder.StartPrefillPhase()
		metricsRecorder.IncActiveUpstreamRequests()
	}
	if hooks != nil && hooks.IncrPrefill != nil {
		hooks.IncrPrefill()
	}

	ctxResult, err := t.prefill(prefillReq, prefillAddr, timeout, rt)

	if hooks != nil && hooks.DecrPrefill != nil {
		hooks.DecrPrefill()
	}
	if metricsRecorder != nil {
		statusCode := "200"
		if err != nil {
			statusCode = "500"
		}
		metricsRecorder.FinishPrefillPhase(statusCode)
		metricsRecorder.DecActiveUpstreamRequests()
	}
	if err != nil {
		return 0, err
	}

	// Official disagg server skips generation when finish_reason is terminal
	// (not length / not_finished): JSON body for non-stream, SSE DONE for stream.
	if !needsGeneration(ctxResult.finishReason) {
		if stream, _ := reqBody["stream"].(bool); stream {
			return writeSSEDone(c)
		}
		return writeJSONResponse(c, ctxResult.cleanedResponse)
	}

	decodeBody := cloneReqBody(reqBody)
	decodeBody = AddTokenUsage(c, decodeBody)
	applyTensorRTLLMDecodeBody(decodeBody, ctxResult)

	decodeReq, err := buildRequest(c.Request, decodeBody)
	if err != nil {
		return 0, &NonRetryableError{Err: fmt.Errorf("tensorrtllm: build decode request: %w", err)}
	}

	if metricsRecorder != nil {
		metricsRecorder.StartDecodePhase()
		metricsRecorder.IncActiveUpstreamRequests()
	}
	if hooks != nil && hooks.IncrDecode != nil {
		hooks.IncrDecode()
	}

	result, decodeErr := t.decode(c, decodeReq, decodeAddr, timeout, rt)

	if hooks != nil && hooks.DecrDecode != nil {
		hooks.DecrDecode()
	}
	if metricsRecorder != nil {
		statusCode := "200"
		if decodeErr != nil {
			statusCode = "500"
		}
		metricsRecorder.FinishDecodePhase(statusCode)
		metricsRecorder.DecActiveUpstreamRequests()
	}

	return result, decodeErr
}

type tensorrtllmContextResult struct {
	disaggregatedParams map[string]interface{}
	promptTokenIDs      interface{}
	finishReason        string
	cleanedResponse     map[string]interface{}
}

func (t *TensorRTLLMConnector) prefill(req *http.Request, prefillAddr string, timeout time.Duration, rt http.RoundTripper) (*tensorrtllmContextResult, error) {
	req.URL.Host = prefillAddr
	req.URL.Scheme = "http"
	klog.V(4).Infof("tensorrtllm prefill: sending context_only to %s", req.URL.String())

	if timeout > 0 {
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("tensorrtllm prefill failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var buf strings.Builder
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return nil, err
	}

	var prefillerResponse map[string]interface{}
	if err := json.Unmarshal([]byte(buf.String()), &prefillerResponse); err != nil {
		return nil, fmt.Errorf("tensorrtllm: parse prefill response: %w", err)
	}

	choices, ok := prefillerResponse["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return nil, fmt.Errorf("tensorrtllm: prefill response missing choices")
	}
	if len(choices) > 1 {
		return nil, fmt.Errorf("tensorrtllm: prefill returned %d choices; only one is supported", len(choices))
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("tensorrtllm: invalid choice in prefill response")
	}

	rawParams, ok := choice["disaggregated_params"].(map[string]interface{})
	if !ok || rawParams == nil {
		return nil, fmt.Errorf("tensorrtllm: context server did not return disaggregated_params")
	}
	if _, ok := rawParams["ctx_request_id"]; !ok {
		return nil, fmt.Errorf("tensorrtllm: disaggregated_params missing ctx_request_id")
	}

	finishReason, _ := choice["finish_reason"].(string)

	// Strip disaggregated_params before optionally returning the context response
	// to the client (matches openai_disagg_server early-return path).
	cleaned := cloneReqBody(prefillerResponse)
	if cleanedChoices, ok := cleaned["choices"].([]interface{}); ok && len(cleanedChoices) > 0 {
		if cleanedChoice, ok := cleanedChoices[0].(map[string]interface{}); ok {
			delete(cleanedChoice, "disaggregated_params")
		}
	}

	return &tensorrtllmContextResult{
		disaggregatedParams: rawParams,
		promptTokenIDs:      prefillerResponse["prompt_token_ids"],
		finishReason:        finishReason,
		cleanedResponse:     cleaned,
	}, nil
}

func applyTensorRTLLMDecodeBody(decodeBody map[string]interface{}, ctx *tensorrtllmContextResult) {
	params := cloneReqBody(ctx.disaggregatedParams)
	params["request_type"] = "generation_only"
	decodeBody["disaggregated_params"] = params

	if ctx.promptTokenIDs == nil {
		return
	}
	// Completions: replace string prompt with token ids.
	// Chat: set prompt_token_ids (messages remain for API shape).
	if _, hasMessages := decodeBody["messages"]; hasMessages {
		decodeBody["prompt_token_ids"] = ctx.promptTokenIDs
	} else if _, hasPrompt := decodeBody["prompt"]; hasPrompt {
		decodeBody["prompt"] = ctx.promptTokenIDs
	} else {
		decodeBody["prompt_token_ids"] = ctx.promptTokenIDs
	}
}

func needsGeneration(finishReason string) bool {
	switch finishReason {
	case "", "length", "not_finished":
		return true
	default:
		return false
	}
}

func writeJSONResponse(c *gin.Context, payload map[string]interface{}) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, &NonRetryableError{Err: fmt.Errorf("tensorrtllm: marshal context response: %w", err)}
	}
	c.Header("Content-Type", "application/json")
	c.Status(http.StatusOK)
	if _, err := c.Writer.Write(body); err != nil {
		return 0, err
	}
	// Approximate output tokens for metrics as 1 (context-only produced one token).
	return 1, nil
}

// writeSSEDone matches openai_disagg_server early-return for stream=true:
// only `data: [DONE]` (no chat.completion JSON body).
func writeSSEDone(c *gin.Context) (int, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Status(http.StatusOK)
	if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
		return 0, err
	}
	return 0, nil
}

func (t *TensorRTLLMConnector) decode(c *gin.Context, req *http.Request, decodeAddr string, timeout time.Duration, rt http.RoundTripper) (int, error) {
	req.URL.Host = decodeAddr
	req.URL.Scheme = "http"
	klog.V(4).Infof("tensorrtllm decode: sending generation_only to %s", req.URL.String())
	return decoderProxy(c, req, timeout, rt)
}
