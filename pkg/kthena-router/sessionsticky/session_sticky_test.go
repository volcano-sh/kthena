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

package sessionsticky

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestRedisStoreSetRefreshesBinding(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	store, err := NewRedisStore(mr.Addr(), "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	store.Set("default/route", "session", types.NamespacedName{Namespace: "default", Name: "pod-a"}, time.Minute)
	store.Set("default/route", "session", types.NamespacedName{Namespace: "default", Name: "pod-b"}, time.Minute)

	pod, ok := store.Get("default/route", "session")
	require.True(t, ok)
	require.Equal(t, types.NamespacedName{Namespace: "default", Name: "pod-b"}, pod)
}

func TestRedisStoreSetRefreshesExistingSamePod(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	store, err := NewRedisStore(mr.Addr(), "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	store.Set("default/route", "session", types.NamespacedName{Namespace: "default", Name: "pod-a"}, time.Second)
	mr.FastForward(900 * time.Millisecond)
	store.Set("default/route", "session", types.NamespacedName{Namespace: "default", Name: "pod-a"}, time.Second)
	mr.FastForward(500 * time.Millisecond)

	pod, ok := store.Get("default/route", "session")
	require.True(t, ok)
	require.Equal(t, types.NamespacedName{Namespace: "default", Name: "pod-a"}, pod)
}

func TestMemoryStoreSetDoesNotScanEveryInsert(t *testing.T) {
	store := NewMemoryStore()
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	store.cleanupInterval = time.Minute
	store.nextCleanup = now.Add(time.Minute)

	expiredKey := storeKey("route", "expired")
	store.bindings[expiredKey] = sessionBinding{
		pod:       types.NamespacedName{Namespace: "default", Name: "old"},
		expiresAt: now.Add(-time.Second),
	}

	store.Set("route", "new", types.NamespacedName{Namespace: "default", Name: "new"}, time.Minute)

	require.Contains(t, store.bindings, expiredKey, "expired entries should be cleaned lazily, not on every Set")
	require.Contains(t, store.bindings, storeKey("route", "new"))
}

func TestMemoryStorePeriodicCleanup(t *testing.T) {
	store := NewMemoryStore()
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }
	store.cleanupInterval = time.Minute
	store.nextCleanup = now

	expiredKey := storeKey("route", "expired")
	store.bindings[expiredKey] = sessionBinding{
		pod:       types.NamespacedName{Namespace: "default", Name: "old"},
		expiresAt: now.Add(-time.Second),
	}

	store.Set("route", "new", types.NamespacedName{Namespace: "default", Name: "new"}, time.Minute)

	require.NotContains(t, store.bindings, expiredKey)
	require.Contains(t, store.bindings, storeKey("route", "new"))
	require.Equal(t, now.Add(time.Minute), store.nextCleanup)
}

func TestMemoryStoreSetPreservesExistingDifferentPod(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewMemoryStore()
	t.Cleanup(func() { _ = store.Close() })
	store.now = func() time.Time {
		return now
	}

	routeKey := "default/route"
	sessionKey := "session-1"
	pod1 := types.NamespacedName{Namespace: "default", Name: "pod-1"}
	pod2 := types.NamespacedName{Namespace: "default", Name: "pod-2"}

	store.Set(routeKey, sessionKey, pod1, time.Minute)
	store.Set(routeKey, sessionKey, pod2, time.Minute)

	got, ok := store.Get(routeKey, sessionKey)
	require.True(t, ok)
	require.Equal(t, pod1, got, "live binding to another pod should not be overwritten")

	now = now.Add(30 * time.Second)
	store.Set(routeKey, sessionKey, pod1, 2*time.Minute)

	now = now.Add(45 * time.Second)
	got, ok = store.Get(routeKey, sessionKey)
	require.True(t, ok, "same-pod set should refresh the TTL")
	require.Equal(t, pod1, got)

	now = now.Add(2 * time.Minute)
	store.Set(routeKey, sessionKey, pod2, time.Minute)

	got, ok = store.Get(routeKey, sessionKey)
	require.True(t, ok)
	require.Equal(t, pod2, got, "expired binding should be replaced")
}
