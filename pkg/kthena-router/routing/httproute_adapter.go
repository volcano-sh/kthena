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
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// HTTPRouteAdapter adapts an HTTPRoute to the Route interface
type HTTPRouteAdapter struct {
	httpRoute *gatewayv1.HTTPRoute
	store     Store
}

// NewHTTPRouteAdapter creates a new HTTPRouteAdapter
func NewHTTPRouteAdapter(httpRoute *gatewayv1.HTTPRoute, store Store) *HTTPRouteAdapter {
	return &HTTPRouteAdapter{
		httpRoute: httpRoute,
		store:     store,
	}
}

// GetName returns the route name
func (a *HTTPRouteAdapter) GetName() string {
	return a.httpRoute.Name
}

// GetNamespace returns the route namespace
func (a *HTTPRouteAdapter) GetNamespace() string {
	return a.httpRoute.Namespace
}

// GetKey returns the namespaced name as a string key
func (a *HTTPRouteAdapter) GetKey() string {
	return fmt.Sprintf("%s/%s", a.httpRoute.Namespace, a.httpRoute.Name)
}

// Matches checks if this HTTPRoute matches the given request
func (a *HTTPRouteAdapter) Matches(request *RouteRequest) (MatchResult, error) {
	// HTTPRoute requires a gateway context
	if request.GatewayKey == "" {
		return MatchResult{Matched: false}, nil
	}

	// Check if HTTPRoute is attached to the gateway
	if !a.isAttachedToGateway(request.GatewayKey) {
		return MatchResult{Matched: false}, nil
	}

	// Try to match rules
	for _, rule := range a.httpRoute.Spec.Rules {
		if matched, target, err := a.matchRule(&rule, request); matched {
			if err != nil {
				return MatchResult{Matched: false}, err
			}
			return MatchResult{
				Matched:       true,
				Target:        target,
				IsLora:        false,
				OriginalRoute: a.httpRoute,
			}, nil
		}
	}

	return MatchResult{Matched: false}, nil
}

// isAttachedToGateway checks if the HTTPRoute is attached to the specified gateway
func (a *HTTPRouteAdapter) isAttachedToGateway(gatewayKey string) bool {
	for _, parentRef := range a.httpRoute.Spec.ParentRefs {
		namespace := a.httpRoute.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		name := string(parentRef.Name)
		key := fmt.Sprintf("%s/%s", namespace, name)

		if key == gatewayKey {
			return true
		}
	}
	return false
}

// matchRule attempts to match a rule against the request
func (a *HTTPRouteAdapter) matchRule(rule *gatewayv1.HTTPRouteRule, request *RouteRequest) (bool, RouteTarget, error) {
	// If no matches specified, it matches all
	if len(rule.Matches) == 0 {
		return a.selectInferencePoolTarget(rule)
	}

	// Try each match
	for _, match := range rule.Matches {
		if a.matchHTTPRouteMatch(&match, request) {
			return a.selectInferencePoolTarget(rule)
		}
	}

	return false, nil, nil
}

// matchHTTPRouteMatch checks if a single HTTPRouteMatch matches the request
func (a *HTTPRouteAdapter) matchHTTPRouteMatch(match *gatewayv1.HTTPRouteMatch, request *RouteRequest) bool {
	// Check path match
	if match.Path != nil {
		if !a.matchPath(match.Path, request.HTTPRequest.URL.Path) {
			return false
		}
	}

	// Check headers
	for _, headerMatch := range match.Headers {
		headerName := string(headerMatch.Name)
		headerValue := request.HTTPRequest.Header.Get(headerName)
		if !a.matchHeaderValue(headerMatch, headerValue) {
			return false
		}
	}

	// Check query params
	for _, queryMatch := range match.QueryParams {
		queryName := string(queryMatch.Name)
		queryValue := request.HTTPRequest.URL.Query().Get(queryName)
		if !a.matchQueryValue(queryMatch, queryValue) {
			return false
		}
	}

	// Check method
	if match.Method != nil {
		method := string(*match.Method)
		if request.HTTPRequest.Method != method {
			return false
		}
	}

	return true
}

// matchPath matches a path against a PathMatch specification
func (a *HTTPRouteAdapter) matchPath(pathMatch *gatewayv1.HTTPPathMatch, path string) bool {
	if pathMatch.Type == nil || pathMatch.Value == nil {
		return true
	}

	switch *pathMatch.Type {
	case gatewayv1.PathMatchExact:
		return path == *pathMatch.Value
	case gatewayv1.PathMatchPathPrefix:
		return strings.HasPrefix(path, *pathMatch.Value)
	case gatewayv1.PathMatchRegularExpression:
		matched, err := regexp.MatchString(*pathMatch.Value, path)
		return err == nil && matched
	default:
		return true
	}
}

// matchHeaderValue matches a header value against a HeaderMatch specification
func (a *HTTPRouteAdapter) matchHeaderValue(headerMatch gatewayv1.HTTPHeaderMatch, value string) bool {
	if headerMatch.Type == nil || headerMatch.Value == "" {
		return true
	}

	switch *headerMatch.Type {
	case gatewayv1.HeaderMatchExact:
		return value == headerMatch.Value
	case gatewayv1.HeaderMatchRegularExpression:
		matched, err := regexp.MatchString(headerMatch.Value, value)
		return err == nil && matched
	default:
		return true
	}
}

// matchQueryValue matches a query param value against a QueryParamMatch specification
func (a *HTTPRouteAdapter) matchQueryValue(queryMatch gatewayv1.HTTPQueryParamMatch, value string) bool {
	if queryMatch.Type == nil || queryMatch.Value == "" {
		return true
	}

	switch *queryMatch.Type {
	case gatewayv1.QueryParamMatchExact:
		return value == queryMatch.Value
	case gatewayv1.QueryParamMatchRegularExpression:
		matched, err := regexp.MatchString(queryMatch.Value, value)
		return err == nil && matched
	default:
		return true
	}
}

// selectInferencePoolTarget finds an InferencePool backend and creates a target
func (a *HTTPRouteAdapter) selectInferencePoolTarget(rule *gatewayv1.HTTPRouteRule) (bool, RouteTarget, error) {
	// Look for InferencePool backendRef
	for _, backendRef := range rule.BackendRefs {
		if backendRef.Group != nil && *backendRef.Group == "inference.networking.k8s.io" &&
			backendRef.Kind != nil && *backendRef.Kind == "InferencePool" {

			namespace := a.httpRoute.Namespace
			if backendRef.Namespace != nil {
				namespace = string(*backendRef.Namespace)
			}

			inferencePoolName := types.NamespacedName{
				Namespace: namespace,
				Name:      string(backendRef.Name),
			}

			target := NewInferencePoolTarget(inferencePoolName, a.store)
			return true, target, nil
		}
	}

	return false, nil, nil
}

// GetRateLimit returns nil as HTTPRoute doesn't have rate limiting configuration
func (a *HTTPRouteAdapter) GetRateLimit() *RateLimitConfig {
	// HTTPRoute in Gateway API doesn't have native rate limiting
	// This could be extended in the future through extension filters
	return nil
}

// GetParentRefs returns the parent gateway references
func (a *HTTPRouteAdapter) GetParentRefs() []ParentReference {
	refs := make([]ParentReference, 0, len(a.httpRoute.Spec.ParentRefs))
	for _, ref := range a.httpRoute.Spec.ParentRefs {
		namespace := a.httpRoute.Namespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}

		sectionName := ""
		if ref.SectionName != nil {
			sectionName = string(*ref.SectionName)
		}

		refs = append(refs, ParentReference{
			Name:        string(ref.Name),
			Namespace:   namespace,
			SectionName: sectionName,
		})
	}
	return refs
}

// InferencePoolTarget implements RouteTarget for InferencePool
type InferencePoolTarget struct {
	name  types.NamespacedName
	store Store
}

// NewInferencePoolTarget creates a new InferencePoolTarget
func NewInferencePoolTarget(name types.NamespacedName, store Store) *InferencePoolTarget {
	return &InferencePoolTarget{
		name:  name,
		store: store,
	}
}

// GetPods returns the pods for this InferencePool
func (t *InferencePoolTarget) GetPods() ([]*datastore.PodInfo, error) {
	return t.store.GetPodsByInferencePool(t.name)
}

// GetPort returns the port for this InferencePool
func (t *InferencePoolTarget) GetPort() int32 {
	key := fmt.Sprintf("%s/%s", t.name.Namespace, t.name.Name)
	poolInterface := t.store.GetInferencePool(key)
	if poolInterface == nil {
		return 0
	}

	// Type assert to InferencePool
	pool, ok := poolInterface.(*inferencev1.InferencePool)
	if !ok {
		return 0
	}

	// Get the first target port
	if len(pool.Spec.TargetPorts) == 0 {
		return 0
	}

	return int32(pool.Spec.TargetPorts[0].Number)
}

// GetTargetType returns the target type
func (t *InferencePoolTarget) GetTargetType() TargetType {
	return TargetTypeInferencePool
}

// GetNamespacedName returns the namespaced name
func (t *InferencePoolTarget) GetNamespacedName() types.NamespacedName {
	return t.name
}

// GetModelServer returns nil as InferencePool is not a ModelServer
func (t *InferencePoolTarget) GetModelServer() *aiv1alpha1.ModelServer {
	return nil
}
