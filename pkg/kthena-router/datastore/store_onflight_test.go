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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// fakeOnFlightCounter mimics RedisOnFlightCounter: counters are keyed by pod
// name, Decr resets negative values to 0, and Delete drops the key. When
// deleteErr is set Delete fails; the key is left in place unless
// deleteDespiteErr is also set, which models an HDEL that ran but whose
// reply was lost.
type fakeOnFlightCounter struct {
	mu               sync.Mutex
	counts           map[types.NamespacedName]int64
	deleteErr        error
	deleteDespiteErr bool
}

func newFakeOnFlightCounter() *fakeOnFlightCounter {
	return &fakeOnFlightCounter{counts: make(map[types.NamespacedName]int64)}
}

func (f *fakeOnFlightCounter) Incr(_ context.Context, podName types.NamespacedName) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[podName]++
	return f.counts[podName], nil
}

func (f *fakeOnFlightCounter) Decr(_ context.Context, podName types.NamespacedName) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[podName]--
	if f.counts[podName] < 0 {
		f.counts[podName] = 0
	}
	return f.counts[podName], nil
}

func (f *fakeOnFlightCounter) Delete(_ context.Context, podName types.NamespacedName) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil && !f.deleteDespiteErr {
		return f.deleteErr
	}
	delete(f.counts, podName)
	return f.deleteErr
}

func (f *fakeOnFlightCounter) BatchGet(_ context.Context, podNames []types.NamespacedName) (map[types.NamespacedName]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[types.NamespacedName]int64, len(podNames))
	for _, name := range podNames {
		if v, ok := f.counts[name]; ok {
			result[name] = v
		}
	}
	return result, nil
}

func (f *fakeOnFlightCounter) get(podName types.NamespacedName) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.counts[podName]
	return v, ok
}

func TestOnFlightCountAfterPodRecreated(t *testing.T) {
	tests := []struct {
		name  string
		redis bool
	}{
		{name: "local counter", redis: false},
		{name: "redis counter", redis: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(&fakePodRuntimeInspector{})
			var counter *fakeOnFlightCounter
			if tt.redis {
				counter = newFakeOnFlightCounter()
				s.onFlightCounter = counter
			}
			ms := newTestModelServerWithPDGroup("test-model", "default")
			servers := []*aiv1alpha1.ModelServer{ms}
			pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
			podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
			require.NoError(t, s.AddOrUpdatePod(pod, servers))

			// Two requests are sent to the pod.
			old1 := s.IncrPodOnFlightRequests(podName)
			old2 := s.IncrPodOnFlightRequests(podName)
			require.NotNil(t, old1)
			require.Same(t, old1, old2)

			// The pod goes NotReady and comes back while they are still running,
			// and a new request is sent to the new PodInfo.
			require.NoError(t, s.DeletePod(podName))
			require.NoError(t, s.AddOrUpdatePod(pod, servers))
			current := s.IncrPodOnFlightRequests(podName)
			require.NotNil(t, current)
			require.NotSame(t, old1, current, "the pod should have a new PodInfo")

			// The old requests finish.
			s.DecrPodOnFlightRequests(old1)
			s.DecrPodOnFlightRequests(old2)

			assert.Equal(t, int64(1), s.GetPodInfo(podName).GetOnFlightRequestNum(),
				"old requests must not change the new PodInfo")
			if tt.redis {
				v, _ := counter.get(podName)
				assert.Equal(t, int64(1), v, "old requests must not change the Redis counter")
			}

			// The new request finishes.
			s.DecrPodOnFlightRequests(current)
			assert.Equal(t, int64(0), s.GetPodInfo(podName).GetOnFlightRequestNum())
			if tt.redis {
				v, _ := counter.get(podName)
				assert.Equal(t, int64(0), v)
			}
		})
	}
}

func TestOnFlightDecrAfterPodDeleted(t *testing.T) {
	s := newStore(&fakePodRuntimeInspector{})
	counter := newFakeOnFlightCounter()
	s.onFlightCounter = counter
	ms := newTestModelServerWithPDGroup("test-model", "default")
	pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
	podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
	require.NoError(t, s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms}))

	counted := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, counted)
	require.NoError(t, s.DeletePod(podName))

	// The request finishes after the pod is gone and never comes back.
	s.DecrPodOnFlightRequests(counted)

	assert.Nil(t, s.GetPodInfo(podName))
	_, exists := counter.get(podName)
	assert.False(t, exists, "the Redis key of a deleted pod must not be recreated")
}

func TestOnFlightPodNotInStore(t *testing.T) {
	s := newStore(&fakePodRuntimeInspector{})
	counted := s.IncrPodOnFlightRequests(types.NamespacedName{Namespace: "default", Name: "missing"})
	assert.Nil(t, counted)
	assert.NotPanics(t, func() { s.DecrPodOnFlightRequests(counted) })
}

// recreatingOnFlightCounter runs recreate in another goroutine during the first
// Incr, to recreate the pod between IncrPodOnFlightRequests loading the
// PodInfo and the Redis increment.
type recreatingOnFlightCounter struct {
	*fakeOnFlightCounter
	once     sync.Once
	recreate func()
	done     chan struct{}
}

func (r *recreatingOnFlightCounter) Incr(ctx context.Context, podName types.NamespacedName) (int64, error) {
	r.once.Do(func() {
		go func() {
			r.recreate()
			close(r.done)
		}()
		// Without onFlightMu the recreate finishes here, before the increment.
		// With it, DeletePod waits for Incr to return, so this always times out.
		select {
		case <-r.done:
		case <-time.After(100 * time.Millisecond):
		}
	})
	return r.fakeOnFlightCounter.Incr(ctx, podName)
}

func TestOnFlightPodRecreatedDuringIncr(t *testing.T) {
	s := newStore(&fakePodRuntimeInspector{})
	ms := newTestModelServerWithPDGroup("test-model", "default")
	servers := []*aiv1alpha1.ModelServer{ms}
	pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
	podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
	require.NoError(t, s.AddOrUpdatePod(pod, servers))

	counter := &recreatingOnFlightCounter{
		fakeOnFlightCounter: newFakeOnFlightCounter(),
		done:                make(chan struct{}),
	}
	counter.recreate = func() {
		assert.NoError(t, s.DeletePod(podName))
		assert.NoError(t, s.AddOrUpdatePod(pod, servers))
	}
	s.onFlightCounter = counter

	counted := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, counted)
	<-counter.done
	current := s.GetPodInfo(podName)
	require.NotNil(t, current)
	require.NotSame(t, counted, current, "the pod should have a new PodInfo")

	// The request finishes. Nothing was sent to the new PodInfo.
	s.DecrPodOnFlightRequests(counted)

	v, _ := counter.get(podName)
	assert.Equal(t, int64(0), v, "the request must not be left on the new pod's Redis key")
	s.SyncOnFlightCounts()
	assert.Equal(t, int64(0), current.GetOnFlightRequestNum())
}

func TestOnFlightDecrAfterModelServerDeleted(t *testing.T) {
	s := newStore(&fakePodRuntimeInspector{})
	counter := newFakeOnFlightCounter()
	s.onFlightCounter = counter
	ms := newTestModelServerWithPDGroup("test-model", "default")
	servers := []*aiv1alpha1.ModelServer{ms}
	msName := types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name}
	pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
	podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
	require.NoError(t, s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]()))
	require.NoError(t, s.AddOrUpdatePod(pod, servers))

	old := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, old)

	// The pod's only ModelServer is deleted, which removes the pod and its
	// Redis key from the store.
	require.NoError(t, s.DeleteModelServer(msName))
	require.Nil(t, s.GetPodInfo(podName))
	_, exists := counter.get(podName)
	assert.False(t, exists, "the Redis key must be deleted with the pod")

	// Both come back while the old request is still running, and a new
	// request is sent to the new PodInfo.
	require.NoError(t, s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]()))
	require.NoError(t, s.AddOrUpdatePod(pod, servers))
	current := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, current)
	require.NotSame(t, old, current, "the pod should have a new PodInfo")

	// The old request finishes.
	s.DecrPodOnFlightRequests(old)
	v, _ := counter.get(podName)
	assert.Equal(t, int64(1), v, "only the new request is in flight")

	// The new request finishes.
	s.DecrPodOnFlightRequests(current)
	v, _ = counter.get(podName)
	assert.Equal(t, int64(0), v)
}

// TestOnFlightRedisDeleteFails covers a pod removed from the store while
// deleting its Redis counter fails: the key keeps the old PodInfo's requests,
// so they must still be decremented from it when they finish, and only they.
func TestOnFlightRedisDeleteFails(t *testing.T) {
	ms := newTestModelServerWithPDGroup("test-model", "default")
	msName := types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name}
	podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
	tests := []struct {
		name   string
		remove func(t *testing.T, s *store)
	}{
		{
			name: "DeletePod",
			remove: func(t *testing.T, s *store) {
				require.NoError(t, s.DeletePod(podName))
			},
		},
		{
			name: "DeleteModelServer",
			remove: func(t *testing.T, s *store) {
				require.NoError(t, s.DeleteModelServer(msName))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(&fakePodRuntimeInspector{})
			counter := newFakeOnFlightCounter()
			s.onFlightCounter = counter
			servers := []*aiv1alpha1.ModelServer{ms}
			pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
			require.NoError(t, s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]()))
			require.NoError(t, s.AddOrUpdatePod(pod, servers))

			old := s.IncrPodOnFlightRequests(podName)
			require.NotNil(t, old)
			require.Same(t, old, s.IncrPodOnFlightRequests(podName))

			// The pod is removed but its Redis key cannot be deleted.
			counter.deleteErr = errors.New("redis unavailable")
			tt.remove(t, s)
			require.Nil(t, s.GetPodInfo(podName))
			v, exists := counter.get(podName)
			require.True(t, exists)
			require.Equal(t, int64(2), v, "the key is left in place when Delete fails")

			// The pod comes back and receives a new request while the two
			// old ones are still running.
			counter.deleteErr = nil
			require.NoError(t, s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]()))
			require.NoError(t, s.AddOrUpdatePod(pod, servers))
			current := s.IncrPodOnFlightRequests(podName)
			require.NotNil(t, current)
			require.NotSame(t, old, current, "the pod should have a new PodInfo")
			v, _ = counter.get(podName)
			assert.Equal(t, int64(3), v)

			// The old requests finish: they come off the retained key.
			s.DecrPodOnFlightRequests(old)
			s.DecrPodOnFlightRequests(old)
			v, _ = counter.get(podName)
			assert.Equal(t, int64(1), v, "only the new request is in flight")

			// The replacement sees the correct count and ends at 0.
			s.lastOnFlightSync.Store(0)
			s.SyncOnFlightCounts()
			assert.Equal(t, int64(1), current.GetOnFlightRequestNum())
			s.DecrPodOnFlightRequests(current)
			v, _ = counter.get(podName)
			assert.Equal(t, int64(0), v)
			assert.Equal(t, int64(0), current.GetOnFlightRequestNum())
		})
	}
}

// TestOnFlightRetainedKeyDeletedLater covers a retained key that a later
// generation of the same pod deletes successfully: the earlier PodInfo's
// requests are gone with the key, so finishing them must not decrement the
// counter of whichever generation is current by then.
func TestOnFlightRetainedKeyDeletedLater(t *testing.T) {
	s := newStore(&fakePodRuntimeInspector{})
	counter := newFakeOnFlightCounter()
	s.onFlightCounter = counter
	ms := newTestModelServerWithPDGroup("test-model", "default")
	servers := []*aiv1alpha1.ModelServer{ms}
	pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
	podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
	require.NoError(t, s.AddOrUpdatePod(pod, servers))

	// Generation A: one request in flight, removed while Redis is down.
	a := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, a)
	counter.deleteErr = errors.New("redis unavailable")
	require.NoError(t, s.DeletePod(podName))
	counter.deleteErr = nil
	v, _ := counter.get(podName)
	require.Equal(t, int64(1), v)

	// Generation B: removed again, this time the key is deleted.
	require.NoError(t, s.AddOrUpdatePod(pod, servers))
	b := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, b)
	require.NotSame(t, a, b)
	require.NoError(t, s.DeletePod(podName))
	_, exists := counter.get(podName)
	require.False(t, exists, "the key is deleted with generation B")
	assert.Empty(t, s.retainedOnFlight, "generation A is no longer tracked")

	// Generation C has a request in flight when A's and B's finish.
	require.NoError(t, s.AddOrUpdatePod(pod, servers))
	c := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, c)
	s.DecrPodOnFlightRequests(a)
	s.DecrPodOnFlightRequests(b)
	v, _ = counter.get(podName)
	assert.Equal(t, int64(1), v, "C's request must stay counted")
	assert.Equal(t, int64(1), c.GetOnFlightRequestNum())

	s.DecrPodOnFlightRequests(c)
	v, _ = counter.get(podName)
	assert.Equal(t, int64(0), v)
}

// TestOnFlightDeleteErrorAfterKeyRemoved covers a Delete that removed the key
// but reported an error: the removed PodInfo then decrements a key that no
// longer holds its requests. The count can only be too low, never negative,
// and it recovers once the replacement's own requests finish.
func TestOnFlightDeleteErrorAfterKeyRemoved(t *testing.T) {
	s := newStore(&fakePodRuntimeInspector{})
	counter := newFakeOnFlightCounter()
	s.onFlightCounter = counter
	ms := newTestModelServerWithPDGroup("test-model", "default")
	servers := []*aiv1alpha1.ModelServer{ms}
	pod := newTestPod("pod-0", "default", map[string]string{"app": ms.Name})
	podName := types.NamespacedName{Namespace: "default", Name: "pod-0"}
	require.NoError(t, s.AddOrUpdatePod(pod, servers))

	old := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, old)
	counter.deleteErr = errors.New("reply lost")
	counter.deleteDespiteErr = true
	require.NoError(t, s.DeletePod(podName))
	counter.deleteErr = nil
	counter.deleteDespiteErr = false
	_, exists := counter.get(podName)
	require.False(t, exists)

	// The replacement has one request in flight when the old one finishes:
	// the count dips to 0 for the replacement's request, but no lower.
	require.NoError(t, s.AddOrUpdatePod(pod, servers))
	current := s.IncrPodOnFlightRequests(podName)
	require.NotNil(t, current)
	s.DecrPodOnFlightRequests(old)
	v, _ := counter.get(podName)
	assert.Equal(t, int64(0), v)

	// It settles at 0 once the replacement's request finishes.
	s.DecrPodOnFlightRequests(current)
	v, _ = counter.get(podName)
	assert.Equal(t, int64(0), v)
	assert.Equal(t, int64(0), current.GetOnFlightRequestNum())
}
