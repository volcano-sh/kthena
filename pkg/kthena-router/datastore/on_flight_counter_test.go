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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// mockOnFlightCounter is a fake OnFlightCounter for testing
type mockOnFlightCounter struct {
	counts    map[types.NamespacedName]int64
	failNext  bool
	failAdd   bool
}

func newMockOnFlightCounter() *mockOnFlightCounter {
	return &mockOnFlightCounter{
		counts: make(map[types.NamespacedName]int64),
	}
}

func (m *mockOnFlightCounter) Incr(ctx context.Context, podName types.NamespacedName) (int64, error) {
	if m.failNext {
		m.failNext = false
		return 0, fmt.Errorf("mock redis failure")
	}
	m.counts[podName]++
	return m.counts[podName], nil
}

func (m *mockOnFlightCounter) Decr(ctx context.Context, podName types.NamespacedName) (int64, error) {
	if m.failNext {
		m.failNext = false
		return 0, fmt.Errorf("mock redis failure")
	}
	m.counts[podName]--
	if m.counts[podName] < 0 {
		m.counts[podName] = 0
	}
	return m.counts[podName], nil
}

func (m *mockOnFlightCounter) Add(ctx context.Context, podName types.NamespacedName, delta int64) (int64, error) {
	if m.failAdd {
		return 0, fmt.Errorf("mock redis failure on add")
	}
	m.counts[podName] += delta
	if m.counts[podName] < 0 {
		m.counts[podName] = 0
	}
	return m.counts[podName], nil
}

func (m *mockOnFlightCounter) Delete(ctx context.Context, podName types.NamespacedName) error {
	delete(m.counts, podName)
	return nil
}

func (m *mockOnFlightCounter) BatchGet(ctx context.Context, podNames []types.NamespacedName) (map[types.NamespacedName]int64, error) {
	if m.failNext {
		m.failNext = false
		return nil, fmt.Errorf("mock redis batch get failure")
	}
	res := make(map[types.NamespacedName]int64)
	for _, name := range podNames {
		res[name] = m.counts[name]
	}
	return res, nil
}

func TestPendingOnFlightDeltaLogic(t *testing.T) {
	mockRedis := newMockOnFlightCounter()
	s := New(WithRedisOnFlightCounter(mockRedis))
	
	podName := types.NamespacedName{Namespace: "default", Name: "test-pod"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podName.Namespace,
			Name:      podName.Name,
		},
	}
	
	// Add pod to store
	err := s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{})
	if err != nil {
		t.Fatalf("Failed to add pod: %v", err)
	}
	
	podInfo := s.GetPodInfo(podName)
	if podInfo == nil {
		t.Fatalf("Pod not found in store")
	}
	
	// Test normal Incr
	s.IncrPodOnFlightRequests(podName)
	if podInfo.GetOnFlightRequestNum() != 1 {
		t.Errorf("Expected local count 1, got %d", podInfo.GetOnFlightRequestNum())
	}
	if mockRedis.counts[podName] != 1 {
		t.Errorf("Expected redis count 1, got %d", mockRedis.counts[podName])
	}
	if podInfo.pendingOnFlightDelta.Load() != 0 {
		t.Errorf("Expected pending delta 0, got %d", podInfo.pendingOnFlightDelta.Load())
	}
	
	// Test Redis failure on Incr
	mockRedis.failNext = true
	s.IncrPodOnFlightRequests(podName)
	if podInfo.GetOnFlightRequestNum() != 2 {
		t.Errorf("Expected local count 2, got %d", podInfo.GetOnFlightRequestNum())
	}
	if mockRedis.counts[podName] != 1 {
		t.Errorf("Expected redis count 1, got %d", mockRedis.counts[podName])
	}
	if podInfo.pendingOnFlightDelta.Load() != 1 {
		t.Errorf("Expected pending delta 1, got %d", podInfo.pendingOnFlightDelta.Load())
	}
	
	// Test Decr success while pending delta exists (does not overwrite local)
	// Local goes to 1, Redis goes to 0 (because we missed one Incr in Redis).
	// Because pending delta != 0, local should remain 1.
	s.DecrPodOnFlightRequests(podName)
	if podInfo.GetOnFlightRequestNum() != 1 {
		t.Errorf("Expected local count 1 after Decr, got %d", podInfo.GetOnFlightRequestNum())
	}
	if mockRedis.counts[podName] != 0 {
		t.Errorf("Expected redis count 0 after Decr, got %d", mockRedis.counts[podName])
	}
	if podInfo.pendingOnFlightDelta.Load() != 1 {
		t.Errorf("Expected pending delta 1, got %d", podInfo.pendingOnFlightDelta.Load())
	}
	
	// Test background loop (Run) flushes pending deltas
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	// Ensure run handles the pending delta
	s.Run(ctx)
	
	// Wait a bit for the background loop to process the pending delta
	time.Sleep(300 * time.Millisecond)
	
	if podInfo.pendingOnFlightDelta.Load() != 0 {
		t.Errorf("Expected pending delta to be flushed to 0, got %d", podInfo.pendingOnFlightDelta.Load())
	}
	if mockRedis.counts[podName] != 1 {
		t.Errorf("Expected redis count to be updated to 1 after background flush, got %d", mockRedis.counts[podName])
	}
	
	// Test SyncOnFlightCounts overwrites local count when pending is 0
	// Let's modify redis count to 5
	mockRedis.counts[podName] = 5
	s.SyncOnFlightCounts()
	if podInfo.GetOnFlightRequestNum() != 5 {
		t.Errorf("Expected local count to be synced to 5, got %d", podInfo.GetOnFlightRequestNum())
	}
	
	// Test Decr logic doesn't go below 0
	mockRedis.counts[podName] = 0
	podInfo.SetOnFlightRequestNum(0)
	
	s.DecrPodOnFlightRequests(podName) // local tries to go to -1, but should be clamped to 0
	if podInfo.GetOnFlightRequestNum() != 0 {
		t.Errorf("Expected local count to not drop below 0, got %d", podInfo.GetOnFlightRequestNum())
	}
}
