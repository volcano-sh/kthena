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
	"fmt"

	"k8s.io/klog/v2"
)

// UnifiedRouteMatcher implements RouteMatcher by trying both ModelRoute and HTTPRoute
type UnifiedRouteMatcher struct {
	store Store
}

// NewUnifiedRouteMatcher creates a new UnifiedRouteMatcher
func NewUnifiedRouteMatcher(store Store) *UnifiedRouteMatcher {
	return &UnifiedRouteMatcher{
		store: store,
	}
}

// Match attempts to match a route for the given request
// It first tries to match ModelRoutes, then HTTPRoutes
func (m *UnifiedRouteMatcher) Match(request *RouteRequest) (*RouteMatch, error) {
	// Try ModelRoute first (for backward compatibility and /v1/ paths)
	if match, err := m.matchModelRoutes(request); err == nil && match != nil {
		return match, nil
	}

	// Try HTTPRoute second (for InferencePool routing)
	if match, err := m.matchHTTPRoutes(request); err == nil && match != nil {
		return match, nil
	}

	return nil, fmt.Errorf("no matching route found for model %s", request.ModelName)
}

// matchModelRoutes attempts to match against ModelRoutes
func (m *UnifiedRouteMatcher) matchModelRoutes(request *RouteRequest) (*RouteMatch, error) {
	// Use legacy method to get all candidate ModelRoutes
	// This leverages existing datastore indexes for efficiency
	modelServerName, isLora, modelRoute, err := m.store.MatchModelServer(
		request.ModelName,
		request.HTTPRequest,
		request.GatewayKey,
	)
	if err != nil {
		klog.V(4).Infof("No ModelRoute match: %v", err)
		return nil, err
	}

	// Create adapter for matched ModelRoute
	adapter := NewModelRouteAdapter(modelRoute, m.store)

	// Create target
	target := NewModelServerTarget(modelServerName, m.store)

	return &RouteMatch{
		Route:     adapter,
		Target:    target,
		IsLora:    isLora,
		RouteType: RouteTypeModel,
	}, nil
}

// matchHTTPRoutes attempts to match against HTTPRoutes
func (m *UnifiedRouteMatcher) matchHTTPRoutes(request *RouteRequest) (*RouteMatch, error) {
	// HTTPRoute requires gateway context
	if request.GatewayKey == "" {
		return nil, fmt.Errorf("gateway key required for HTTPRoute matching")
	}

	// Get HTTPRoutes attached to this gateway
	httpRoutes := m.store.GetHTTPRoutesByGateway(request.GatewayKey)
	if len(httpRoutes) == 0 {
		return nil, fmt.Errorf("no HTTPRoutes found for gateway %s", request.GatewayKey)
	}

	// Try each HTTPRoute
	for _, httpRoute := range httpRoutes {
		adapter := NewHTTPRouteAdapter(httpRoute, m.store)
		result, err := adapter.Matches(request)
		if err != nil {
			klog.Warningf("Error matching HTTPRoute %s/%s: %v", httpRoute.Namespace, httpRoute.Name, err)
			continue
		}

		if result.Matched {
			return &RouteMatch{
				Route:     adapter,
				Target:    result.Target,
				IsLora:    false,
				RouteType: RouteTypeHTTP,
			}, nil
		}
	}

	return nil, fmt.Errorf("no HTTPRoute matched for gateway %s", request.GatewayKey)
}

// MatchWithFallback is a convenience method that matches routes with legacy fallback
// This can be used during migration to maintain backward compatibility
func (m *UnifiedRouteMatcher) MatchWithFallback(request *RouteRequest) (*RouteMatch, error) {
	match, err := m.Match(request)
	if err != nil {
		// Log the error but continue with legacy behavior if needed
		klog.V(4).Infof("Unified matcher failed: %v", err)
	}
	return match, err
}
