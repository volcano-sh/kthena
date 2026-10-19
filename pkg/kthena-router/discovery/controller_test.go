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

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

type testSource struct {
	snapshot func(context.Context) ([]api.Endpoint, error)
	closed   atomic.Int32
}

func (s *testSource) Snapshot(ctx context.Context) ([]api.Endpoint, error) { return s.snapshot(ctx) }
func (s *testSource) Close()                                               { s.closed.Add(1) }

func snapshotSource(endpoints []api.Endpoint, err error) Source {
	return &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) { return endpoints, err }}
}

func testConfig() Config {
	return Config{Provider: "test", RefreshInterval: "1h", SnapshotTimeout: "5s",
		Mappings: []Mapping{{ModelServer: "default/model"}}}
}

func testModel() *api.ModelServer {
	return &api.ModelServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "model", UID: "first"},
		Spec: api.ModelServerSpec{InferenceEngine: api.VLLM, WorkloadPort: api.WorkloadPort{Port: 8000}}}
}

func testController(t *testing.T, config Config, store datastore.Store, factory Factory) *Controller {
	t.Helper()
	if factory == nil {
		factory = func(types.NamespacedName, map[string]interface{}, func()) (Source, error) {
			return snapshotSource(nil, nil), nil
		}
	}
	if store == nil {
		store = datastore.New(datastore.WithDeferredRuntimeInspection())
	}
	c, err := NewController(config, store, Registry{"test": factory})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}

// Install a target without a background worker so reconciliation can be exercised
// synchronously, including failure and stale-result paths, without timing sleeps.
func installTarget(t *testing.T, c *Controller, ms *api.ModelServer) (*target, *subscription) {
	t.Helper()
	ctx, cancel := context.WithCancel(c.ctx)
	t.Cleanup(cancel)
	sub := &subscription{ms: ms, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	target := c.targets[utils.GetNamespaceName(ms)]
	target.current = sub
	require.NoError(t, c.store.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]()))
	return target, sub
}

func TestSnapshotReconciliation(t *testing.T) {
	endpoints := []api.Endpoint{{Name: "a", Address: "10.0.0.1", Port: ptr.To(int32(9000)), Labels: map[string]string{"role": "decode"}}}
	var lookupErr error
	c := testController(t, testConfig(), nil, nil)
	store := c.store
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	var source Source = &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) { return endpoints, lookupErr }}
	refresh := func() error { return c.refresh(utils.GetNamespaceName(ms), target, sub, &source) }
	podName := types.NamespacedName{Namespace: "default", Name: "model:a"}

	require.NoError(t, refresh())
	info := store.GetPodInfo(podName)
	require.NotNil(t, info)
	require.Equal(t, int32(9000), utils.EndpointPort(info.GetPod(), 8000))
	require.Equal(t, "decode", info.GetPod().Labels["role"])
	// Updating metadata preserves existing PodInfo (runtime counters and metrics).
	endpoints[0].Labels = map[string]string{"role": "prefill"}
	require.NoError(t, refresh())
	require.Same(t, info, store.GetPodInfo(podName))
	require.Equal(t, "prefill", info.GetPod().Labels["role"])

	m := metrics.DefaultMetrics
	name := utils.GetNamespaceName(ms).String()
	lastSuccess := m.DiscoveryLastSuccessTimestamp.WithLabelValues(name)
	count := m.DiscoveryEndpoints.WithLabelValues(name)
	value := func(gauge prometheus.Gauge) float64 {
		t.Helper()
		var metric dto.Metric
		require.NoError(t, gauge.Write(&metric))
		return metric.GetGauge().GetValue()
	}
	timestamp := value(lastSuccess)
	require.Positive(t, timestamp)
	assertRetained := func() {
		t.Helper()
		require.Same(t, info, store.GetPodInfo(podName))
		require.Equal(t, timestamp, value(lastSuccess))
		require.Equal(t, float64(1), value(count))
	}
	lookupErr = errors.New("registry unavailable")
	endpoints = nil
	require.Error(t, refresh())
	assertRetained()
	lookupErr = nil
	endpoints = []api.Endpoint{{Name: "bad", Address: "http://not-an-address"}}
	require.Error(t, refresh())
	assertRetained()
	endpoints = []api.Endpoint{{Name: "a", Address: "10.0.0.2", Port: ptr.To(int32(9000))}}
	require.ErrorContains(t, refresh(), "without changing its name")
	assertRetained()

	// Timeouts must retain the same membership as lookup and validation errors.
	saved := source
	timeout := c.timeout
	c.timeout = time.Millisecond
	source = &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	require.ErrorIs(t, refresh(), context.DeadlineExceeded)
	assertRetained()
	source = saved
	c.timeout = timeout

	endpoints = []api.Endpoint{}
	require.NoError(t, refresh())
	require.Zero(t, value(count))
	require.Nil(t, store.GetPodInfo(podName))
	pods, err := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.Empty(t, pods)
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(ms)))
	require.False(t, m.DiscoveryEndpoints.DeleteLabelValues(name))
	require.False(t, m.DiscoveryLastSuccessTimestamp.DeleteLabelValues(name))
}

func TestSnapshotValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoints []api.Endpoint
		valid     bool
	}{
		{"empty", nil, true},
		{"dns", []api.Endpoint{{Name: "a", Address: "model.example.org"}}, true},
		{"ipv6", []api.Endpoint{{Name: "a", Address: "2001:db8::1"}}, true},
		{"missing name", []api.Endpoint{{Address: "10.0.0.1"}}, false},
		{"name with slash", []api.Endpoint{{Name: "a/b", Address: "10.0.0.1"}}, false},
		{"name with port", []api.Endpoint{{Name: "10.0.0.1:8000", Address: "10.0.0.1"}}, false},
		{"uppercase name", []api.Endpoint{{Name: "Instance-A", Address: "10.0.0.1"}}, false},
		{"name too long", []api.Endpoint{{Name: strings.Repeat("a", 254), Address: "10.0.0.1"}}, false},
		{"DNS name", []api.Endpoint{{Name: "instance-a.example", Address: "10.0.0.1"}}, true},
		{"duplicates", []api.Endpoint{{Name: "a", Address: "10.0.0.1"}, {Name: "a", Address: "10.0.0.2"}}, false},
		{"port", []api.Endpoint{{Name: "a", Address: "10.0.0.1", Port: ptr.To(int32(0))}}, false},
		{"label", []api.Endpoint{{Name: "a", Address: "10.0.0.1", Labels: map[string]string{"bad key": "v"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSnapshot(testModel(), tc.endpoints, nil)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestLateSnapshotCannotRestoreDeletedModelServer(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	s := &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
		close(started)
		<-release
		return []api.Endpoint{{Name: "old", Address: "10.0.0.1"}}, nil
	}}
	c := testController(t, testConfig(), nil, nil)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	var source Source = s
	go func() { done <- c.refresh(utils.GetNamespaceName(ms), target, sub, &source) }()
	receive(t, started)
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(ms)))
	// A replacement with the same name has a separate subscription and identity.
	replacement := ms.DeepCopy()
	replacement.UID = "replacement"
	installTarget(t, c, replacement)
	unblock()
	require.ErrorIs(t, receive(t, done), context.Canceled)
	require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source), "an obsolete subscription cannot refresh again")
	require.Equal(t, types.UID("replacement"), c.store.GetModelServer(utils.GetNamespaceName(ms)).UID)
	require.Nil(t, c.store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "model:old"}))
}

func TestModelServerUpdateDuringSnapshot(t *testing.T) {
	for _, specChanged := range []bool{false, true} {
		t.Run(fmt.Sprintf("specChanged=%t", specChanged), func(t *testing.T) {
			c := testController(t, testConfig(), nil, nil)
			ms := testModel()
			target, sub := installTarget(t, c, ms)
			next := ms.DeepCopy()
			next.ResourceVersion = "2"
			next.Annotations = map[string]string{"note": "updated"}
			if specChanged {
				next.Spec.Model = ptr.To("new-model")
			}
			var source Source = &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
				require.NoError(t, c.UpdateModelServer(next))
				return []api.Endpoint{{Name: "a", Address: "10.0.0.1"}}, nil
			}}
			require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
			require.Equal(t, next, c.store.GetModelServer(utils.GetNamespaceName(ms)))
			pod := c.store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "model:a"})
			if specChanged {
				require.Nil(t, pod, "spec changes discard the in-flight snapshot")
			} else {
				require.NotNil(t, pod, "metadata changes keep the in-flight snapshot")
			}
		})
	}
}

func TestModelServerRuntimeChangeKeepsMembership(t *testing.T) {
	c := testController(t, testConfig(), nil, nil)
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	endpoints := []api.Endpoint{{Name: "inherited", Address: "10.0.0.1"}, {Name: "explicit", Address: "10.0.0.2", Port: ptr.To(int32(9000))}}
	source := snapshotSource(endpoints, nil)
	require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	info := c.store.GetPodInfo(types.NamespacedName{Namespace: ms.Namespace, Name: "model:inherited"})
	info.IncrOnFlightRequests()
	for _, change := range []string{"port", "engine"} {
		next := ms.DeepCopy()
		if change == "port" {
			next.Spec.WorkloadPort.Port++
		} else {
			next.Spec.InferenceEngine = api.SGLang
		}
		require.NoError(t, c.UpdateModelServer(next))
		pods, err := c.store.GetPodsByModelServer(utils.GetNamespaceName(ms))
		require.NoError(t, err)
		require.Len(t, pods, 2)
		require.Same(t, info, c.store.GetPodInfo(info.GetPodNamespacedName()))
		require.Equal(t, int64(1), info.GetOnFlightRequestNum())
		require.Equal(t, next.Spec.WorkloadPort.Port, utils.EndpointPort(info.GetPod(), 0))
		require.Equal(t, string(next.Spec.InferenceEngine), info.GetEngine())
		explicit := c.store.GetPodInfo(types.NamespacedName{Namespace: ms.Namespace, Name: "model:explicit"})
		require.Equal(t, int32(9000), utils.EndpointPort(explicit.GetPod(), 0))
		require.True(t, c.HasSynced())
		// The same raw snapshot remains valid after changing the fallback port.
		require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
		ms = next
	}
	source = snapshotSource(nil, errors.New("unavailable"))
	require.Error(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	require.Same(t, info, c.store.GetPodInfo(info.GetPodNamespacedName()))
}

type failingStore struct {
	datastore.Store
	failUpdate bool
}

func (s *failingStore) AddOrUpdateModelServer(ms *api.ModelServer, pods sets.Set[types.NamespacedName]) error {
	if s.failUpdate {
		return errors.New("store update failed")
	}
	return s.Store.AddOrUpdateModelServer(ms, pods)
}

func TestFailedModelServerUpdateCanBeRetried(t *testing.T) {
	c := testController(t, testConfig(), nil, nil)
	ms := testModel()
	_, sub := installTarget(t, c, ms)
	failing := &failingStore{Store: c.store, failUpdate: true}
	c.store = failing
	next := ms.DeepCopy()
	next.Spec.Model = ptr.To("updated-model")
	require.Error(t, c.UpdateModelServer(next))
	require.Equal(t, ms.Spec.Model, sub.ms.Spec.Model)
	failing.failUpdate = false
	require.NoError(t, c.UpdateModelServer(next))
	require.Equal(t, ptr.To("updated-model"), c.store.GetModelServer(utils.GetNamespaceName(ms)).Spec.Model)
}

func TestSourceLifecycleAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		s := &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
			close(started)
			<-ctx.Done()
			<-release // Keep the slot occupied until cancellation reaches both workers.
			return nil, ctx.Err()
		}}
		config := testConfig()
		config.MaxConcurrentSnapshots = 1
		config.Mappings = append(config.Mappings, Mapping{ModelServer: "default/other"})
		var factories atomic.Int32
		c := testController(t, config, nil, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) {
			factories.Add(1)
			return s, nil
		})
		unblock := sync.OnceFunc(func() { close(release) })
		t.Cleanup(unblock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.Run(ctx) }()
		require.NoError(t, c.UpdateModelServer(testModel()))
		<-started
		other := testModel()
		other.Name = "other"
		require.NoError(t, c.UpdateModelServer(other))
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.Equal(t, int32(1), factories.Load())
		unblock()
		require.NoError(t, <-done)
		c.Close()
		require.Equal(t, int32(1), factories.Load())
		require.Equal(t, int32(1), s.closed.Load())
		require.Error(t, c.UpdateModelServer(testModel()))
	})
}

func TestUnmappedStaticAndEmptyPools(t *testing.T) {
	c := testController(t, testConfig(), nil, nil)
	store := c.store
	ms := testModel()
	ms.Name = "unmapped"
	ms.Spec.Endpoints = []api.Endpoint{{Name: "a", Address: "10.0.0.1"}}
	require.NoError(t, c.UpdateModelServer(ms))
	podName := types.NamespacedName{Namespace: "default", Name: "unmapped:a"}
	require.NotNil(t, store.GetPodInfo(podName))
	ms.Spec.Endpoints = nil
	ms.Spec.WorkloadSelector = &api.WorkloadSelector{MatchLabels: map[string]string{"app": "model"}}
	require.NoError(t, c.UpdateModelServer(ms))
	require.Nil(t, store.GetPodInfo(podName))
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(ms)))
	require.Nil(t, store.GetModelServer(utils.GetNamespaceName(ms)))
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
		valid  bool
	}{
		{"default", Config{}, true},
		{"native", Config{Provider: "kubernetes"}, true},
		{"external", testConfig(), true},
		{"no mappings", Config{Provider: "test"}, false},
		{"native mappings", Config{Mappings: testConfig().Mappings}, false},
		{"bad mapping", Config{Provider: "test", Mappings: []Mapping{{ModelServer: "model"}}}, false},
		{"duplicate", Config{Provider: "test", Mappings: []Mapping{{ModelServer: "ns/ms"}, {ModelServer: "ns/ms"}}}, false},
		{"bad interval", Config{RefreshInterval: "0s"}, false},
		{"bad timeout", Config{SnapshotTimeout: "invalid"}, false},
		{"negative concurrency", Config{MaxConcurrentSnapshots: -1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	_, err := NewController(testConfig(), nil, Registry{})
	require.ErrorContains(t, err, "not registered")
}

func TestNotificationsAndPeriodicRefresh(t *testing.T) {
	for _, mode := range []string{"notification", "periodic"} {
		t.Run(mode, func(t *testing.T) {
			config := testConfig()
			if mode == "periodic" {
				config.RefreshInterval = "20ms"
			}
			var count atomic.Int32
			notifications := make(chan func(), 1)
			c := testController(t, config, nil, func(_ types.NamespacedName, _ map[string]interface{}, notify func()) (Source, error) {
				notifications <- notify
				return &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
					n := count.Add(1)
					return []api.Endpoint{{Name: fmt.Sprintf("instance-%d", n), Address: "10.0.0.1"}}, nil
				}}, nil
			})
			store := c.store
			require.NoError(t, c.UpdateModelServer(testModel()))
			notify := receive(t, notifications)
			require.Eventually(t, func() bool {
				pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(testModel()))
				return len(pods) == 1
			}, 5*time.Second, time.Millisecond)
			if mode == "notification" {
				notify()
			}
			require.Eventually(t, func() bool {
				pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(testModel()))
				return len(pods) == 1 && pods[0].GetPod().Name != "model:instance-1"
			}, 5*time.Second, time.Millisecond)
			c.Close()
			// A late callback cannot reopen the subscription or block shutdown.
			notify()
		})
	}
}

func TestInitialRetryAndRefreshJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		config.RefreshInterval = "4s"
		starts := make(chan time.Time, 1)
		attempt := 0
		// Exercise both factory and snapshot failures before the first success.
		c := testController(t, config, nil, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) {
			attempt++
			if attempt == 1 {
				starts <- time.Now()
				return nil, errors.New("factory unavailable")
			}
			return &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
				starts <- time.Now()
				attempt++
				if attempt != 6 {
					return nil, errors.New("lookup unavailable")
				}
				return nil, nil
			}}, nil
		})
		require.NoError(t, c.UpdateModelServer(testModel()))
		previous := <-starts
		var delays []time.Duration
		for i, expected := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second} {
			start := <-starts
			delay := start.Sub(previous)
			minimum, maximum := max(time.Second, expected*9/10), expected*11/10
			if i < 4 { // Initial retries are capped at refreshInterval.
				maximum = min(maximum, c.interval)
			}
			require.GreaterOrEqual(t, delay, minimum)
			require.LessOrEqual(t, delay, maximum)
			delays = append(delays, delay)
			previous = start
		}
		require.Greater(t, sets.New(delays[4:]...).Len(), 1, "periodic refreshes must not use a fixed interval")
		c.Close() // Cancel a pending timer without waiting for its deadline.
	})
}

func TestBlockedSourceDoesNotBlockOtherTargets(t *testing.T) {
	config := testConfig()
	config.Mappings = append(config.Mappings, Mapping{ModelServer: "default/other"})
	started := make(chan struct{})
	c := testController(t, config, nil, func(name types.NamespacedName, _ map[string]interface{}, _ func()) (Source, error) {
		return &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
			if name.Name == "model" {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []api.Endpoint{{Name: "a", Address: "10.0.0.1"}}, nil
		}}, nil
	})
	require.NoError(t, c.UpdateModelServer(testModel()))
	receive(t, started)
	other := testModel()
	other.Name = "other"
	require.NoError(t, c.UpdateModelServer(other))
	require.Eventually(t, func() bool {
		return c.store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "other:a"}) != nil
	}, 5*time.Second, time.Millisecond)
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(testModel())))
	require.Nil(t, c.store.GetModelServer(utils.GetNamespaceName(testModel())))
}

func TestInitialSnapshotReadiness(t *testing.T) {
	c := testController(t, testConfig(), nil, nil)
	ms := testModel()
	// Mappings whose resources do not exist do not prevent deploying the router first.
	require.True(t, c.HasSynced())
	target, sub := installTarget(t, c, ms)
	require.False(t, c.HasSynced())
	source := snapshotSource(nil, errors.New("unavailable"))
	require.Error(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	require.False(t, c.HasSynced())
	source = snapshotSource(nil, nil)
	require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	require.True(t, c.HasSynced(), "a successful empty snapshot completes initialization")
	source = snapshotSource(nil, errors.New("unavailable again"))
	require.Error(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	require.True(t, c.HasSynced(), "readiness does not claim continued registry health")
	next := ms.DeepCopy()
	next.Spec.WorkloadPort.Port++
	require.NoError(t, c.UpdateModelServer(next))
	require.True(t, c.HasSynced(), "configuration changes reuse the successful snapshot")
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(ms)))
	require.True(t, c.HasSynced())
	replacement := ms.DeepCopy()
	replacement.UID = "replacement"
	installTarget(t, c, replacement)
	require.False(t, c.HasSynced())
	c.Close()
	require.False(t, c.HasSynced())
}

func TestInitialSnapshotReadinessCanBeDisabled(t *testing.T) {
	config := testConfig()
	config.RequireInitialSnapshot = ptr.To(false)
	c := testController(t, config, nil, nil)
	installTarget(t, c, testModel())
	require.True(t, c.HasSynced())
}

func TestSnapshotConcurrencyLimit(t *testing.T) {
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			config := testConfig()
			config.MaxConcurrentSnapshots = limit
			effectiveLimit := limit
			if effectiveLimit == 0 {
				effectiveLimit = 4
			}
			config.Mappings = nil
			for i := 0; i < effectiveLimit+2; i++ {
				config.Mappings = append(config.Mappings, Mapping{ModelServer: fmt.Sprintf("default/model-%d", i)})
			}
			started := make(chan struct{}, len(config.Mappings))
			release := make(chan struct{})
			var active, peak atomic.Int32
			factory := func(types.NamespacedName, map[string]interface{}, func()) (Source, error) {
				return &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
					n := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
					}
					started <- struct{}{}
					select {
					case <-release:
						return nil, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}}, nil
			}
			c := testController(t, config, nil, factory)
			for i := range config.Mappings {
				ms := testModel()
				ms.Name = fmt.Sprintf("model-%d", i)
				require.NoError(t, c.UpdateModelServer(ms))
			}
			for i := 0; i < effectiveLimit; i++ {
				receive(t, started)
			}
			require.False(t, c.HasSynced())
			close(release)
			require.Eventually(t, c.HasSynced, 5*time.Second, time.Millisecond)
			require.Equal(t, int32(effectiveLimit), peak.Load())
		})
	}
}

type gatedStore struct {
	datastore.Store
	started chan struct{}
	release chan struct{}
}

func (s *gatedStore) AddOrUpdateModelServer(ms *api.ModelServer, pods sets.Set[types.NamespacedName]) error {
	if ms.Name == "model" && len(pods) > 0 {
		close(s.started)
		<-s.release
	}
	return s.Store.AddOrUpdateModelServer(ms, pods)
}

func TestSnapshotSlotReleasedBeforeStoreWrite(t *testing.T) {
	config := testConfig()
	config.MaxConcurrentSnapshots = 1
	config.Mappings = append(config.Mappings, Mapping{ModelServer: "default/other"})
	store := &gatedStore{Store: datastore.New(datastore.WithDeferredRuntimeInspection()), started: make(chan struct{}), release: make(chan struct{})}
	c := testController(t, config, store, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) {
		return snapshotSource([]api.Endpoint{{Name: "a", Address: "10.0.0.1"}}, nil), nil
	})
	t.Cleanup(func() { close(store.release) })
	require.NoError(t, c.UpdateModelServer(testModel()))
	receive(t, store.started)
	other := testModel()
	other.Name = "other"
	require.NoError(t, c.UpdateModelServer(other))
	require.Eventually(t, func() bool {
		return store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "other:a"}) != nil
	}, 5*time.Second, time.Millisecond)
}

type blockedInspector struct {
	started chan struct{}
	release chan struct{}
}

func (i *blockedInspector) GetPodMetrics(string, *corev1.Pod, uint32, map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	close(i.started)
	<-i.release
	return nil, nil
}

func (*blockedInspector) GetPodModels(string, *corev1.Pod, uint32) ([]string, error) {
	return nil, nil
}

func TestSlowMetricsDoNotBlockMembershipOrClose(t *testing.T) {
	i := &blockedInspector{started: make(chan struct{}), release: make(chan struct{})}
	store := datastore.New(datastore.WithPodRuntimeInspector(i), datastore.WithDeferredRuntimeInspection())
	c := testController(t, testConfig(), store, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) {
		return snapshotSource([]api.Endpoint{{Name: "a", Address: "10.0.0.1"}}, nil), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); close(i.release) })
	require.NoError(t, c.UpdateModelServer(testModel()))
	require.Eventually(t, c.HasSynced, 5*time.Second, time.Millisecond)
	select {
	case <-i.started:
		t.Fatal("membership reconciliation performed synchronous metrics I/O")
	default:
	}
	store.Run(ctx)
	receive(t, i.started)
	done := make(chan error, 1)
	go func() {
		next := testModel()
		next.Spec.Model = ptr.To("updated")
		if err := c.UpdateModelServer(next); err != nil {
			done <- err
			return
		}
		err := c.DeleteModelServer(utils.GetNamespaceName(next))
		c.Close()
		done <- err
	}()
	require.NoError(t, receive(t, done))
	require.Nil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "model:a"}))
}

func TestNotificationStormIsRateLimited(t *testing.T) {
	config := testConfig()
	config.RefreshInterval = "20ms"
	starts := make(chan time.Time, 3)
	c := testController(t, config, nil, func(_ types.NamespacedName, _ map[string]interface{}, notify func()) (Source, error) {
		return &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
			select {
			case starts <- time.Now():
			default:
			}
			for i := 0; i < 100; i++ {
				notify()
			}
			return nil, errors.New("registry unavailable")
		}}, nil
	})
	require.NoError(t, c.UpdateModelServer(testModel()))
	var previous time.Time
	for i := 0; i < 3; i++ {
		start := receive(t, starts)
		if !previous.IsZero() {
			require.GreaterOrEqual(t, start.Sub(previous), c.minRefreshInterval)
		}
		previous = start
	}
}
