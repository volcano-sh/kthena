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

package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/discovery"
)

type assemblySource struct {
	release chan struct{}
}

func (s *assemblySource) Snapshot(ctx context.Context) ([]api.Endpoint, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return nil, nil
	}
}

func (*assemblySource) Close() {}

func TestEndpointDiscoveryAssembly(t *testing.T) {
	registry := discovery.Registry{"custom": func(types.NamespacedName, map[string]interface{}, func()) (discovery.Source, error) {
		return &assemblySource{}, nil
	}}
	for _, tc := range []struct {
		name, provider string
		registry       discovery.Registry
		gie, enabled   bool
		wantError      string
	}{
		{name: "default"},
		{name: "native", provider: "kubernetes", gie: true},
		{name: "injected registry", provider: "custom", registry: registry, enabled: true},
		{name: "no registry", provider: "custom", wantError: "not registered"},
		{name: "unknown", provider: "unregistered", registry: registry, wantError: "not registered"},
		{name: "conflicting discovery", provider: "custom", registry: registry, gie: true, wantError: "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := discovery.Config{Provider: tc.provider}
			if config.External() {
				config.Mappings = []discovery.Mapping{{ModelServer: "default/model"}}
			}
			d, err := newEndpointDiscovery(config, nil, tc.gie, tc.registry)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, d)
				return
			}
			require.NoError(t, err)
			if tc.enabled {
				require.NotNil(t, d)
				d.Close()
			} else {
				require.Nil(t, d)
			}
		})
	}
}

func TestServerDiscoveryWiring(t *testing.T) {
	type factoryCall struct {
		target     types.NamespacedName
		parameters map[string]interface{}
		notify     func()
		release    chan struct{}
	}
	calls := make(chan factoryCall, 1)
	store := &readinessStore{Store: datastore.New()}
	s := &Server{store: store, controllers: readinessController(false), EndpointDiscoveryRegistry: discovery.Registry{
		"custom": func(target types.NamespacedName, parameters map[string]interface{}, notify func()) (discovery.Source, error) {
			source := &assemblySource{release: make(chan struct{})}
			calls <- factoryCall{target, parameters, notify, source.release}
			return source, nil
		},
	}}
	config := discovery.Config{
		Provider: "custom", RefreshInterval: "1h",
		Mappings: []discovery.Mapping{{ModelServer: "default/model", Parameters: map[string]interface{}{"service": "model-service"}}, {ModelServer: "default/other"}},
	}
	d, err := newEndpointDiscovery(config, store, false, s.EndpointDiscoveryRegistry)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	s.endpointDiscovery = d
	require.False(t, s.HasSynced(), "do not latch before resource initialization")
	s.controllers = readinessController(true)
	require.False(t, s.HasSynced(), "do not latch before Store initialization")
	store.ready = true
	lateBinding := &Server{store: store, controllers: readinessController(true), endpointDiscovery: d}
	require.True(t, lateBinding.HasSynced(), "absent mapped resources do not block startup")
	ms := &api.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "model"},
		Spec:       api.ModelServerSpec{InferenceEngine: api.VLLM, WorkloadPort: api.WorkloadPort{Port: 8000}},
	}
	require.NoError(t, d.UpdateModelServer(ms))
	getCall := func() factoryCall {
		select {
		case call := <-calls:
			return call
		case <-time.After(5 * time.Second):
			t.Fatal("injected factory did not run")
			return factoryCall{}
		}
	}
	call := getCall()
	require.Equal(t, types.NamespacedName{Namespace: "default", Name: "model"}, call.target)
	require.Equal(t, config.Mappings[0].Parameters, call.parameters)
	require.NotNil(t, call.notify)
	require.False(t, s.HasSynced())
	require.True(t, lateBinding.HasSynced())
	close(call.release)
	require.Eventually(t, s.HasSynced, 5*time.Second, time.Millisecond)
	for _, name := range []string{"model", "other"} {
		next := ms.DeepCopy()
		next.Name, next.UID = name, "replacement"
		require.NoError(t, d.UpdateModelServer(next))
		getCall() // Leave the new source blocked until cancellation.
		require.False(t, d.HasSynced())
		require.True(t, s.HasSynced(), "replacement/new models cannot close the startup gate")
	}
	store.ready = false
	require.False(t, s.HasSynced())
	store.ready = true
	s.controllers = readinessController(false)
	require.False(t, s.HasSynced())
	s.controllers = readinessController(true)
	s.endpointDiscovery = nil
	require.True(t, s.HasSynced(), "native readiness is unchanged")
}

type readinessController bool

func (c readinessController) HasSynced() bool { return bool(c) }

type readinessStore struct {
	datastore.Store
	ready bool
}

func (s *readinessStore) HasSynced() bool { return s.ready }
