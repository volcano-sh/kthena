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
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Sliding window configuration
const (
	defaultTokenTrackerWindowSize = 5 * time.Minute // Default window size 5 minutes
	defaultInputTokenWeight       = 1.0
	defaultOutputTokenWeight      = 2.0
)

// TokenTracker tracks token usage per user
type TokenTracker interface {
	GetTokenCount(user, model string) (float64, error)
	UpdateTokenCount(user, model string, inputTokens, outputTokens float64) error
	GetRequestCount(user, model string) (int, error)
}

// bucketNode stores the data for a single time bucket with cumulative sum.
type bucketNode struct {
	timestamp int64
	tokens    float64 // Token count for this specific bucket
	cumSum    float64 // Cumulative sum up to this bucket (inclusive)
	reqCount  int     // Request count for this specific bucket
	reqCumSum int     // Cumulative request count up to this bucket (inclusive)
}

// userBucketData holds the token tracking data structures for a single user using a slice-based buffer.
type userBucketData struct {
	buckets []bucketNode // Monotonically increasing by timestamp with cumulative sums
	start   int          // Index of the first valid bucket in the sliding window
}

// InMemorySlidingWindowTokenTracker tracks tokens per user in a fixed-size sliding window (in-memory, thread-safe).
// This function refers to aibrix(https://github.com/vllm-project/aibrix/)
type InMemorySlidingWindowTokenTracker struct {
	mu                sync.RWMutex
	windowSize        time.Duration
	inputTokenWeight  float64
	outputTokenWeight float64
	userBucketStore   map[string]map[string]*userBucketData // [user][model] -> buckets
}

// TokenTrackerOption is a function that configures a token tracker
type TokenTrackerOption func(*InMemorySlidingWindowTokenTracker)

// WithWindowSize overrides the default window size
func WithWindowSize(size time.Duration) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		if size <= 0 {
			return
		}
		// This is to prevent unlimited memory cost
		if size < time.Minute || size > time.Hour {
			klog.Warningf("Invalid window size %v, must be between 1 minute and 1 hour", size)
			return
		}
		t.windowSize = size
	}
}

// WithTokenWeights overrides the default token weights
func WithTokenWeights(inputWeight, outputWeight float64) TokenTrackerOption {
	return func(t *InMemorySlidingWindowTokenTracker) {
		if inputWeight < 0 || outputWeight < 0 {
			klog.Warningf("Invalid token weights: input=%v, output=%v. Weights must be non-negative", inputWeight, outputWeight)
			return
		}
		t.inputTokenWeight = inputWeight
		t.outputTokenWeight = outputWeight
	}
}

// NewInMemorySlidingWindowTokenTracker creates a new tracker with optional configuration
func NewInMemorySlidingWindowTokenTracker(opts ...TokenTrackerOption) TokenTracker {
	// Use internal defaults; callers can override via options
	tracker := &InMemorySlidingWindowTokenTracker{
		windowSize:        defaultTokenTrackerWindowSize,
		inputTokenWeight:  defaultInputTokenWeight,
		outputTokenWeight: defaultOutputTokenWeight,
		userBucketStore:   make(map[string]map[string]*userBucketData),
	}

	for _, opt := range opts {
		opt(tracker)
	}
	return tracker
}

func (t *InMemorySlidingWindowTokenTracker) getCutoffTimestamp() int64 {
	cutoffTime := time.Now().Add(-t.windowSize)
	return cutoffTime.Unix()
}

// Caller must hold the write lock
func (t *InMemorySlidingWindowTokenTracker) pruneExpiredBuckets(user, model string, cutoff int64) {
	modelBuckets, userOk := t.userBucketStore[user]
	if !userOk {
		return
	}
	bucketData, modelOk := modelBuckets[model]
	if !modelOk || bucketData == nil || bucketData.start >= len(bucketData.buckets) {
		return
	}

	// Simple linear scan from start to find first bucket >= cutoff
	newStart := bucketData.start
	for newStart < len(bucketData.buckets) && bucketData.buckets[newStart].timestamp < cutoff {
		newStart++
	}

	// Update start index
	bucketData.start = newStart

	// Compact if we've skipped more than half the slice
	if newStart > 0 && newStart >= len(bucketData.buckets)/2 {
		tokenBase := 0.0
		requestBase := 0
		if newStart > 0 {
			tokenBase = bucketData.buckets[newStart-1].cumSum
			requestBase = bucketData.buckets[newStart-1].reqCumSum
		}
		// More memory-efficient compaction: explicit copy to avoid holding reference to old array
		remainingCount := len(bucketData.buckets) - newStart
		newBuckets := make([]bucketNode, remainingCount)
		copy(newBuckets, bucketData.buckets[newStart:])
		for i := range newBuckets {
			newBuckets[i].cumSum -= tokenBase
			newBuckets[i].reqCumSum -= requestBase
		}
		bucketData.buckets = newBuckets
		bucketData.start = 0
	}
}

// getActiveTotal computes the total tokens for the active window
func (t *InMemorySlidingWindowTokenTracker) getActiveTotal(bucketData *userBucketData) float64 {
	if bucketData == nil || bucketData.start >= len(bucketData.buckets) {
		return 0
	}

	end := len(bucketData.buckets) - 1
	if end < bucketData.start {
		return 0
	}

	total := bucketData.buckets[end].cumSum
	if bucketData.start > 0 {
		total -= bucketData.buckets[bucketData.start-1].cumSum
	}
	return total
}

// getActiveRequestCount computes the total request count for the active window
func (t *InMemorySlidingWindowTokenTracker) getActiveRequestCount(bucketData *userBucketData) int {
	if bucketData == nil || bucketData.start >= len(bucketData.buckets) {
		return 0
	}

	end := len(bucketData.buckets) - 1
	if end < bucketData.start {
		return 0
	}

	total := bucketData.buckets[end].reqCumSum
	if bucketData.start > 0 {
		total -= bucketData.buckets[bucketData.start-1].reqCumSum
	}
	return total
}

// Time: Avg O(1) (amortized), Worst O(B_u) where B_u = buckets for user u | Space: O(1)
func (t *InMemorySlidingWindowTokenTracker) GetTokenCount(user, model string) (float64, error) {
	if user == "" || model == "" {
		return 0, nil
	}

	t.mu.RLock()
	modelBuckets, userOk := t.userBucketStore[user]
	if !userOk || modelBuckets == nil {
		t.mu.RUnlock()
		return 0, nil
	}
	bucketData, modelOk := modelBuckets[model]
	if !modelOk || bucketData == nil || bucketData.start >= len(bucketData.buckets) {
		t.mu.RUnlock()
		return 0, nil
	}

	cutoff := t.getCutoffTimestamp()
	needsPruning := false

	if bucketData.start < len(bucketData.buckets) {
		oldestTs := bucketData.buckets[bucketData.start].timestamp
		if oldestTs < cutoff {
			needsPruning = true
		}
	}
	t.mu.RUnlock()

	// Only acquire write lock if we need to prune
	if needsPruning {
		t.mu.Lock()
		// Re-check user data existence in case it was deleted between RUnlock and Lock
		if modelBuckets, userOk := t.userBucketStore[user]; userOk {
			if _, modelOk := modelBuckets[model]; modelOk {
				t.pruneExpiredBuckets(user, model, cutoff)
			}
		}
		t.mu.Unlock()
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	if modelBuckets, userOk := t.userBucketStore[user]; userOk {
		if bucketData, modelOk := modelBuckets[model]; modelOk {
			return t.getActiveTotal(bucketData), nil
		}
	}
	return 0, nil
}

func (t *InMemorySlidingWindowTokenTracker) UpdateTokenCount(user, model string, inputTokens, outputTokens float64) error {
	if user == "" || model == "" {
		return fmt.Errorf("user ID and model cannot be empty")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	currentTimestamp := now.Unix()
	cutoff := t.getCutoffTimestamp()

	if _, ok := t.userBucketStore[user]; !ok {
		t.userBucketStore[user] = make(map[string]*userBucketData)
	}
	if _, ok := t.userBucketStore[user][model]; !ok {
		t.userBucketStore[user][model] = &userBucketData{
			buckets: make([]bucketNode, 0, 128),
			start:   0,
		}
	}

	// Prune first before adding/updating to maintain window size constraint accurately
	t.pruneExpiredBuckets(user, model, cutoff)

	// Clamp negative tokens to zero
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}

	newTokens := inputTokens*t.inputTokenWeight + outputTokens*t.outputTokenWeight

	// Append or update the last bucket based on timestamp
	bucketData := t.userBucketStore[user][model]
	end := len(bucketData.buckets)
	if end > 0 && bucketData.buckets[end-1].timestamp == currentTimestamp {
		// Update last bucket
		bucketData.buckets[end-1].tokens += newTokens
		bucketData.buckets[end-1].cumSum += newTokens
		bucketData.buckets[end-1].reqCount++
		bucketData.buckets[end-1].reqCumSum++
	} else {
		// Append new bucket
		cumSum := newTokens
		reqCumSum := 1
		if end > 0 {
			cumSum += bucketData.buckets[end-1].cumSum
			reqCumSum += bucketData.buckets[end-1].reqCumSum
		}
		bucketData.buckets = append(bucketData.buckets, bucketNode{
			timestamp: currentTimestamp,
			tokens:    newTokens,
			cumSum:    cumSum,
			reqCount:  1,
			reqCumSum: reqCumSum,
		})
	}

	return nil
}

// GetRequestCount returns the total request count for a user/model in the current sliding window.
func (t *InMemorySlidingWindowTokenTracker) GetRequestCount(user, model string) (int, error) {
	if user == "" || model == "" {
		return 0, nil
	}

	t.mu.RLock()
	modelBuckets, userOk := t.userBucketStore[user]
	if !userOk || modelBuckets == nil {
		t.mu.RUnlock()
		return 0, nil
	}
	bucketData, modelOk := modelBuckets[model]
	if !modelOk || bucketData == nil || bucketData.start >= len(bucketData.buckets) {
		t.mu.RUnlock()
		return 0, nil
	}

	cutoff := t.getCutoffTimestamp()
	needsPruning := false

	if bucketData.start < len(bucketData.buckets) {
		oldestTs := bucketData.buckets[bucketData.start].timestamp
		if oldestTs < cutoff {
			needsPruning = true
		}
	}
	t.mu.RUnlock()

	if needsPruning {
		t.mu.Lock()
		if modelBuckets, userOk := t.userBucketStore[user]; userOk {
			if _, modelOk := modelBuckets[model]; modelOk {
				t.pruneExpiredBuckets(user, model, cutoff)
			}
		}
		t.mu.Unlock()
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	if modelBuckets, userOk := t.userBucketStore[user]; userOk {
		if bucketData, modelOk := modelBuckets[model]; modelOk {
			return t.getActiveRequestCount(bucketData), nil
		}
	}
	return 0, nil
}
