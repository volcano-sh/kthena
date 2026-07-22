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
	"math"
	"sync"
	"testing"
	"time"
)

func TestVTCTokenTracker_UpdateAndGetTokenCount(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)

	if err := tracker.UpdateTokenCount("alice", "model-a", 10, 5); err != nil {
		t.Fatalf("UpdateTokenCount returned error: %v", err)
	}

	want := 10*defaultInputTokenWeight + 5*defaultOutputTokenWeight
	got, err := tracker.GetTokenCount("alice", "model-a")
	if err != nil {
		t.Fatalf("GetTokenCount returned error: %v", err)
	}
	if got != want {
		t.Fatalf("GetTokenCount = %v, want %v", got, want)
	}

	// A second update accumulates rather than replaces.
	if err := tracker.UpdateTokenCount("alice", "model-a", 10, 5); err != nil {
		t.Fatalf("UpdateTokenCount returned error: %v", err)
	}
	got, _ = tracker.GetTokenCount("alice", "model-a")
	if got != want*2 {
		t.Fatalf("GetTokenCount after second update = %v, want %v", got, want*2)
	}
}

func TestVTCTokenTracker_UnknownUserReturnsZero(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)

	got, err := tracker.GetTokenCount("nobody", "model-a")
	if err != nil {
		t.Fatalf("GetTokenCount returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("GetTokenCount for unknown user = %v, want 0", got)
	}

	count, err := tracker.GetRequestCount("nobody", "model-a")
	if err != nil {
		t.Fatalf("GetRequestCount returned error: %v", err)
	}
	if count != 0 {
		t.Fatalf("GetRequestCount for unknown user = %v, want 0", count)
	}
}

func TestVTCTokenTracker_RequestCount(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)

	for range 3 {
		if err := tracker.UpdateTokenCount("bob", "model-a", 1, 1); err != nil {
			t.Fatalf("UpdateTokenCount returned error: %v", err)
		}
	}

	count, err := tracker.GetRequestCount("bob", "model-a")
	if err != nil {
		t.Fatalf("GetRequestCount returned error: %v", err)
	}
	if count != 3 {
		t.Fatalf("GetRequestCount = %d, want 3", count)
	}
}

// TestVTCTokenTracker_CounterLift verifies the core VTC fairness property:
// a user rejoining the active set is lifted to the minimum counter among
// currently active users, so idle time never earns them priority over a user
// who has been serving requests the whole time.
func TestVTCTokenTracker_CounterLift(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)

	// bob joins first, while no one else is active, so no lift applies. He
	// accrues light usage, then goes idle.
	tracker.OnRequestStart("bob", "model-a")
	if err := tracker.UpdateTokenCount("bob", "model-a", 5, 5); err != nil {
		t.Fatalf("UpdateTokenCount returned error: %v", err)
	}
	bobCount, _ := tracker.GetTokenCount("bob", "model-a")
	tracker.OnRequestFinish("bob", "model-a")

	// alice joins next, also with no one else active (bob is now idle), so she
	// starts at zero and accrues heavy usage while bob stays idle.
	tracker.OnRequestStart("alice", "model-a")
	if err := tracker.UpdateTokenCount("alice", "model-a", 500, 250); err != nil {
		t.Fatalf("UpdateTokenCount returned error: %v", err)
	}
	aliceCount, _ := tracker.GetTokenCount("alice", "model-a")
	if bobCount >= aliceCount {
		t.Fatalf("expected bob's stale counter (%v) to be lower than alice's active counter (%v)", bobCount, aliceCount)
	}

	// bob rejoins while alice is the only active user and alice's counter is
	// higher than bob's stale, low counter: bob must be lifted up to alice's
	// counter so his idle time doesn't let him jump the queue ahead of alice.
	tracker.OnRequestStart("bob", "model-a")
	liftedBobCount, _ := tracker.GetTokenCount("bob", "model-a")
	if liftedBobCount != aliceCount {
		t.Fatalf("bob counter after rejoin = %v, want lifted to active min %v", liftedBobCount, aliceCount)
	}

	// carol joins next with the same active minimum in play (alice=aliceCount,
	// bob=aliceCount). Her stale counter of 0 is below the min, so she is also
	// lifted, and the lift must not exceed the min (it should equal it exactly).
	tracker.OnRequestStart("carol", "model-a")
	carolCount, _ := tracker.GetTokenCount("carol", "model-a")
	if carolCount != aliceCount {
		t.Fatalf("carol counter after joining = %v, want lifted to active min %v", carolCount, aliceCount)
	}

	// A user whose counter is already above the active minimum must not be
	// lowered by the lift: finish and rejoin alice (still highest) and confirm
	// her counter is unchanged.
	tracker.OnRequestFinish("alice", "model-a")
	tracker.OnRequestStart("alice", "model-a")
	aliceCountAfter, _ := tracker.GetTokenCount("alice", "model-a")
	if aliceCountAfter != aliceCount {
		t.Fatalf("alice counter changed on rejoin: got %v, want unchanged %v", aliceCountAfter, aliceCount)
	}
}

func TestVTCTokenTracker_OnRequestStartIgnoresEmptyIDs(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)
	// Must not panic and must not create bogus state.
	tracker.OnRequestStart("", "model-a")
	tracker.OnRequestFinish("", "model-a")
	tracker.OnRequestStart("user", "")
	tracker.OnRequestFinish("user", "")

	tracker.mu.RLock()
	stateCount := len(tracker.userState)
	tracker.mu.RUnlock()
	if stateCount != 0 {
		t.Fatalf("expected no state created for empty user/model, got %d entries", stateCount)
	}
}

func TestVTCTokenTracker_OnRequestFinishWithoutStartIsSafe(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)
	// Finishing a request that was never started must not panic or underflow.
	tracker.OnRequestFinish("alice", "model-a")

	tracker.mu.RLock()
	state := tracker.getState("alice", "model-a")
	tracker.mu.RUnlock()
	if state != nil && state.activeRequests < 0 {
		t.Fatalf("activeRequests underflowed: %d", state.activeRequests)
	}
}

func TestVTCTokenWeightsConfiguration(t *testing.T) {
	tests := []struct {
		name             string
		inputWeight      float64
		outputWeight     float64
		wantInputWeight  float64
		wantOutputWeight float64
	}{
		{
			name:             "valid weights applied",
			inputWeight:      2.0,
			outputWeight:     3.0,
			wantInputWeight:  2.0,
			wantOutputWeight: 3.0,
		},
		{
			name:             "NaN input weight ignored",
			inputWeight:      math.NaN(),
			outputWeight:     3.0,
			wantInputWeight:  defaultInputTokenWeight,
			wantOutputWeight: defaultOutputTokenWeight,
		},
		{
			name:             "negative output weight ignored",
			inputWeight:      2.0,
			outputWeight:     -1.0,
			wantInputWeight:  defaultInputTokenWeight,
			wantOutputWeight: defaultOutputTokenWeight,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewInMemoryVTCTokenTracker(
				WithVTCTokenWeights(tt.inputWeight, tt.outputWeight),
			).(*InMemoryVTCTokenTracker)

			if tracker.inputTokenWeight != tt.wantInputWeight {
				t.Errorf("inputTokenWeight = %v, want %v", tracker.inputTokenWeight, tt.wantInputWeight)
			}
			if tracker.outputTokenWeight != tt.wantOutputWeight {
				t.Errorf("outputTokenWeight = %v, want %v", tracker.outputTokenWeight, tt.wantOutputWeight)
			}
		})
	}
}

// fakeClock lets tests fast-forward time deterministically instead of sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestVTCTokenTracker_PruneEvictsOnlyExpiredIdleEntries(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	tracker := NewInMemoryVTCTokenTracker(WithVTCIdleTTL(time.Hour)).(*InMemoryVTCTokenTracker)
	tracker.now = clock.Now
	tracker.lastPrune = clock.Now()

	// alice goes idle right away; bob stays active the whole time and accrues usage.
	tracker.OnRequestStart("alice", "model-a")
	tracker.OnRequestFinish("alice", "model-a")
	tracker.OnRequestStart("bob", "model-a")
	if err := tracker.UpdateTokenCount("bob", "model-a", 10, 10); err != nil {
		t.Fatalf("UpdateTokenCount returned error: %v", err)
	}

	// Advance well past both the idle TTL and the prune throttle interval, then
	// trigger a prune via OnRequestStart for an unrelated user.
	clock.Advance(2 * time.Hour)
	tracker.OnRequestStart("carol", "model-a")
	tracker.OnRequestFinish("carol", "model-a")

	tracker.mu.RLock()
	_, aliceStillTracked := tracker.userState["alice"]
	_, bobStillTracked := tracker.userState["bob"]
	tracker.mu.RUnlock()

	if aliceStillTracked {
		t.Fatal("expected alice's idle entry to be evicted after exceeding idleTTL")
	}
	if !bobStillTracked {
		t.Fatal("expected bob's active entry to survive pruning")
	}

	// bob's counter and request count must survive pruning unchanged, proving
	// his entry wasn't evicted and silently recreated from scratch.
	bobCount, _ := tracker.GetTokenCount("bob", "model-a")
	if bobCount != 30 {
		t.Fatalf("bob counter changed by pruning: got %v, want 30", bobCount)
	}
	bobRequests, _ := tracker.GetRequestCount("bob", "model-a")
	if bobRequests != 1 {
		t.Fatalf("bob request count changed by pruning: got %d, want 1", bobRequests)
	}
}

func TestVTCTokenTracker_ConcurrentAccess(t *testing.T) {
	tracker := NewInMemoryVTCTokenTracker().(*InMemoryVTCTokenTracker)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := "user"
			model := "model-a"
			tracker.OnRequestStart(user, model)
			_ = tracker.UpdateTokenCount(user, model, 1, 1)
			_, _ = tracker.GetTokenCount(user, model)
			_, _ = tracker.GetRequestCount(user, model)
			tracker.OnRequestFinish(user, model)
		}()
	}
	wg.Wait()

	count, _ := tracker.GetRequestCount("user", "model-a")
	if count != 20 {
		t.Fatalf("GetRequestCount after concurrent updates = %d, want 20", count)
	}
}
