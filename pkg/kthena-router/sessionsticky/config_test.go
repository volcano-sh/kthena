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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestNewStore(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *conf.SessionStickyConfig
		wantStore bool
		wantErr   string
	}{
		{
			name:      "nil config disables session sticky",
			cfg:       nil,
			wantStore: false,
		},
		{
			name:      "an empty backend defaults to memory",
			cfg:       &conf.SessionStickyConfig{},
			wantStore: true,
		},
		{
			name:      "memory backend",
			cfg:       &conf.SessionStickyConfig{Backend: "memory"},
			wantStore: true,
		},
		{
			name:      "the backend name is case insensitive",
			cfg:       &conf.SessionStickyConfig{Backend: "MEMORY"},
			wantStore: true,
		},
		{
			name:    "redis without a redis block",
			cfg:     &conf.SessionStickyConfig{Backend: "redis"},
			wantErr: "sessionSticky.redis.address is required",
		},
		{
			name:    "redis with an empty address",
			cfg:     &conf.SessionStickyConfig{Backend: "redis", Redis: &conf.SessionStickyRedis{}},
			wantErr: "sessionSticky.redis.address is required",
		},
		{
			name:    "redis with a whitespace address",
			cfg:     &conf.SessionStickyConfig{Backend: "redis", Redis: &conf.SessionStickyRedis{Address: "   "}},
			wantErr: "sessionSticky.redis.address is required",
		},
		{
			name:    "an unknown backend is rejected",
			cfg:     &conf.SessionStickyConfig{Backend: "etcd"},
			wantErr: "is invalid (use memory or redis)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(tt.cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.Nil(t, store)
				return
			}
			require.NoError(t, err)
			if !tt.wantStore {
				require.Nil(t, store)
				return
			}
			require.NotNil(t, store)
			require.NoError(t, store.Close())
		})
	}
}

func TestTTL(t *testing.T) {
	seconds := func(v int32) *int32 { return &v }

	tests := []struct {
		name string
		spec *networkingv1alpha1.SessionSticky
		want time.Duration
	}{
		{name: "nil spec uses the default", spec: nil, want: 5 * time.Minute},
		{name: "unset seconds uses the default", spec: &networkingv1alpha1.SessionSticky{}, want: 5 * time.Minute},
		{
			name: "zero falls back to the default",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: seconds(0)},
			want: 5 * time.Minute,
		},
		{
			name: "a negative value falls back to the default",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: seconds(-30)},
			want: 5 * time.Minute,
		},
		{
			name: "one second is honoured",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: seconds(1)},
			want: time.Second,
		},
		{
			name: "a configured value is honoured",
			spec: &networkingv1alpha1.SessionSticky{SessionAffinitySeconds: seconds(90)},
			want: 90 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, TTL(tt.spec))
		})
	}
}

func lookupContext(t *testing.T, header, value string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	c.Request = req
	return c
}

func headerSpec(name string) *networkingv1alpha1.SessionSticky {
	return &networkingv1alpha1.SessionSticky{
		Sources: []networkingv1alpha1.SessionKeySource{
			{Type: networkingv1alpha1.SessionKeySourceHeader, Name: name},
		},
	}
}

func TestLookupBinding(t *testing.T) {
	owner := types.NamespacedName{Namespace: "ns", Name: "llama"}
	bound := Binding{ModelServer: "llama", Pod: "llama-0"}

	t.Run("returns a stored binding", func(t *testing.T) {
		store := NewMemoryStore()
		defer func() { require.NoError(t, store.Close()) }()
		c := lookupContext(t, "X-Session", "abc")
		key := MappingKey(owner, "abc")
		_, err := store.Commit(c.Request.Context(), key, bound, time.Minute)
		require.NoError(t, err)

		sessionKey, storeKey, binding, ok := LookupBinding(c, owner, headerSpec("X-Session"), store)
		require.True(t, ok)
		require.Equal(t, "abc", sessionKey)
		require.Equal(t, key, storeKey)
		require.Equal(t, bound, binding)
	})

	t.Run("reports a miss but still returns the keys", func(t *testing.T) {
		store := NewMemoryStore()
		defer func() { require.NoError(t, store.Close()) }()
		c := lookupContext(t, "X-Session", "abc")

		sessionKey, storeKey, binding, ok := LookupBinding(c, owner, headerSpec("X-Session"), store)
		require.False(t, ok)
		require.Equal(t, "abc", sessionKey)
		require.Equal(t, MappingKey(owner, "abc"), storeKey)
		require.Equal(t, Binding{}, binding)
	})

	t.Run("no session key in the request", func(t *testing.T) {
		store := NewMemoryStore()
		defer func() { require.NoError(t, store.Close()) }()
		c := lookupContext(t, "", "")

		sessionKey, storeKey, _, ok := LookupBinding(c, owner, headerSpec("X-Session"), store)
		require.False(t, ok)
		require.Empty(t, sessionKey)
		require.Empty(t, storeKey)
	})

	t.Run("nil spec, store or context", func(t *testing.T) {
		store := NewMemoryStore()
		defer func() { require.NoError(t, store.Close()) }()
		c := lookupContext(t, "X-Session", "abc")

		_, _, _, ok := LookupBinding(c, owner, nil, store)
		require.False(t, ok)
		_, _, _, ok = LookupBinding(c, owner, headerSpec("X-Session"), nil)
		require.False(t, ok)
		_, _, _, ok = LookupBinding(nil, owner, headerSpec("X-Session"), store)
		require.False(t, ok)
	})
}

func TestMemoryStoreSweepExpired(t *testing.T) {
	s := NewMemoryStore()
	defer func() { require.NoError(t, s.Close()) }()

	live := Binding{ModelServer: "llama", Pod: "llama-0"}
	stale := Binding{ModelServer: "llama", Pod: "llama-1"}

	// Seeded directly so the sweep is exercised without waiting out a TTL.
	s.mu.Lock()
	s.m["live"] = memoryEntry{binding: live, until: time.Now().Add(time.Hour)}
	s.m["stale"] = memoryEntry{binding: stale, until: time.Now().Add(-time.Hour)}
	s.mu.Unlock()

	s.sweepExpired()

	s.mu.RLock()
	_, liveKept := s.m["live"]
	_, staleKept := s.m["stale"]
	s.mu.RUnlock()

	require.True(t, liveKept, "an unexpired binding must survive the sweep")
	require.False(t, staleKept, "an expired binding must be swept")
}

func TestMemoryStoreDelete(t *testing.T) {
	s := NewMemoryStore()
	defer func() { require.NoError(t, s.Close()) }()
	ctx := t.Context()

	binding := Binding{ModelServer: "llama", Pod: "llama-0"}
	_, err := s.Commit(ctx, "k", binding, time.Minute)
	require.NoError(t, err)
	got, ok := s.Get(ctx, "k")
	require.True(t, ok)
	require.Equal(t, binding, got)

	s.Delete(ctx, "k")
	_, ok = s.Get(ctx, "k")
	require.False(t, ok)

	// Deleting a key that is not present is a no-op.
	s.Delete(ctx, "missing")
}

func TestBindingFromRedisFields(t *testing.T) {
	tests := []struct {
		name string
		vals []interface{}
		want Binding
		ok   bool
	}{
		{
			name: "aggregated binding",
			vals: []interface{}{"llama", "llama-0", ""},
			want: Binding{ModelServer: "llama", Pod: "llama-0"},
			ok:   true,
		},
		{
			name: "PD binding",
			vals: []interface{}{"llama", "decode-0", "prefill-0"},
			want: Binding{ModelServer: "llama", Pod: "decode-0", PrefillPod: "prefill-0"},
			ok:   true,
		},
		{name: "too few fields", vals: []interface{}{"llama", "llama-0"}},
		{name: "no fields", vals: nil},
		{name: "a missing key reads back as nil", vals: []interface{}{nil, nil, nil}},
		{name: "missing model server", vals: []interface{}{"", "llama-0", ""}},
		{name: "missing pod", vals: []interface{}{"llama", "", ""}},
		{name: "non-string fields", vals: []interface{}{42, true, ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := bindingFromRedisFields(tt.vals)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestBindingFromLuaResult(t *testing.T) {
	got, ok := bindingFromLuaResult([]interface{}{"llama", "llama-0", ""})
	require.True(t, ok)
	require.Equal(t, Binding{ModelServer: "llama", Pod: "llama-0"}, got)

	// The script returns a Lua table; anything else is not a binding.
	_, ok = bindingFromLuaResult("llama")
	require.False(t, ok)
	_, ok = bindingFromLuaResult(nil)
	require.False(t, ok)
}
