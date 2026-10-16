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
	"sync/atomic"
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
	snapshots atomic.Int32
	closed    atomic.Bool
}

func (s *assemblySource) Snapshot(ctx context.Context) ([]api.Endpoint, error) {
	s.snapshots.Add(1)
	return nil, ctx.Err()
}

func (s *assemblySource) Close() { s.closed.Store(true) }

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

func TestEndpointDiscoveryUsesInjectedFactory(t *testing.T) {
	type factoryCall struct {
		target     types.NamespacedName
		parameters map[string]interface{}
		notify     func()
	}
	calls := make(chan factoryCall, 1)
	source := &assemblySource{}
	s := &Server{EndpointDiscoveryRegistry: discovery.Registry{
		"custom": func(target types.NamespacedName, parameters map[string]interface{}, notify func()) (discovery.Source, error) {
			calls <- factoryCall{target, parameters, notify}
			return source, nil
		},
	}}
	config := discovery.Config{
		Provider: "custom", RefreshInterval: "1h",
		Mappings: []discovery.Mapping{{ModelServer: "default/model", Parameters: map[string]interface{}{"service": "model-service"}}},
	}
	d, err := newEndpointDiscovery(config, datastore.New(), false, s.EndpointDiscoveryRegistry)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	require.NoError(t, d.UpdateModelServer(&api.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "model"},
		Spec:       api.ModelServerSpec{InferenceEngine: api.VLLM, WorkloadPort: api.WorkloadPort{Port: 8000}},
	}))
	var call factoryCall
	select {
	case call = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("injected factory did not run")
	}
	require.Equal(t, types.NamespacedName{Namespace: "default", Name: "model"}, call.target)
	require.Equal(t, config.Mappings[0].Parameters, call.parameters)
	require.NotNil(t, call.notify)
	require.Eventually(t, func() bool { return source.snapshots.Load() == 1 }, 5*time.Second, time.Millisecond)
	call.notify()
	require.Eventually(t, func() bool { return source.snapshots.Load() >= 2 }, 5*time.Second, time.Millisecond)
	d.Close()
	require.True(t, source.closed.Load())
}
