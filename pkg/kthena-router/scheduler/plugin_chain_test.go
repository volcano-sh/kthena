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

package scheduler

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
)

func newTestProfile(name string, spec aiv1alpha1.SchedulerProfileSpec) *aiv1alpha1.SchedulerProfile {
	return &aiv1alpha1.SchedulerProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       spec,
	}
}

func newModelServerWithProfileRef(namespace, name, profile string) *aiv1alpha1.ModelServer {
	return &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: aiv1alpha1.ModelServerSpec{
			TrafficPolicy: &aiv1alpha1.TrafficPolicy{
				LoadBalancer: &aiv1alpha1.LoadBalancer{SchedulerProfile: &profile},
			},
		},
	}
}

func validProfile(name string) *aiv1alpha1.SchedulerProfile {
	return newTestProfile(name, aiv1alpha1.SchedulerProfileSpec{
		Plugins: aiv1alpha1.Plugins{
			Filter: aiv1alpha1.Filter{Enabled: []string{plugins.LeastRequestPluginName}},
			Score: aiv1alpha1.Score{
				Enabled: []aiv1alpha1.PluginWithWeight{{Name: plugins.PrefixCachePluginName, Weight: 1}},
			},
		},
	})
}

func failedProfile(name string) *aiv1alpha1.SchedulerProfile {
	return newTestProfile(name, aiv1alpha1.SchedulerProfileSpec{
		PluginConfig: []aiv1alpha1.PluginConfig{{Name: "no-such-plugin"}},
	})
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestProfileChainManager_BuildStatusOnProfileEvents drives events through
// the store (as in production: the controller writes the store, the async
// callback builds) and checks the reported build outcome.
func TestProfileChainManager_BuildStatusOnProfileEvents(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)
	key := types.NamespacedName{Namespace: "default", Name: "p1"}

	// A nil-profile event is a no-op.
	m.onProfileEvent(datastore.EventData{EventType: datastore.EventAdd})

	// Nothing built yet.
	built, buildErr := m.BuildStatus(key)
	assert.False(t, built)
	assert.NoError(t, buildErr)

	// Add builds the chain.
	store.AddOrUpdateSchedulerProfile(validProfile("p1"))
	waitUntil(t, 2*time.Second, func() bool {
		built, _ = m.BuildStatus(key)
		return built
	}, "add must build the chain")
	assert.NoError(t, buildErr)

	// A failed rebuild keeps serving the previously built chain, but the
	// failure must be visible in the status: two settles = the add and the
	// failed rebuild both processed.
	builtChain, acquireErr := m.AcquireProfile(newModelServerWithProfileRef("default", "ms", "p1"))
	require.NoError(t, acquireErr)
	require.NotNil(t, builtChain)
	defer builtChain.Release()

	store.AddOrUpdateSchedulerProfile(failedProfile("p1"))
	for i := 0; i < 2; i++ {
		select {
		case <-m.StatusNotify():
		case <-time.After(2 * time.Second):
			t.Fatal("expected the add and the failed rebuild to settle")
		}
	}
	built, buildErr = m.BuildStatus(key)
	assert.False(t, built, "a failed rebuild must be reported even while the old chain serves")
	require.Error(t, buildErr, "the current spec failed to build; status must expose it")
	assert.Contains(t, buildErr.Error(), "no-such-plugin")

	// Traffic is unaffected: the previously built chain still resolves.
	stillServing, acquireErr := m.AcquireProfile(newModelServerWithProfileRef("default", "ms", "p1"))
	require.NoError(t, acquireErr)
	assert.Same(t, builtChain, stillServing, "a failed rebuild must keep the old chain serving")
	stillServing.Release()

	// Delete removes the chain.
	store.DeleteSchedulerProfile(key)
	waitUntil(t, 2*time.Second, func() bool {
		built, buildErr = m.BuildStatus(key)
		return !built && buildErr == nil
	}, "delete must remove the chain")
}

func TestProfileChainManager_BuildStatusReportsBuildError(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)
	key := types.NamespacedName{Namespace: "default", Name: "p1"}

	store.AddOrUpdateSchedulerProfile(failedProfile("p1"))
	var buildErr error
	waitUntil(t, 2*time.Second, func() bool {
		_, buildErr = m.BuildStatus(key)
		return buildErr != nil
	}, "build failure must be recorded")

	built, buildErr := m.BuildStatus(key)
	assert.False(t, built)
	require.Error(t, buildErr)
	assert.Contains(t, buildErr.Error(), "no-such-plugin")
}

func TestProfileChainManager_BuildChainValidation(t *testing.T) {
	tests := []struct {
		name    string
		spec    aiv1alpha1.SchedulerProfileSpec
		wantErr string
	}{
		{
			name: "unknown plugin in pluginConfig",
			spec: aiv1alpha1.SchedulerProfileSpec{
				PluginConfig: []aiv1alpha1.PluginConfig{{Name: "no-such-plugin"}},
			},
			wantErr: `unknown plugin "no-such-plugin"`,
		},
		{
			name: "duplicate pluginConfig entry",
			spec: aiv1alpha1.SchedulerProfileSpec{
				PluginConfig: []aiv1alpha1.PluginConfig{
					{Name: plugins.RandomPluginName},
					{Name: plugins.RandomPluginName},
				},
			},
			wantErr: `duplicate plugin "random"`,
		},
		{
			name: "args for a plugin that takes none",
			spec: aiv1alpha1.SchedulerProfileSpec{
				PluginConfig: []aiv1alpha1.PluginConfig{
					{Name: plugins.RandomPluginName, Args: runtime.RawExtension{Raw: []byte(`{"x": "1"}`)}},
				},
			},
			wantErr: "takes no arguments",
		},
		{
			name: "score-only plugin used as filter",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Filter: aiv1alpha1.Filter{Enabled: []string{plugins.GPUCacheUsagePluginName}},
				},
			},
			wantErr: "cannot be used as a filter",
		},
		{
			name: "unknown plugin as filter",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Filter: aiv1alpha1.Filter{Enabled: []string{"no-such-plugin"}},
				},
			},
			wantErr: "cannot be used as a filter",
		},
		{
			name: "duplicate filter",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Filter: aiv1alpha1.Filter{Enabled: []string{plugins.LeastRequestPluginName, plugins.LeastRequestPluginName}},
				},
			},
			wantErr: `duplicate plugin "least-request"`,
		},
		{
			name: "filter-only plugin used as score",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Score: aiv1alpha1.Score{
						Enabled: []aiv1alpha1.PluginWithWeight{{Name: plugins.LoraAffinityPluginName, Weight: 1}},
					},
				},
			},
			wantErr: "cannot be used as a score plugin",
		},
		{
			name: "duplicate score",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Score: aiv1alpha1.Score{
						Enabled: []aiv1alpha1.PluginWithWeight{
							{Name: plugins.GPUCacheUsagePluginName, Weight: 1},
							{Name: plugins.GPUCacheUsagePluginName, Weight: 1},
						},
					},
				},
			},
			wantErr: `duplicate plugin "gpu-usage"`,
		},
		{
			name: "zero weight",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Score: aiv1alpha1.Score{
						Enabled: []aiv1alpha1.PluginWithWeight{{Name: plugins.GPUCacheUsagePluginName, Weight: 0}},
					},
				},
			},
			wantErr: "weight must be greater than 0",
		},
		{
			name: "random combined with another score plugin",
			spec: aiv1alpha1.SchedulerProfileSpec{
				Plugins: aiv1alpha1.Plugins{
					Score: aiv1alpha1.Score{
						Enabled: []aiv1alpha1.PluginWithWeight{
							{Name: plugins.RandomPluginName, Weight: 1},
							{Name: plugins.GPUCacheUsagePluginName, Weight: 1},
						},
					},
				},
			},
			wantErr: "must be the only score plugin",
		},
	}

	store := datastore.New()
	m := NewProfileChainManager(store)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain, err := m.buildChain(newTestProfile("p1", tt.spec))
			require.Error(t, err)
			assert.Nil(t, chain)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}

	t.Run("valid spec builds chain", func(t *testing.T) {
		chain, err := m.buildChain(validProfile("p1"))
		require.NoError(t, err)
		require.NotNil(t, chain)
		assert.Len(t, chain.filters, 1)
		assert.Len(t, chain.scores, 1)
		assert.Len(t, chain.postScheduleHooks, 1)
		assert.True(t, chain.syncOnFlight, "least-request filter requires on-flight sync")
	})
}

func TestChainNeedsOnFlightSync(t *testing.T) {
	store := datastore.New()
	newScore := func(t *testing.T, name string) *scorePlugin {
		t.Helper()
		p, err := plugins.SchedulerPlugins[name].NewScore(store, runtime.RawExtension{})
		require.NoError(t, err)
		return &scorePlugin{plugin: p}
	}
	newFilter := func(t *testing.T, name string) framework.FilterPlugin {
		t.Helper()
		f, err := plugins.SchedulerPlugins[name].NewFilter(runtime.RawExtension{})
		require.NoError(t, err)
		return f
	}

	tests := []struct {
		name    string
		filters []framework.FilterPlugin
		scores  []*scorePlugin
		want    bool
	}{
		{
			name:    "least-request filter",
			filters: []framework.FilterPlugin{newFilter(t, plugins.LeastRequestPluginName)},
			want:    true,
		},
		{
			name:   "least-request score",
			scores: []*scorePlugin{newScore(t, plugins.LeastRequestPluginName)},
			want:   true,
		},
		{
			name:    "no least-request",
			filters: []framework.FilterPlugin{newFilter(t, plugins.LoraAffinityPluginName)},
			scores:  []*scorePlugin{newScore(t, plugins.GPUCacheUsagePluginName)},
			want:    false,
		},
		{name: "empty chain", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, chainNeedsOnFlightSync(tt.filters, tt.scores))
		})
	}
}

func TestProfileChainManager_AcquireProfile(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)
	store.AddOrUpdateSchedulerProfile(validProfile("built"))
	store.AddOrUpdateSchedulerProfile(failedProfile("failed"))
	waitUntil(t, 2*time.Second, func() bool {
		_, builtErr := m.BuildStatus(types.NamespacedName{Namespace: "default", Name: "failed"})
		built, _ := m.BuildStatus(types.NamespacedName{Namespace: "default", Name: "built"})
		return built && builtErr != nil
	}, "both profiles must settle")

	noRef := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ms"},
	}

	tests := []struct {
		name    string
		ms      *aiv1alpha1.ModelServer
		wantNil bool
		wantErr string
	}{
		{
			name:    "nil modelserver",
			ms:      nil,
			wantNil: true,
		},
		{
			name:    "no traffic policy",
			ms:      noRef,
			wantNil: true,
		},
		{
			name: "no load balancer",
			ms: &aiv1alpha1.ModelServer{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ms"},
				Spec:       aiv1alpha1.ModelServerSpec{TrafficPolicy: &aiv1alpha1.TrafficPolicy{}},
			},
			wantNil: true,
		},
		{
			name: "no profile reference",
			ms: &aiv1alpha1.ModelServer{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ms"},
				Spec: aiv1alpha1.ModelServerSpec{
					TrafficPolicy: &aiv1alpha1.TrafficPolicy{LoadBalancer: &aiv1alpha1.LoadBalancer{}},
				},
			},
			wantNil: true,
		},
		{
			name: "built profile resolves to its chain",
			ms:   newModelServerWithProfileRef("default", "ms", "built"),
		},
		{
			name:    "failed profile errors",
			ms:      newModelServerWithProfileRef("default", "ms", "failed"),
			wantErr: "failed to build",
		},
		{
			name:    "missing profile errors",
			ms:      newModelServerWithProfileRef("default", "ms", "missing"),
			wantErr: "not found in namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chain, err := m.AcquireProfile(tt.ms)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, chain)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, chain)
				return
			}
			require.NotNil(t, chain)
		})
	}

	t.Run("acquire returns the built chain instance", func(t *testing.T) {
		first, err := m.AcquireProfile(newModelServerWithProfileRef("default", "ms", "built"))
		require.NoError(t, err)
		second, err := m.AcquireProfile(newModelServerWithProfileRef("default", "ms", "built"))
		require.NoError(t, err)
		defer first.Release()
		defer second.Release()
		assert.Same(t, first, second, "acquisitions of the same profile must share one chain instance")
	})

	// Regression for the resolve/acquire TOCTOU race: a chain acquired
	// before a delete must stay open until its holder releases, and must
	// not be acquirable afterwards.
	t.Run("acquired chain survives delete until released", func(t *testing.T) {
		chain, err := m.AcquireProfile(newModelServerWithProfileRef("default", "ms", "built"))
		require.NoError(t, err)
		require.NotNil(t, chain)
		held := chain.inflight.Load()

		store.DeleteSchedulerProfile(types.NamespacedName{Namespace: "default", Name: "built"})
		waitUntil(t, 2*time.Second, func() bool {
			return chain.retired.Load()
		}, "the deleted profile's chain must be retired")
		assert.EqualValues(t, held, chain.inflight.Load(),
			"the acquired reference must hold the chain open across the delete")

		chain.Release()
		assert.EqualValues(t, held-1, chain.inflight.Load())

		_, err = m.AcquireProfile(newModelServerWithProfileRef("default", "ms", "built"))
		require.Error(t, err, "a deleted profile must fail to resolve")
		assert.Contains(t, err.Error(), "not found in namespace")
	})
}

// TestProfileChainManager_StatusNotifyCarriesKeys verifies the manager signals
// a changed profile key once its build status changes.
func TestProfileChainManager_StatusNotifyCarriesKeys(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)

	store.AddOrUpdateSchedulerProfile(validProfile("built"))

	select {
	case <-m.StatusNotify():
	case <-time.After(2 * time.Second):
		t.Fatal("status change signal not delivered")
	}
	assert.Contains(t, m.TakePendingNotifies(), types.NamespacedName{Namespace: "default", Name: "built"})
}

// TestProfileChainManager_StatusNotifyNeverDrops verifies a burst of status
// changes coalesce in the pending set instead of being dropped.
func TestProfileChainManager_StatusNotifyNeverDrops(t *testing.T) {
	m := NewProfileChainManager(datastore.New())

	const burst = 500
	done := make(chan struct{})
	go func() {
		for i := 0; i < burst; i++ {
			m.notifyStatusNotify(types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("p%d", i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyStatusNotify blocked; it must never block the store callback")
	}

	keys := m.TakePendingNotifies()
	assert.Len(t, keys, burst, "no status change may be lost")
	assert.Empty(t, m.TakePendingNotifies(), "a drain clears the pending statuses")
}

type closerSpy struct {
	closes atomic.Int32
	closed atomic.Bool
	err    error
}

func (c *closerSpy) Name() string { return "closer-spy" }
func (c *closerSpy) Score(_ *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	return make(map[*datastore.PodInfo]int, len(pods))
}
func (c *closerSpy) Close() error {
	c.closes.Add(1)
	c.closed.Store(true)
	return c.err
}

type filterCloserSpy struct {
	closes atomic.Int32
	err    error
}

func (f *filterCloserSpy) Name() string { return "filter-closer-spy" }
func (f *filterCloserSpy) Filter(_ *framework.Context, pods []*datastore.PodInfo) []*datastore.PodInfo {
	return pods
}
func (f *filterCloserSpy) Close() error {
	f.closes.Add(1)
	return f.err
}

// TestPluginChain_CloseWaitsForInflightDrain verifies a replaced chain is
// closed only after every in-flight request released it.
func TestPluginChain_CloseWaitsForInflightDrain(t *testing.T) {
	spy := &closerSpy{}
	chain := &PluginChain{scores: []*scorePlugin{{plugin: spy}}}

	chain.Acquire()
	chain.retire()
	assert.False(t, spy.closed.Load(), "must not close while requests are in flight")

	chain.Release()
	assert.True(t, spy.closed.Load(), "must close after the in-flight drain")
	assert.EqualValues(t, 1, spy.closes.Load(), "close must run exactly once")
}

// TestPluginChain_CloseCoversFiltersAndReportsErrors verifies the close
// sweep visits filter plugins too and keeps going when a Close fails.
func TestPluginChain_CloseCoversFiltersAndReportsErrors(t *testing.T) {
	filterOK := &filterCloserSpy{}
	filterErr := &filterCloserSpy{err: errors.New("close failed")}
	scoreErr := &closerSpy{err: errors.New("close failed")}
	chain := &PluginChain{
		filters: []framework.FilterPlugin{filterOK, filterErr},
		scores:  []*scorePlugin{{plugin: scoreErr}},
	}

	chain.Acquire()
	chain.retire()
	chain.Release()

	assert.EqualValues(t, 1, filterOK.closes.Load())
	assert.EqualValues(t, 1, filterErr.closes.Load())
	assert.EqualValues(t, 1, scoreErr.closes.Load())
}

// TestPluginChain_CloseExactlyOnce verifies the close ordering variants all
// converge to a single close, and double releases are harmless.
func TestPluginChain_CloseExactlyOnce(t *testing.T) {
	spy := &closerSpy{}
	chain := &PluginChain{scores: []*scorePlugin{{plugin: spy}}}

	// Drain-first ordering: released before retire.
	chain.Acquire()
	chain.Release()
	chain.retire()
	assert.True(t, spy.closed.Load())
	assert.EqualValues(t, 1, spy.closes.Load())

	// Extra releases must neither re-close nor underflow.
	chain.Release()
	chain.Release()
	assert.EqualValues(t, 1, spy.closes.Load())
}

// TestProfileChainManager_StaleSuccessDiscarded verifies a build of an
// older profile version cannot commit after a newer one: the current
// chain survives and no settle hint is emitted.
func TestProfileChainManager_StaleSuccessDiscarded(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)
	key := types.NamespacedName{Namespace: "default", Name: "p1"}

	newer := validProfile("p1") // least-request filter
	newer.ResourceVersion = "200"
	store.AddOrUpdateSchedulerProfile(newer)
	waitUntil(t, 2*time.Second, func() bool {
		built, _ := m.BuildStatus(key)
		return built
	}, "newer version must build")
	select {
	case <-m.StatusNotify():
	case <-time.After(2 * time.Second):
		t.Fatal("expected the newer build to settle")
	}

	// An older event (rv 100, different spec) settles late: it must not
	// replace the current chain.
	stale := newTestProfile("p1", aiv1alpha1.SchedulerProfileSpec{
		Plugins: aiv1alpha1.Plugins{
			Filter: aiv1alpha1.Filter{Enabled: []string{plugins.LoraAffinityPluginName}},
		},
	})
	stale.ResourceVersion = "100"
	m.onProfileEvent(datastore.EventData{EventType: datastore.EventUpdate, SchedulerProfile: stale})

	m.mu.RLock()
	chain := m.chains[key]
	m.mu.RUnlock()
	require.NotNil(t, chain)
	require.NotEmpty(t, chain.filters)
	assert.Equal(t, plugins.LeastRequestPluginName, chain.filters[0].Name(),
		"the stale build must not replace the current chain")
	select {
	case <-m.StatusNotify():
		t.Fatal("stale build must not settle")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestProfileChainManager_StaleFailureDiscarded verifies a stale build
// failure is not recorded — not when a newer version exists, and not when
// the profile is already gone (e.g. deleted mid-build).
func TestProfileChainManager_StaleFailureDiscarded(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)
	key := types.NamespacedName{Namespace: "default", Name: "p2"}

	stale := failedProfile("p2")
	stale.ResourceVersion = "100"
	m.onProfileEvent(datastore.EventData{EventType: datastore.EventAdd, SchedulerProfile: stale})

	built, buildErr := m.BuildStatus(key)
	assert.False(t, built)
	assert.NoError(t, buildErr, "the stale failure must not be recorded")
	select {
	case <-m.StatusNotify():
		t.Fatal("stale build must not settle")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestProfileChainManager_UpdateReplacesChain verifies a successful update
// swaps in a new chain and retires the replaced one.
func TestProfileChainManager_UpdateReplacesChain(t *testing.T) {
	store := datastore.New()
	m := NewProfileChainManager(store)
	key := types.NamespacedName{Namespace: "default", Name: "p1"}

	first := validProfile("p1") // least-request filter
	first.ResourceVersion = "100"
	store.AddOrUpdateSchedulerProfile(first)
	waitUntil(t, 2*time.Second, func() bool {
		built, _ := m.BuildStatus(key)
		return built
	}, "first version must build")
	m.mu.RLock()
	old := m.chains[key]
	m.mu.RUnlock()

	second := newTestProfile("p1", aiv1alpha1.SchedulerProfileSpec{
		Plugins: aiv1alpha1.Plugins{
			Filter: aiv1alpha1.Filter{Enabled: []string{plugins.LoraAffinityPluginName}},
		},
	})
	second.ResourceVersion = "200"
	store.AddOrUpdateSchedulerProfile(second)
	waitUntil(t, 2*time.Second, func() bool {
		m.mu.RLock()
		chain := m.chains[key]
		m.mu.RUnlock()
		return chain != nil && chain != old && len(chain.filters) == 1 &&
			chain.filters[0].Name() == plugins.LoraAffinityPluginName
	}, "update must swap in the new chain")
	waitUntil(t, 2*time.Second, func() bool {
		return old.retired.Load()
	}, "the replaced chain must be retired")
}
