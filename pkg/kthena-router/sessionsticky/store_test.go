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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestMemoryStoreClose_ConcurrentCallsAreSafe(t *testing.T) {
	// Two callers could both find stopCh open and both close it.
	for i := 0; i < 64; i++ {
		store := NewMemoryStore()
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				require.NoError(t, store.Close())
			}()
		}
		wg.Wait()
	}
}

func TestMemoryStoreClose_IsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Close())
	require.NoError(t, store.Close())
}

func TestMemoryStoreClose_StopsTheSweeper(t *testing.T) {
	store := NewMemoryStore()
	binding := Binding{ModelServer: "ms", Pod: "pod"}
	_, err := store.Commit(context.Background(), "key", binding, time.Minute)
	require.NoError(t, err)

	require.NoError(t, store.Close())

	got, ok := store.Get(context.Background(), "key")
	require.True(t, ok)
	require.Equal(t, binding, got)
}

func TestNewStore_NilOrMemory(t *testing.T) {
	store, err := NewStore(nil)
	require.NoError(t, err)
	require.Nil(t, store)

	store, err = NewStore(&conf.SessionStickyConfig{})
	require.NoError(t, err)
	require.IsType(t, &MemoryStore{}, store)
	require.NoError(t, store.Close())

	store, err = NewStore(&conf.SessionStickyConfig{Backend: "memory"})
	require.NoError(t, err)
	require.IsType(t, &MemoryStore{}, store)
	require.NoError(t, store.Close())
}

func TestNewStore_InvalidBackend(t *testing.T) {
	_, err := NewStore(&conf.SessionStickyConfig{Backend: "etcd"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `sessionSticky.backend "etcd" is invalid`)
}

func TestNewStore_Redis_AddressRequired(t *testing.T) {
	_, err := NewStore(&conf.SessionStickyConfig{
		Backend: "redis",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessionSticky.redis.address is required")

	_, err = NewStore(&conf.SessionStickyConfig{
		Backend: "redis",
		Redis:   &conf.SessionStickyRedis{Address: "   "},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessionSticky.redis.address is required")
}

func TestNewStore_Redis_Unauthenticated(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	t.Setenv(sessionStickyRedisPasswordEnv, "")

	cfg := &conf.SessionStickyConfig{
		Backend: "redis",
		Redis:   &conf.SessionStickyRedis{Address: mr.Addr()},
	}

	store, err := NewStore(cfg)
	require.NoError(t, err)
	require.NotNil(t, store)
	require.IsType(t, &RedisStore{}, store)
	require.NoError(t, store.Close())
}

func TestNewStore_Redis_Authenticated(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	secretPassword := "test-secret-password-123"
	mr.RequireAuth(secretPassword)

	t.Setenv(sessionStickyRedisPasswordEnv, secretPassword)

	cfg := &conf.SessionStickyConfig{
		Backend: "redis",
		Redis:   &conf.SessionStickyRedis{Address: mr.Addr()},
	}

	store, err := NewStore(cfg)
	require.NoError(t, err)
	require.NotNil(t, store)
	require.IsType(t, &RedisStore{}, store)
	require.NoError(t, store.Close())
}

func TestNewStore_Redis_AuthRequired_MissingPassword(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	mr.RequireAuth("redis-requires-auth")

	t.Setenv(sessionStickyRedisPasswordEnv, "")

	cfg := &conf.SessionStickyConfig{
		Backend: "redis",
		Redis:   &conf.SessionStickyRedis{Address: mr.Addr()},
	}

	store, err := NewStore(cfg)
	require.Error(t, err)
	require.Nil(t, store)
	assert.Contains(t, err.Error(), "redis ping for session sticky")
}

func TestNewStore_Redis_AuthRequired_InvalidPassword(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	mr.RequireAuth("correct-password")

	wrongPassword := "leaked-candidate-password-xyz"
	t.Setenv(sessionStickyRedisPasswordEnv, wrongPassword)

	cfg := &conf.SessionStickyConfig{
		Backend: "redis",
		Redis:   &conf.SessionStickyRedis{Address: mr.Addr()},
	}

	store, err := NewStore(cfg)
	require.Error(t, err)
	require.Nil(t, store)
	assert.Contains(t, err.Error(), "redis ping for session sticky")
	assert.NotContains(t, err.Error(), wrongPassword, "password must never be leaked in error message")
}

func TestRedisStore_Authenticated_Operations(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	password := "auth-token-xyz"
	mr.RequireAuth(password)

	store, err := newRedisStore(mr.Addr(), password)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()

	ctx := context.Background()
	key := "test-session-binding-key"
	binding := Binding{
		ModelServer: "model-server-pd",
		Pod:         "decode-pod-0",
		PrefillPod:  "prefill-pod-0",
	}

	// 1. Get before commit -> not found
	_, ok := store.Get(ctx, key)
	require.False(t, ok)

	// 2. Commit binding with TTL
	committed, err := store.Commit(ctx, key, binding, 10*time.Minute)
	require.NoError(t, err)
	require.Equal(t, binding, committed)

	// 3. Get after commit -> found
	got, ok := store.Get(ctx, key)
	require.True(t, ok)
	require.Equal(t, binding, got)

	// 4. Delete
	store.Delete(ctx, key)
	_, ok = store.Get(ctx, key)
	require.False(t, ok)
}

func TestNewRedisStore_Unauthenticated_OriginalSignature(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Calling exported NewRedisStore with only addr preserves original exported signature.
	store, err := NewRedisStore(mr.Addr())
	require.NoError(t, err)
	require.NotNil(t, store)
	require.NoError(t, store.Close())
}
