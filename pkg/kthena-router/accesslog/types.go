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
	"time"
)

// AccessLogEntry represents a structured access log entry for AI router requests
type AccessLogEntry struct {
	// Standard HTTP fields (following Envoy format)
	Timestamp  time.Time `json:"timestamp"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Protocol   string    `json:"protocol"`
	StatusCode int       `json:"status_code"`

	// Error information (if applicable)
	Error *ErrorInfo `json:"error,omitempty"`

	// AI-specific routing information
	ModelName   string `json:"model_name"`
	ModelRoute  string `json:"model_route,omitempty"`
	ModelServer string `json:"model_server,omitempty"`
	SelectedPod string `json:"selected_pod,omitempty"`
	RequestID   string `json:"request_id,omitempty"`

	// Gateway API information
	Gateway       string `json:"gateway,omitempty"`
	HTTPRoute     string `json:"http_route,omitempty"`
	InferencePool string `json:"inference_pool,omitempty"`

	// Token information
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// Timing breakdown (in milliseconds) - flattened fields
	DurationTotal              int64 `json:"duration_total"`
	DurationRequestProcessing  int64 `json:"duration_request_processing"`
	DurationUpstreamProcessing int64 `json:"duration_upstream_processing"`
	DurationResponseProcessing int64 `json:"duration_response_processing"`
}

// ErrorInfo contains error details for failed requests
type ErrorInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// AccessLogContext tracks timing and metadata throughout request lifecycle
type AccessLogContext struct {
	// Request metadata
	RequestID   string
	StartTime   time.Time
	Method      string
	Path        string
	Protocol    string
	ModelName   string
	ModelRoute  string
	ModelServer string
	SelectedPod string

	// Gateway API information
	Gateway       string
	HTTPRoute     string
	InferencePool string

	// Token counts
	InputTokens  int
	OutputTokens int

	// Timing checkpoints
	RequestProcessingStart  time.Time
	RequestProcessingEnd    time.Time
	UpstreamStart           time.Time
	UpstreamEnd             time.Time
	ResponseProcessingStart time.Time
	ResponseProcessingEnd   time.Time

	// Error tracking
	Error *ErrorInfo

	// Final status
	StatusCode int
}

// NewAccessLogContext creates a new context for tracking request lifecycle
func NewAccessLogContext(requestID, method, path, protocol, modelName string) *AccessLogContext {
	now := time.Now()
	return &AccessLogContext{
		RequestID:              requestID,
		StartTime:              now,
		Method:                 method,
		Path:                   path,
		Protocol:               protocol,
		ModelName:              modelName,
		RequestProcessingStart: now,
	}
}

// SetModelRouting sets the model routing information
func (ctx *AccessLogContext) SetModelRouting(modelRoute string, modelServer string, selectedPod string) {
	ctx.ModelRoute = modelRoute
	ctx.ModelServer = modelServer
	ctx.SelectedPod = selectedPod
}

// SetTokenCounts sets the input and output token counts
func (ctx *AccessLogContext) SetTokenCounts(inputTokens, outputTokens int) {
	ctx.InputTokens = inputTokens
	ctx.OutputTokens = outputTokens
}

// SetError sets error information
func (ctx *AccessLogContext) SetError(errorType, message string) {
	ctx.Error = &ErrorInfo{
		Type:    errorType,
		Message: message,
	}
}

// MarkRequestProcessingEnd marks the end of request processing phase
func (ctx *AccessLogContext) MarkRequestProcessingEnd() {
	ctx.RequestProcessingEnd = time.Now()
	ctx.UpstreamStart = ctx.RequestProcessingEnd
}

// MarkUpstreamStart marks the start of upstream processing
func (ctx *AccessLogContext) MarkUpstreamStart() {
	ctx.UpstreamStart = time.Now()
}

// MarkUpstreamEnd marks the end of upstream processing
func (ctx *AccessLogContext) MarkUpstreamEnd() {
	ctx.UpstreamEnd = time.Now()
	ctx.ResponseProcessingStart = ctx.UpstreamEnd
}

// MarkResponseProcessingEnd marks the end of response processing
func (ctx *AccessLogContext) MarkResponseProcessingEnd() {
	ctx.ResponseProcessingEnd = time.Now()
}

// ToAccessLogEntry converts the context to an access log entry
func (ctx *AccessLogContext) ToAccessLogEntry(statusCode int) *AccessLogEntry {
	ctx.StatusCode = statusCode

	// Calculate durations
	endTime := time.Now()
	if ctx.ResponseProcessingEnd.IsZero() {
		ctx.ResponseProcessingEnd = endTime
	}
	if ctx.UpstreamEnd.IsZero() {
		ctx.UpstreamEnd = ctx.ResponseProcessingEnd
	}
	if ctx.RequestProcessingEnd.IsZero() {
		ctx.RequestProcessingEnd = ctx.UpstreamEnd
	}
	if ctx.UpstreamStart.IsZero() {
		ctx.UpstreamStart = ctx.RequestProcessingEnd
	}
	if ctx.ResponseProcessingStart.IsZero() {
		ctx.ResponseProcessingStart = ctx.UpstreamEnd
	}

	total := endTime.Sub(ctx.StartTime).Milliseconds()
	requestProcessing := ctx.RequestProcessingEnd.Sub(ctx.RequestProcessingStart).Milliseconds()
	upstreamProcessing := ctx.UpstreamEnd.Sub(ctx.UpstreamStart).Milliseconds()
	responseProcessing := ctx.ResponseProcessingEnd.Sub(ctx.ResponseProcessingStart).Milliseconds()

	// Model server name is already in namespace/name format
	modelServerName := ctx.ModelServer

	entry := &AccessLogEntry{
		Timestamp:                  ctx.StartTime,
		Method:                     ctx.Method,
		Path:                       ctx.Path,
		Protocol:                   ctx.Protocol,
		StatusCode:                 statusCode,
		Error:                      ctx.Error,
		ModelName:                  ctx.ModelName,
		ModelRoute:                 ctx.ModelRoute,
		ModelServer:                modelServerName,
		SelectedPod:                ctx.SelectedPod,
		RequestID:                  ctx.RequestID,
		InputTokens:                ctx.InputTokens,
		OutputTokens:               ctx.OutputTokens,
		Gateway:                    ctx.Gateway,
		HTTPRoute:                  ctx.HTTPRoute,
		InferencePool:              ctx.InferencePool,
		DurationTotal:              total,
		DurationRequestProcessing:  requestProcessing,
		DurationUpstreamProcessing: upstreamProcessing,
		DurationResponseProcessing: responseProcessing,
	}

	return entry
}
