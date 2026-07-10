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

package datastore

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// FairnessQueueConfig holds configurable parameters for the fairness queue.
type FairnessQueueConfig struct {
	// MaxConcurrent is the maximum number of in-flight requests allowed for this
	// model in user-fairness mode. It is a global (total) limit, not per-pod:
	// total concurrent admissions through the fairness gate. When 0, falls back to
	// MaxQPS-based rate limiting. It is not used in session-boost mode.
	MaxConcurrent int

	// MaxQPS is the upper-bound dequeue rate used only in ticker/QPS mode.
	MaxQPS int

	// MaxPriorityRefreshRetries bounds refresh-and-reinsert loops before a heap rebuild.
	// 0 disables dequeue-time refresh (current behavior).
	MaxPriorityRefreshRetries int

	// RebuildThreshold controls when to refresh all queued priorities and rebuild the heap.
	RebuildThreshold int

	// TokenWeight is the token-usage weight in the composite priority score.
	TokenWeight float64

	// RequestNumWeight is the request-count weight in the composite priority score.
	RequestNumWeight float64

	// SessionBoostEnabled switches the queue from user-based fairness ordering to
	// session-boost ordering. When true, per-user fairness scheduling is disabled
	// and request priority is derived from recent session completions: requests
	// belonging to a recently completed session are promoted ahead of others to
	// maximize prefix-cache reuse on the inference backends.
	SessionBoostEnabled bool

	// SessionIDHeader is the HTTP header name used to identify conversation
	// sessions. Only meaningful when SessionBoostEnabled is true. Defaults to
	// defaultSessionBoostHeader ("X-Session-ID") when not set.
	SessionIDHeader string

	// SessionBoostMaxSessions is the maximum number of recently-completed sessions
	// the queue remembers for boosting. It bounds an LRU cache: when the limit is
	// exceeded, the least-recently-used session is evicted. Sizing this by the
	// number of conversations to keep "warm" is more intuitive than a time-based
	// TTL and mirrors how inference engines evict their prefix cache. When <= 0, a
	// default of defaultSessionBoostMaxSessions is used.
	SessionBoostMaxSessions int

	// SessionBoostGracePeriod is the duration to wait after a release before
	// dequeuing the next request in backpressure mode. This gives the same session
	// time to submit a follow-up request that benefits from prefix cache.
	// 0 disables the grace period.
	SessionBoostGracePeriod time.Duration

	// InflightPerPod is the maximum number of inflight requests allowed per backend
	// pod in session-boost mode. The total inflight limit is InflightPerPod times
	// the number of backend pods serving the model. When <= 0, a default of
	// defaultSessionBoostInflightPerPod is used. Only meaningful when
	// SessionBoostEnabled is true.
	InflightPerPod int
}

// defaultSessionBoostInflightPerPod is the per-pod inflight limit used in
// session-boost mode when InflightPerPod is not set (<= 0).
const defaultSessionBoostInflightPerPod = 16

// defaultSessionBoostMaxSessions is the LRU capacity (number of recently-completed
// sessions remembered for boosting) used when SessionBoostMaxSessions is not set
// (<= 0). Each entry is tiny (a session ID), so the default is generous.
const defaultSessionBoostMaxSessions = 4096

// defaultSessionBoostHeader is the HTTP header used to identify conversation
// sessions when SESSION_BOOST_HEADER is not set.
const defaultSessionBoostHeader = "X-Session-ID"

// DefaultFairnessQueueConfig returns backward-compatible defaults.
func DefaultFairnessQueueConfig() FairnessQueueConfig {
	return FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 0,
		RebuildThreshold:          64,
		TokenWeight:               1.0,
		RequestNumWeight:          0.0,
		SessionBoostEnabled:       false,
		SessionIDHeader:           defaultSessionBoostHeader,
		SessionBoostMaxSessions:   defaultSessionBoostMaxSessions,
		SessionBoostGracePeriod:   0,
		InflightPerPod:            defaultSessionBoostInflightPerPod,
	}
}

type FairnessPrioritySource interface {
	GetTokenCount(user, model string) (float64, error)
	GetRequestCount(user, model string) (int, error)
}

func CalculateFairnessPriority(source FairnessPrioritySource, userID, modelName string, tokenWeight, requestNumWeight float64) (float64, error) {
	tokenCount, err := source.GetTokenCount(userID, modelName)
	if err != nil {
		return 0, err
	}

	priority := tokenWeight * tokenCount
	if requestNumWeight == 0 {
		return priority, nil
	}

	requestCount, err := source.GetRequestCount(userID, modelName)
	if err != nil {
		return 0, err
	}

	return priority + requestNumWeight*float64(requestCount), nil
}

// Request represents a request item in the priority queue
type Request struct {
	UserID       string  // User ID for fairness scheduling
	ModelName    string  // Target model for per-model fair queuing
	SessionID    string  // Session identifier for multi-turn conversations
	Priority     float64 // Priority (lower value means higher priority)
	SessionBoost bool    // Whether this request has session priority boost (recently completed session)
	// LastTurnCompletedAt is the time the session's previous turn completed, captured
	// when the request is boosted. Boosted requests are ordered by this timestamp
	// (most recent first) so the session with the warmest prefix cache runs first.
	LastTurnCompletedAt time.Time
	RequestTime         time.Time
	NotifyChan          chan struct{}
	CancelCh            <-chan struct{} // Request-scoped cancellation signal
	Release             func()          // Set by the queue when a permit is acquired

	// admitMu serializes admission (by the dequeue loop) against abandonment (by
	// the waiting caller on timeout/cancel), closing the race where the loop has
	// popped the request and passed its cancellation check but has not yet
	// installed Release / closed NotifyChan. Guards admitted and abandoned.
	admitMu   sync.Mutex
	admitted  bool // admission committed: Release is set and about to be signalled
	abandoned bool // caller gave up before admission; the loop must not admit
}

// commitAdmission runs fn under the request lock, but only if the caller has not
// already abandoned the request. fn performs the admission side effects that must
// be fully visible before the request is considered admitted: acquiring the
// inflight permit, installing Release, and incrementing the inflight metric.
// It returns true if admission was committed. When it returns false the request
// was abandoned first, so the dequeue loop must not mark it inflight or signal it.
//
// Because fn (including installing Release) completes before admitted is set, any
// caller that observes admitted via abandon() is guaranteed to see a non-nil
// Release, so the permit can always be returned.
func (r *Request) commitAdmission(fn func()) bool {
	r.admitMu.Lock()
	defer r.admitMu.Unlock()
	if r.abandoned {
		return false
	}
	fn()
	r.admitted = true
	return true
}

// Abandon marks the request as given up by the waiting caller (queue timeout,
// wait-reject, or client cancellation). If the request had already been admitted,
// the caller owned the inflight permit, so Abandon releases it to avoid leaking
// capacity. Otherwise it marks the request abandoned so admission is guaranteed
// not to proceed (commitAdmission observes abandoned and skips) and no permit can
// leak.
func (r *Request) Abandon() {
	r.admitMu.Lock()
	if !r.admitted {
		r.abandoned = true
		r.admitMu.Unlock()
		return
	}
	r.admitMu.Unlock()
	// Admission raced in first, so we own the inflight permit; release it here.
	// Release is guaranteed non-nil once admitted is set (installed by fn before
	// commitAdmission sets admitted).
	r.Release()
}

// RequestPriorityQueue implements the heap.Interface
type RequestPriorityQueue struct {
	stopCh   chan struct{}    // Context for cancellation
	notifyCh chan struct{}    // Channel for item availability notification
	mu       sync.RWMutex     // Ensure concurrent safety with read/write locks
	heap     []*Request       // Underlying storage structure
	metrics  *metrics.Metrics // Metrics instance for recording queue stats

	// Backpressure-aware dequeue (Phase 2)
	sem    chan struct{} // Semaphore for capacity-based admission; nil means QPS mode
	config FairnessQueueConfig

	// Priority refresh (Phase 2)
	tokenTracker TokenTracker // Optional; when set, enables dequeue-time priority refresh

	// Session-boost mode (enabled via FairnessQueueConfig.SessionBoostEnabled).
	// When sessionBoost is true the queue orders requests by session boost instead
	// of per-user fairness, and dequeues using backend backpressure.
	sessionBoost   bool
	sessionTracker *SessionTracker       // Tracks recently completed sessions for boosting
	backendChecker BackendWaitingChecker // Gates dequeue on backend capacity in session-boost mode
	podCounter     PodCounter            // Optional; counts backend pods for inflight scaling
	inflightCount  atomic.Int64          // In-flight requests in session-boost mode
	releaseCh      chan struct{}         // Signals a permit release in session-boost mode
}

var _ heap.Interface = &RequestPriorityQueue{}

// NewRequestPriorityQueue creates a new priority queue. Pass nil metrics to use defaults.
func NewRequestPriorityQueue(metricsInstance *metrics.Metrics) *RequestPriorityQueue {
	return NewRequestPriorityQueueWithConfig(metricsInstance, DefaultFairnessQueueConfig(), nil, nil)
}

// NewRequestPriorityQueueWithConfig creates a priority queue with explicit configuration.
// When cfg.SessionBoostEnabled is true the queue operates in session-boost mode, where
// the BackendWaitingChecker gates dequeue on backend capacity.
func NewRequestPriorityQueueWithConfig(metricsInstance *metrics.Metrics, cfg FairnessQueueConfig, tracker TokenTracker, checker BackendWaitingChecker) *RequestPriorityQueue {
	if metricsInstance == nil {
		metricsInstance = metrics.DefaultMetrics
	}
	if cfg.TokenWeight == 0 && cfg.RequestNumWeight == 0 {
		cfg.TokenWeight = DefaultFairnessQueueConfig().TokenWeight
	}
	pq := &RequestPriorityQueue{
		stopCh:       make(chan struct{}),
		notifyCh:     make(chan struct{}, 1), // Buffered to prevent blocking
		heap:         make([]*Request, 0),
		metrics:      metricsInstance,
		config:       cfg,
		tokenTracker: tracker,
	}
	if cfg.SessionBoostEnabled {
		pq.sessionBoost = true
		maxSessions := cfg.SessionBoostMaxSessions
		if maxSessions <= 0 {
			maxSessions = defaultSessionBoostMaxSessions
		}
		pq.sessionTracker = NewSessionTracker(maxSessions)
		pq.releaseCh = make(chan struct{}, 1)
		pq.backendChecker = checker
	} else if cfg.MaxConcurrent > 0 {
		pq.sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return pq
}

// Implement heap.Interface methods
func (pq *RequestPriorityQueue) Len() int { return len(pq.heap) }

func (pq *RequestPriorityQueue) Less(i, j int) bool {
	// Session-boost mode: boosted requests outrank others. Among boosted requests,
	// the session whose previous turn completed most recently wins, because its
	// prefix cache is the most likely to still be warm on the backend; ties are
	// broken FIFO by arrival time. Non-boosted requests keep FIFO ordering.
	if pq.sessionBoost {
		if pq.heap[i].SessionBoost != pq.heap[j].SessionBoost {
			return pq.heap[i].SessionBoost
		}
		if pq.heap[i].SessionBoost {
			if !pq.heap[i].LastTurnCompletedAt.Equal(pq.heap[j].LastTurnCompletedAt) {
				return pq.heap[i].LastTurnCompletedAt.After(pq.heap[j].LastTurnCompletedAt)
			}
			return pq.heap[i].RequestTime.Before(pq.heap[j].RequestTime)
		}
		return pq.heap[i].RequestTime.Before(pq.heap[j].RequestTime)
	}
	// same user, FIFO
	if pq.heap[i].UserID == pq.heap[j].UserID {
		return pq.heap[i].RequestTime.Before(pq.heap[j].RequestTime)
	}
	// different users, compare priority, actually token usage here
	if pq.heap[i].Priority != pq.heap[j].Priority {
		return pq.heap[i].Priority < pq.heap[j].Priority
	}
	// When priorities are equal, compare request arrival times: earlier times have higher priority
	return pq.heap[i].RequestTime.Before(pq.heap[j].RequestTime)
}

func (pq *RequestPriorityQueue) Swap(i, j int) {
	pq.heap[i], pq.heap[j] = pq.heap[j], pq.heap[i]
}

func (pq *RequestPriorityQueue) Push(x interface{}) {
	item := x.(*Request)
	pq.heap = append(pq.heap, item)
}

func (pq *RequestPriorityQueue) Pop() interface{} {
	n := len(pq.heap)
	if n == 0 {
		return nil
	}
	item := pq.heap[n-1]
	pq.heap[n-1] = nil
	pq.heap = pq.heap[0 : n-1]
	return item
}

func (pq *RequestPriorityQueue) PushRequest(r *Request) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// In session-boost mode, promote requests whose session recently completed.
	// Capture the session's completion time so boosted requests can be ordered by
	// prefix-cache warmth (most recently completed first).
	if pq.sessionBoost && pq.sessionTracker != nil && r.SessionID != "" {
		if completedAt, ok := pq.sessionTracker.CompletionTime(r.SessionID); ok {
			r.SessionBoost = true
			r.LastTurnCompletedAt = completedAt
		}
	}

	heap.Push(pq, r)

	// Update queue size metrics
	pq.metricIncSize(r.ModelName, r.UserID)

	if r.SessionBoost {
		klog.V(4).Infof("[SessionBoost] session boost: sessionID=%s promoted, queueLen=%d",
			r.SessionID, len(pq.heap))
	}

	// Signal that a new item is available
	select {
	case pq.notifyCh <- struct{}{}:
	default: // Channel is full, notification already pending
	}
	return nil
}

// popWhenAvailable blocks until an item is available or the context is done, then pops one item.
// Cancelled/timed-out requests are skipped automatically.
func (pq *RequestPriorityQueue) popWhenAvailable(ctx context.Context) (*Request, error) {
	refreshRetries := 0
	for {
		pq.mu.Lock()
		if len(pq.heap) > 0 {
			req := heap.Pop(pq).(*Request)

			// Skip cancelled/timed-out requests
			if req.isCancelled() {
				pq.metricDecSize(req.ModelName, req.UserID)
				pq.metricRecordDuration(req.ModelName, req.UserID, time.Since(req.RequestTime))
				pq.metricIncCancelled(req.ModelName, req.UserID)
				pq.mu.Unlock()
				continue
			}

			// Bounded priority refresh: re-evaluate root priority at dequeue time
			if pq.tokenTracker != nil && pq.config.MaxPriorityRefreshRetries > 0 {
				newPri, err := CalculateFairnessPriority(
					pq.tokenTracker,
					req.UserID,
					req.ModelName,
					pq.config.TokenWeight,
					pq.config.RequestNumWeight,
				)
				if err == nil && newPri != req.Priority {
					req.Priority = newPri
					// Check if this request should still be dequeued
					if len(pq.heap) > 0 && newPri > pq.heap[0].Priority {
						refreshRetries++
						if pq.metrics != nil {
							pq.metrics.IncFairnessQueuePriorityRefresh(req.ModelName)
						}
						if refreshRetries >= pq.config.MaxPriorityRefreshRetries {
							heap.Push(pq, req)
							if pq.shouldRebuildLocked() {
								pq.rebuildHeap()
								if pq.metrics != nil {
									pq.metrics.IncFairnessQueueHeapRebuild(req.ModelName)
								}
							}
							refreshRetries = 0
							pq.mu.Unlock()
							continue
						}
						// Reinsert with updated priority and retry
						heap.Push(pq, req)
						pq.mu.Unlock()
						continue
					}
				}
			}

			// Update fairness queue size metrics and record queue duration
			pq.metricDecSize(req.ModelName, req.UserID)
			pq.metricRecordDuration(req.ModelName, req.UserID, time.Since(req.RequestTime))
			pq.metricIncDequeue(req.ModelName, req.UserID)

			pq.mu.Unlock()
			return req, nil
		}
		pq.mu.Unlock()

		// Wait for notification or cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pq.stopCh:
			return nil, errors.New("queue stopped")
		case <-pq.notifyCh:
			// An item might be available, loop back to check
			continue
		}
	}
}

func (r *Request) isCancelled() bool {
	if r == nil || r.CancelCh == nil {
		return false
	}
	select {
	case <-r.CancelCh:
		return true
	default:
		return false
	}
}

func (pq *RequestPriorityQueue) shouldRebuildLocked() bool {
	return pq.config.RebuildThreshold <= 0 || len(pq.heap) <= pq.config.RebuildThreshold
}

// rebuildHeap refreshes priorities for all queued items and rebuilds the heap.
// Caller must hold pq.mu.
func (pq *RequestPriorityQueue) rebuildHeap() {
	if pq.tokenTracker == nil {
		return
	}
	for _, req := range pq.heap {
		if newPri, err := CalculateFairnessPriority(
			pq.tokenTracker,
			req.UserID,
			req.ModelName,
			pq.config.TokenWeight,
			pq.config.RequestNumWeight,
		); err == nil {
			req.Priority = newPri
		}
	}
	heap.Init(pq)
}

func (pq *RequestPriorityQueue) requeueRequest(req *Request) {
	if req == nil {
		return
	}
	pq.mu.Lock()
	heap.Push(pq, req)
	pq.mu.Unlock()
	select {
	case pq.notifyCh <- struct{}{}:
	default:
	}
}

// Run starts the dequeue loop. In session-boost mode, dequeue is gated by backend
// backpressure and a per-pod inflight limit. Otherwise, in semaphore mode
// (MaxConcurrent > 0), dequeue is gated by available capacity, and as a final
// fallback it uses QPS-based ticker dequeue.
func (pq *RequestPriorityQueue) Run(ctx context.Context, qps int) {
	if pq.sessionBoost {
		pq.runSessionBoostMode(ctx)
		return
	}
	if pq.sem != nil {
		pq.runSemaphoreMode(ctx)
		return
	}
	pq.runQPSMode(ctx, qps)
}

// runQPSMode is the original fixed-rate ticker dequeue loop (backward-compatible).
func (pq *RequestPriorityQueue) runQPSMode(ctx context.Context, qps int) {
	if qps <= 0 {
		qps = 1
	}
	interval := time.Second / time.Duration(qps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pq.stopCh:
			return
		case <-ticker.C:
			req, err := pq.popWhenAvailable(ctx)
			if err != nil {
				return
			}
			if req != nil && req.NotifyChan != nil {
				close(req.NotifyChan)
			}
		}
	}
}

// runSemaphoreMode dequeues based on available backend capacity only.
func (pq *RequestPriorityQueue) runSemaphoreMode(ctx context.Context) {
	for {
		req, err := pq.popWhenAvailable(ctx)
		if err != nil {
			return
		}
		if req == nil || req.NotifyChan == nil {
			continue
		}
		if req.isCancelled() {
			continue
		}

		select {
		case <-ctx.Done():
			pq.requeueRequest(req)
			return
		case <-pq.stopCh:
			pq.requeueRequest(req)
			return
		case pq.sem <- struct{}{}:
			// Permit acquired
		}

		// Commit admission atomically with respect to a concurrent Abandon(): install
		// Release and increment the inflight metric under the request lock so the
		// waiting caller either observes the admission (and releases the permit on
		// timeout) or blocks admission entirely. Increment the metric inside the
		// committed section so a racing Release always has a matching increment.
		admitted := req.commitAdmission(func() {
			releaseOnce := sync.Once{}
			req.Release = func() {
				releaseOnce.Do(func() {
					<-pq.sem
					pq.metricDecInflight(req.ModelName)
				})
			}
			pq.metricIncInflight(req.ModelName)
		})
		if !admitted {
			// Caller abandoned before admission; return the just-acquired permit and
			// drop the request without signalling it.
			<-pq.sem
			continue
		}
		close(req.NotifyChan)
	}
}

// Close stops the dequeue loop and drains pending items from the heap.
// Callers waiting on NotifyChan will detect cancellation via their request-scoped signal.
func (pq *RequestPriorityQueue) Close() {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	select {
	case <-pq.stopCh:
		// already closed
		return
	default:
		close(pq.stopCh)
	}

	// Drain pending items: clear metrics for each remaining request
	for len(pq.heap) > 0 {
		req := heap.Pop(pq).(*Request)
		pq.metricDecSize(req.ModelName, req.UserID)
	}
	klog.V(4).Info("fairness queue closed and drained")
}

// --- Metric helpers ---
//
// These select between fairness-mode and session-boost-mode metric vectors based
// on the queue's configured mode, so the shared dequeue paths stay mode-agnostic.

func (pq *RequestPriorityQueue) metricIncSize(model, user string) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.IncSessionBoostQueueSize(model)
	} else {
		pq.metrics.IncFairnessQueueSize(model, user)
	}
}

func (pq *RequestPriorityQueue) metricDecSize(model, user string) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.DecSessionBoostQueueSize(model)
	} else {
		pq.metrics.DecFairnessQueueSize(model, user)
	}
}

func (pq *RequestPriorityQueue) metricRecordDuration(model, user string, d time.Duration) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.RecordSessionBoostQueueDuration(model, d)
	} else {
		pq.metrics.RecordFairnessQueueDuration(model, user, d)
	}
}

func (pq *RequestPriorityQueue) metricIncCancelled(model, user string) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.IncSessionBoostQueueCancelled(model)
	} else {
		pq.metrics.IncFairnessQueueCancelled(model, user)
	}
}

func (pq *RequestPriorityQueue) metricIncDequeue(model, user string) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.IncSessionBoostQueueDequeue(model)
	} else {
		pq.metrics.IncFairnessQueueDequeue(model, user)
	}
}

func (pq *RequestPriorityQueue) metricIncInflight(model string) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.IncSessionBoostQueueInflight(model)
	} else {
		pq.metrics.IncFairnessQueueInflight(model)
	}
}

func (pq *RequestPriorityQueue) metricDecInflight(model string) {
	if pq.metrics == nil {
		return
	}
	if pq.sessionBoost {
		pq.metrics.DecSessionBoostQueueInflight(model)
	} else {
		pq.metrics.DecFairnessQueueInflight(model)
	}
}
