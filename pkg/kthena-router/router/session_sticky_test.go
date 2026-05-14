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

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestRedisSessionStickyStoreSetDoesNotOverwriteExistingDifferentPod(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	store, err := newRedisSessionStickyStore(mr.Addr(), "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	store.Set("default/route", "session", types.NamespacedName{Namespace: "default", Name: "pod-a"}, time.Minute)
	store.Set("default/route", "session", types.NamespacedName{Namespace: "default", Name: "pod-b"}, time.Minute)

	pod, ok := store.Get("default/route", "session")
	require.True(t, ok)
	require.Equal(t, types.NamespacedName{Namespace: "default", Name: "pod-a"}, pod)
}

func TestRedisSessionStickyStoreSetRefreshesExistingSamePod(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	store, err := newRedisSessionStickyStore(mr.Addr(), "")
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
