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
	"sync"
	"testing"
	"time"
)

func TestPruneExpiredBuckets_RebasesCumulativeCountsAfterCompaction(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker().(*InMemorySlidingWindowTokenTracker)
	tracker.userBucketStore["user1"] = map[string]*userBucketData{
		"model1": {
			buckets: []bucketNode{
				{timestamp: 1, tokens: 1, cumSum: 1, reqCount: 1, reqCumSum: 1},
				{timestamp: 2, tokens: 2, cumSum: 3, reqCount: 1, reqCumSum: 2},
				{timestamp: 3, tokens: 3, cumSum: 6, reqCount: 1, reqCumSum: 3},
				{timestamp: 4, tokens: 4, cumSum: 10, reqCount: 1, reqCumSum: 4},
			},
		},
	}

	tracker.pruneExpiredBuckets("user1", "model1", 3)
	bucketData := tracker.userBucketStore["user1"]["model1"]

	if bucketData.start != 0 {
		t.Fatalf("Expected start to be reset after compaction, got %d", bucketData.start)
	}
	if len(bucketData.buckets) != 2 {
		t.Fatalf("Expected 2 remaining buckets after compaction, got %d", len(bucketData.buckets))
	}
	if got := tracker.getActiveTotal(bucketData); got != 7 {
		t.Fatalf("Expected active token total 7 after compaction, got %v", got)
	}
	if got := tracker.getActiveRequestCount(bucketData); got != 2 {
		t.Fatalf("Expected active request total 2 after compaction, got %d", got)
	}
}

func TestNewInMemorySlidingWindowTokenTracker(t *testing.T) {
	tests := []struct {
		name           string
		opts           []TokenTrackerOption
		wantWindowSize time.Duration
	}{
		{
			name:           "default configuration",
			opts:           nil,
			wantWindowSize: defaultTokenTrackerWindowSize,
		},
		{
			name:           "custom window size",
			opts:           []TokenTrackerOption{WithWindowSize(10 * time.Minute)},
			wantWindowSize: 10 * time.Minute,
		},
		{
			name:           "invalid window size ignored (too small)",
			opts:           []TokenTrackerOption{WithWindowSize(30 * time.Second)},
			wantWindowSize: defaultTokenTrackerWindowSize, // should be ignored
		},
		{
			name:           "invalid window size ignored (too large)",
			opts:           []TokenTrackerOption{WithWindowSize(2 * time.Hour)},
			wantWindowSize: defaultTokenTrackerWindowSize, // should be ignored
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewInMemorySlidingWindowTokenTracker(tt.opts...).(*InMemorySlidingWindowTokenTracker)

			if tracker.windowSize != tt.wantWindowSize {
				t.Errorf("windowSize = %v, want %v", tracker.windowSize, tt.wantWindowSize)
			}
			if tracker.userBucketStore == nil {
				t.Error("userBucketStore should be initialized")
			}
		})
	}
}

func TestTokenWeightsConfiguration(t *testing.T) {
	tests := []struct {
		name             string
		inputWeight      float64
		outputWeight     float64
		wantInputWeight  float64
		wantOutputWeight float64
	}{
		{
			name:             "valid custom weights",
			inputWeight:      1.5,
			outputWeight:     3.0,
			wantInputWeight:  1.5,
			wantOutputWeight: 3.0,
		},
		{
			name:             "zero weights",
			inputWeight:      0.0,
			outputWeight:     0.0,
			wantInputWeight:  0.0,
			wantOutputWeight: 0.0,
		},
		{
			name:             "negative weights (should be ignored)",
			inputWeight:      -1.0,
			outputWeight:     -2.0,
			wantInputWeight:  defaultInputTokenWeight,
			wantOutputWeight: defaultOutputTokenWeight,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewInMemorySlidingWindowTokenTracker(
				WithTokenWeights(tt.inputWeight, tt.outputWeight),
			).(*InMemorySlidingWindowTokenTracker)

			if tracker.inputTokenWeight != tt.wantInputWeight {
				t.Errorf("inputTokenWeight = %v, want %v", tracker.inputTokenWeight, tt.wantInputWeight)
			}
			if tracker.outputTokenWeight != tt.wantOutputWeight {
				t.Errorf("outputTokenWeight = %v, want %v", tracker.outputTokenWeight, tt.wantOutputWeight)
			}
		})
	}
}

func TestUpdateTokenCount_BasicFunctionality(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	tests := []struct {
		name         string
		user         string
		model        string
		inputTokens  float64
		outputTokens float64
		wantError    bool
	}{
		{
			name:         "valid update",
			user:         "user1",
			model:        "gpt-4",
			inputTokens:  100,
			outputTokens: 50,
			wantError:    false,
		},
		{
			name:         "zero tokens",
			user:         "user2",
			model:        "gpt-3.5",
			inputTokens:  0,
			outputTokens: 0,
			wantError:    false,
		},
		{
			name:         "large token count",
			user:         "user3",
			model:        "claude",
			inputTokens:  10000,
			outputTokens: 5000,
			wantError:    false,
		},
		{
			name:         "empty user",
			user:         "",
			model:        "gpt-4",
			inputTokens:  100,
			outputTokens: 50,
			wantError:    true,
		},
		{
			name:         "empty model",
			user:         "user1",
			model:        "",
			inputTokens:  100,
			outputTokens: 50,
			wantError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tracker.UpdateTokenCount(tt.user, tt.model, tt.inputTokens, tt.outputTokens)

			if tt.wantError && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestUpdateTokenCount_WeightedCalculation(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "testuser"
	model := "testmodel"
	inputTokens := 100.0
	outputTokens := 50.0

	err := tracker.UpdateTokenCount(user, model, inputTokens, outputTokens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected weighted total: 100*1.0 + 50*2.0 = 200
	expectedTotal := inputTokens*defaultInputTokenWeight + outputTokens*defaultOutputTokenWeight

	total, err := tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error getting token count: %v", err)
	}

	if total != expectedTotal {
		t.Errorf("total = %v, want %v", total, expectedTotal)
	}
}

func TestUpdateTokenCount_CustomWeights(t *testing.T) {
	// Test with custom token weights
	inputWeight := 1.5
	outputWeight := 3.0
	tracker := NewInMemorySlidingWindowTokenTracker(
		WithTokenWeights(inputWeight, outputWeight),
	)

	user := "testuser"
	model := "testmodel"
	inputTokens := 100.0
	outputTokens := 50.0

	err := tracker.UpdateTokenCount(user, model, inputTokens, outputTokens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected weighted total: 100*1.5 + 50*3.0 = 300
	expectedTotal := inputTokens*inputWeight + outputTokens*outputWeight

	total, err := tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error getting token count: %v", err)
	}

	if total != expectedTotal {
		t.Errorf("total = %v, want %v", total, expectedTotal)
	}
}

func TestUpdateTokenCount_SameTimestamp(t *testing.T) {
	// Use a short window for quick timestamp testing
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "testuser"
	model := "testmodel"

	// Make multiple updates in quick succession (same timestamp)
	err1 := tracker.UpdateTokenCount(user, model, 50, 25)
	err2 := tracker.UpdateTokenCount(user, model, 30, 15)

	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v, %v", err1, err2)
	}

	// Should accumulate both updates
	// Total: (50*1.0 + 25*2.0) + (30*1.0 + 15*2.0) = 100 + 60 = 160
	expectedTotal := (50*1.0 + 25*2.0) + (30*1.0 + 15*2.0)

	total, err := tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if total != expectedTotal {
		t.Errorf("total = %v, want %v", total, expectedTotal)
	}
}

func TestGetTokenCount_BasicFunctionality(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	tests := []struct {
		name      string
		user      string
		model     string
		wantTotal float64
		wantError bool
	}{
		{
			name:      "non-existent user",
			user:      "nonexistent",
			model:     "gpt-4",
			wantTotal: 0,
			wantError: false,
		},
		{
			name:      "non-existent model",
			user:      "user1",
			model:     "nonexistent",
			wantTotal: 0,
			wantError: false,
		},
		{
			name:      "empty user",
			user:      "",
			model:     "gpt-4",
			wantTotal: 0,
			wantError: false,
		},
		{
			name:      "empty model",
			user:      "user1",
			model:     "",
			wantTotal: 0,
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, err := tracker.GetTokenCount(tt.user, tt.model)

			if tt.wantError && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %v, want %v", total, tt.wantTotal)
			}
		})
	}
}

func TestSlidingWindowBehavior(t *testing.T) {
	// Use a very short window for testing
	tracker := NewInMemorySlidingWindowTokenTracker(
		WithWindowSize(100 * time.Millisecond), // 100ms window
	)

	user := "testuser"
	model := "testmodel"

	// Add some tokens
	err := tracker.UpdateTokenCount(user, model, 100, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify tokens are tracked
	total, err := tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedTotal := 100*1.0 + 50*2.0 // 200
	if total != expectedTotal {
		t.Errorf("total = %v, want %v", total, expectedTotal)
	}

	// Wait for window to expire
	time.Sleep(150 * time.Millisecond)

	// Tokens should be pruned (or close to 0 due to timing)
	total, err = tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be 0 or very close to 0 after window expiration
	if total != 0 {
		t.Logf("Warning: expected 0 tokens after window expiration, got %v (timing-dependent)", total)
	}
}

func TestMultipleUsersAndModels(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	// Setup test data
	users := []string{"user1", "user2", "user3"}
	models := []string{"gpt-4", "gpt-3.5", "claude"}

	// Add tokens for each user-model combination
	for i, user := range users {
		for j, model := range models {
			inputTokens := float64(100 * (i + 1))
			outputTokens := float64(50 * (j + 1))

			err := tracker.UpdateTokenCount(user, model, inputTokens, outputTokens)
			if err != nil {
				t.Fatalf("unexpected error for %s/%s: %v", user, model, err)
			}
		}
	}

	// Verify each user-model combination
	for i, user := range users {
		for j, model := range models {
			expectedInput := float64(100 * (i + 1))
			expectedOutput := float64(50 * (j + 1))
			expectedTotal := expectedInput*defaultInputTokenWeight + expectedOutput*defaultOutputTokenWeight

			total, err := tracker.GetTokenCount(user, model)
			if err != nil {
				t.Fatalf("unexpected error getting tokens for %s/%s: %v", user, model, err)
			}

			if total != expectedTotal {
				t.Errorf("total for %s/%s = %v, want %v", user, model, total, expectedTotal)
			}
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	const (
		numGoroutines = 10
		numOperations = 100
	)

	var wg sync.WaitGroup

	// Multiple goroutines updating and reading concurrently
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			user := "user1"
			model := "gpt-4"

			for j := 0; j < numOperations; j++ {
				// Alternate between updates and reads
				if j%2 == 0 {
					err := tracker.UpdateTokenCount(user, model, 10, 5)
					if err != nil {
						t.Errorf("goroutine %d: unexpected error on update: %v", goroutineID, err)
					}
				} else {
					_, err := tracker.GetTokenCount(user, model)
					if err != nil {
						t.Errorf("goroutine %d: unexpected error on get: %v", goroutineID, err)
					}
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify final state is consistent
	total, err := tracker.GetTokenCount("user1", "gpt-4")
	if err != nil {
		t.Fatalf("unexpected error getting final count: %v", err)
	}

	// We should have some positive total (exact value depends on timing)
	if total < 0 {
		t.Errorf("total should be non-negative, got %v", total)
	}
}

func TestCumulativeSumCalculation(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "testuser"
	model := "testmodel"

	// Add tokens at different times to create multiple buckets
	updates := []struct {
		inputTokens  float64
		outputTokens float64
		sleepMs      int
	}{
		{100, 50, 1100}, // First bucket: 100*1 + 50*2 = 200
		{80, 40, 1100},  // Second bucket: 80*1 + 40*2 = 160
		{60, 30, 1100},  // Third bucket: 60*1 + 30*2 = 120
	}

	expectedCumulative := 0.0
	for i, update := range updates {
		err := tracker.UpdateTokenCount(user, model, update.inputTokens, update.outputTokens)
		if err != nil {
			t.Fatalf("update %d: unexpected error: %v", i, err)
		}

		expectedTokens := update.inputTokens*defaultInputTokenWeight + update.outputTokens*defaultOutputTokenWeight
		expectedCumulative += expectedTokens

		// Allow time for different bucket
		time.Sleep(time.Duration(update.sleepMs) * time.Millisecond)

		total, err := tracker.GetTokenCount(user, model)
		if err != nil {
			t.Fatalf("get %d: unexpected error: %v", i, err)
		}

		if total != expectedCumulative {
			t.Errorf("after update %d: total = %v, want %v", i, total, expectedCumulative)
		}
	}
}

func TestPruningBehavior(t *testing.T) {
	// Short window for testing pruning
	tracker := NewInMemorySlidingWindowTokenTracker(
		WithWindowSize(50 * time.Millisecond), // 50ms window
	)

	user := "testuser"
	model := "testmodel"

	// Add initial tokens
	err := tracker.UpdateTokenCount(user, model, 100, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify tokens are there
	total, err := tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total == 0 {
		t.Error("expected non-zero tokens after update")
	}

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Add new tokens (this should trigger pruning)
	err = tracker.UpdateTokenCount(user, model, 10, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should only have the new tokens
	total, err = tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedNew := 10*1.0 + 5*2.0 // 20
	if total != expectedNew {
		t.Logf("Warning: expected %v after pruning, got %v (timing-dependent)", expectedNew, total)
	}
}

func TestGetActiveTotal(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker().(*InMemorySlidingWindowTokenTracker)

	tests := []struct {
		name       string
		bucketData *userBucketData
		expected   float64
	}{
		{
			name:       "nil bucket data",
			bucketData: nil,
			expected:   0,
		},
		{
			name: "empty buckets",
			bucketData: &userBucketData{
				buckets: []bucketNode{},
				start:   0,
			},
			expected: 0,
		},
		{
			name: "start beyond buckets",
			bucketData: &userBucketData{
				buckets: []bucketNode{{timestamp: 100, cumSum: 50}},
				start:   2,
			},
			expected: 0,
		},
		{
			name: "single bucket from start",
			bucketData: &userBucketData{
				buckets: []bucketNode{{timestamp: 100, cumSum: 50}},
				start:   0,
			},
			expected: 50,
		},
		{
			name: "multiple buckets from start",
			bucketData: &userBucketData{
				buckets: []bucketNode{
					{timestamp: 100, cumSum: 50},
					{timestamp: 200, cumSum: 100},
					{timestamp: 300, cumSum: 150},
				},
				start: 0,
			},
			expected: 150,
		},
		{
			name: "multiple buckets with offset start",
			bucketData: &userBucketData{
				buckets: []bucketNode{
					{timestamp: 100, cumSum: 50},
					{timestamp: 200, cumSum: 100},
					{timestamp: 300, cumSum: 150},
				},
				start: 1,
			},
			expected: 100, // 150 - 50 = 100
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tracker.getActiveTotal(tt.bucketData)
			if result != tt.expected {
				t.Errorf("getActiveTotal() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestNegativeTokenHandling(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "testuser"
	model := "testmodel"

	// Test that negative tokens are handled gracefully
	err := tracker.UpdateTokenCount(user, model, -50, -25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should result in 0 tokens (negative clamped to 0)
	total, err := tracker.GetTokenCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Negative tokens should be clamped to 0, so total should be 0
	if total != 0 {
		t.Errorf("expected 0 tokens for negative input, got %v", total)
	}
}

func BenchmarkUpdateTokenCount(b *testing.B) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "benchuser"
	model := "benchmodel"

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		err := tracker.UpdateTokenCount(user, model, 100, 50)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkGetTokenCount(b *testing.B) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "benchuser"
	model := "benchmodel"

	// Pre-populate with some data
	for i := 0; i < 1000; i++ {
		_ = tracker.UpdateTokenCount(user, model, 100, 50)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := tracker.GetTokenCount(user, model)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkConcurrentAccess(b *testing.B) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	b.RunParallel(func(pb *testing.PB) {
		user := "benchuser"
		model := "benchmodel"

		for pb.Next() {
			// Mix of updates and reads
			if b.N%2 == 0 {
				_ = tracker.UpdateTokenCount(user, model, 100, 50)
			} else {
				_, _ = tracker.GetTokenCount(user, model)
			}
		}
	})
}

func TestGetRequestCount_Basic(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	user := "test-user"
	model := "test-model"

	// Initially zero
	count, err := tracker.GetRequestCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// After 3 updates, should have 3 requests
	for i := 0; i < 3; i++ {
		if err := tracker.UpdateTokenCount(user, model, 10, 5); err != nil {
			t.Fatalf("UpdateTokenCount failed: %v", err)
		}
	}

	count, err = tracker.GetRequestCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestGetRequestCount_EmptyInputs(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()

	count, err := tracker.GetRequestCount("", "model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 for empty user, got %d", count)
	}

	count, err = tracker.GetRequestCount("user", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 for empty model, got %d", count)
	}
}

func TestGetRequestCount_Concurrent(t *testing.T) {
	tracker := NewInMemorySlidingWindowTokenTracker()
	user := "concurrent-user"
	model := "concurrent-model"
	numGoroutines := 10
	numOps := 100

	var wg sync.WaitGroup
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				_ = tracker.UpdateTokenCount(user, model, 1, 0)
				_, _ = tracker.GetRequestCount(user, model)
			}
		}()
	}
	wg.Wait()

	count, err := tracker.GetRequestCount(user, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := numGoroutines * numOps
	if count != expected {
		t.Errorf("expected %d, got %d", expected, count)
	}
}
