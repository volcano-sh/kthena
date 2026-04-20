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
	"time"

	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// FairnessQueueConfig holds configurable parameters for the fairness queue.
type FairnessQueueConfig struct {
	// MaxConcurrent is the maximum number of in-flight requests allowed through
	// the fairness gate for this model. When 0, falls back to MaxQPS-based rate limiting.
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
}

// DefaultFairnessQueueConfig returns backward-compatible defaults.
func DefaultFairnessQueueConfig() FairnessQueueConfig {
	return FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 0,
		RebuildThreshold:          64,
		TokenWeight:               1.0,
		RequestNumWeight:          0.0,
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
	ReqID     string
	UserID    string  // User ID for fairness scheduling
	ModelName string  // Target model for per-model fair queuing
	Priority  float64 // Priority (lower value means higher priority)
	// PriorityOffset stores enqueue-time request cost so refresh keeps request-size sensitivity.
	PriorityOffset float64
	RequestTime    time.Time
	NotifyChan     chan struct{}
	CancelCh       <-chan struct{} // Request-scoped cancellation signal
	Release        func()          // Set by the queue when a permit is acquired
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
}

var _ heap.Interface = &RequestPriorityQueue{}

// NewRequestPriorityQueue creates a new priority queue. Pass nil metrics to use defaults.
func NewRequestPriorityQueue(metricsInstance *metrics.Metrics) *RequestPriorityQueue {
	return NewRequestPriorityQueueWithConfig(metricsInstance, DefaultFairnessQueueConfig(), nil)
}

// NewRequestPriorityQueueWithConfig creates a priority queue with explicit configuration.
func NewRequestPriorityQueueWithConfig(metricsInstance *metrics.Metrics, cfg FairnessQueueConfig, tracker TokenTracker) *RequestPriorityQueue {
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
	if cfg.MaxConcurrent > 0 {
		pq.sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return pq
}

// Implement heap.Interface methods
func (pq *RequestPriorityQueue) Len() int { return len(pq.heap) }

func (pq *RequestPriorityQueue) Less(i, j int) bool {
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
	heap.Push(pq, r)

	// Update fairness queue size metrics
	if pq.metrics != nil {
		pq.metrics.IncFairnessQueueSize(r.ModelName, r.UserID)
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
				if pq.metrics != nil {
					pq.metrics.DecFairnessQueueSize(req.ModelName, req.UserID)
					queueDuration := time.Since(req.RequestTime)
					pq.metrics.RecordFairnessQueueDuration(req.ModelName, req.UserID, queueDuration)
					pq.metrics.IncFairnessQueueCancelled(req.ModelName, req.UserID)
				}
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
					newPri += req.PriorityOffset
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
			if pq.metrics != nil {
				pq.metrics.DecFairnessQueueSize(req.ModelName, req.UserID)
				queueDuration := time.Since(req.RequestTime)
				pq.metrics.RecordFairnessQueueDuration(req.ModelName, req.UserID, queueDuration)
				pq.metrics.IncFairnessQueueDequeue(req.ModelName, req.UserID)
			}

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
			req.Priority = newPri + req.PriorityOffset
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

// Run starts the dequeue loop. In semaphore mode (MaxConcurrent > 0), dequeue is
// gated by available capacity. Otherwise, it falls back to QPS-based ticker dequeue.
func (pq *RequestPriorityQueue) Run(ctx context.Context, qps int) {
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

		releaseOnce := sync.Once{}
		trackedInflight := false
		req.Release = func() {
			releaseOnce.Do(func() {
				<-pq.sem
				if trackedInflight && pq.metrics != nil {
					pq.metrics.DecFairnessQueueInflight(req.ModelName)
				}
			})
		}

		if pq.metrics != nil {
			pq.metrics.IncFairnessQueueInflight(req.ModelName)
		}
		trackedInflight = true
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
		if pq.metrics != nil {
			pq.metrics.DecFairnessQueueSize(req.ModelName, req.UserID)
		}
	}
	klog.V(4).Info("fairness queue closed and drained")
}
