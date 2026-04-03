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

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	// Label names
	LabelModel       = "model"
	LabelPath        = "path"
	LabelStatusCode  = "status_code"
	LabelErrorType   = "error_type"
	LabelTokenType   = "token_type"
	LabelPlugin      = "plugin"
	LabelType        = "type"
	LabelLimitType   = "limit_type"
	LabelModelRoute  = "model_route"
	LabelModelServer = "model_server"
	LabelUserID      = "user_id"

	// Token type values
	TokenTypeInput  = "input"
	TokenTypeOutput = "output"

	// Plugin type values
	PluginTypeFilter = "filter"
	PluginTypeScore  = "score"

	// Limit type values
	LimitTypeInputTokens  = "input_tokens"
	LimitTypeOutputTokens = "output_tokens"
	LimitTypeRequests     = "requests"
)

// Metrics holds all Prometheus metrics for the kthena-router
type Metrics struct {
	// Request counters
	RequestsTotal prometheus.CounterVec

	// Request duration histograms
	RequestDuration        prometheus.HistogramVec
	RequestPrefillDuration prometheus.HistogramVec
	RequestDecodeDuration  prometheus.HistogramVec

	// Token metrics
	TokensTotal prometheus.CounterVec

	// Scheduler plugin duration metrics
	SchedulerPluginDuration prometheus.HistogramVec

	// Rate limiting metrics
	RateLimitExceeded prometheus.CounterVec

	// Request and scheduling metrics
	ActiveDownstreamRequests prometheus.GaugeVec
	ActiveUpstreamRequests   prometheus.GaugeVec
	FairnessQueueSize        prometheus.GaugeVec
	FairnessQueueDuration    prometheus.HistogramVec

	// Fairness queue detailed metrics
	FairnessQueueCancelledTotal       prometheus.CounterVec
	FairnessQueueDequeueTotal         prometheus.CounterVec
	FairnessQueueInflight             prometheus.GaugeVec
	FairnessQueuePriorityRefreshTotal prometheus.CounterVec
	FairnessQueueHeapRebuildTotal     prometheus.CounterVec
}

// NewMetrics creates a new Metrics instance with all Prometheus metrics registered
func NewMetrics() *Metrics {
	return &Metrics{
		RequestsTotal: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_requests_total",
				Help: "Total number of HTTP requests processed by the router",
			},
			[]string{LabelModel, LabelPath, LabelStatusCode, LabelErrorType},
		),

		RequestDuration: *promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kthena_router_request_duration_seconds",
				Help:    "End-to-end request processing latency distribution for all requests",
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
			},
			[]string{LabelModel, LabelPath, LabelStatusCode},
		),

		RequestPrefillDuration: *promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kthena_router_request_prefill_duration_seconds",
				Help:    "Prefill phase processing latency distribution for PD-disaggregated requests",
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
			},
			[]string{LabelModel, LabelPath, LabelStatusCode},
		),

		RequestDecodeDuration: *promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kthena_router_request_decode_duration_seconds",
				Help:    "Decode phase processing latency distribution for PD-disaggregated requests",
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
			},
			[]string{LabelModel, LabelPath, LabelStatusCode},
		),

		TokensTotal: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_tokens_total",
				Help: "Total tokens processed/generated",
			},
			[]string{LabelModel, LabelPath, LabelTokenType},
		),

		SchedulerPluginDuration: *promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kthena_router_scheduler_plugin_duration_seconds",
				Help:    "Processing time per scheduler plugin",
				Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
			},
			[]string{LabelModel, LabelPlugin, LabelType},
		),

		RateLimitExceeded: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_rate_limit_exceeded_total",
				Help: "Number of requests rejected due to rate limiting",
			},
			[]string{LabelModel, LabelLimitType, LabelPath},
		),

		ActiveDownstreamRequests: *promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kthena_router_active_downstream_requests",
				Help: "Current number of active downstream requests (from clients to router)",
			},
			[]string{LabelModel},
		),

		ActiveUpstreamRequests: *promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kthena_router_active_upstream_requests",
				Help: "Current number of active upstream requests (from router to backend pods)",
			},
			[]string{LabelModelServer, LabelModelRoute},
		),

		FairnessQueueSize: *promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kthena_router_fairness_queue_size",
				Help: "Current fairness queue size for pending requests",
			},
			[]string{LabelModel, LabelUserID},
		),

		FairnessQueueDuration: *promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kthena_router_fairness_queue_duration_seconds",
				Help:    "Time requests spend in fairness queue before processing",
				Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			},
			[]string{LabelModel, LabelUserID},
		),

		FairnessQueueCancelledTotal: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_fairness_queue_cancelled_total",
				Help: "Total number of requests cancelled or timed out while in fairness queue",
			},
			[]string{LabelModel, LabelUserID},
		),

		FairnessQueueDequeueTotal: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_fairness_queue_dequeue_total",
				Help: "Total number of requests successfully dequeued from fairness queue",
			},
			[]string{LabelModel, LabelUserID},
		),

		FairnessQueueInflight: *promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kthena_router_fairness_queue_inflight",
				Help: "Current number of in-flight requests gated by fairness queue semaphore",
			},
			[]string{LabelModel},
		),

		FairnessQueuePriorityRefreshTotal: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_fairness_queue_priority_refresh_total",
				Help: "Total number of dequeue-time priority refresh-and-reinsert operations",
			},
			[]string{LabelModel},
		),

		FairnessQueueHeapRebuildTotal: *promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kthena_router_fairness_queue_heap_rebuild_total",
				Help: "Total number of full heap rebuild operations due to priority drift",
			},
			[]string{LabelModel},
		),
	}
}

// RecordRequest records a completed request with all relevant metrics
func (m *Metrics) RecordRequest(model, path, statusCode, errorType string, duration time.Duration) {
	m.RequestsTotal.WithLabelValues(model, path, statusCode, errorType).Inc()
	m.RequestDuration.WithLabelValues(model, path, statusCode).Observe(duration.Seconds())
}

// RecordPrefillDuration records prefill phase duration for PD-disaggregated requests
func (m *Metrics) RecordPrefillDuration(model, path, statusCode string, duration time.Duration) {
	m.RequestPrefillDuration.WithLabelValues(model, path, statusCode).Observe(duration.Seconds())
}

// RecordDecodeDuration records decode phase duration for PD-disaggregated requests
func (m *Metrics) RecordDecodeDuration(model, path, statusCode string, duration time.Duration) {
	m.RequestDecodeDuration.WithLabelValues(model, path, statusCode).Observe(duration.Seconds())
}

// RecordTokens records input and output token counts
func (m *Metrics) RecordTokens(model, path string, inputTokens, outputTokens int) {
	if inputTokens > 0 {
		m.TokensTotal.WithLabelValues(model, path, TokenTypeInput).Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		m.TokensTotal.WithLabelValues(model, path, TokenTypeOutput).Add(float64(outputTokens))
	}
}

// RecordRateLimitExceeded records when a request is rejected due to rate limiting
func (m *Metrics) RecordRateLimitExceeded(model, limitType, path string) {
	m.RateLimitExceeded.WithLabelValues(model, limitType, path).Inc()
}

// RecordSchedulerPluginDuration records the processing time for a specific scheduler plugin
func (m *Metrics) RecordSchedulerPluginDuration(model, pluginName, pluginType string, duration time.Duration) {
	m.SchedulerPluginDuration.WithLabelValues(model, pluginName, pluginType).Observe(duration.Seconds())
}

// SetActiveDownstreamRequests sets the current number of active downstream requests
func (m *Metrics) SetActiveDownstreamRequests(model string, count float64) {
	m.ActiveDownstreamRequests.WithLabelValues(model).Set(count)
}

// SetActiveUpstreamRequests sets the current number of active upstream requests
func (m *Metrics) SetActiveUpstreamRequests(modelServer, modelRoute string, count float64) {
	m.ActiveUpstreamRequests.WithLabelValues(modelServer, modelRoute).Set(count)
}

// IncActiveDownstreamRequests increments the active downstream requests counter
func (m *Metrics) IncActiveDownstreamRequests(model string) {
	m.ActiveDownstreamRequests.WithLabelValues(model).Inc()
}

// DecActiveDownstreamRequests decrements the active downstream requests counter
func (m *Metrics) DecActiveDownstreamRequests(model string) {
	m.ActiveDownstreamRequests.WithLabelValues(model).Dec()
}

// IncActiveUpstreamRequests increments the active upstream requests counter
func (m *Metrics) IncActiveUpstreamRequests(modelServer, modelRoute string) {
	m.ActiveUpstreamRequests.WithLabelValues(modelServer, modelRoute).Inc()
}

// DecActiveUpstreamRequests decrements the active upstream requests counter
func (m *Metrics) DecActiveUpstreamRequests(modelServer, modelRoute string) {
	m.ActiveUpstreamRequests.WithLabelValues(modelServer, modelRoute).Dec()
}

// IncFairnessQueueSize increments the fairness queue size
func (m *Metrics) IncFairnessQueueSize(model, userID string) {
	m.FairnessQueueSize.WithLabelValues(model, userID).Inc()
}

// DecFairnessQueueSize decrements the fairness queue size
func (m *Metrics) DecFairnessQueueSize(model, userID string) {
	m.FairnessQueueSize.WithLabelValues(model, userID).Dec()
}

// SetFairnessQueueSize sets the current fairness queue size
func (m *Metrics) SetFairnessQueueSize(model, userID string, size float64) {
	m.FairnessQueueSize.WithLabelValues(model, userID).Set(size)
}

// RecordFairnessQueueDuration records the time a request spent in fairness queue
func (m *Metrics) RecordFairnessQueueDuration(model, userID string, duration time.Duration) {
	m.FairnessQueueDuration.WithLabelValues(model, userID).Observe(duration.Seconds())
}

// IncFairnessQueueCancelled increments the fairness queue cancelled counter
func (m *Metrics) IncFairnessQueueCancelled(model, userID string) {
	m.FairnessQueueCancelledTotal.WithLabelValues(model, userID).Inc()
}

// IncFairnessQueueDequeue increments the fairness queue dequeue counter
func (m *Metrics) IncFairnessQueueDequeue(model, userID string) {
	m.FairnessQueueDequeueTotal.WithLabelValues(model, userID).Inc()
}

// IncFairnessQueueInflight increments the fairness queue inflight gauge
func (m *Metrics) IncFairnessQueueInflight(model string) {
	m.FairnessQueueInflight.WithLabelValues(model).Inc()
}

// DecFairnessQueueInflight decrements the fairness queue inflight gauge
func (m *Metrics) DecFairnessQueueInflight(model string) {
	m.FairnessQueueInflight.WithLabelValues(model).Dec()
}

// IncFairnessQueuePriorityRefresh increments the priority refresh counter
func (m *Metrics) IncFairnessQueuePriorityRefresh(model string) {
	m.FairnessQueuePriorityRefreshTotal.WithLabelValues(model).Inc()
}

// IncFairnessQueueHeapRebuild increments the heap rebuild counter
func (m *Metrics) IncFairnessQueueHeapRebuild(model string) {
	m.FairnessQueueHeapRebuildTotal.WithLabelValues(model).Inc()
}

// RequestMetricsRecorder is a helper struct to record detailed metrics for individual requests
type RequestMetricsRecorder struct {
	metrics          *Metrics
	model            string
	path             string
	modelServer      string
	modelRoute       string
	startTime        time.Time
	prefillStartTime *time.Time
	decodeStartTime  *time.Time
}

// NewRequestMetricsRecorder creates a new recorder for a specific request
func NewRequestMetricsRecorder(metrics *Metrics, model, path string) *RequestMetricsRecorder {
	return &RequestMetricsRecorder{
		metrics:   metrics,
		model:     model,
		path:      path,
		startTime: time.Now(),
	}
}

// SetUpstreamConnectionInfo sets the upstream connection information for this request
func (r *RequestMetricsRecorder) SetUpstreamConnectionInfo(modelServer, modelRoute string) {
	r.modelServer = modelServer
	r.modelRoute = modelRoute
}

// RecordInputTokens records input token usage for this request
func (r *RequestMetricsRecorder) RecordInputTokens(tokens int) {
	if tokens > 0 {
		r.metrics.TokensTotal.WithLabelValues(r.model, r.path, TokenTypeInput).Add(float64(tokens))
	}
}

// RecordOutputTokens records output token usage for this request
func (r *RequestMetricsRecorder) RecordOutputTokens(tokens int) {
	if tokens > 0 {
		r.metrics.TokensTotal.WithLabelValues(r.model, r.path, TokenTypeOutput).Add(float64(tokens))
	}
}

// RecordRateLimitExceeded records when rate limiting is applied
func (r *RequestMetricsRecorder) RecordRateLimitExceeded(limitType string) {
	r.metrics.RecordRateLimitExceeded(r.model, limitType, r.path)
}

// StartPrefillPhase marks the start of prefill phase for PD-disaggregated requests
func (r *RequestMetricsRecorder) StartPrefillPhase() {
	now := time.Now()
	r.prefillStartTime = &now
}

// FinishPrefillPhase marks the end of prefill phase and records duration
func (r *RequestMetricsRecorder) FinishPrefillPhase(statusCode string) {
	if r.prefillStartTime != nil {
		duration := time.Since(*r.prefillStartTime)
		r.metrics.RecordPrefillDuration(r.model, r.path, statusCode, duration)
	}
}

// StartDecodePhase marks the start of decode phase for PD-disaggregated requests
func (r *RequestMetricsRecorder) StartDecodePhase() {
	now := time.Now()
	r.decodeStartTime = &now
}

// FinishDecodePhase marks the end of decode phase and records duration
func (r *RequestMetricsRecorder) FinishDecodePhase(statusCode string) {
	if r.decodeStartTime != nil {
		duration := time.Since(*r.decodeStartTime)
		r.metrics.RecordDecodeDuration(r.model, r.path, statusCode, duration)
	}
}

// Finish completes the request recording with final status
func (r *RequestMetricsRecorder) Finish(statusCode, errorType string) {
	duration := time.Since(r.startTime)
	r.metrics.RecordRequest(r.model, r.path, statusCode, errorType, duration)
}

// RecordSchedulerPluginDuration records the execution time for a scheduler plugin
func (r *RequestMetricsRecorder) RecordSchedulerPluginDuration(pluginName, pluginType string, duration time.Duration) {
	r.metrics.RecordSchedulerPluginDuration(r.model, pluginName, pluginType, duration)
}

// RecordFairnessQueueDuration records the time spent in fairness queue
func (r *RequestMetricsRecorder) RecordFairnessQueueDuration(userID string, duration time.Duration) {
	r.metrics.RecordFairnessQueueDuration(r.model, userID, duration)
}

// IncActiveUpstreamRequests increments the active upstream requests counter for this request
func (r *RequestMetricsRecorder) IncActiveUpstreamRequests() {
	if r.modelServer != "" && r.modelRoute != "" {
		r.metrics.IncActiveUpstreamRequests(r.modelServer, r.modelRoute)
	}
}

// DecActiveUpstreamRequests decrements the active upstream requests counter for this request
func (r *RequestMetricsRecorder) DecActiveUpstreamRequests() {
	if r.modelServer != "" && r.modelRoute != "" {
		r.metrics.DecActiveUpstreamRequests(r.modelServer, r.modelRoute)
	}
}

// Global metrics instance
var DefaultMetrics = NewMetrics()
