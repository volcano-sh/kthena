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
	"reflect"
	"sync"

	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/ratelimit"
)

// rateLimitSyncer keeps the per-model rate limiters in line with the ModelRoutes
// held by the datastore.
//
// Datastore callbacks run in their own goroutines, so ModelRoute events can be
// handled out of order. Instead of applying the event payload, sync re-derives
// the desired limiters from the store's current routes while holding mu, so the
// read and the limiter mutation are serialized: whichever sync runs last sees the
// latest routes, and a stale event can never undo a newer one.
type rateLimitSyncer struct {
	mu      sync.Mutex
	store   datastore.Store
	limiter *ratelimit.TokenRateLimiter
	// applied records the RateLimit spec currently installed for each model by
	// this syncer. Limiters it did not install are left untouched.
	applied map[string]*v1alpha1.RateLimit
}

func newRateLimitSyncer(store datastore.Store, limiter *ratelimit.TokenRateLimiter) *rateLimitSyncer {
	return &rateLimitSyncer{
		store:   store,
		limiter: limiter,
		applied: make(map[string]*v1alpha1.RateLimit),
	}
}

func (s *rateLimitSyncer) sync() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Several ModelRoutes can share a model name, so a model keeps its limiter as
	// long as any of them still configures one. If more than one does, the oldest
	// route wins, which is the same order the datastore matches routes in.
	desired := make(map[string]*v1alpha1.ModelRoute)
	for _, mr := range s.store.GetAllModelRoutes() {
		if mr == nil || mr.Spec.RateLimit == nil {
			continue
		}
		if current, ok := desired[mr.Spec.ModelName]; !ok || routeBefore(mr, current) {
			desired[mr.Spec.ModelName] = mr
		}
	}

	for model := range s.applied {
		if _, ok := desired[model]; !ok {
			klog.Infof("delete rate limit for model %s", model)
			s.limiter.DeleteLimiter(model)
			delete(s.applied, model)
		}
	}

	for model, mr := range desired {
		// Rebuilding a limiter refills its bucket, so only do it when the spec
		// actually changed.
		if applied, ok := s.applied[model]; ok && reflect.DeepEqual(applied, mr.Spec.RateLimit) {
			continue
		}
		klog.Infof("add or update rate limit for model %s", model)
		if err := s.limiter.AddOrUpdateLimiter(model, mr.Spec.RateLimit); err != nil {
			klog.Errorf("failed to configure rate limiter for model %s: %v", model, err)
			continue
		}
		s.applied[model] = mr.Spec.RateLimit.DeepCopy()
	}
}

// routeBefore reports whether a sorts before b, using the same ordering as the
// datastore: creation time, then resource version, then namespaced name.
func routeBefore(a, b *v1alpha1.ModelRoute) bool {
	ta, tb := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	if a.ResourceVersion != b.ResourceVersion {
		return a.ResourceVersion < b.ResourceVersion
	}
	return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
}
