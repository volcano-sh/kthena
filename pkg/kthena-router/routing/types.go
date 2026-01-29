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

	"k8s.io/apimachinery/pkg/types"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// TargetType represents the type of routing target
type TargetType string

const (
	// TargetTypeModelServer represents a ModelServer target
	TargetTypeModelServer TargetType = "ModelServer"
	// TargetTypeInferencePool represents an InferencePool target
	TargetTypeInferencePool TargetType = "InferencePool"
)

// RouteTarget represents a unified routing target that can be either a ModelServer or InferencePool
type RouteTarget interface {
	// GetPods returns the list of pods backing this target
	GetPods() ([]*datastore.PodInfo, error)

	// GetPort returns the port to connect to on the pods
	GetPort() int32

	// GetTargetType returns the type of this target
	GetTargetType() TargetType

	// GetNamespacedName returns the namespaced name of this target
	GetNamespacedName() types.NamespacedName

	// GetModelServer returns the underlying ModelServer if this is a ModelServer target, nil otherwise
	GetModelServer() *aiv1alpha1.ModelServer
}

// Route represents a unified route that can be either a ModelRoute or HTTPRoute
type Route interface {
	// GetName returns the route name
	GetName() string

	// GetNamespace returns the route namespace
	GetNamespace() string

	// GetKey returns the namespaced name as a string key (namespace/name)
	GetKey() string

	// Matches checks if this route matches the given request
	Matches(request *RouteRequest) (MatchResult, error)

	// GetRateLimit returns the rate limit configuration for this route, nil if not set
	GetRateLimit() *RateLimitConfig

	// GetParentRefs returns the parent gateway references
	GetParentRefs() []ParentReference
}

// RouteType represents the type of route
type RouteType string

const (
	// RouteTypeModel represents a ModelRoute
	RouteTypeModel RouteType = "ModelRoute"
	// RouteTypeHTTP represents an HTTPRoute
	RouteTypeHTTP RouteType = "HTTPRoute"
)

// RouteRequest contains the information needed to match a route
type RouteRequest struct {
	// ModelName is the model name from the request body
	ModelName string

	// HTTPRequest is the underlying HTTP request
	HTTPRequest *http.Request

	// GatewayKey is the gateway this request came through (namespace/name)
	GatewayKey string
}

// MatchResult represents the result of a route match
type MatchResult struct {
	// Matched indicates whether the route matched
	Matched bool

	// Target is the selected target if matched
	Target RouteTarget

	// IsLora indicates if this is a LoRA adapter match (only for ModelRoute)
	IsLora bool

	// OriginalRoute is the original route object (ModelRoute or HTTPRoute)
	// This is kept for backward compatibility and accessing route-specific features
	OriginalRoute interface{}
}

// ParentReference represents a reference to a parent Gateway
type ParentReference struct {
	// Name is the gateway name
	Name string

	// Namespace is the gateway namespace
	Namespace string

	// SectionName is the listener name (optional)
	SectionName string
}

// RateLimitConfig represents rate limiting configuration
type RateLimitConfig struct {
	// InputTokensPerUnit is the maximum number of input tokens allowed per unit of time
	InputTokensPerUnit *uint32

	// OutputTokensPerUnit is the maximum number of output tokens allowed per unit of time
	OutputTokensPerUnit *uint32

	// Unit is the time unit for the rate limit
	Unit string

	// Global contains configuration for global rate limiting
	Global *GlobalRateLimitConfig
}

// GlobalRateLimitConfig contains configuration for global rate limiting
type GlobalRateLimitConfig struct {
	// Redis contains configuration for Redis-based global rate limiting
	Redis *RedisConfig
}

// RedisConfig contains Redis connection configuration
type RedisConfig struct {
	// Address is the Redis server address
	Address string
}

// RouteMatcher is responsible for matching routes against requests
type RouteMatcher interface {
	// Match attempts to match a route for the given request
	// Returns the matched route and target, or an error if no match found
	Match(request *RouteRequest) (*RouteMatch, error)
}

// RouteMatch contains the complete result of a successful route match
type RouteMatch struct {
	// Route is the matched route
	Route Route

	// Target is the selected routing target
	Target RouteTarget

	// IsLora indicates if this is a LoRA adapter match
	IsLora bool

	// RouteType indicates the type of route that was matched
	RouteType RouteType
}

// Store defines the interface for retrieving routing information from the datastore
// This is a subset of datastore.Store focused on routing operations
type Store interface {
	// GetModelRoute retrieves a ModelRoute by namespaced name key
	GetModelRoute(namespacedName string) *aiv1alpha1.ModelRoute

	// GetModelServer retrieves a ModelServer by namespaced name
	GetModelServer(name types.NamespacedName) *aiv1alpha1.ModelServer

	// GetPodsByModelServer retrieves pods for a ModelServer
	GetPodsByModelServer(name types.NamespacedName) ([]*datastore.PodInfo, error)

	// GetInferencePool retrieves an InferencePool by namespaced name key
	GetInferencePool(key string) *inferencev1.InferencePool

	// GetPodsByInferencePool retrieves pods for an InferencePool
	GetPodsByInferencePool(name types.NamespacedName) ([]*datastore.PodInfo, error)

	// GetHTTPRoutesByGateway retrieves HTTPRoutes attached to a gateway
	GetHTTPRoutesByGateway(gatewayKey string) []*gatewayv1.HTTPRoute

	// GetGateway retrieves a Gateway by key
	GetGateway(key string) *gatewayv1.Gateway

	// MatchModelServer is the legacy method for matching ModelRoutes
	// This is kept for backward compatibility during migration
	MatchModelServer(model string, req *http.Request, gatewayKey string) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error)
}
