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
)

// vtcUserState holds the per (user, model) bookkeeping for the VTC tracker.
type vtcUserState struct {
	virtualTokens  float64 // Cumulative weighted token usage (the VTC counter)
	requestCount   int     // Cumulative request count, for GetRequestCount
	activeRequests int     // Requests currently in the fairness queue for this user/model
}

// vtcActiveMin caches the minimum counter among currently active users for a
// model, along with which user holds it, so OnRequestStart doesn't need to
// scan every user on the common path. It goes stale (valid=false) only when
// the holder itself might no longer be the minimum: it left the active set
// (OnRequestFinish) or its counter increased (UpdateTokenCount). A stale cache
// is recomputed by a single O(active users) scan the next time it's needed.
type vtcActiveMin struct {
	user  string
	value float64
	valid bool
}

// InMemoryVTCTokenTracker implements the Virtual Token Counter fairness
// algorithm described in the S-LoRA paper (https://arxiv.org/pdf/2401.00588,
// section 4). Instead of a sliding time window, each user accrues a monotonic
// counter of weighted token usage; the fairness queue prioritizes the user
// with the lowest counter.
//
// When a user transitions from idle to active (OnRequestStart), if their
// counter sits below the current minimum among active users, it is raised to
// that minimum. Without this, a user who was idle for a while would rejoin
// with a stale, low counter and queue-jump ahead of users who kept sending
// requests the whole time.
type InMemoryVTCTokenTracker struct {
	mu                sync.RWMutex
	inputTokenWeight  float64
	outputTokenWeight float64
	userState         map[string]map[string]*vtcUserState // [user][model]
	activeMin         map[string]*vtcActiveMin            // [model] -> cached active minimum
}

// VTCTokenTrackerOption configures an InMemoryVTCTokenTracker.
type VTCTokenTrackerOption func(*InMemoryVTCTokenTracker)

// WithVTCTokenWeights overrides the default token weights used to compute the
// weighted token cost added to a user's counter on each update.
func WithVTCTokenWeights(inputWeight, outputWeight float64) VTCTokenTrackerOption {
	return func(t *InMemoryVTCTokenTracker) {
		if !isValidTokenWeight(inputWeight) || !isValidTokenWeight(outputWeight) {
			return
		}
		t.inputTokenWeight = inputWeight
		t.outputTokenWeight = outputWeight
	}
}

// NewInMemoryVTCTokenTracker creates a new VTC tracker with optional configuration.
func NewInMemoryVTCTokenTracker(opts ...VTCTokenTrackerOption) TokenTracker {
	tracker := &InMemoryVTCTokenTracker{
		inputTokenWeight:  defaultInputTokenWeight,
		outputTokenWeight: defaultOutputTokenWeight,
		userState:         make(map[string]map[string]*vtcUserState),
		activeMin:         make(map[string]*vtcActiveMin),
	}
	for _, opt := range opts {
		opt(tracker)
	}
	return tracker
}

// Caller must hold t.mu (read or write).
func (t *InMemoryVTCTokenTracker) getState(user, model string) *vtcUserState {
	models, ok := t.userState[user]
	if !ok {
		return nil
	}
	return models[model]
}

// Caller must hold t.mu (write).
func (t *InMemoryVTCTokenTracker) getOrCreateState(user, model string) *vtcUserState {
	models, ok := t.userState[user]
	if !ok {
		models = make(map[string]*vtcUserState)
		t.userState[user] = models
	}
	state, ok := models[model]
	if !ok {
		state = &vtcUserState{}
		models[model] = state
	}
	return state
}

// scanActiveMinLocked recomputes the minimum counter among active users for a
// model by scanning every user. Caller must hold t.mu (write) and only call
// this when the cache is stale. Time: O(number of users).
func (t *InMemoryVTCTokenTracker) scanActiveMinLocked(model string) (user string, value float64, found bool) {
	for u, models := range t.userState {
		state, ok := models[model]
		if !ok || state.activeRequests == 0 {
			continue
		}
		if !found || state.virtualTokens < value {
			value = state.virtualTokens
			user = u
			found = true
		}
	}
	return user, value, found
}

// ensureActiveMinLocked returns the current minimum counter among active
// users for a model, using the cache when valid and falling back to a full
// scan (and repopulating the cache) otherwise. Caller must hold t.mu (write).
func (t *InMemoryVTCTokenTracker) ensureActiveMinLocked(model string) (float64, bool) {
	cache, ok := t.activeMin[model]
	if ok && cache.valid {
		return cache.value, true
	}
	user, value, found := t.scanActiveMinLocked(model)
	if !ok {
		cache = &vtcActiveMin{}
		t.activeMin[model] = cache
	}
	cache.valid = found
	if found {
		cache.user = user
		cache.value = value
	}
	return value, found
}

// invalidateActiveMinLocked marks the cached minimum for a model stale if the
// given user was (or might have been) the cached holder. Caller must hold
// t.mu (write).
func (t *InMemoryVTCTokenTracker) invalidateActiveMinLocked(model, user string) {
	if cache, ok := t.activeMin[model]; ok && cache.valid && cache.user == user {
		cache.valid = false
	}
}

// GetTokenCount returns the user's current VTC counter for the given model.
func (t *InMemoryVTCTokenTracker) GetTokenCount(user, model string) (float64, error) {
	if user == "" || model == "" {
		return 0, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	state := t.getState(user, model)
	if state == nil {
		return 0, nil
	}
	return state.virtualTokens, nil
}

// UpdateTokenCount adds the weighted cost of the given tokens to the user's counter.
func (t *InMemoryVTCTokenTracker) UpdateTokenCount(user, model string, inputTokens, outputTokens float64) error {
	if user == "" || model == "" {
		return nil
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.getOrCreateState(user, model)
	state.virtualTokens += inputTokens*t.inputTokenWeight + outputTokens*t.outputTokenWeight
	state.requestCount++
	// The counter only grows here, so if this user held the cached minimum,
	// another active user may now be lower; the cache can't confirm that
	// cheaply, so invalidate it and let the next lookup rescan.
	if state.activeRequests > 0 {
		t.invalidateActiveMinLocked(model, user)
	}
	return nil
}

// GetRequestCount returns the cumulative request count for a user/model.
func (t *InMemoryVTCTokenTracker) GetRequestCount(user, model string) (int, error) {
	if user == "" || model == "" {
		return 0, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	state := t.getState(user, model)
	if state == nil {
		return 0, nil
	}
	return state.requestCount, nil
}

// OnRequestStart runs the counter-lift when a user transitions from idle to
// active: their counter is raised to the minimum counter among currently
// active users, so time spent idle never earns them priority over users who
// have been active the whole time.
func (t *InMemoryVTCTokenTracker) OnRequestStart(user, model string) {
	if user == "" || model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.getOrCreateState(user, model)
	if state.activeRequests == 0 {
		min, hasActive := t.ensureActiveMinLocked(model)
		if hasActive && state.virtualTokens < min {
			state.virtualTokens = min
		}
		// This user is about to join the active set: keep the cache valid by
		// recording them as the new holder if they're at or below the current
		// minimum (or if there was no active user before them).
		cache, ok := t.activeMin[model]
		if !ok {
			cache = &vtcActiveMin{}
			t.activeMin[model] = cache
		}
		if !hasActive || state.virtualTokens <= cache.value {
			cache.user = user
			cache.value = state.virtualTokens
		}
		cache.valid = true
	}
	state.activeRequests++
}

// OnRequestFinish marks a request started via OnRequestStart as no longer active.
func (t *InMemoryVTCTokenTracker) OnRequestFinish(user, model string) {
	if user == "" || model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.getState(user, model)
	if state == nil || state.activeRequests == 0 {
		return
	}
	state.activeRequests--
	if state.activeRequests == 0 {
		// The user leaving might have been the cached minimum holder; the next
		// active user, if any, is unknown without a rescan.
		t.invalidateActiveMinLocked(model, user)
	}
}
