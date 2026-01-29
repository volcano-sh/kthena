# Routing Abstraction Layer

## Overview

The routing abstraction layer provides a unified interface for handling both ModelRoute/ModelServer and HTTPRoute/InferencePool routing in Kthena Router. This abstraction makes the core routing logic API-agnostic and easier to maintain.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Request Handler                         │
└───────────────────────────┬─────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                  UnifiedRouteMatcher                         │
│  - Tries ModelRoute first                                    │
│  - Falls back to HTTPRoute                                   │
└─────┬─────────────────────────────┬─────────────────────────┘
      │                             │
      ▼                             ▼
┌──────────────────┐      ┌────────────────────┐
│ ModelRouteAdapter│      │ HTTPRouteAdapter   │
│  - Wraps         │      │  - Wraps           │
│    ModelRoute    │      │    HTTPRoute       │
│  - Returns       │      │  - Returns         │
│    ModelServer   │      │    InferencePool   │
│    Target        │      │    Target          │
└──────────────────┘      └────────────────────┘
      │                             │
      ▼                             ▼
┌──────────────────┐      ┌────────────────────┐
│ModelServerTarget │      │InferencePoolTarget │
│  - Gets pods via │      │  - Gets pods via   │
│    label selector│      │    label selector  │
│  - Returns port  │      │  - Returns port    │
└──────────────────┘      └────────────────────┘
```

## Core Interfaces

### Route

Represents a routing rule (ModelRoute or HTTPRoute):

```go
type Route interface {
    GetName() string
    GetNamespace() string
    GetKey() string
    Matches(request *RouteRequest) (MatchResult, error)
    GetRateLimit() *RateLimitConfig
    GetParentRefs() []ParentReference
}
```

### RouteTarget

Represents a routing destination (ModelServer or InferencePool):

```go
type RouteTarget interface {
    GetPods() ([]*datastore.PodInfo, error)
    GetPort() int32
    GetTargetType() TargetType
    GetNamespacedName() types.NamespacedName
    GetModelServer() *aiv1alpha1.ModelServer
}
```

### RouteMatcher

Handles route matching logic:

```go
type RouteMatcher interface {
    Match(request *RouteRequest) (*RouteMatch, error)
}
```

## Usage

### Using the Unified Matcher

```go
// Create matcher
matcher := routing.NewUnifiedRouteMatcher(store)

// Create request
routeRequest := &routing.RouteRequest{
    ModelName:   "llama-2-7b",
    HTTPRequest: httpReq,
    GatewayKey:  "default/gateway-1",
}

// Match route
match, err := matcher.Match(routeRequest)
if err != nil {
    // Handle error
}

// Use matched route and target
pods, _ := match.Target.GetPods()
port := match.Target.GetPort()
```

### In Router Handler

The router now has a simplified `doLoadbalanceUnified` method that uses the abstraction:

```go
func (r *Router) doLoadbalanceUnified(c *gin.Context, modelRequest ModelRequest) {
    // Build route request
    routeRequest := &routing.RouteRequest{
        ModelName:   modelName,
        HTTPRequest: c.Request,
        GatewayKey:  gatewayKey,
    }

    // Match route (handles both ModelRoute and HTTPRoute)
    routeMatch, err := r.routeMatcher.Match(routeRequest)

    // Get pods and port from abstract target
    pods, _ := routeMatch.Target.GetPods()
    port := routeMatch.Target.GetPort()

    // Continue with scheduling...
}
```

## Benefits

1. **Single Code Path**: One routing flow instead of branching logic for ModelRoute vs HTTPRoute
2. **Type Safety**: Core logic works with interfaces, not concrete CRD types
3. **Easy Testing**: Mock interfaces instead of complex Kubernetes objects
4. **Extensibility**: Add new routing APIs by implementing adapters
5. **Maintainability**: Changes to CRDs don't affect core routing logic

## Migration Path

### Phase 1: Coexistence (Current)
- Both old and new routing methods exist
- `doLoadbalance()` uses legacy path
- `doLoadbalanceUnified()` uses abstraction
- Allows gradual testing and validation

### Phase 2: Switch Default
- Change HandlerFunc to use `doLoadbalanceUnified()`
- Keep `doLoadbalance()` as fallback
- Monitor metrics and logs

### Phase 3: Cleanup
- Remove legacy `doLoadbalance()` method
- Remove `handleHTTPRoute()` method
- Simplify datastore interfaces

## Extending with New APIs

To add support for a new routing API:

1. **Create Route Adapter**: Implement `Route` interface
2. **Create Target Adapter**: Implement `RouteTarget` interface
3. **Update Matcher**: Add matching logic to `UnifiedRouteMatcher`
4. **Add Tests**: Test new adapters

Example:

```go
// New API adapter
type CustomRouteAdapter struct {
    customRoute *customv1.CustomRoute
    store       Store
}

func (a *CustomRouteAdapter) Matches(request *RouteRequest) (MatchResult, error) {
    // Implement matching logic
}

// Add to matcher
func (m *UnifiedRouteMatcher) Match(request *RouteRequest) (*RouteMatch, error) {
    // Try ModelRoute
    // Try HTTPRoute
    // Try CustomRoute (new)
}
```

## Testing

Run tests:

```bash
go test ./pkg/kthena-router/routing/...
```

Key test files:
- `matcher_test.go`: Tests for UnifiedRouteMatcher
- Future: Add integration tests for adapters

## Implementation Files

- `types.go`: Core interfaces and types
- `modelroute_adapter.go`: ModelRoute/ModelServer adapters
- `httproute_adapter.go`: HTTPRoute/InferencePool adapters
- `matcher.go`: UnifiedRouteMatcher implementation
- `utils.go`: Utility functions (weight selection, etc.)
- `matcher_test.go`: Unit tests

## Backward Compatibility

The abstraction layer maintains full backward compatibility:
- All existing ModelRoute and HTTPRoute features work
- No changes to CRD definitions
- No changes to user-facing APIs
- Internal refactoring only

## Performance

The abstraction layer adds minimal overhead:
- Uses existing datastore indexes (ModelRoute matching via `MatchModelServer`)
- No additional allocations for common cases
- Interface calls are inlined by Go compiler
- Overall performance is equivalent to legacy code
