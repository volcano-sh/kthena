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
)

// vtcUserState holds the per (user, model) bookkeeping for the VTC tracker.
type vtcUserState struct {
	virtualTokens  float64 // Cumulative weighted token usage (the VTC counter)
	requestCount   int     // Cumulative request count, for GetRequestCount
	activeRequests int     // Requests currently in the fairness queue for this user/model
}

// InMemoryVTCTokenTracker implements the Virtual Token Counter fairness
// algorithm described in the S-LoRA paper (https://arxiv.org/pdf/2401.00588,
// section 4). Instead of a sliding time window, each user accrues a monotonic
// counter of weighted token usage; the fairness queue prioritizes the user
// with the lowest counter.
//
// To keep a user who was idle for a while from being penalized by everyone
// else's usage catching up forever (or, symmetrically, from a bursty new user
// starving everyone else with a counter stuck at zero), the counter is lifted
// to the minimum counter among currently active users whenever a user goes
// from idle to active (OnRequestStart).
type InMemoryVTCTokenTracker struct {
	mu                sync.RWMutex
	inputTokenWeight  float64
	outputTokenWeight float64
	userState         map[string]map[string]*vtcUserState // [user][model]
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

// Caller must hold t.mu (read or write).
func (t *InMemoryVTCTokenTracker) activeMinLocked(model string) (float64, bool) {
	min := math.Inf(1)
	found := false
	for _, models := range t.userState {
		state, ok := models[model]
		if !ok || state.activeRequests == 0 {
			continue
		}
		if state.virtualTokens < min {
			min = state.virtualTokens
			found = true
		}
	}
	return min, found
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
		if min, ok := t.activeMinLocked(model); ok && state.virtualTokens < min {
			state.virtualTokens = min
		}
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
}
