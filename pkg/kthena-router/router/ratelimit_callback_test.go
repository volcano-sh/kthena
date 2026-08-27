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

package router

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/ratelimit"
)

const rateLimitCallbackModel = "rate-limited-model"

func uint32Ptr(v uint32) *uint32 { return &v }

// rlRoute describes a ModelRoute for the shared test model. created controls the
// oldest-first ordering the callback uses to pick the effective rate limit.
type rlRoute struct {
	name    string
	created int64
	rl      *aiv1alpha1.RateLimit
}

func upsertRLRoute(t *testing.T, store datastore.Store, r rlRoute) {
	t.Helper()
	assert.NoError(t, store.AddOrUpdateModelRoute(&aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{
			Name:              r.name,
			Namespace:         "default",
			CreationTimestamp: v1.Unix(r.created, 0),
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: rateLimitCallbackModel,
			RateLimit: r.rl,
			Rules: []*aiv1alpha1.Rule{
				{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "model-server"}}},
			},
		},
	}))
}

// inputRateLimited reports whether an input-token limiter is currently rejecting
// requests for the model. It issues several requests so a freshly created
// limiter is exhausted and a cleared one is seen to consistently allow traffic.
func inputRateLimited(r *Router) bool {
	for i := 0; i < 20; i++ {
		if _, ok := r.loadRateLimiter.RateLimit(rateLimitCallbackModel, "hello world").(*ratelimit.InputRateLimitExceededError); ok {
			return true
		}
	}
	return false
}

// outputRateLimited reports whether an output-token limiter is currently in
// effect for the model.
func outputRateLimited(r *Router) bool {
	// Drain the bucket one token at a time; an oversized single AllowN call is
	// rejected without consuming anything.
	for i := 0; i < 200; i++ {
		r.loadRateLimiter.RecordOutputTokens(rateLimitCallbackModel, 1)
	}
	_, ok := r.loadRateLimiter.RateLimit(rateLimitCallbackModel, "short").(*ratelimit.OutputRateLimitExceededError)
	return ok
}

// TestRouter_RateLimitCallback exercises the real ModelRoute datastore callback
// registered in NewRouter. It applies an initial set of ModelRoutes for one
// model name, performs a transition (upsert or delete of one route), and checks
// the resulting in-process limiter state - including the case where several
// ModelRoutes share a model name.
func TestRouter_RateLimitCallback(t *testing.T) {
	// Callbacks are asynchronous, so poll for the converged state.
	const (
		eventuallyWait = 3 * time.Second
		eventuallyTick = 20 * time.Millisecond
	)

	inputOnly := &aiv1alpha1.RateLimit{InputTokensPerUnit: uint32Ptr(3), Unit: aiv1alpha1.Second}
	inputAndOutput := &aiv1alpha1.RateLimit{InputTokensPerUnit: uint32Ptr(3), OutputTokensPerUnit: uint32Ptr(5), Unit: aiv1alpha1.Second}
	highInputAndOutput := &aiv1alpha1.RateLimit{InputTokensPerUnit: uint32Ptr(100), OutputTokensPerUnit: uint32Ptr(5), Unit: aiv1alpha1.Second}
	highInputOnly := &aiv1alpha1.RateLimit{InputTokensPerUnit: uint32Ptr(100), Unit: aiv1alpha1.Second}
	hugeInputOnly := &aiv1alpha1.RateLimit{InputTokensPerUnit: uint32Ptr(1_000_000), Unit: aiv1alpha1.Second}
	outputOnly := &aiv1alpha1.RateLimit{OutputTokensPerUnit: uint32Ptr(5), Unit: aiv1alpha1.Second}
	emptyLimit := &aiv1alpha1.RateLimit{Unit: aiv1alpha1.Second}

	tests := []struct {
		name    string
		initial []rlRoute
		// state to wait for after the initial routes are applied.
		initialInputLimited  bool
		initialOutputLimited bool
		// transition under test: upsert this route, or delete deleteRoute.
		upsert     *rlRoute
		deleteName string
		// expected state after the transition.
		wantInputLimited  bool
		wantOutputLimited bool
	}{
		{
			name:                "single route: rateLimit -> nil clears the limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, nil},
		},
		{
			name:                "single route: empty RateLimit clears the limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, emptyLimit},
		},
		{
			name:                "single route: input limit removed, output kept",
			initial:             []rlRoute{{"a", 1, inputAndOutput}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, outputOnly},
			wantOutputLimited:   true,
		},
		{
			name:                 "single route: output limit removed, input kept",
			initial:              []rlRoute{{"a", 1, highInputAndOutput}},
			initialOutputLimited: true,
			upsert:               &rlRoute{"a", 1, highInputOnly},
		},
		{
			name:                "single route: valid limit A -> B",
			initial:             []rlRoute{{"a", 1, inputOnly}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, hugeInputOnly},
		},
		{
			name:                "single route: deleting the only route clears the limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}},
			initialInputLimited: true,
			deleteName:          "a",
		},
		{
			name:                "shared model, both limited: removing rateLimit from one keeps the other's limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}, {"b", 2, inputOnly}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, nil},
			wantInputLimited:    true,
		},
		{
			name:                "shared model, both limited: deleting one keeps the other's limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}, {"b", 2, inputOnly}},
			initialInputLimited: true,
			deleteName:          "a",
			wantInputLimited:    true,
		},
		{
			name:                "shared model, one limited: deleting the limited route removes the limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}, {"b", 2, nil}},
			initialInputLimited: true,
			deleteName:          "a",
		},
		{
			name:                "shared model, one limited: clearing the limited route removes the limiter",
			initial:             []rlRoute{{"a", 1, inputOnly}, {"b", 2, nil}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, nil},
		},
		{
			name:                "shared model, different limits: deleting the first makes the second effective",
			initial:             []rlRoute{{"a", 1, inputOnly}, {"b", 2, outputOnly}},
			initialInputLimited: true,
			deleteName:          "a",
			wantOutputLimited:   true,
		},
		{
			name:                "shared model, different limits: clearing the first makes the second effective",
			initial:             []rlRoute{{"a", 1, inputOnly}, {"b", 2, outputOnly}},
			initialInputLimited: true,
			upsert:              &rlRoute{"a", 1, nil},
			wantOutputLimited:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := datastore.New()
			router := NewRouter(store, "../scheduler/testdata/configmap.yaml")

			for _, r := range tc.initial {
				upsertRLRoute(t, store, r)
			}
			if tc.initialInputLimited {
				assert.Eventually(t, func() bool { return inputRateLimited(router) }, eventuallyWait, eventuallyTick,
					"input limiter should be active after the initial routes are applied")
			}
			if tc.initialOutputLimited {
				assert.Eventually(t, func() bool { return outputRateLimited(router) }, eventuallyWait, eventuallyTick,
					"output limiter should be active after the initial routes are applied")
			}

			if tc.deleteName != "" {
				assert.NoError(t, store.DeleteModelRoute("default/"+tc.deleteName))
			} else {
				upsertRLRoute(t, store, *tc.upsert)
			}

			assert.Eventually(t, func() bool { return inputRateLimited(router) == tc.wantInputLimited }, eventuallyWait, eventuallyTick,
				"input limiter state after the transition")
			assert.Eventually(t, func() bool { return outputRateLimited(router) == tc.wantOutputLimited }, eventuallyWait, eventuallyTick,
				"output limiter state after the transition")
		})
	}
}
