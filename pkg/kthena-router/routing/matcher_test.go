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

package routing

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// MockStore implements Store for testing
type MockStore struct {
	modelRoutes    map[string]*aiv1alpha1.ModelRoute
	modelServers   map[types.NamespacedName]*aiv1alpha1.ModelServer
	httpRoutes     map[string][]*gatewayv1.HTTPRoute
	inferencePools map[string]*inferencev1.InferencePool
	gateways       map[string]*gatewayv1.Gateway
	podsByMS       map[types.NamespacedName][]*datastore.PodInfo
	podsByIP       map[types.NamespacedName][]*datastore.PodInfo
}

func NewMockStore() *MockStore {
	return &MockStore{
		modelRoutes:    make(map[string]*aiv1alpha1.ModelRoute),
		modelServers:   make(map[types.NamespacedName]*aiv1alpha1.ModelServer),
		httpRoutes:     make(map[string][]*gatewayv1.HTTPRoute),
		inferencePools: make(map[string]*inferencev1.InferencePool),
		gateways:       make(map[string]*gatewayv1.Gateway),
		podsByMS:       make(map[types.NamespacedName][]*datastore.PodInfo),
		podsByIP:       make(map[types.NamespacedName][]*datastore.PodInfo),
	}
}

func (m *MockStore) GetModelRoute(namespacedName string) *aiv1alpha1.ModelRoute {
	return m.modelRoutes[namespacedName]
}

func (m *MockStore) GetModelServer(name types.NamespacedName) *aiv1alpha1.ModelServer {
	return m.modelServers[name]
}

func (m *MockStore) GetPodsByModelServer(name types.NamespacedName) ([]*datastore.PodInfo, error) {
	pods, ok := m.podsByMS[name]
	if !ok {
		return nil, nil
	}
	return pods, nil
}

func (m *MockStore) GetInferencePool(key string) *inferencev1.InferencePool {
	return m.inferencePools[key]
}

func (m *MockStore) GetPodsByInferencePool(name types.NamespacedName) ([]*datastore.PodInfo, error) {
	pods, ok := m.podsByIP[name]
	if !ok {
		return nil, nil
	}
	return pods, nil
}

func (m *MockStore) GetHTTPRoutesByGateway(gatewayKey string) []*gatewayv1.HTTPRoute {
	return m.httpRoutes[gatewayKey]
}

func (m *MockStore) GetGateway(key string) *gatewayv1.Gateway {
	return m.gateways[key]
}

func (m *MockStore) MatchModelServer(model string, req *http.Request, gatewayKey string) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error) {
	// Simple implementation for testing
	for key, mr := range m.modelRoutes {
		if mr.Spec.ModelName == model {
			// Check gateway matching if needed
			if len(mr.Spec.ParentRefs) > 0 && gatewayKey != "" {
				// Simplified: just check if any parentRef matches
				for _, parentRef := range mr.Spec.ParentRefs {
					namespace := mr.Namespace
					if parentRef.Namespace != nil {
						namespace = string(*parentRef.Namespace)
					}
					refKey := namespace + "/" + string(parentRef.Name)
					if refKey == gatewayKey {
						// Return first target
						if len(mr.Spec.Rules) > 0 && len(mr.Spec.Rules[0].TargetModels) > 0 {
							msName := types.NamespacedName{
								Namespace: mr.Namespace,
								Name:      mr.Spec.Rules[0].TargetModels[0].ModelServerName,
							}
							return msName, false, mr, nil
						}
					}
				}
			} else if len(mr.Spec.ParentRefs) == 0 && gatewayKey == "" {
				// No gateway restrictions
				if len(mr.Spec.Rules) > 0 && len(mr.Spec.Rules[0].TargetModels) > 0 {
					msName := types.NamespacedName{
						Namespace: mr.Namespace,
						Name:      mr.Spec.Rules[0].TargetModels[0].ModelServerName,
					}
					return msName, false, mr, nil
				}
			}
		}
		_ = key // avoid unused warning
	}
	return types.NamespacedName{}, false, nil, nil
}

func TestUnifiedRouteMatcher_MatchModelRoute(t *testing.T) {
	store := NewMockStore()

	// Setup test data
	modelRoute := &aiv1alpha1.ModelRoute{
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "test-model",
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "test-server",
						},
					},
				},
			},
		},
	}
	modelRoute.Name = "test-route"
	modelRoute.Namespace = "default"

	modelServer := &aiv1alpha1.ModelServer{
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadPort: aiv1alpha1.WorkloadPort{
				Port: 8080,
			},
		},
	}
	modelServer.Name = "test-server"
	modelServer.Namespace = "default"

	store.modelRoutes["default/test-route"] = modelRoute
	store.modelServers[types.NamespacedName{Namespace: "default", Name: "test-server"}] = modelServer

	matcher := NewUnifiedRouteMatcher(store)

	req, _ := http.NewRequest("GET", "/v1/completions", nil)
	routeRequest := &RouteRequest{
		ModelName:   "test-model",
		HTTPRequest: req,
		GatewayKey:  "",
	}

	match, err := matcher.Match(routeRequest)
	if err != nil {
		t.Fatalf("Expected successful match, got error: %v", err)
	}

	if match == nil {
		t.Fatal("Expected non-nil match")
	}

	if match.RouteType != RouteTypeModel {
		t.Errorf("Expected RouteTypeModel, got %v", match.RouteType)
	}

	if match.Target.GetTargetType() != TargetTypeModelServer {
		t.Errorf("Expected TargetTypeModelServer, got %v", match.Target.GetTargetType())
	}

	if match.Target.GetPort() != 8080 {
		t.Errorf("Expected port 8080, got %d", match.Target.GetPort())
	}
}

func TestModelRouteAdapter_Matches(t *testing.T) {
	store := NewMockStore()

	modelRoute := &aiv1alpha1.ModelRoute{
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "test-model",
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "test-server",
						},
					},
				},
			},
		},
	}
	modelRoute.Name = "test-route"
	modelRoute.Namespace = "default"

	modelServer := &aiv1alpha1.ModelServer{
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadPort: aiv1alpha1.WorkloadPort{
				Port: 8080,
			},
		},
	}

	store.modelServers[types.NamespacedName{Namespace: "default", Name: "test-server"}] = modelServer

	adapter := NewModelRouteAdapter(modelRoute, store)

	req, _ := http.NewRequest("GET", "/v1/completions", nil)
	routeRequest := &RouteRequest{
		ModelName:   "test-model",
		HTTPRequest: req,
		GatewayKey:  "",
	}

	result, err := adapter.Matches(routeRequest)
	if err != nil {
		t.Fatalf("Expected successful match, got error: %v", err)
	}

	if !result.Matched {
		t.Error("Expected route to match")
	}

	if result.Target == nil {
		t.Fatal("Expected non-nil target")
	}

	if result.Target.GetTargetType() != TargetTypeModelServer {
		t.Errorf("Expected TargetTypeModelServer, got %v", result.Target.GetTargetType())
	}
}

func TestModelRouteAdapter_LoraMatch(t *testing.T) {
	store := NewMockStore()

	modelRoute := &aiv1alpha1.ModelRoute{
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName:    "base-model",
			LoraAdapters: []string{"lora-adapter-1", "lora-adapter-2"},
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "test-server",
						},
					},
				},
			},
		},
	}
	modelRoute.Name = "test-route"
	modelRoute.Namespace = "default"

	modelServer := &aiv1alpha1.ModelServer{
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadPort: aiv1alpha1.WorkloadPort{
				Port: 8080,
			},
		},
	}

	store.modelServers[types.NamespacedName{Namespace: "default", Name: "test-server"}] = modelServer

	adapter := NewModelRouteAdapter(modelRoute, store)

	// Test LoRA match
	req, _ := http.NewRequest("GET", "/v1/completions", nil)
	routeRequest := &RouteRequest{
		ModelName:   "lora-adapter-1",
		HTTPRequest: req,
		GatewayKey:  "",
	}

	result, err := adapter.Matches(routeRequest)
	if err != nil {
		t.Fatalf("Expected successful match, got error: %v", err)
	}

	if !result.Matched {
		t.Error("Expected route to match LoRA adapter")
	}

	if !result.IsLora {
		t.Error("Expected IsLora to be true")
	}
}

func TestModelServerTarget(t *testing.T) {
	store := NewMockStore()

	modelServer := &aiv1alpha1.ModelServer{
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadPort: aiv1alpha1.WorkloadPort{
				Port: 8080,
			},
		},
	}
	modelServer.Name = "test-server"
	modelServer.Namespace = "default"

	msName := types.NamespacedName{Namespace: "default", Name: "test-server"}
	store.modelServers[msName] = modelServer

	target := NewModelServerTarget(msName, store)

	if target.GetTargetType() != TargetTypeModelServer {
		t.Errorf("Expected TargetTypeModelServer, got %v", target.GetTargetType())
	}

	if target.GetPort() != 8080 {
		t.Errorf("Expected port 8080, got %d", target.GetPort())
	}

	if target.GetNamespacedName() != msName {
		t.Errorf("Expected name %v, got %v", msName, target.GetNamespacedName())
	}

	ms := target.GetModelServer()
	if ms == nil {
		t.Error("Expected non-nil ModelServer")
	}
}

func TestWeightedSelection(t *testing.T) {
	weights := []uint32{50, 30, 20}

	// Run selection many times to verify distribution
	counts := make(map[int]int)
	iterations := 1000

	for i := 0; i < iterations; i++ {
		index := selectFromWeights(weights)
		counts[index]++
	}

	// Verify all indices were selected at least once
	for i := 0; i < len(weights); i++ {
		if counts[i] == 0 {
			t.Errorf("Index %d was never selected", i)
		}
	}

	// Index 0 should be selected most often (roughly 50%)
	if counts[0] < counts[1] || counts[0] < counts[2] {
		t.Errorf("Expected index 0 to have highest count, got %v", counts)
	}
}

func TestSelectFromWeights_EmptyWeights(t *testing.T) {
	weights := []uint32{}
	index := selectFromWeights(weights)
	if index != 0 {
		t.Errorf("Expected 0 for empty weights, got %d", index)
	}
}

func TestSelectFromWeights_AllZeroWeights(t *testing.T) {
	weights := []uint32{0, 0, 0}
	index := selectFromWeights(weights)
	if index != 0 {
		t.Errorf("Expected 0 for all zero weights, got %d", index)
	}
}
