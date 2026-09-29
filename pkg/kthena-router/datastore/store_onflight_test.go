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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// fakeOnFlightCounter mimics RedisOnFlightCounter: counters are keyed by pod
// name, Decr resets negative values to 0, and Delete drops the key.
type fakeOnFlightCounter struct {
	mu     sync.Mutex
	counts map[types.NamespacedName]int64
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
	delete(f.counts, podName)
	return nil
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
