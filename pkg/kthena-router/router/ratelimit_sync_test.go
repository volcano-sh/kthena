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
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/ratelimit"
)

func newRateLimitedRoute(name, model string, created time.Time, inputTokens *uint32) *aiv1alpha1.ModelRoute {
	mr := &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: aiv1alpha1.ModelRouteSpec{ModelName: model},
	}
	if inputTokens != nil {
		mr.Spec.RateLimit = &aiv1alpha1.RateLimit{
			InputTokensPerUnit: inputTokens,
			Unit:               aiv1alpha1.Minute,
		}
	}
	return mr
}

func TestRateLimitSyncer(t *testing.T) {
	// "hello world" is estimated at 3 tokens.
	const prompt = "hello world"
	five, hundred := uint32(5), uint32(100)
	older := time.Unix(1000, 0)
	newer := time.Unix(2000, 0)

	tests := []struct {
		name string
		// steps are applied to the store in order, with a sync after each one.
		steps []func(s datastore.Store)
		// extraSyncs runs sync again afterwards, as a stale callback would.
		extraSyncs int
		model      string
		// allowed is how many prompt-sized requests pass before one is limited;
		// -1 means the model must not be rate limited at all.
		allowed int
	}{
		{
			name: "rate limit applied from route",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &five)) },
			},
			model:   "m",
			allowed: 1,
		},
		{
			name: "clearing spec.rateLimit removes the limiter",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &five)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, nil)) },
			},
			model:   "m",
			allowed: -1,
		},
		{
			name: "stale sync after re-enabling keeps the limiter",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &five)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, nil)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &five)) },
			},
			extraSyncs: 2,
			model:      "m",
			allowed:    1,
		},
		{
			name: "route without rate limit does not remove a sibling route's limiter",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &five)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("b", "m", newer, nil)) },
				func(s datastore.Store) { _ = s.DeleteModelRoute("default/b") },
			},
			model:   "m",
			allowed: 1,
		},
		{
			name: "deleting one of two rate-limited routes keeps the other's limiter",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &hundred)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("b", "m", newer, &five)) },
				func(s datastore.Store) { _ = s.DeleteModelRoute("default/a") },
			},
			model:   "m",
			allowed: 1,
		},
		{
			name: "deleting the last rate-limited route removes the limiter",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", older, &five)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("b", "m", newer, nil)) },
				func(s datastore.Store) { _ = s.DeleteModelRoute("default/a") },
			},
			model:   "m",
			allowed: -1,
		},
		{
			name: "changing the model name removes the old model's limiter",
			steps: []func(s datastore.Store){
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "old", older, &five)) },
				func(s datastore.Store) { _ = s.AddOrUpdateModelRoute(newRateLimitedRoute("a", "new", older, &five)) },
			},
			model:   "old",
			allowed: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := datastore.New()
			limiter := ratelimit.NewTokenRateLimiter()
			syncer := newRateLimitSyncer(store, limiter)

			for _, step := range tt.steps {
				step(store)
				syncer.sync()
			}
			for i := 0; i < tt.extraSyncs; i++ {
				syncer.sync()
			}

			if tt.allowed < 0 {
				assert.False(t, limiter.HasLimiter(tt.model))
				for i := 0; i < 3; i++ {
					assert.NoError(t, limiter.RateLimit(tt.model, prompt))
				}
				return
			}
			require.True(t, limiter.HasLimiter(tt.model))
			for i := 0; i < tt.allowed; i++ {
				assert.NoError(t, limiter.RateLimit(tt.model, prompt))
			}
			assert.Error(t, limiter.RateLimit(tt.model, prompt))
		})
	}
}

func TestRateLimitSyncer_UnchangedSpecKeepsBucket(t *testing.T) {
	store := datastore.New()
	limiter := ratelimit.NewTokenRateLimiter()
	syncer := newRateLimitSyncer(store, limiter)

	five := uint32(5)
	require.NoError(t, store.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", time.Unix(1000, 0), &five)))
	syncer.sync()
	require.NoError(t, limiter.RateLimit("m", "hello world"))

	// An event for an unrelated route, or a repeated event for the same spec,
	// must not rebuild the limiter and refill its bucket.
	require.NoError(t, store.AddOrUpdateModelRoute(newRateLimitedRoute("other", "other-model", time.Unix(1000, 0), nil)))
	syncer.sync()
	require.NoError(t, store.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", time.Unix(1000, 0), &five)))
	syncer.sync()

	assert.Error(t, limiter.RateLimit("m", "hello world"))
}

func TestRateLimitSyncer_LeavesForeignLimiters(t *testing.T) {
	store := datastore.New()
	limiter := ratelimit.NewTokenRateLimiter()
	syncer := newRateLimitSyncer(store, limiter)

	five := uint32(5)
	require.NoError(t, limiter.AddOrUpdateLimiter("manual", &aiv1alpha1.RateLimit{
		InputTokensPerUnit: &five,
		Unit:               aiv1alpha1.Minute,
	}))
	require.NoError(t, store.AddOrUpdateModelRoute(newRateLimitedRoute("a", "m", time.Unix(1000, 0), nil)))
	syncer.sync()

	assert.True(t, limiter.HasLimiter("manual"))
}
