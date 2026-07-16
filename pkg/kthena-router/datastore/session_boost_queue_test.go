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
	"context"
	"sync"
	"testing"
	"time"
)

// sessionBoostConfig returns a FairnessQueueConfig with session-boost mode enabled.
func sessionBoostConfig() FairnessQueueConfig {
	cfg := DefaultFairnessQueueConfig()
	cfg.SessionBoostEnabled = true
	return cfg
}

// newSessionBoostQueue constructs a session-boost-mode priority queue for tests.
func newSessionBoostQueue(cfg FairnessQueueConfig, checker BackendWaitingChecker) *RequestPriorityQueue {
	return NewRequestPriorityQueueWithConfig(nil, cfg, nil, checker)
}

func TestSessionBoostQueue_BasicPriorityOrdering(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	// Simulate a completed session
	q.MarkSessionRequestCompleted("conv-123")

	now := time.Now()

	normalReq := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		SessionID:   "conv-999",
		Priority:    1.0,
		RequestTime: now,
	}
	boostReq := &Request{
		UserID:      "user-B",
		ModelName:   "model-1",
		SessionID:   "conv-123",
		Priority:    100.0,
		RequestTime: now.Add(time.Second),
	}

	if err := q.PushRequest(normalReq); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}
	if err := q.PushRequest(boostReq); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	first, err := q.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if first.UserID != "user-B" {
		t.Errorf("Expected boosted request first, got %s", first.UserID)
	}
	if !first.SessionBoost {
		t.Error("Boosted request should have SessionBoost=true")
	}

	second, err := q.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if second.UserID != "user-A" {
		t.Errorf("Expected normal request second, got %s", second.UserID)
	}
	if second.SessionBoost {
		t.Error("Normal request should have SessionBoost=false")
	}
}

func TestSessionBoostQueue_FIFOWithinBoostStatus(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	now := time.Now()

	// Three non-boosted requests should come out in FIFO order
	reqs := []*Request{
		{UserID: "u1", ModelName: "m", RequestTime: now},
		{UserID: "u2", ModelName: "m", RequestTime: now.Add(time.Millisecond)},
		{UserID: "u3", ModelName: "m", RequestTime: now.Add(2 * time.Millisecond)},
	}
	for _, r := range reqs {
		if err := q.PushRequest(r); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	for i, expected := range []string{"u1", "u2", "u3"} {
		got, err := q.popWhenAvailable(context.Background())
		if err != nil {
			t.Fatalf("Pop %d failed: %v", i, err)
		}
		if got.UserID != expected {
			t.Errorf("Position %d: expected %s, got %s", i, expected, got.UserID)
		}
	}
}

func TestSessionBoostQueue_BoostEvictedByLRU(t *testing.T) {
	cfg := sessionBoostConfig()
	cfg.SessionBoostMaxSessions = 2 // remember only the 2 most-recently-completed sessions
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	// conv-123 completes first; two newer sessions then complete and push it out
	// of the LRU cache.
	q.MarkSessionRequestCompleted("conv-123")
	q.MarkSessionRequestCompleted("conv-456")
	q.MarkSessionRequestCompleted("conv-789")

	now := time.Now()
	req := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		SessionID:   "conv-123",
		RequestTime: now,
	}
	if err := q.PushRequest(req); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	popped, err := q.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if popped.SessionBoost {
		t.Error("Request should not have SessionBoost after its session was evicted from the LRU cache")
	}
}

// TestSessionTracker_LRU verifies bounded capacity, eviction order, and that
// re-completing a session refreshes its recency (promotes it away from eviction).
func TestSessionTracker_LRU(t *testing.T) {
	st := NewSessionTracker(2)

	st.MarkRequestCompleted("a")
	st.MarkRequestCompleted("b")
	if !st.HasRecentCompletion("a") || !st.HasRecentCompletion("b") {
		t.Fatal("both a and b should be tracked")
	}

	// Re-complete "a" so it becomes most-recently-used; "b" is now the LRU entry.
	st.MarkRequestCompleted("a")
	// Completing "c" exceeds capacity and should evict "b" (the LRU), not "a".
	st.MarkRequestCompleted("c")

	if st.HasRecentCompletion("b") {
		t.Error("b should have been evicted as the least-recently-used session")
	}
	if !st.HasRecentCompletion("a") {
		t.Error("a should still be tracked after being promoted")
	}
	if !st.HasRecentCompletion("c") {
		t.Error("c should be tracked")
	}
	if got := st.ActiveSessions(); got != 2 {
		t.Errorf("expected 2 tracked sessions, got %d", got)
	}
}

func TestSessionBoostQueue_MultipleSessions(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	// Use a controllable clock so conv-B's completion is strictly after conv-A's,
	// keeping the completion-time ordering below deterministic.
	st := q.GetSessionTracker()
	base := time.Now()
	markCompletedAt := func(sessionID string, completedAt time.Time) {
		st.now = func() time.Time { return completedAt }
		q.MarkSessionRequestCompleted(sessionID)
	}
	markCompletedAt("conv-A", base)
	markCompletedAt("conv-B", base.Add(time.Second))

	now := time.Now()
	requests := []*Request{
		{UserID: "u1", ModelName: "m", SessionID: "conv-X", RequestTime: now},
		{UserID: "u2", ModelName: "m", SessionID: "conv-A", RequestTime: now.Add(time.Millisecond)},
		{UserID: "u3", ModelName: "m", SessionID: "conv-B", RequestTime: now.Add(2 * time.Millisecond)},
		{UserID: "u4", ModelName: "m", SessionID: "", RequestTime: now.Add(3 * time.Millisecond)},
	}

	for _, r := range requests {
		if err := q.PushRequest(r); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	first, _ := q.popWhenAvailable(context.Background())
	second, _ := q.popWhenAvailable(context.Background())
	third, _ := q.popWhenAvailable(context.Background())
	fourth, _ := q.popWhenAvailable(context.Background())

	// Both boosted should come before normal requests
	if !first.SessionBoost || !second.SessionBoost {
		t.Errorf("First two should be boosted: first=%v second=%v", first.SessionBoost, second.SessionBoost)
	}
	if third.SessionBoost || fourth.SessionBoost {
		t.Errorf("Last two should not be boosted: third=%v fourth=%v", third.SessionBoost, fourth.SessionBoost)
	}

	// Among boosted: the session that completed most recently wins. conv-B was
	// marked completed after conv-A, so boost-B (u3) is dequeued before boost-A (u2).
	if first.UserID != "u3" {
		t.Errorf("Expected boost-B first (most recently completed session), got %s", first.UserID)
	}
	if second.UserID != "u2" {
		t.Errorf("Expected boost-A second, got %s", second.UserID)
	}

	// Among normal: FIFO order
	if third.UserID != "u1" {
		t.Errorf("Expected normal-1 third, got %s", third.UserID)
	}
	if fourth.UserID != "u4" {
		t.Errorf("Expected normal-2 fourth, got %s", fourth.UserID)
	}
}

// TestSessionBoostQueue_OrderedByCompletionTime verifies that boosted requests are
// dequeued by their session's completion time (most recently completed first),
// independent of the order in which the requests arrived in the queue.
func TestSessionBoostQueue_OrderedByCompletionTime(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	// Drive the tracker with a controllable clock so each completion gets a
	// strictly increasing, distinct timestamp regardless of the wall clock's
	// resolution or how fast the test executes. This keeps the completion-time
	// ordering deterministic instead of falling back to the RequestTime tiebreak.
	st := q.GetSessionTracker()
	base := time.Now()
	markCompletedAt := func(sessionID string, completedAt time.Time) {
		st.now = func() time.Time { return completedAt }
		q.MarkSessionRequestCompleted(sessionID)
	}

	// Completion order: conv-A, then conv-B, then conv-C (conv-C most recent).
	markCompletedAt("conv-A", base)
	markCompletedAt("conv-B", base.Add(time.Second))
	markCompletedAt("conv-C", base.Add(2*time.Second))

	now := time.Now()
	// Arrival order deliberately differs from completion order: B arrives first,
	// then C, then A. This distinguishes completion-time ordering from both FIFO
	// (which would yield B, C, A) and LIFO (which would yield A, C, B).
	requests := []*Request{
		{UserID: "u-B", ModelName: "m", SessionID: "conv-B", RequestTime: now},
		{UserID: "u-C", ModelName: "m", SessionID: "conv-C", RequestTime: now.Add(time.Millisecond)},
		{UserID: "u-A", ModelName: "m", SessionID: "conv-A", RequestTime: now.Add(2 * time.Millisecond)},
	}
	for _, r := range requests {
		if err := q.PushRequest(r); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	// Expect dequeue order by most-recent completion: C, then B, then A.
	for i, expected := range []string{"u-C", "u-B", "u-A"} {
		got, err := q.popWhenAvailable(context.Background())
		if err != nil {
			t.Fatalf("Pop %d failed: %v", i, err)
		}
		if !got.SessionBoost {
			t.Errorf("Position %d: expected boosted request, got non-boosted %s", i, got.UserID)
		}
		if got.UserID != expected {
			t.Errorf("Position %d: expected %s (by completion time), got %s", i, expected, got.UserID)
		}
	}
}

func TestSessionBoostQueue_BackpressureDrainsCancelledWhenBusy(t *testing.T) {
	// Backends are always busy, so tryBackpressureDequeue never pops. Cancelled
	// requests must still be drained from the heap so the queue size does not stay
	// inflated until capacity returns.
	checker := func() bool { return false } // backends always busy

	cfg := sessionBoostConfig()
	cfg.SessionBoostGracePeriod = 0
	cfg.InflightPerPod = 4
	q := newSessionBoostQueue(cfg, checker)
	defer q.Close()

	now := time.Now()
	// Two requests whose context is already cancelled, plus one live request.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled1 := &Request{UserID: "c1", ModelName: "m", RequestTime: now, NotifyChan: make(chan struct{}), CancelCh: cancelledCtx.Done()}
	cancelled2 := &Request{UserID: "c2", ModelName: "m", RequestTime: now.Add(time.Millisecond), NotifyChan: make(chan struct{}), CancelCh: cancelledCtx.Done()}
	live := &Request{UserID: "live", ModelName: "m", RequestTime: now.Add(2 * time.Millisecond), NotifyChan: make(chan struct{})}

	for _, r := range []*Request{cancelled1, cancelled2, live} {
		if err := q.PushRequest(r); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}
	if got := q.Len(); got != 3 {
		t.Fatalf("Expected queue length 3 before draining, got %d", got)
	}

	// One dequeue pass with backends busy: no request can be admitted, but the two
	// cancelled requests must be drained from the heap, leaving only the live one.
	q.tryBackpressureDequeue(context.Background())

	if got := q.Len(); got != 1 {
		t.Fatalf("Expected queue length 1 after draining cancelled requests, got %d", got)
	}

	// The remaining request must be the live one, and it must not have been admitted.
	if q.heap[0].UserID != "live" {
		t.Errorf("Expected remaining request to be 'live', got %q", q.heap[0].UserID)
	}
	select {
	case <-live.NotifyChan:
		t.Fatal("live request should not be admitted while backends are busy")
	default:
	}
}

func TestSessionBoostQueue_BackpressureMode(t *testing.T) {
	backendHasCapacity := true
	checker := func() bool { return backendHasCapacity }

	cfg := sessionBoostConfig()
	cfg.SessionBoostGracePeriod = 0
	cfg.InflightPerPod = 2 // total inflight limit (no pod counter, perPod = total)
	q := newSessionBoostQueue(cfg, checker)
	defer q.Close()

	now := time.Now()
	req1 := &Request{UserID: "u1", ModelName: "m", RequestTime: now, NotifyChan: make(chan struct{})}
	req2 := &Request{UserID: "u2", ModelName: "m", RequestTime: now.Add(time.Millisecond), NotifyChan: make(chan struct{})}
	req3 := &Request{UserID: "u3", ModelName: "m", RequestTime: now.Add(2 * time.Millisecond), NotifyChan: make(chan struct{})}

	for _, r := range []*Request{req1, req2, req3} {
		if err := q.PushRequest(r); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go q.Run(ctx, 0)

	// First 2 should be dequeued (inflight limit = 2)
	select {
	case <-req1.NotifyChan:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout: req-1 should be dequeued")
	}
	select {
	case <-req2.NotifyChan:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout: req-2 should be dequeued")
	}

	// Third should be blocked
	select {
	case <-req3.NotifyChan:
		t.Fatal("req-3 should be blocked by inflight limit")
	case <-time.After(100 * time.Millisecond):
	}

	if q.GetInflightCount() != 2 {
		t.Errorf("Expected inflight=2, got %d", q.GetInflightCount())
	}

	// Release one -> unblocks req3
	req1.Release()
	select {
	case <-req3.NotifyChan:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Timeout: req-3 should be dequeued after Release()")
	}
}

func TestSessionBoostQueue_GracePeriod_BoostedArrives(t *testing.T) {
	checker := func() bool { return true }

	cfg := sessionBoostConfig()
	cfg.SessionBoostGracePeriod = 200 * time.Millisecond
	cfg.InflightPerPod = 1
	q := newSessionBoostQueue(cfg, checker)
	defer q.Close()

	now := time.Now()
	req1 := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		SessionID:   "session-1",
		RequestTime: now,
		NotifyChan:  make(chan struct{}),
	}
	if err := q.PushRequest(req1); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go q.Run(ctx, 0)

	select {
	case <-req1.NotifyChan:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout: req-1 should be dequeued")
	}

	// Mark session completed and push a non-boosted request
	q.MarkSessionRequestCompleted("session-1")
	nonBoosted := &Request{
		UserID:      "user-B",
		ModelName:   "model-1",
		SessionID:   "other-session",
		RequestTime: now.Add(time.Millisecond),
		NotifyChan:  make(chan struct{}),
	}
	if err := q.PushRequest(nonBoosted); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	// Release req1 -> triggers grace period
	req1.Release()

	// During grace, push a session-boosted follow-up
	time.Sleep(20 * time.Millisecond)
	boostedFollowUp := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		SessionID:   "session-1",
		RequestTime: now.Add(2 * time.Millisecond),
		NotifyChan:  make(chan struct{}),
	}
	if err := q.PushRequest(boostedFollowUp); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	// Boosted follow-up should be dequeued first
	select {
	case <-boostedFollowUp.NotifyChan:
		// Success
	case <-nonBoosted.NotifyChan:
		t.Fatal("Non-boosted request should not be dequeued before the session-boosted follow-up")
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Timeout: boosted follow-up should be dequeued within grace period")
	}
}

func TestSessionBoostQueue_GracePeriod_NoBoostArrives(t *testing.T) {
	checker := func() bool { return true }

	cfg := sessionBoostConfig()
	cfg.SessionBoostGracePeriod = 100 * time.Millisecond
	cfg.InflightPerPod = 1
	q := newSessionBoostQueue(cfg, checker)
	defer q.Close()

	now := time.Now()
	req1 := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		RequestTime: now,
		NotifyChan:  make(chan struct{}),
	}
	if err := q.PushRequest(req1); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go q.Run(ctx, 0)

	select {
	case <-req1.NotifyChan:
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout: req-1 should be dequeued")
	}

	normalReq := &Request{
		UserID:      "user-B",
		ModelName:   "model-1",
		RequestTime: now.Add(time.Millisecond),
		NotifyChan:  make(chan struct{}),
	}
	if err := q.PushRequest(normalReq); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	start := time.Now()
	req1.Release()

	select {
	case <-normalReq.NotifyChan:
		elapsed := time.Since(start)
		if elapsed < 80*time.Millisecond {
			t.Errorf("Dequeue happened too early (before grace period): %v", elapsed)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("Dequeue took too long after grace period: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout: normal request should be dequeued after grace period expires")
	}
}

func TestSessionBoostQueue_CancelledRequestsSkipped(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	now := time.Now()
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	cancelledReq := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		RequestTime: now,
		CancelCh:    cancelCtx.Done(),
	}
	normalReq := &Request{
		UserID:      "user-B",
		ModelName:   "model-1",
		RequestTime: now.Add(time.Millisecond),
	}

	if err := q.PushRequest(cancelledReq); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}
	if err := q.PushRequest(normalReq); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	popped, err := q.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if popped.UserID != "user-B" {
		t.Errorf("Expected normal request (cancelled should be skipped), got %s", popped.UserID)
	}
}

func TestSessionBoostQueue_EmptySessionID(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	q.MarkSessionRequestCompleted("conv-123")

	now := time.Now()
	req := &Request{
		UserID:      "user-A",
		ModelName:   "model-1",
		SessionID:   "", // No session ID
		RequestTime: now,
	}

	if err := q.PushRequest(req); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	popped, err := q.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("Pop failed: %v", err)
	}
	if popped.SessionBoost {
		t.Error("Request without SessionID should not have SessionBoost")
	}
}

func TestSessionBoostQueue_Len(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	if q.Len() != 0 {
		t.Errorf("Expected empty queue, got len=%d", q.Len())
	}

	now := time.Now()
	for i := 0; i < 5; i++ {
		req := &Request{
			UserID:      "user",
			ModelName:   "model",
			RequestTime: now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := q.PushRequest(req); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	if q.Len() != 5 {
		t.Errorf("Expected len=5, got %d", q.Len())
	}
}

// TestSessionBoost_AbandonBeforeAdmissionBlocksAdmission verifies that a request
// abandoned by the caller before the dequeue loop admits it consumes no inflight
// slot: admitSessionBoost must observe the abandonment and skip.
func TestSessionBoost_AbandonBeforeAdmissionBlocksAdmission(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	req := &Request{
		UserID:     "user-A",
		ModelName:  "model-1",
		NotifyChan: make(chan struct{}),
	}

	// Caller gives up first.
	req.Abandon()

	// The dequeue loop then tries to admit; it must skip.
	if q.admitSessionBoost(req) {
		t.Fatal("admitSessionBoost should skip an already-abandoned request")
	}
	if got := q.GetInflightCount(); got != 0 {
		t.Fatalf("inflight count should stay 0 after skipped admission, got %d", got)
	}
	select {
	case <-req.NotifyChan:
		t.Fatal("NotifyChan must not be closed for a skipped admission")
	default:
	}
}

// TestSessionBoost_AdmissionBeforeAbandonReleases verifies that when admission wins
// the race, the caller's Abandon() releases the permit it owns, returning the
// inflight count to zero.
func TestSessionBoost_AdmissionBeforeAbandonReleases(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	req := &Request{
		UserID:     "user-A",
		ModelName:  "model-1",
		NotifyChan: make(chan struct{}),
	}

	if !q.admitSessionBoost(req) {
		t.Fatal("admitSessionBoost should admit a live request")
	}
	select {
	case <-req.NotifyChan:
	default:
		t.Fatal("NotifyChan should be closed after admission")
	}
	if got := q.GetInflightCount(); got != 1 {
		t.Fatalf("inflight count should be 1 after admission, got %d", got)
	}

	// Caller times out after admission raced in: Abandon() must release the permit
	// it owns.
	req.Abandon()
	if got := q.GetInflightCount(); got != 0 {
		t.Fatalf("inflight count should return to 0 after Abandon, got %d", got)
	}
}

// TestSessionBoost_ConcurrentAdmitAbandonNoLeak stresses the admission/abandonment
// race directly: for each request one goroutine admits while another abandons. No
// matter which wins, the inflight count must return to zero, proving the permit is
// never leaked and never double-released. Run with -race to catch data races.
func TestSessionBoost_ConcurrentAdmitAbandonNoLeak(t *testing.T) {
	cfg := sessionBoostConfig()
	q := newSessionBoostQueue(cfg, nil)
	defer q.Close()

	const iterations = 2000
	for i := 0; i < iterations; i++ {
		req := &Request{
			UserID:     "user-A",
			ModelName:  "model-1",
			NotifyChan: make(chan struct{}),
		}

		var wg sync.WaitGroup
		wg.Add(2)
		// Dequeue-loop side: attempt admission.
		go func() {
			defer wg.Done()
			q.admitSessionBoost(req)
		}()
		// Handler side: time out and abandon; Abandon releases if admission raced in.
		// This mirrors the router's queue-wait timeout handling.
		go func() {
			defer wg.Done()
			req.Abandon()
		}()
		wg.Wait()

		// After both settle, drain any releaseCh signal the release may have posted so
		// it does not affect the next iteration's assertions.
		select {
		case <-q.releaseCh:
		default:
		}

		if got := q.GetInflightCount(); got != 0 {
			t.Fatalf("iteration %d: inflight leaked, count=%d", i, got)
		}
	}
}
