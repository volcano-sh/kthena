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
