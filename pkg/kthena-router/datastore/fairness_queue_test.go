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
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNewRequestPriorityQueue(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	if pq == nil {
		t.Fatal("NewRequestPriorityQueue(nil) returned nil")
	}
	if pq.Len() != 0 {
		t.Errorf("Expected empty queue, got length %d", pq.Len())
	}
	if pq.stopCh == nil {
		t.Error("stopCh should be initialized")
	}
	if pq.notifyCh == nil {
		t.Error("notifyCh should be initialized")
	}
}

func TestPushAndPopRequest(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	req := &Request{
		ReqID:       "test-1",
		UserID:      "user-1",
		ModelName:   "model-1",
		Priority:    1.0,
		RequestTime: time.Now(),
	}

	// Test push
	err := pq.PushRequest(req)
	if err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}
	if pq.Len() != 1 {
		t.Errorf("Expected queue length 1, got %d", pq.Len())
	}

	// Test pop
	popped, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("PopRequest failed: %v", err)
	}
	if popped.ReqID != req.ReqID {
		t.Errorf("Expected ReqID %s, got %s", req.ReqID, popped.ReqID)
	}
	if pq.Len() != 0 {
		t.Errorf("Expected queue length 0, got %d", pq.Len())
	}
}

func TestPriorityOrdering(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	now := time.Now()
	requests := []*Request{
		{ReqID: "high", UserID: "user1", Priority: 1.0, RequestTime: now},
		{ReqID: "medium", UserID: "user2", Priority: 2.0, RequestTime: now.Add(time.Second)},
		{ReqID: "low", UserID: "user3", Priority: 3.0, RequestTime: now.Add(2 * time.Second)},
	}

	// Push in reverse priority order
	for i := len(requests) - 1; i >= 0; i-- {
		err := pq.PushRequest(requests[i])
		if err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	// Pop should return in priority order (lower priority value = higher priority)
	expectedOrder := []string{"high", "medium", "low"}
	for i, expected := range expectedOrder {
		req, err := pq.popWhenAvailable(context.Background())
		if err != nil {
			t.Fatalf("PopRequest failed at index %d: %v", i, err)
		}
		if req.ReqID != expected {
			t.Errorf("Expected ReqID %s at index %d, got %s", expected, i, req.ReqID)
		}
	}
}

func TestFairnessSameUser(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	now := time.Now()
	requests := []*Request{
		{ReqID: "req1", UserID: "user1", Priority: 1.0, RequestTime: now},
		{ReqID: "req2", UserID: "user1", Priority: 1.0, RequestTime: now.Add(time.Second)},
		{ReqID: "req3", UserID: "user1", Priority: 1.0, RequestTime: now.Add(2 * time.Second)},
	}

	// Push in reverse time order
	for i := len(requests) - 1; i >= 0; i-- {
		err := pq.PushRequest(requests[i])
		if err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	// Should return in FIFO order for same user
	expectedOrder := []string{"req1", "req2", "req3"}
	for i, expected := range expectedOrder {
		req, err := pq.popWhenAvailable(context.TODO())
		if err != nil {
			t.Fatalf("PopRequest failed at index %d: %v", i, err)
		}
		if req.ReqID != expected {
			t.Errorf("Expected ReqID %s at index %d, got %s", expected, i, req.ReqID)
		}
	}
}

func TestPopWhenAvailable(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	ctx := context.Background()
	req := &Request{
		ReqID:       "test-async",
		UserID:      "user1",
		Priority:    1.0,
		RequestTime: time.Now(),
	}

	// Start a goroutine to pop when available
	resultCh := make(chan *Request, 1)
	errorCh := make(chan error, 1)
	go func() {
		result, err := pq.popWhenAvailable(ctx)
		if err != nil {
			errorCh <- err
		} else {
			resultCh <- result
		}
	}()

	// Wait a bit to ensure the goroutine is waiting
	time.Sleep(10 * time.Millisecond)

	// Push a request
	err := pq.PushRequest(req)
	if err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	// Should receive the request
	select {
	case result := <-resultCh:
		if result.ReqID != req.ReqID {
			t.Errorf("Expected ReqID %s, got %s", req.ReqID, result.ReqID)
		}
	case err := <-errorCh:
		t.Fatalf("popWhenAvailable failed: %v", err)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("popWhenAvailable timed out")
	}
}

func TestPopWhenAvailableContextCancellation(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start popWhenAvailable
	errorCh := make(chan error, 1)
	go func() {
		_, err := pq.popWhenAvailable(ctx)
		errorCh <- err
	}()

	// Wait a bit, then cancel context
	time.Sleep(10 * time.Millisecond)
	cancel()

	// Should receive context cancellation error
	select {
	case err := <-errorCh:
		if err != context.Canceled {
			t.Errorf("Expected context.Canceled, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("popWhenAvailable should have returned with context cancellation")
	}
}

func TestPopWhenAvailableStopChannel(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)

	ctx := context.Background()

	// Start popWhenAvailable
	errorCh := make(chan error, 1)
	go func() {
		_, err := pq.popWhenAvailable(ctx)
		errorCh <- err
	}()

	// Wait a bit, then close the queue
	time.Sleep(10 * time.Millisecond)
	pq.Close()

	// Should receive queue stopped error
	select {
	case err := <-errorCh:
		if err.Error() != "queue stopped" {
			t.Errorf("Expected 'queue stopped', got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("popWhenAvailable should have returned with stop signal")
	}
}

func TestRunMethod(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Track processed requests
	processedCh := make(chan *Request, 10)

	// Add requests with notification channels
	for i := 0; i < 3; i++ {
		notifyCh := make(chan struct{})
		req := &Request{
			ReqID:       "req-" + string(rune('1'+i)),
			UserID:      "user1",
			Priority:    float64(i + 1),
			RequestTime: time.Now(),
			NotifyChan:  notifyCh,
		}

		err := pq.PushRequest(req)
		if err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}

		// Monitor for processing
		go func(r *Request) {
			select {
			case <-r.NotifyChan:
				processedCh <- r
			case <-time.After(time.Second):
				t.Errorf("Request %s was not processed", r.ReqID)
			}
		}(req)
	}

	// Run the queue with 20 QPS (faster processing)
	go pq.Run(ctx, 20)

	// Collect processed requests
	var processed []*Request
	timeout := time.After(400 * time.Millisecond)
	for len(processed) < 3 {
		select {
		case req := <-processedCh:
			processed = append(processed, req)
		case <-timeout:
			t.Fatalf("Timeout waiting for requests to be processed. Got %d requests", len(processed))
		}
	}

	if len(processed) != 3 {
		t.Errorf("Expected 3 processed requests, got %d", len(processed))
	}

	// Validate all expected requests were processed (order can vary due to concurrency)
	expectedSet := map[string]struct{}{"req-1": {}, "req-2": {}, "req-3": {}}
	for _, r := range processed {
		if _, ok := expectedSet[r.ReqID]; !ok {
			t.Errorf("Unexpected ReqID processed: %s", r.ReqID)
		}
	}
}

func TestConcurrentPushPop(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	numGoroutines := 10
	numRequests := 100
	var wg sync.WaitGroup

	// Concurrent pushers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numRequests; j++ {
				req := &Request{
					ReqID:       fmt.Sprintf("req-%d-%d", id, j),
					UserID:      fmt.Sprintf("user-%d", id),
					Priority:    float64(j),
					RequestTime: time.Now(),
				}
				err := pq.PushRequest(req)
				if err != nil {
					t.Errorf("PushRequest failed: %v", err)
				}
			}
		}(i)
	}

	// Concurrent poppers
	popped := make(chan *Request, numGoroutines*numRequests)
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numRequests; j++ {
				req, err := pq.popWhenAvailable(context.Background())

				if err != nil {
					t.Errorf("PopRequest failed: %v", err)
					return
				}
				popped <- req
			}
		}()
	}

	wg.Wait()
	close(popped)

	// Count results
	count := 0
	for range popped {
		count++
	}

	expected := numGoroutines * numRequests
	if count != expected {
		t.Errorf("Expected %d requests, got %d", expected, count)
	}

	// Queue should be empty
	if pq.Len() != 0 {
		t.Errorf("Expected empty queue, got length %d", pq.Len())
	}
}

func TestClose(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)

	// Close should be idempotent
	pq.Close()
	pq.Close() // Should not panic

	// Verify stop channel is closed
	select {
	case <-pq.stopCh:
		// Expected
	default:
		t.Error("stopCh should be closed")
	}
}

func TestPopWhenAvailable_SkipsCancelledRequests(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	now := time.Now()

	// Create a cancelled context for the first request
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	requests := []*Request{
		{ReqID: "cancelled-req", UserID: "user1", Priority: 0.5, RequestTime: now, CancelCh: cancelledCtx.Done()},
		{ReqID: "valid-req", UserID: "user2", Priority: 1.0, RequestTime: now.Add(time.Second)},
	}

	for _, req := range requests {
		if err := pq.PushRequest(req); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	// Pop should skip cancelled request and return the valid one
	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "valid-req" {
		t.Errorf("Expected valid-req, got %s", result.ReqID)
	}
}

func TestPopWhenAvailable_SkipsTimedOutRequests(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)
	defer pq.Close()

	now := time.Now()

	// Create an already-expired context
	expiredCtx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Second))
	defer cancel()

	requests := []*Request{
		{ReqID: "expired-req", UserID: "user1", Priority: 0.5, RequestTime: now, CancelCh: expiredCtx.Done()},
		{ReqID: "fresh-req", UserID: "user2", Priority: 1.0, RequestTime: now.Add(time.Second)},
	}

	for _, req := range requests {
		if err := pq.PushRequest(req); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "fresh-req" {
		t.Errorf("Expected fresh-req, got %s", result.ReqID)
	}
}

func TestClose_DrainsPendingWaiters(t *testing.T) {
	pq := NewRequestPriorityQueue(nil)

	// Push requests that will remain in the queue
	for i := 0; i < 5; i++ {
		req := &Request{
			ReqID:       fmt.Sprintf("req-%d", i),
			UserID:      "user1",
			ModelName:   "model-1",
			Priority:    float64(i),
			RequestTime: time.Now(),
			NotifyChan:  make(chan struct{}),
		}
		if err := pq.PushRequest(req); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	if pq.Len() != 5 {
		t.Errorf("Expected 5 items before close, got %d", pq.Len())
	}

	// Close should drain the heap
	pq.Close()

	if pq.Len() != 0 {
		t.Errorf("Expected 0 items after close, got %d", pq.Len())
	}
}

func TestNewRequestPriorityQueueWithConfig(t *testing.T) {
	cfg := FairnessQueueConfig{
		MaxConcurrent:             5,
		MaxQPS:                    50,
		MaxPriorityRefreshRetries: 3,
		RebuildThreshold:          32,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, nil)
	if pq == nil {
		t.Fatal("NewRequestPriorityQueueWithConfig returned nil")
	}
	if pq.sem == nil {
		t.Error("Expected semaphore to be initialized with MaxConcurrent > 0")
	}
	if cap(pq.sem) != 5 {
		t.Errorf("Expected sem capacity 5, got %d", cap(pq.sem))
	}
	if pq.config.MaxQPS != 50 {
		t.Errorf("Expected MaxQPS 50, got %d", pq.config.MaxQPS)
	}
}

func TestNewRequestPriorityQueueWithConfig_NoSemaphore(t *testing.T) {
	cfg := FairnessQueueConfig{
		MaxConcurrent: 0,
		MaxQPS:        100,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, nil)
	if pq.sem != nil {
		t.Error("Expected nil semaphore with MaxConcurrent = 0")
	}
}

func TestRun_SemaphoreMode(t *testing.T) {
	cfg := FairnessQueueConfig{
		MaxConcurrent: 2,
		MaxQPS:        0, // no QPS cap
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, nil)
	defer pq.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	requests := make([]*Request, 4)
	notifyChs := make([]chan struct{}, 4)
	for i := 0; i < 4; i++ {
		notifyCh := make(chan struct{})
		notifyChs[i] = notifyCh
		req := &Request{
			ReqID:       fmt.Sprintf("req-%d", i),
			UserID:      fmt.Sprintf("user-%d", i),
			ModelName:   "test-model",
			Priority:    float64(i),
			RequestTime: time.Now(),
			NotifyChan:  notifyCh,
		}
		requests[i] = req
		if err := pq.PushRequest(req); err != nil {
			t.Fatalf("PushRequest failed: %v", err)
		}
	}

	go pq.Run(ctx, 0)

	// Wait for first 2 to be notified (semaphore capacity = 2)
	for i := 0; i < 2; i++ {
		select {
		case <-notifyChs[i]:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("Request %d was not dequeued in time", i)
		}
	}

	// Third request should NOT be dequeued yet (sem full)
	select {
	case <-notifyChs[2]:
		t.Error("Request 2 should not have been dequeued while semaphore is full")
	case <-time.After(100 * time.Millisecond):
		// expected
	}

	// Complete first request to release a permit
	requests[0].Release()

	// Third request should now be dequeued
	select {
	case <-notifyChs[2]:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Error("Request 2 should have been dequeued after permit release")
	}

	// Complete remaining
	requests[1].Release()
	requests[2].Release()

	select {
	case <-notifyChs[3]:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Error("Request 3 should have been dequeued")
	}
	requests[3].Release()
}

// mockTokenTracker implements TokenTracker for testing priority refresh.
type mockTokenTracker struct {
	mu            sync.Mutex
	counts        map[string]float64 // key: "user|model"
	requestCounts map[string]int
}

func newMockTokenTracker() *mockTokenTracker {
	return &mockTokenTracker{
		counts:        make(map[string]float64),
		requestCounts: make(map[string]int),
	}
}

func (m *mockTokenTracker) SetTokenCount(user, model string, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[user+"|"+model] = count
}

func (m *mockTokenTracker) GetTokenCount(user, model string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[user+"|"+model], nil
}

func (m *mockTokenTracker) SetRequestCount(user, model string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestCounts[user+"|"+model] = count
}

func (m *mockTokenTracker) UpdateTokenCount(user, model string, inputTokens, outputTokens float64) error {
	return nil
}

func (m *mockTokenTracker) GetRequestCount(user, model string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requestCounts[user+"|"+model], nil
}

func TestPriorityRefresh_ReinsertOnDrift(t *testing.T) {
	tracker := newMockTokenTracker()
	tracker.SetTokenCount("user-low", "model-1", 1.0)
	tracker.SetTokenCount("user-high", "model-1", 10.0)

	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 3,
		RebuildThreshold:          64,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	// user-low has low initial priority (should be dequeued first)
	req1 := &Request{ReqID: "low", UserID: "user-low", ModelName: "model-1", Priority: 1.0, RequestTime: now}
	// user-high has high initial priority
	req2 := &Request{ReqID: "high", UserID: "user-high", ModelName: "model-1", Priority: 10.0, RequestTime: now.Add(time.Second)}

	pq.PushRequest(req1)
	pq.PushRequest(req2)

	// Now drift user-low to be high priority (more tokens used)
	tracker.SetTokenCount("user-low", "model-1", 20.0)
	// And user-high to be low priority
	tracker.SetTokenCount("user-high", "model-1", 2.0)

	// Pop should detect the drift and reinsert user-low, returning user-high instead
	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "high" {
		t.Errorf("Expected 'high' (user-high now has lower token count), got %s", result.ReqID)
	}

	// Pop again should get the reinserted request
	result2, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result2.ReqID != "low" {
		t.Errorf("Expected 'low' on second pop, got %s", result2.ReqID)
	}
}

func TestPriorityRefresh_HeapRebuild(t *testing.T) {
	tracker := newMockTokenTracker()
	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 1, // Will rebuild after 1 reinsert
		RebuildThreshold:          64,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	// Set initial priorities: user-a=1, user-b=5, user-c=10
	tracker.SetTokenCount("user-a", "model-1", 1.0)
	tracker.SetTokenCount("user-b", "model-1", 5.0)
	tracker.SetTokenCount("user-c", "model-1", 10.0)

	pq.PushRequest(&Request{ReqID: "a", UserID: "user-a", ModelName: "model-1", Priority: 1.0, RequestTime: now})
	pq.PushRequest(&Request{ReqID: "b", UserID: "user-b", ModelName: "model-1", Priority: 5.0, RequestTime: now.Add(time.Second)})
	pq.PushRequest(&Request{ReqID: "c", UserID: "user-c", ModelName: "model-1", Priority: 10.0, RequestTime: now.Add(2 * time.Second)})

	// Drift user-a to highest usage
	tracker.SetTokenCount("user-a", "model-1", 100.0)
	// user-b stays
	// user-c becomes lowest
	tracker.SetTokenCount("user-c", "model-1", 0.5)

	// Pop should eventually rebuild and return user-c (lowest after refresh)
	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "c" {
		t.Errorf("Expected 'c' (lowest after rebuild), got %s", result.ReqID)
	}
}

func TestRelease_ReleasesPermit(t *testing.T) {
	cfg := FairnessQueueConfig{
		MaxConcurrent: 1,
		MaxQPS:        0,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, nil)
	defer pq.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Push 2 requests
	notifyCh1 := make(chan struct{})
	req1 := &Request{
		ReqID: "req-1", UserID: "user1", ModelName: "m1",
		Priority: 1.0, RequestTime: time.Now(),
		NotifyChan: notifyCh1,
	}
	pq.PushRequest(req1)

	notifyCh2 := make(chan struct{})
	req2 := &Request{
		ReqID: "req-2", UserID: "user2", ModelName: "m1",
		Priority: 2.0, RequestTime: time.Now(),
		NotifyChan: notifyCh2,
	}
	pq.PushRequest(req2)

	go pq.Run(ctx, 0)

	// First request should be dequeued
	select {
	case <-notifyCh1:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("First request should have been dequeued")
	}

	// Second should be blocked (sem capacity = 1)
	select {
	case <-notifyCh2:
		t.Fatal("Second request should not be dequeued while first is in-flight")
	case <-time.After(100 * time.Millisecond):
	}

	// Release first permit from the handler side
	req1.Release()

	// Second should now be dequeued
	select {
	case <-notifyCh2:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Second request should have been dequeued after release")
	}
	req2.Release()
}

func TestPriorityRefresh_SkipsHeapRebuildAboveThreshold(t *testing.T) {
	tracker := newMockTokenTracker()
	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 1,
		RebuildThreshold:          1,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	tracker.SetTokenCount("user-a", "model-1", 1.0)
	tracker.SetTokenCount("user-b", "model-1", 2.0)

	pq.PushRequest(&Request{ReqID: "a", UserID: "user-a", ModelName: "model-1", Priority: 1.0, RequestTime: now})
	pq.PushRequest(&Request{ReqID: "b", UserID: "user-b", ModelName: "model-1", Priority: 2.0, RequestTime: now.Add(time.Second)})

	tracker.SetTokenCount("user-a", "model-1", 10.0)
	tracker.SetTokenCount("user-b", "model-1", 1.5)

	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "b" {
		t.Fatalf("Expected b to be dequeued after refresh, got %s", result.ReqID)
	}
}

func TestPriorityRefresh_UsesCompositePriority(t *testing.T) {
	tracker := newMockTokenTracker()
	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 2,
		RebuildThreshold:          64,
		TokenWeight:               1.0,
		RequestNumWeight:          10.0,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	tracker.SetTokenCount("user-a", "model-1", 1.0)
	tracker.SetTokenCount("user-b", "model-1", 2.0)
	tracker.SetRequestCount("user-a", "model-1", 0)
	tracker.SetRequestCount("user-b", "model-1", 0)

	pq.PushRequest(&Request{ReqID: "a", UserID: "user-a", ModelName: "model-1", Priority: 1.0, RequestTime: now})
	pq.PushRequest(&Request{ReqID: "b", UserID: "user-b", ModelName: "model-1", Priority: 2.0, RequestTime: now.Add(time.Second)})

	tracker.SetRequestCount("user-a", "model-1", 5)
	tracker.SetRequestCount("user-b", "model-1", 0)

	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "b" {
		t.Fatalf("Expected b to be dequeued after composite refresh, got %s", result.ReqID)
	}
}

func TestPriorityRefresh_PreservesPriorityOffset(t *testing.T) {
	tracker := newMockTokenTracker()
	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 1,
		RebuildThreshold:          64,
		TokenWeight:               1.0,
		RequestNumWeight:          0.0,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	tracker.SetTokenCount("user-a", "model-1", 1.0)

	req := &Request{
		ReqID:          "a",
		UserID:         "user-a",
		ModelName:      "model-1",
		Priority:       11.0,
		PriorityOffset: 10.0,
		RequestTime:    now,
		NotifyChan:     make(chan struct{}),
	}
	pq.PushRequest(req)

	// Drift historical usage; refreshed priority should be historical + offset.
	tracker.SetTokenCount("user-a", "model-1", 5.0)

	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.Priority != 15.0 {
		t.Fatalf("Expected refreshed priority 15.0 with offset preserved, got %v", result.Priority)
	}
}

func TestPriorityRefresh_DoesNotReinsertWhenFinalPriorityIsUnchanged(t *testing.T) {
	tracker := newMockTokenTracker()
	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 1,
		RebuildThreshold:          64,
		TokenWeight:               1.0,
		RequestNumWeight:          0.0,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	tracker.SetTokenCount("user-a", "model-1", 5.0)

	req := &Request{
		ReqID:          "a",
		UserID:         "user-a",
		ModelName:      "model-1",
		Priority:       15.0,
		PriorityOffset: 10.0,
		RequestTime:    now,
		NotifyChan:     make(chan struct{}),
	}
	pq.PushRequest(req)

	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.Priority != 15.0 {
		t.Fatalf("Expected unchanged final priority 15.0, got %v", result.Priority)
	}
	if len(pq.heap) != 0 {
		t.Fatalf("Expected queue to be empty after pop, got %d items", len(pq.heap))
	}
}

func TestRebuildHeap_PreservesPriorityOffset(t *testing.T) {
	tracker := newMockTokenTracker()
	cfg := FairnessQueueConfig{
		MaxConcurrent:             0,
		MaxQPS:                    100,
		MaxPriorityRefreshRetries: 1,
		RebuildThreshold:          64,
		TokenWeight:               1.0,
		RequestNumWeight:          0.0,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, tracker)
	defer pq.Close()

	now := time.Now()
	tracker.SetTokenCount("user-a", "model-1", 3.0)
	tracker.SetTokenCount("user-b", "model-1", 4.0)

	pq.PushRequest(&Request{ReqID: "a", UserID: "user-a", ModelName: "model-1", Priority: 13.0, PriorityOffset: 10.0, RequestTime: now})
	pq.PushRequest(&Request{ReqID: "b", UserID: "user-b", ModelName: "model-1", Priority: 24.0, PriorityOffset: 20.0, RequestTime: now.Add(time.Second)})

	pq.mu.Lock()
	pq.rebuildHeap()
	pq.mu.Unlock()

	result, err := pq.popWhenAvailable(context.Background())
	if err != nil {
		t.Fatalf("popWhenAvailable failed: %v", err)
	}
	if result.ReqID != "a" {
		t.Fatalf("Expected request a to remain lower priority (3+10), got %s", result.ReqID)
	}
	if result.Priority != 13.0 {
		t.Fatalf("Expected rebuilt priority 13.0 for request a, got %v", result.Priority)
	}
}

func TestRun_SemaphoreMode_EmptyQueueDoesNotConsumePermit(t *testing.T) {
	cfg := FairnessQueueConfig{
		MaxConcurrent: 1,
		MaxQPS:        0,
	}
	pq := NewRequestPriorityQueueWithConfig(nil, cfg, nil)
	defer pq.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go pq.Run(ctx, 0)
	time.Sleep(50 * time.Millisecond)

	notifyCh := make(chan struct{})
	req := &Request{
		ReqID:       "req-1",
		UserID:      "user1",
		ModelName:   "m1",
		Priority:    1.0,
		RequestTime: time.Now(),
		NotifyChan:  notifyCh,
	}
	if err := pq.PushRequest(req); err != nil {
		t.Fatalf("PushRequest failed: %v", err)
	}

	select {
	case <-notifyCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Request should have been dequeued after queue starts empty")
	}
	req.Release()
}
