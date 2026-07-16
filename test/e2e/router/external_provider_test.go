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

package router

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const successfulExternalRequest = "successful_request"

func TestExternalModelProviders(t *testing.T) {
	fixture := setupExternalProviderFixture(t, testCtx, testNamespace, kthenaNamespace)

	t.Run("OpenAIChatNonStreaming", fixture.testOpenAIChatNonStreaming)
	t.Run("OpenAIChatStreaming", fixture.testOpenAIChatStreaming)
	t.Run("OpenAIResponsesNonStreaming", fixture.testOpenAIResponsesNonStreaming)
	t.Run("OpenAIResponsesStreaming", fixture.testOpenAIResponsesStreaming)
	t.Run("AnthropicNonStreaming", fixture.testAnthropicNonStreaming)
	t.Run("AnthropicStreaming", fixture.testAnthropicStreaming)
	t.Run("NonTextInputAccounting", fixture.testNonTextInputAccounting)
	t.Run("Upstream429", fixture.testUpstream429)
	t.Run("ActiveGaugeLifecycle", fixture.testActiveGaugeLifecycle)
}

func (f externalProviderFixture) testOpenAIChatNonStreaming(t *testing.T) {
	requestID := "external-chat-" + utils.RandomString(10)
	path := "/v1/chat/completions"
	body := []byte(`{
		"model":"` + externalOpenAIChatModel + `",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"Use the image and tool."},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}
			]},
			{"role":"assistant","tool_calls":[{"id":"call-weather","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Shanghai\"}"}}]},
			{"role":"tool","tool_call_id":"call-weather","content":"sunny"}
		],
		"tools":[{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
		"tool_choice":"auto",
		"stream":false,
		"max_tokens":16
	}`)

	before := readExternalMetricSnapshot(t, externalOpenAIChatModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	assert.Equal(t, "application/json", response.header.Get("Content-Type"))
	var responseObject map[string]any
	require.NoError(t, json.Unmarshal(response.body, &responseObject))
	assert.Equal(t, "chat-mock", responseObject["id"])

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assertRewrittenExternalPayload(t, body, capture.Body, externalOpenAIChatUpstreamModel, false)

	after := waitForExternalMetricDelta(t, before, externalOpenAIChatModel, path, http.StatusOK, successfulExternalRequest, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalOpenAIChatModel, path, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	assertPositiveExternalInputAccounting(t, before, after, entry)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testOpenAIChatStreaming(t *testing.T) {
	requestID := "external-chat-stream-" + utils.RandomString(10)
	path := "/v1/chat/completions"
	body := []byte(`{
		"model":"` + externalOpenAIChatModel + `",
		"messages":[{"role":"user","content":"Stream a short answer."}],
		"stream":true,
		"stream_options":{"vendor_option":"preserve-me"},
		"max_tokens":16
	}`)

	before := readExternalMetricSnapshot(t, externalOpenAIChatModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	assert.Contains(t, response.header.Get("Content-Type"), "text/event-stream")
	assert.Contains(t, string(response.body), `"object":"chat.completion.chunk"`)
	assert.Contains(t, string(response.body), `data: [DONE]`)

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assertRewrittenExternalPayload(t, body, capture.Body, externalOpenAIChatUpstreamModel, true)

	after := waitForExternalMetricDelta(t, before, externalOpenAIChatModel, path, http.StatusOK, successfulExternalRequest, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalOpenAIChatModel, path, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	assertPositiveExternalInputAccounting(t, before, after, entry)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testOpenAIResponsesNonStreaming(t *testing.T) {
	requestID := "external-responses-json-" + utils.RandomString(10)
	path := "/v1/responses"
	body := []byte(`{
		"model":"` + externalResponsesModel + `",
		"input":"Return a short JSON response.",
		"stream":false
	}`)

	before := readExternalMetricSnapshot(t, externalResponsesModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalResponsesRouteName, externalResponsesProviderName, externalResponsesUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	assert.Equal(t, "application/json", response.header.Get("Content-Type"))
	var responseObject map[string]any
	require.NoError(t, json.Unmarshal(response.body, &responseObject))
	assert.Equal(t, "resp-mock", responseObject["id"])

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assertRewrittenExternalPayload(t, body, capture.Body, externalResponsesUpstreamModel, false)

	after := waitForExternalMetricDelta(t, before, externalResponsesModel, path, http.StatusOK, successfulExternalRequest, externalResponsesRouteName, externalResponsesProviderName, externalResponsesUpstreamModel, 4)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalResponsesModel, path, externalResponsesRouteName, externalResponsesProviderName, externalResponsesUpstreamModel, 4)
	assertPositiveExternalInputAccounting(t, before, after, entry)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testOpenAIResponsesStreaming(t *testing.T) {
	requestID := "external-responses-" + utils.RandomString(10)
	path := "/v1/responses"
	body := []byte(`{
		"model":"` + externalResponsesModel + `",
		"stream":true,
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"Inspect the inputs."},
				{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="},
				{"type":"input_file","file_id":"file-e2e"}
			]},
			{"type":"additional_tools","tools":[{"type":"custom","name":"shell"}]},
			{"type":"custom_tool_call","call_id":"custom-call","name":"shell","input":"echo ok"},
			{"type":"custom_tool_call_output","call_id":"custom-call","output":"ok"}
		],
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]
	}`)

	before := readExternalMetricSnapshot(t, externalResponsesModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalResponsesRouteName, externalResponsesProviderName, externalResponsesUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	assert.Contains(t, response.header.Get("Content-Type"), "text/event-stream")
	assert.Contains(t, string(response.body), `"type":"response.completed"`)

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assertRewrittenExternalPayload(t, body, capture.Body, externalResponsesUpstreamModel, false)

	after := waitForExternalMetricDelta(t, before, externalResponsesModel, path, http.StatusOK, successfulExternalRequest, externalResponsesRouteName, externalResponsesProviderName, externalResponsesUpstreamModel, 4)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalResponsesModel, path, externalResponsesRouteName, externalResponsesProviderName, externalResponsesUpstreamModel, 4)
	assertPositiveExternalInputAccounting(t, before, after, entry)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testAnthropicNonStreaming(t *testing.T) {
	requestID := "external-anthropic-json-" + utils.RandomString(10)
	path := "/v1/messages"
	body := []byte(`{
		"model":"` + externalAnthropicModel + `",
		"max_tokens":16,
		"stream":false,
		"messages":[{"role":"user","content":"Return a short JSON response."}]
	}`)

	before := readExternalMetricSnapshot(t, externalAnthropicModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalAnthropicRouteName, externalAnthropicProviderName, externalAnthropicUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	assert.Equal(t, "application/json", response.header.Get("Content-Type"))
	var responseObject map[string]any
	require.NoError(t, json.Unmarshal(response.body, &responseObject))
	assert.Equal(t, "msg-mock", responseObject["id"])

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, true)
	assertRewrittenExternalPayload(t, body, capture.Body, externalAnthropicUpstreamModel, false)

	after := waitForExternalMetricDelta(t, before, externalAnthropicModel, path, http.StatusOK, successfulExternalRequest, externalAnthropicRouteName, externalAnthropicProviderName, externalAnthropicUpstreamModel, 6)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalAnthropicModel, path, externalAnthropicRouteName, externalAnthropicProviderName, externalAnthropicUpstreamModel, 6)
	assertPositiveExternalInputAccounting(t, before, after, entry)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testAnthropicStreaming(t *testing.T) {
	requestID := "external-anthropic-" + utils.RandomString(10)
	path := "/v1/messages"
	body := []byte(`{
		"model":"` + externalAnthropicModel + `",
		"max_tokens":32,
		"stream":true,
		"tools":[{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"Inspect the attachments."},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"ZmlsZQ=="}}
			]},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool-weather","name":"weather","input":{"city":"Shanghai"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-weather","content":[{"type":"text","text":"sunny"}]}]}
		]
	}`)

	before := readExternalMetricSnapshot(t, externalAnthropicModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalAnthropicRouteName, externalAnthropicProviderName, externalAnthropicUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	assert.Contains(t, response.header.Get("Content-Type"), "text/event-stream")
	assert.Contains(t, string(response.body), `"type":"message_stop"`)

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, true)
	assertRewrittenExternalPayload(t, body, capture.Body, externalAnthropicUpstreamModel, false)

	after := waitForExternalMetricDelta(t, before, externalAnthropicModel, path, http.StatusOK, successfulExternalRequest, externalAnthropicRouteName, externalAnthropicProviderName, externalAnthropicUpstreamModel, 6)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalAnthropicModel, path, externalAnthropicRouteName, externalAnthropicProviderName, externalAnthropicUpstreamModel, 6)
	assertPositiveExternalInputAccounting(t, before, after, entry)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testNonTextInputAccounting(t *testing.T) {
	requestID := "external-image-only-" + utils.RandomString(10)
	path := "/v1/chat/completions"
	body := []byte(`{
		"model":"` + externalOpenAIChatModel + `",
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]}],
		"stream":false,
		"max_tokens":16
	}`)

	before := readExternalMetricSnapshot(t, externalOpenAIChatModel, path, http.StatusOK, successfulExternalRequest, testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)
	response := sendExternalJSONRequest(t, path, requestID, body)
	require.Equal(t, http.StatusOK, response.statusCode, "response: %s", response.body)
	var providerResponse struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(response.body, &providerResponse))
	assert.Positive(t, providerResponse.Usage.PromptTokens, "mock provider usage must prove that local input accounting ignores provider input usage")

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assertRewrittenExternalPayload(t, body, capture.Body, externalOpenAIChatUpstreamModel, false)
	assert.Contains(t, string(capture.Body), `"type":"image_url"`)

	after := waitForExternalMetricDelta(t, before, externalOpenAIChatModel, path, http.StatusOK, successfulExternalRequest, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	assert.Equal(t, float64(0), after.inputTokens-before.inputTokens)
	assert.Equal(t, float64(5), after.outputTokens-before.outputTokens)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalOpenAIChatModel, path, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	assert.Equal(t, 0, entry.InputTokens)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testUpstream429(t *testing.T) {
	requestID := "external-429-" + utils.RandomString(10)
	path := "/v1/chat/completions"
	body := []byte(`{"model":"` + externalOpenAIChatModel + `","messages":[{"role":"user","content":"rate limit me"}],"stream":false}`)

	before := readExternalMetricSnapshot(t, externalOpenAIChatModel, path, http.StatusTooManyRequests, "upstream_response", testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)
	response := sendExternalJSONRequest(t, path+"?mock_status=429", requestID, body)
	require.Equal(t, http.StatusTooManyRequests, response.statusCode, "response: %s", response.body)
	assert.Contains(t, string(response.body), `"type":"rate_limit_error"`)

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assert.Equal(t, "mock_status=429", capture.RawQuery)
	assertRewrittenExternalPayload(t, body, capture.Body, externalOpenAIChatUpstreamModel, false)

	after := waitForExternalMetricDelta(t, before, externalOpenAIChatModel, path, http.StatusTooManyRequests, "upstream_response", externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 0)
	assert.Equal(t, float64(0), after.outputTokens-before.outputTokens)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assert.Equal(t, http.StatusTooManyRequests, entry.StatusCode)
	assert.Equal(t, http.StatusTooManyRequests, entry.UpstreamStatusCode)
	assert.Equal(t, 1, entry.UpstreamAttempts)
	assert.Equal(t, "external_provider", entry.BackendType)
	assert.Equal(t, testNamespace+"/"+externalOpenAIChatRouteName, entry.ModelRoute)
	assert.Equal(t, testNamespace+"/"+externalOpenAIChatProviderName, entry.BackendName)
	assert.Equal(t, externalOpenAIChatUpstreamModel, entry.UpstreamModel)
	assert.Equal(t, "upstream", entry.ErrorOrigin)
	assert.Equal(t, 0, entry.OutputTokens)
	require.NotNil(t, entry.Error)
	assert.Equal(t, "upstream_response", entry.Error.Type)
	deleteMockCapture(t, f.adminURL, requestID)
}

func (f externalProviderFixture) testActiveGaugeLifecycle(t *testing.T) {
	requestID := "external-active-" + utils.RandomString(10)
	path := "/v1/chat/completions"
	body := []byte(`{"model":"` + externalOpenAIChatModel + `","messages":[{"role":"user","content":"wait"}],"stream":false}`)
	downstreamBaseline, upstreamBaseline := readExternalActiveGauges(t, externalOpenAIChatModel, testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)

	type requestResult struct {
		response externalHTTPResponse
		err      error
	}
	resultCh := make(chan requestResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		response, err := doExternalJSONRequest(ctx, path+"?mock_delay_ms=1500", requestID, body)
		resultCh <- requestResult{response: response, err: err}
	}()

	require.Eventually(t, func() bool {
		downstream, upstream, err := tryReadExternalActiveGauges(externalOpenAIChatModel, testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)
		if err != nil {
			return false
		}
		return downstream >= downstreamBaseline+1 && upstream >= upstreamBaseline+1
	}, 3*time.Second, 100*time.Millisecond, "active downstream and upstream gauges did not rise during the delayed request")

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.response.statusCode, "response: %s", result.response.body)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delayed external provider request")
	}

	require.Eventually(t, func() bool {
		downstream, upstream, err := tryReadExternalActiveGauges(externalOpenAIChatModel, testNamespace, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel)
		if err != nil {
			return false
		}
		return downstream == downstreamBaseline && upstream == upstreamBaseline
	}, 3*time.Second, 100*time.Millisecond, "active downstream and upstream gauges did not return to baseline")

	capture := fetchMockCapture(t, f.adminURL, requestID)
	assertExternalCapture(t, capture, path, false)
	assert.Equal(t, "mock_delay_ms=1500", capture.RawQuery)
	entry := waitForExternalAccessLog(t, testCtx.KubeClient, kthenaNamespace, requestID)
	assertSuccessfulExternalAccessLog(t, entry, externalOpenAIChatModel, path, externalOpenAIChatRouteName, externalOpenAIChatProviderName, externalOpenAIChatUpstreamModel, 5)
	assert.Positive(t, entry.InputTokens)
	assert.GreaterOrEqual(t, entry.DurationUpstreamProcessing, int64(1000))
	assert.GreaterOrEqual(t, entry.DurationTotal, entry.DurationUpstreamProcessing)
	deleteMockCapture(t, f.adminURL, requestID)
}

func assertExternalCapture(t *testing.T, capture mockCapture, path string, anthropic bool) {
	t.Helper()
	assert.Equal(t, http.MethodPost, capture.Method)
	assert.Equal(t, path, capture.Path)
	assert.Equal(t, "application/json", capture.ContentType)
	assert.True(t, capture.Authorized)
	if anthropic {
		assert.Equal(t, "2023-06-01", capture.AnthropicVersion)
	} else {
		assert.Empty(t, capture.AnthropicVersion)
	}
}

func assertPositiveExternalInputAccounting(t *testing.T, before, after externalMetricSnapshot, entry accesslog.AccessLogEntry) {
	t.Helper()
	delta := after.inputTokens - before.inputTokens
	require.Positive(t, delta, "text input must increment the locally estimated input-token metric")
	assert.Equal(t, delta, float64(entry.InputTokens), "metric and access log must use the same local input-token estimate")
}

func assertRewrittenExternalPayload(t *testing.T, original, captured []byte, upstreamModel string, expectStreamUsageInjection bool) {
	t.Helper()
	var want, got map[string]any
	require.NoError(t, json.Unmarshal(original, &want))
	require.NoError(t, json.Unmarshal(captured, &got))
	assert.Equal(t, upstreamModel, got["model"])
	delete(want, "model")
	delete(got, "model")
	if expectStreamUsageInjection {
		wantOptions, _ := want["stream_options"].(map[string]any)
		gotOptions, ok := got["stream_options"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, gotOptions["include_usage"])
		delete(gotOptions, "include_usage")
		assert.Equal(t, wantOptions, gotOptions)
		delete(want, "stream_options")
		delete(got, "stream_options")
	} else {
		assert.NotContains(t, got, "include_usage")
		assert.NotContains(t, got, "stream_options")
	}
	assert.Equal(t, want, got, "opaque non-model payload fields must be preserved")
}

func waitForExternalMetricDelta(t *testing.T, before externalMetricSnapshot, model, path string, statusCode int, errorType, route, provider, upstreamModel string, outputTokens float64) externalMetricSnapshot {
	t.Helper()
	var after externalMetricSnapshot
	require.Eventually(t, func() bool {
		var err error
		after, err = tryReadExternalMetricSnapshot(model, path, statusCode, errorType, testNamespace, route, provider, upstreamModel)
		if err != nil {
			return false
		}
		return after.requests-before.requests == 1 &&
			after.durationCount-before.durationCount == 1 &&
			after.outputTokens-before.outputTokens == outputTokens
	}, 10*time.Second, 250*time.Millisecond, "external provider metrics did not record one request and %.0f output tokens", outputTokens)
	return after
}

func assertSuccessfulExternalAccessLog(t *testing.T, entry accesslog.AccessLogEntry, model, path, route, provider, upstreamModel string, outputTokens int) {
	t.Helper()
	assert.Equal(t, http.MethodPost, entry.Method)
	assert.Equal(t, path, entry.Path)
	assert.Equal(t, http.StatusOK, entry.StatusCode)
	assert.Equal(t, model, entry.ModelName)
	assert.Equal(t, testNamespace+"/"+route, entry.ModelRoute)
	assert.Equal(t, "external_provider", entry.BackendType)
	assert.Equal(t, testNamespace+"/"+provider, entry.BackendName)
	assert.Equal(t, upstreamModel, entry.UpstreamModel)
	assert.Equal(t, http.StatusOK, entry.UpstreamStatusCode)
	assert.Equal(t, 1, entry.UpstreamAttempts)
	assert.Equal(t, outputTokens, entry.OutputTokens)
	assert.Empty(t, entry.ModelServer)
	assert.Empty(t, entry.SelectedPod)
	assert.Empty(t, entry.ErrorOrigin)
	assert.Nil(t, entry.Error)
	assert.GreaterOrEqual(t, entry.DurationTotal, int64(0))
	assert.GreaterOrEqual(t, entry.DurationUpstreamProcessing, int64(0))
}
