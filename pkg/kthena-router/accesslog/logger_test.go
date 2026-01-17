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

package accesslog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccessLogEntry_ToJSON(t *testing.T) {
	entry := &AccessLogEntry{
		Timestamp:                  time.Date(2024, 1, 15, 10, 30, 45, 123000000, time.UTC),
		Method:                     "POST",
		Path:                       "/v1/chat/completions",
		Protocol:                   "HTTP/1.1",
		StatusCode:                 200,
		ModelName:                  "llama2-7b",
		ModelRoute:                 "default/llama2-route-v1",
		ModelServer:                "default/llama2-server",
		SelectedPod:                "llama2-deployment-5f7b8c9d-xk2p4",
		RequestID:                  "test-request-id",
		Gateway:                    "default/test-gateway",
		HTTPRoute:                  "default/test-httproute",
		InferencePool:              "default/test-inferencepool",
		InputTokens:                150,
		OutputTokens:               75,
		DurationTotal:              2350,
		DurationRequestProcessing:  45,
		DurationUpstreamProcessing: 2180,
		DurationResponseProcessing: 5,
	}

	// Create a logger with JSON format
	config := &AccessLoggerConfig{
		Format:  FormatJSON,
		Output:  "stdout",
		Enabled: true,
	}

	logger := &accessLoggerImpl{config: config}
	output, err := logger.formatJSON(entry)
	require.NoError(t, err)

	// Parse the JSON to verify structure
	var parsed map[string]interface{}
	err = json.Unmarshal([]byte(output), &parsed)
	require.NoError(t, err)

	assert.Equal(t, "POST", parsed["method"])
	assert.Equal(t, "/v1/chat/completions", parsed["path"])
	assert.Equal(t, float64(200), parsed["status_code"])
	assert.Equal(t, "llama2-7b", parsed["model_name"])
	assert.Equal(t, "default/llama2-route-v1", parsed["model_route"])
	assert.Equal(t, "default/llama2-server", parsed["model_server"])
	assert.Equal(t, "llama2-deployment-5f7b8c9d-xk2p4", parsed["selected_pod"])
	assert.Equal(t, "default/test-gateway", parsed["gateway"])
	assert.Equal(t, "default/test-httproute", parsed["http_route"])
	assert.Equal(t, "default/test-inferencepool", parsed["inference_pool"])
	assert.Equal(t, float64(150), parsed["input_tokens"])
	assert.Equal(t, float64(75), parsed["output_tokens"])

	// Check flattened duration fields
	assert.Equal(t, float64(2350), parsed["duration_total"])
	assert.Equal(t, float64(45), parsed["duration_request_processing"])
	assert.Equal(t, float64(2180), parsed["duration_upstream_processing"])
	assert.Equal(t, float64(5), parsed["duration_response_processing"])
}

func TestAccessLogEntry_ToText(t *testing.T) {
	entry := &AccessLogEntry{
		Timestamp:                  time.Date(2024, 1, 15, 10, 30, 45, 123000000, time.UTC),
		Method:                     "POST",
		Path:                       "/v1/chat/completions",
		Protocol:                   "HTTP/1.1",
		StatusCode:                 200,
		ModelName:                  "llama2-7b",
		ModelRoute:                 "default/llama2-route-v1",
		ModelServer:                "default/llama2-server",
		SelectedPod:                "llama2-deployment-5f7b8c9d-xk2p4",
		RequestID:                  "test-request-id",
		Gateway:                    "default/test-gateway",
		HTTPRoute:                  "default/test-httproute",
		InferencePool:              "default/test-inferencepool",
		InputTokens:                150,
		OutputTokens:               75,
		DurationTotal:              2350,
		DurationRequestProcessing:  45,
		DurationUpstreamProcessing: 2180,
		DurationResponseProcessing: 5,
	}

	// Create a logger with text format
	config := &AccessLoggerConfig{
		Format:  FormatText,
		Output:  "stdout",
		Enabled: true,
	}

	logger := &accessLoggerImpl{config: config}
	output, err := logger.formatText(entry)
	require.NoError(t, err)

	expectedParts := []string{
		`[2024-01-15T10:30:45.123Z]`,
		`"POST /v1/chat/completions HTTP/1.1"`,
		`200`,
		`model_name=llama2-7b`,
		`model_route=default/llama2-route-v1`,
		`model_server=default/llama2-server`,
		`selected_pod=llama2-deployment-5f7b8c9d-xk2p4`,
		`request_id=test-request-id`,
		`gateway=default/test-gateway`,
		`http_route=default/test-httproute`,
		`inference_pool=default/test-inferencepool`,
		`tokens=150/75`,
		`timings=2350ms(45+2180+5)`,
	}

	for _, part := range expectedParts {
		assert.Contains(t, output, part, "Output should contain: %s", part)
	}
}

func TestAccessLogEntry_WithError(t *testing.T) {
	entry := &AccessLogEntry{
		Timestamp:  time.Date(2024, 1, 15, 10, 30, 45, 123000000, time.UTC),
		Method:     "POST",
		Path:       "/v1/chat/completions",
		Protocol:   "HTTP/1.1",
		StatusCode: 500,
		Error: &ErrorInfo{
			Type:    "timeout",
			Message: "Model inference timeout after 30s",
		},
		ModelName:                  "llama2-7b",
		Gateway:                    "default/test-gateway",
		HTTPRoute:                  "default/test-httproute",
		InferencePool:              "default/test-inferencepool",
		DurationTotal:              100,
		DurationRequestProcessing:  50,
		DurationUpstreamProcessing: 0,
		DurationResponseProcessing: 50,
	}

	// Test JSON format
	config := &AccessLoggerConfig{
		Format:  FormatJSON,
		Output:  "stdout",
		Enabled: true,
	}

	logger := &accessLoggerImpl{config: config}
	output, err := logger.formatJSON(entry)
	require.NoError(t, err)

	var parsed map[string]interface{}
	err = json.Unmarshal([]byte(output), &parsed)
	require.NoError(t, err)

	errorInfo := parsed["error"].(map[string]interface{})
	assert.Equal(t, "timeout", errorInfo["type"])
	assert.Equal(t, "Model inference timeout after 30s", errorInfo["message"])
	assert.Equal(t, "default/test-gateway", parsed["gateway"])
	assert.Equal(t, "default/test-httproute", parsed["http_route"])
	assert.Equal(t, "default/test-inferencepool", parsed["inference_pool"])

	// Test text format
	config.Format = FormatText
	output, err = logger.formatText(entry)
	require.NoError(t, err)
	assert.Contains(t, output, "error=timeout:Model inference timeout after 30s")
	assert.Contains(t, output, "gateway=default/test-gateway")
	assert.Contains(t, output, "http_route=default/test-httproute")
	assert.Contains(t, output, "inference_pool=default/test-inferencepool")
}

func TestAccessLogContext_Lifecycle(t *testing.T) {
	modelName := "test-model"
	requestID := "test-request-123"

	ctx := NewAccessLogContext(requestID, "POST", "/v1/chat/completions", "HTTP/1.1", modelName)

	// Verify initial state
	assert.Equal(t, requestID, ctx.RequestID)
	assert.Equal(t, "POST", ctx.Method)
	assert.Equal(t, "/v1/chat/completions", ctx.Path)
	assert.Equal(t, "HTTP/1.1", ctx.Protocol)
	assert.Equal(t, modelName, ctx.ModelName)
	assert.False(t, ctx.StartTime.IsZero())
	assert.False(t, ctx.RequestProcessingStart.IsZero())

	// Set model routing info
	ctx.SetModelRouting("default/test-route", "default/test-server", "test-pod-123")
	assert.Equal(t, "default/test-route", ctx.ModelRoute)
	assert.Equal(t, "default/test-server", ctx.ModelServer)
	assert.Equal(t, "test-pod-123", ctx.SelectedPod)

	// Set token counts
	ctx.SetTokenCounts(100, 50)
	assert.Equal(t, 100, ctx.InputTokens)
	assert.Equal(t, 50, ctx.OutputTokens)

	// Set error
	ctx.SetError("rate_limit", "Too many requests")
	require.NotNil(t, ctx.Error)
	assert.Equal(t, "rate_limit", ctx.Error.Type)
	assert.Equal(t, "Too many requests", ctx.Error.Message)

	// Set Gateway API info
	ctx.Gateway = "default/test-gateway"
	ctx.HTTPRoute = "default/test-httproute"
	ctx.InferencePool = "default/test-inferencepool"

	// Mark timing phases
	time.Sleep(1 * time.Millisecond) // Ensure time difference
	ctx.MarkRequestProcessingEnd()
	assert.False(t, ctx.RequestProcessingEnd.IsZero())
	assert.False(t, ctx.UpstreamStart.IsZero())

	time.Sleep(1 * time.Millisecond)
	ctx.MarkUpstreamEnd()
	assert.False(t, ctx.UpstreamEnd.IsZero())
	assert.False(t, ctx.ResponseProcessingStart.IsZero())

	time.Sleep(1 * time.Millisecond)
	ctx.MarkResponseProcessingEnd()
	assert.False(t, ctx.ResponseProcessingEnd.IsZero())

	// Convert to access log entry
	entry := ctx.ToAccessLogEntry(429)
	assert.Equal(t, 429, entry.StatusCode)
	assert.Equal(t, modelName, entry.ModelName)
	assert.Equal(t, "default/test-route", entry.ModelRoute)
	assert.Equal(t, "default/test-server", entry.ModelServer)
	assert.Equal(t, "test-pod-123", entry.SelectedPod)
	assert.Equal(t, 100, entry.InputTokens)
	assert.Equal(t, 50, entry.OutputTokens)
	assert.Greater(t, entry.DurationTotal, int64(0))
	assert.NotNil(t, entry.Error)
	assert.Equal(t, "rate_limit", entry.Error.Type)
	assert.Equal(t, "default/test-gateway", entry.Gateway)
	assert.Equal(t, "default/test-httproute", entry.HTTPRoute)
	assert.Equal(t, "default/test-inferencepool", entry.InferencePool)
}

func TestNoopAccessLogger(t *testing.T) {
	logger := &noopAccessLogger{}

	entry := &AccessLogEntry{
		Method: "POST",
		Path:   "/test",
	}

	// Should not return any errors
	err := logger.Log(entry)
	assert.NoError(t, err)

	err = logger.Close()
	assert.NoError(t, err)
}

func TestAccessLoggerConfig(t *testing.T) {
	// Test default config
	config := DefaultAccessLoggerConfig()
	assert.Equal(t, FormatJSON, config.Format)
	assert.Equal(t, "stdout", config.Output)
	assert.True(t, config.Enabled)

	// Test disabled logger
	config.Enabled = false
	logger, err := NewAccessLogger(config)
	require.NoError(t, err)
	assert.IsType(t, &noopAccessLogger{}, logger)

	// Test with nil config
	logger, err = NewAccessLogger(nil)
	require.NoError(t, err)
	assert.NotNil(t, logger)
}
