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
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

type inspector struct{}

func (inspector) GetPodMetrics(string, *corev1.Pod, uint32, map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	return nil, nil
}
func (inspector) GetPodModels(string, *corev1.Pod, uint32) ([]string, error) { return nil, nil }

type testSource struct {
	snapshot func(context.Context) ([]api.Endpoint, error)
	closed   atomic.Int32
}

func (s *testSource) Snapshot(ctx context.Context) ([]api.Endpoint, error) { return s.snapshot(ctx) }
func (s *testSource) Close()                                               { s.closed.Add(1) }

func testConfig() Config {
	return Config{Provider: "test", RefreshInterval: "1h", SnapshotTimeout: "5s",
		Mappings: []Mapping{{ModelServer: "default/model"}}}
}

func testModel() *api.ModelServer {
	return &api.ModelServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "model", UID: "first"},
		Spec: api.ModelServerSpec{InferenceEngine: api.VLLM, WorkloadPort: api.WorkloadPort{Port: 8000}}}
}

func testController(t *testing.T, factory Factory) (*Controller, datastore.Store) {
	t.Helper()
	store := datastore.New(datastore.WithPodRuntimeInspector(inspector{}))
	c, err := NewController(testConfig(), store, Registry{"test": factory})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c, store
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
	s := &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) { return endpoints, lookupErr }}
	c, store := testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return s, nil })
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	var source Source
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

	lookupErr = errors.New("registry unavailable")
	endpoints = nil
	require.Error(t, refresh())
	require.NotNil(t, store.GetPodInfo(podName))
	lookupErr = nil
	endpoints = []api.Endpoint{{Name: "bad", Address: "http://not-an-address"}}
	require.Error(t, refresh())
	require.NotNil(t, store.GetPodInfo(podName))
	endpoints = []api.Endpoint{{Name: "a", Address: "10.0.0.2", Port: ptr.To(int32(9000))}}
	require.ErrorContains(t, refresh(), "without changing its name")

	endpoints = []api.Endpoint{}
	require.NoError(t, refresh())
	require.Nil(t, store.GetPodInfo(podName))
	pods, err := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.Empty(t, pods)
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
	c, store := testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return s, nil })
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	var source Source
	go func() { done <- c.refresh(utils.GetNamespaceName(ms), target, sub, &source) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not start")
	}
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(ms)))
	// A replacement with the same name has a separate subscription and identity.
	replacement := ms.DeepCopy()
	replacement.UID = "replacement"
	installTarget(t, c, replacement)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not finish")
	}
	require.Equal(t, types.UID("replacement"), store.GetModelServer(utils.GetNamespaceName(ms)).UID)
	require.Nil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "model:old"}))
}

func TestMetadataUpdateDiscardsInFlightSnapshot(t *testing.T) {
	var c *Controller
	s := &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
		next := testModel()
		next.Spec.Model = ptr.To("new-model")
		require.NoError(t, c.UpdateModelServer(next))
		return []api.Endpoint{{Name: "stale", Address: "10.0.0.1"}}, nil
	}}
	c, _ = testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return s, nil })
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	var source Source
	require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	require.Equal(t, ptr.To("new-model"), c.store.GetModelServer(utils.GetNamespaceName(ms)).Spec.Model)
	require.Nil(t, c.store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "model:stale"}))
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

func TestFailedMetadataUpdateCanBeRetried(t *testing.T) {
	c, store := testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return nil, nil })
	ms := testModel()
	_, sub := installTarget(t, c, ms)
	failing := &failingStore{Store: store, failUpdate: true}
	c.store = failing
	next := ms.DeepCopy()
	next.Spec.Model = ptr.To("updated-model")
	require.Error(t, c.UpdateModelServer(next))
	require.Equal(t, ms.Spec.Model, sub.ms.Spec.Model)
	failing.failUpdate = false
	require.NoError(t, c.UpdateModelServer(next))
	require.Equal(t, ptr.To("updated-model"), store.GetModelServer(utils.GetNamespaceName(ms)).Spec.Model)
}

func TestSourceLifecycleAndCancellation(t *testing.T) {
	started := make(chan struct{})
	s := &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c, _ := testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return s, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	require.NoError(t, c.UpdateModelServer(testModel()))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop")
	}
	c.Close()
	require.Equal(t, int32(1), s.closed.Load())
	require.Error(t, c.UpdateModelServer(testModel()))
}

func TestUnmappedStaticAndEmptyPools(t *testing.T) {
	c, store := testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return nil, nil })
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
			store := datastore.New(datastore.WithPodRuntimeInspector(inspector{}))
			c, err := NewController(config, store, Registry{"test": func(_ types.NamespacedName, _ map[string]interface{}, notify func()) (Source, error) {
				notifications <- notify
				return &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
					n := count.Add(1)
					return []api.Endpoint{{Name: fmt.Sprintf("instance-%d", n), Address: "10.0.0.1"}}, nil
				}}, nil
			}})
			require.NoError(t, err)
			defer c.Close()
			require.NoError(t, c.UpdateModelServer(testModel()))
			var notify func()
			select {
			case notify = <-notifications:
			case <-time.After(5 * time.Second):
				t.Fatal("factory did not run")
			}
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

func TestSnapshotTimeoutKeepsLastMembership(t *testing.T) {
	c, store := testController(t, func(types.NamespacedName, map[string]interface{}, func()) (Source, error) { return nil, nil })
	ms := testModel()
	target, sub := installTarget(t, c, ms)
	var source Source = &testSource{snapshot: func(context.Context) ([]api.Endpoint, error) {
		return []api.Endpoint{{Name: "a", Address: "10.0.0.1"}}, nil
	}}
	require.NoError(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source))
	c.timeout = time.Millisecond
	source = &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	require.ErrorIs(t, c.refresh(utils.GetNamespaceName(ms), target, sub, &source), context.DeadlineExceeded)
	require.NotNil(t, store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "model:a"}))
}

func TestBlockedSourceDoesNotBlockOtherTargets(t *testing.T) {
	config := testConfig()
	config.Mappings = append(config.Mappings, Mapping{ModelServer: "default/other"})
	started := make(chan struct{})
	store := datastore.New(datastore.WithPodRuntimeInspector(inspector{}))
	c, err := NewController(config, store, Registry{"test": func(name types.NamespacedName, _ map[string]interface{}, _ func()) (Source, error) {
		return &testSource{snapshot: func(ctx context.Context) ([]api.Endpoint, error) {
			if name.Name == "model" {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []api.Endpoint{{Name: "a", Address: "10.0.0.1"}}, nil
		}}, nil
	}})
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.UpdateModelServer(testModel()))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not start")
	}
	other := testModel()
	other.Name = "other"
	require.NoError(t, c.UpdateModelServer(other))
	require.Eventually(t, func() bool {
		return store.GetPodInfo(types.NamespacedName{Namespace: "default", Name: "other:a"}) != nil
	}, 5*time.Second, time.Millisecond)
	require.NoError(t, c.DeleteModelServer(utils.GetNamespaceName(testModel())))
	require.Nil(t, store.GetModelServer(utils.GetNamespaceName(testModel())))
}
