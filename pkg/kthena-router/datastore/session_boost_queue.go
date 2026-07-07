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
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"k8s.io/klog/v2"
)

// This file extends RequestPriorityQueue with session-boost behavior. When a
// queue is constructed with FairnessQueueConfig.SessionBoostEnabled, the shared
// priority-queue framework in fairness_queue.go reuses the same heap, push/pop,
// cancellation and shutdown logic, while the ordering and dequeue strategy below
// replace per-user fairness with session-aware boosting for prefix-cache reuse.

// BackendWaitingChecker is a function that checks whether the backend pods
// have capacity to accept new requests. It returns true when at least one pod
// has an empty waiting queue (i.e. RequestWaitingNum == 0), meaning the backend
// can accept a new request without queuing.
type BackendWaitingChecker func() bool

// PodCounter returns the number of backend pods currently serving a model. It is
// used in session-boost mode to scale the total inflight limit by the number of
// pods (InflightPerPod * podCount).
type PodCounter func() int

// SessionTracker tracks recently completed sessions for priority boosting using
// a bounded LRU cache. It remembers the N most-recently-completed sessions (N is
// the configured capacity); follow-up requests belonging to one of those sessions
// are boosted so they can reuse the still-warm prefix cache on the backend.
//
// An LRU bound is used instead of a time-based TTL because it directly mirrors how
// inference engines (e.g. vLLM) evict their KV/prefix cache: the least-recently-used
// sessions fall out first. This means operators only need to size the cache by the
// number of concurrent conversations they want to keep warm, rather than guessing a
// duration. Under high load, stale sessions are evicted quickly; under low load,
// keeping a few extra entries is harmless because boosting only matters when the
// queue is contended.
type SessionTracker struct {
	// cache maps a session ID to a presence marker, ordered by recency. The
	// underlying hashicorp/golang-lru cache is safe for concurrent use and evicts
	// the least-recently-used session once capacity is exceeded.
	cache *lru.Cache[string, struct{}]
}

// NewSessionTracker creates a new session tracker that remembers up to capacity
// most-recently-completed sessions. A non-positive capacity falls back to the
// default.
func NewSessionTracker(capacity int) *SessionTracker {
	if capacity <= 0 {
		capacity = defaultSessionBoostMaxSessions
	}
	cache, err := lru.NewWithEvict(capacity, func(sessionID string, _ struct{}) {
		klog.V(4).Infof("[SessionTracker] evicted LRU session %q", sessionID)
	})
	if err != nil {
		// capacity is guaranteed positive above, so this is unreachable in practice.
		klog.Errorf("[SessionTracker] failed to create LRU cache (capacity=%d): %v", capacity, err)
	}
	return &SessionTracker{cache: cache}
}

// MarkRequestCompleted records that a request from the given session has completed,
// promoting it to the most-recently-used position. When the cache exceeds its
// capacity, the least-recently-used session is evicted.
func (st *SessionTracker) MarkRequestCompleted(sessionID string) {
	if sessionID == "" {
		return
	}
	st.cache.Add(sessionID, struct{}{})
}

// HasRecentCompletion reports whether the given session ID is currently tracked
// (i.e. it is among the N most-recently-completed sessions). It is a pure read and
// does not change recency ordering.
func (st *SessionTracker) HasRecentCompletion(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	return st.cache.Contains(sessionID)
}

// ActiveSessions returns the number of sessions currently tracked.
func (st *SessionTracker) ActiveSessions() int {
	return st.cache.Len()
}

// MarkSessionRequestCompleted records that a request from the given session has completed,
// enabling priority boosting for follow-up requests in the same session. No-op when
// the queue is not in session-boost mode.
func (pq *RequestPriorityQueue) MarkSessionRequestCompleted(sessionID string) {
	if pq.sessionTracker != nil {
		pq.sessionTracker.MarkRequestCompleted(sessionID)
	}
}

// GetSessionTracker returns the session tracker, or nil if session boost is disabled.
func (pq *RequestPriorityQueue) GetSessionTracker() *SessionTracker {
	return pq.sessionTracker
}

// GetInflightCount returns the current number of inflight requests in session-boost mode.
func (pq *RequestPriorityQueue) GetInflightCount() int64 {
	return pq.inflightCount.Load()
}

// admitSessionBoost marks a request as inflight, installs its release callback and
// unblocks the waiting caller by closing its NotifyChan. It returns false without
// admitting when the caller has already abandoned the request (timed out or
// cancelled): in that case the request has already left the queue and admitting it
// would leak an inflight permit that no one will release.
func (pq *RequestPriorityQueue) admitSessionBoost(req *Request) bool {
	// Commit the inflight accounting and Release installation atomically with
	// respect to a concurrent Abandon(). If the caller abandoned first, skip
	// admission entirely so the inflight permit is never consumed.
	admitted := req.commitAdmission(func() {
		pq.inflightCount.Add(1)
		// req.Release returns the inflight permit this admission consumed. The request
		// handler invokes it (via defer) once the backend response is fully proxied or
		// the request fails/times out. It decrements the inflight count, signals
		// releaseCh so the dequeue loop can immediately admit the next waiting request,
		// and updates the inflight metric. sync.Once makes it idempotent so capacity is
		// never released twice on overlapping exit paths.
		releaseOnce := sync.Once{}
		req.Release = func() {
			releaseOnce.Do(func() {
				pq.inflightCount.Add(-1)
				select {
				case pq.releaseCh <- struct{}{}:
				default:
				}
				pq.metricDecInflight(req.ModelName)
			})
		}
		pq.metricIncInflight(req.ModelName)
	})
	if !admitted {
		klog.V(4).Infof("[SessionBoost] admission skipped, request abandoned before admission: user=%s model=%s",
			req.UserID, req.ModelName)
		return false
	}
	// Closing NotifyChan is the admission signal: the caller blocked in Enqueue is
	// waiting on this channel and proceeds to the backend only once it is closed.
	// We notify here because admission (a free inflight slot plus backend capacity)
	// is exactly the condition that lets this request run; there is no separate
	// readiness state to wait on.
	if req.NotifyChan != nil {
		close(req.NotifyChan)
	}
	return true
}

// runSessionBoostMode is the session-boost dequeue loop. It dequeues requests only
// when backend pods have capacity, using two-level admission control:
//  1. Inflight limit: at most InflightPerPod requests in flight per backend pod.
//  2. Backend metrics check: at least one pod reports capacity available.
//
// The loop is fully event-driven: it reacts to releases and new arrivals. There
// is no metrics-refresh signal or independent timer: in single-router operation
// every moment backend capacity frees up coincides with one of our own requests
// completing (a release), so releases and arrivals alone cover every dequeue
// opportunity.
//
// Session Grace Period: when SessionBoostGracePeriod > 0, a release briefly holds
// the freed slot (via waitGraceAndDequeue) so a same-session follow-up has time to
// arrive and reuse the warm prefix cache. Fresh arrivals are still admitted
// immediately, so enabling grace adds no latency to first turns on an idle queue.
// When a release and an arrival are pending at once, the release wins so the
// just-freed slot is the one held for the grace window.
func (pq *RequestPriorityQueue) runSessionBoostMode(ctx context.Context) {
	grace := pq.config.SessionBoostGracePeriod > 0
	klog.V(4).Infof("[SessionBoost] starting backpressure dequeue loop, gracePeriod=%v", pq.config.SessionBoostGracePeriod)
	for {
		select {
		// Lifecycle: the queue's owning context was cancelled — stop dispatching.
		case <-ctx.Done():
			return
		// Lifecycle: the queue was closed via Close() — stop dispatching.
		case <-pq.stopCh:
			return
		// A request finished and freed an inflight slot. With grace enabled, hold
		// the freed slot briefly for a same-session follow-up; otherwise try to
		// admit the next request right away.
		case <-pq.releaseCh:
			if grace {
				pq.waitGraceAndDequeue(ctx)
			} else {
				pq.tryBackpressureDequeue(ctx)
			}
		// A fresh request was enqueued. With grace enabled, if a release is also
		// pending, route through the grace path so the freed slot is held for a
		// same-session follow-up; otherwise admit immediately so an idle queue need
		// not wait for the next signal.
		case <-pq.notifyCh:
			if grace && pq.drainPendingRelease() {
				pq.waitGraceAndDequeue(ctx)
			} else {
				pq.tryBackpressureDequeue(ctx)
			}
		}
	}
}

// drainPendingRelease non-blockingly consumes one pending release signal, reporting
// whether one was present. It lets the arrival branch prefer the grace path when a
// release is also waiting, keeping that ordering deterministic despite select's
// random choice between simultaneously-ready cases.
func (pq *RequestPriorityQueue) drainPendingRelease() bool {
	select {
	case <-pq.releaseCh:
		return true
	default:
		return false
	}
}

// isHeadSessionBoosted checks if the highest-priority request in the queue has a session boost.
func (pq *RequestPriorityQueue) isHeadSessionBoosted() bool {
	pq.mu.RLock()
	defer pq.mu.RUnlock()
	if len(pq.heap) == 0 {
		return false
	}
	return pq.heap[0].SessionBoost
}

// waitGraceAndDequeue holds a just-freed slot for up to SessionBoostGracePeriod
// before dispatching, giving a same-session follow-up time to arrive. It does not
// need to watch for the follow-up itself: boosted requests outrank others in the
// heap, so once the timer fires tryBackpressureDequeue admits the boosted request
// first if one showed up. The wait stays responsive to shutdown via ctx/stopCh.
func (pq *RequestPriorityQueue) waitGraceAndDequeue(ctx context.Context) {
	// Fast path: a boosted follow-up is already at the head, so there is nothing
	// to wait for.
	if pq.isHeadSessionBoosted() {
		klog.V(4).Info("[SessionBoost] grace: head already boosted, skipping wait")
		pq.tryBackpressureDequeue(ctx)
		return
	}

	klog.V(4).Infof("[SessionBoost] grace: holding freed slot for %v", pq.config.SessionBoostGracePeriod)
	timer := time.NewTimer(pq.config.SessionBoostGracePeriod)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-pq.stopCh:
	case <-timer.C:
		pq.tryBackpressureDequeue(ctx)
	}
}

// drainCancelledLocked removes cancelled/timed-out requests from anywhere in the
// heap, decrements queue-size metrics for each, and rebuilds the heap. The caller
// must hold pq.mu. It returns the number of requests drained.
//
// This is needed because while all backends report busy (or the inflight limit is
// reached), popWhenAvailable is never called, so requests whose CancelCh has fired
// would otherwise linger in the heap and keep the reported queue size inflated
// until capacity returns. Draining them here keeps the queue size accurate and
// avoids wasting a future dequeue slot on an already-dead request. The waiting
// caller detects cancellation via its own request-scoped signal, so we deliberately
// do not close NotifyChan here.
func (pq *RequestPriorityQueue) drainCancelledLocked() int {
	origLen := len(pq.heap)
	if origLen == 0 {
		return 0
	}
	kept := pq.heap[:0]
	for _, req := range pq.heap {
		if req.isCancelled() {
			pq.metricDecSize(req.ModelName, req.UserID)
			pq.metricRecordDuration(req.ModelName, req.UserID, time.Since(req.RequestTime))
			pq.metricIncCancelled(req.ModelName, req.UserID)
			continue
		}
		kept = append(kept, req)
	}
	drained := origLen - len(kept)
	if drained == 0 {
		return 0
	}
	// Release references to the drained tail before shrinking the heap.
	for i := len(kept); i < origLen; i++ {
		pq.heap[i] = nil
	}
	pq.heap = kept
	heap.Init(pq)
	return drained
}

// tryBackpressureDequeue admits as many queued requests as possible in one pass,
// stopping when the inflight limit is reached, backends report no capacity, or
// the queue is empty. This avoids the one-request-per-tick bottleneck during
// initial ramp-up and whenever spare capacity exists.
func (pq *RequestPriorityQueue) tryBackpressureDequeue(ctx context.Context) {
	// In session-boost mode, the total inflight limit is InflightPerPod scaled by
	// the number of backend pods serving the model.
	perPod := pq.config.InflightPerPod
	if perPod <= 0 {
		perPod = defaultSessionBoostInflightPerPod
	}
	maxInflight := int64(perPod)
	podCount := 0
	if pq.podCounter != nil {
		podCount = pq.podCounter()
		if podCount > 0 {
			maxInflight = int64(podCount) * int64(perPod)
		}
	}

	for {
		currentInflight := pq.inflightCount.Load()

		if currentInflight >= maxInflight {
			pq.mu.Lock()
			drained := pq.drainCancelledLocked()
			pq.mu.Unlock()
			klog.V(4).Infof("[SessionBoost] backpressure: inflight limit reached, inflight=%d maxInflight=%d pods=%d perPod=%d drainedCancelled=%d",
				currentInflight, maxInflight, podCount, perPod, drained)
			return
		}

		if !pq.backendChecker() {
			pq.mu.Lock()
			drained := pq.drainCancelledLocked()
			queueLen := len(pq.heap)
			pq.mu.Unlock()
			klog.V(4).Infof("[SessionBoost] backpressure: backend pods busy, holding dequeue. queueLen=%d inflight=%d drainedCancelled=%d",
				queueLen, currentInflight, drained)
			return
		}

		pq.mu.RLock()
		queueLen := len(pq.heap)
		pq.mu.RUnlock()
		if queueLen == 0 {
			return
		}

		req, err := pq.popWhenAvailable(ctx)
		if err != nil || req == nil {
			return
		}

		if !pq.admitSessionBoost(req) {
			// The request was abandoned (timed out / cancelled) between the
			// cancellation check and admission; it consumed no inflight slot, so
			// continue admitting the next queued request.
			continue
		}

		klog.V(4).Infof("[SessionBoost] backpressure dequeue: user=%s model=%s sessionBoost=%v inflight=%d/%d",
			req.UserID, req.ModelName, req.SessionBoost, pq.inflightCount.Load(), maxInflight)
	}
}
