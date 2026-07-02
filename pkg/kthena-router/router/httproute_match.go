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

package router

import (
	"net"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	inferencePoolBackendGroup gatewayv1.Group = "inference.networking.k8s.io"
	inferencePoolBackendKind  gatewayv1.Kind  = "InferencePool"
)

type httpRouteMatchResult struct {
	// route is the HTTPRoute whose hostnames matched the request and whose rule
	// supplied the best path match within that route.
	route *gatewayv1.HTTPRoute
	// rule is the exact rule that accepted the request path. BackendRefs and
	// URLRewrite filters are rule-scoped, so handleHTTPRoute must use this rule
	// instead of scanning route.Spec.Rules from the beginning again.
	rule *gatewayv1.HTTPRouteRule
	// matchedPrefix is the normalized PathPrefix that matched the request. It is
	// stored in gin.Context so ReplacePrefixMatch URLRewrite can replace exactly
	// the prefix that made this rule match.
	matchedPrefix string
	// path stores the path specificity used only to choose the best rule within
	// this HTTPRoute.
	path httpRoutePathPrecedence
}

type httpRoutePathPrecedence struct {
	// matchType ranks path match kinds. Higher values are more specific:
	// Exact > PathPrefix > RegularExpression.
	matchType int
	// characters is the length of the matched path value. It makes a longer
	// PathPrefix such as "/chat" win over "/" when both match the request.
	characters int
	// ruleIndex is the rule's position in route.Spec.Rules. It is the final
	// tie-breaker before matchIndex, preserving list order when specificity ties.
	ruleIndex int
	// matchIndex is the match's position inside rule.Matches. It preserves list
	// order when two matches in the same rule have equal path specificity.
	matchIndex int
}

// findHTTPRouteMatch keeps Kthena's lightweight HTTPRoute matching model. It
// checks hostnames at the route level, then picks the best path match within the
// first matching HTTPRoute and preserves that rule for backend/filter lookup. It
// intentionally does not implement full Gateway API method/header/query matching
// or cross-route precedence.
func (r *Router) findHTTPRouteMatch(c *gin.Context, gatewayKey string) (httpRouteMatchResult, bool) {
	httpRoutes := r.store.GetHTTPRoutesByGateway(gatewayKey)
	if len(httpRoutes) == 0 {
		return httpRouteMatchResult{}, false
	}

	for _, route := range httpRoutes {
		if route == nil {
			continue
		}
		if !matchHTTPRouteHostnames(route.Spec.Hostnames, c.Request.Host) {
			continue
		}
		result, matched := findBestHTTPRouteRuleMatch(route, c.Request.URL.Path)
		if matched {
			return result, true
		}
	}
	return httpRouteMatchResult{}, false
}

func findBestHTTPRouteRuleMatch(route *gatewayv1.HTTPRoute, requestPath string) (httpRouteMatchResult, bool) {
	var best httpRouteMatchResult
	found := false
	for i := range route.Spec.Rules {
		rule := &route.Spec.Rules[i]
		if len(rule.Matches) == 0 {
			// Gateway API treats an omitted matches list as a default PathPrefix
			// "/" match. Model it explicitly so it can compete with other rules.
			result := httpRouteMatchResult{
				route:         route,
				rule:          rule,
				matchedPrefix: "/",
				path: httpRoutePathPrecedence{
					matchType:  httpPathPrefixPrecedence,
					characters: 1,
					ruleIndex:  i,
				},
			}
			if !found || compareHTTPRoutePathPrecedence(result.path, best.path) < 0 {
				best = result
				found = true
			}
			continue
		}
		for j, match := range rule.Matches {
			if hasUnsupportedHTTPRouteMatchPredicates(match) {
				// Kthena currently evaluates only the path predicate. Gateway API
				// ANDs all predicates in one HTTPRouteMatch, so silently ignoring
				// method/header/query predicates would turn a constrained match
				// into a path-only match and could select the wrong backend.
				continue
			}
			matched, matchedPrefix, pathPrecedence := matchHTTPRoutePath(match.Path, requestPath, route)
			if !matched {
				continue
			}
			pathPrecedence.ruleIndex = i
			pathPrecedence.matchIndex = j
			result := httpRouteMatchResult{
				route:         route,
				rule:          rule,
				matchedPrefix: matchedPrefix,
				path:          pathPrecedence,
			}
			if !found || compareHTTPRoutePathPrecedence(result.path, best.path) < 0 {
				best = result
				found = true
			}
		}
	}
	return best, found
}

func hasUnsupportedHTTPRouteMatchPredicates(match gatewayv1.HTTPRouteMatch) bool {
	return match.Method != nil || len(match.Headers) > 0 || len(match.QueryParams) > 0
}

func compareHTTPRoutePathPrecedence(a, b httpRoutePathPrecedence) int {
	if a.matchType != b.matchType {
		if a.matchType > b.matchType {
			return -1
		}
		return 1
	}
	if a.characters != b.characters {
		if a.characters > b.characters {
			return -1
		}
		return 1
	}
	if a.ruleIndex != b.ruleIndex {
		// When path specificity is equal, keep the HTTPRoute's rule order.
		if a.ruleIndex < b.ruleIndex {
			return -1
		}
		return 1
	}
	if a.matchIndex < b.matchIndex {
		return -1
	}
	if a.matchIndex > b.matchIndex {
		return 1
	}
	return 0
}

// inferencePoolFromHTTPRouteRule extracts the InferencePool backend from the
// already matched rule. Other Gateway API backend kinds are ignored because this
// router currently forwards only through InferencePool.
func inferencePoolFromHTTPRouteRule(route *gatewayv1.HTTPRoute, rule *gatewayv1.HTTPRouteRule) (types.NamespacedName, bool) {
	var inferencePoolName types.NamespacedName
	for _, backendRef := range rule.BackendRefs {
		if backendRef.Group != nil && *backendRef.Group == inferencePoolBackendGroup &&
			backendRef.Kind != nil && *backendRef.Kind == inferencePoolBackendKind {
			inferencePoolName.Namespace = route.Namespace
			if backendRef.Namespace != nil {
				inferencePoolName.Namespace = string(*backendRef.Namespace)
			}
			inferencePoolName.Name = string(backendRef.Name)
			return inferencePoolName, true
		}
	}
	return types.NamespacedName{}, false
}

// httpPathMatchType returns the Gateway API default PathPrefix type when the
// field is omitted.
func httpPathMatchType(path *gatewayv1.HTTPPathMatch) gatewayv1.PathMatchType {
	if path.Type == nil {
		return gatewayv1.PathMatchPathPrefix
	}
	return *path.Type
}

// httpPathMatchValue returns the Gateway API default "/" path when value is
// omitted.
func httpPathMatchValue(path *gatewayv1.HTTPPathMatch) string {
	if path.Value == nil {
		return "/"
	}
	return *path.Value
}

const (
	// Higher values win in compareHTTPRoutePathPrecedence. Regex is kept below
	// Exact and PathPrefix because Kthena's current behavior is path-oriented and
	// this PR is not trying to implement full Gateway API precedence. A catch-all
	// prefix such as "/" will therefore win over a regex match in the same route.
	httpPathRegularExpressionPrecedence = iota + 1
	httpPathPrefixPrecedence
	httpPathExactPrecedence
)

// matchHTTPRoutePath checks only the path predicate. It returns the matched
// prefix needed by ReplacePrefixMatch URLRewrite and the path score used to
// compare rules inside one HTTPRoute.
func matchHTTPRoutePath(path *gatewayv1.HTTPPathMatch, requestPath string, route *gatewayv1.HTTPRoute) (bool, string, httpRoutePathPrecedence) {
	if path == nil {
		return true, "/", httpRoutePathPrecedence{matchType: httpPathPrefixPrecedence, characters: 1}
	}

	pathType := httpPathMatchType(path)
	pathValue := httpPathMatchValue(path)
	switch pathType {
	case gatewayv1.PathMatchExact:
		return requestPath == pathValue, "", httpRoutePathPrecedence{matchType: httpPathExactPrecedence, characters: len(pathValue)}
	case gatewayv1.PathMatchPathPrefix:
		matched, matchedPrefix := matchHTTPPathPrefix(requestPath, pathValue)
		if !matched {
			return false, "", httpRoutePathPrecedence{}
		}
		return true, matchedPrefix, httpRoutePathPrecedence{matchType: httpPathPrefixPrecedence, characters: len(matchedPrefix)}
	case gatewayv1.PathMatchRegularExpression:
		expression, err := regexp.Compile(pathValue)
		if err != nil {
			klog.Warningf("Invalid regex pattern '%s' in HTTPRoute %s/%s: %v", pathValue, route.Namespace, route.Name, err)
			return false, "", httpRoutePathPrecedence{}
		}
		return expression.MatchString(requestPath), "", httpRoutePathPrecedence{matchType: httpPathRegularExpressionPrecedence}
	default:
		return false, "", httpRoutePathPrecedence{}
	}
}

// matchHTTPPathPrefix implements Gateway API PathPrefix semantics: matching is
// path-segment based, and a trailing slash on the configured prefix is ignored.
func matchHTTPPathPrefix(path, prefix string) (bool, string) {
	normalizedPrefix := strings.TrimRight(prefix, "/")
	if normalizedPrefix == "" {
		return strings.HasPrefix(path, "/"), "/"
	}
	return path == normalizedPrefix || strings.HasPrefix(path, normalizedPrefix+"/"), normalizedPrefix
}

// matchHTTPRouteHostnames matches the request Host header against route
// hostnames. Empty hostnames means the route matches all hosts.
func matchHTTPRouteHostnames(hostnames []gatewayv1.Hostname, requestHost string) bool {
	if len(hostnames) == 0 {
		return true
	}
	normalizedHost := normalizeHTTPRouteHost(requestHost)
	if normalizedHost == "" {
		return false
	}
	for _, hostname := range hostnames {
		if matchHTTPRouteHostname(string(hostname), normalizedHost) {
			return true
		}
	}
	return false
}

// normalizeHTTPRouteHost removes the optional Host header port and normalizes
// case/trailing dots before Gateway API hostname comparison.
func normalizeHTTPRouteHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	} else if index := strings.LastIndex(host, ":"); index > -1 && !strings.Contains(host[:index], ":") {
		host = host[:index]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// matchHTTPRouteHostname supports exact hostnames and Gateway API wildcard
// suffix hostnames. A wildcard such as "*.example.com" matches
// "api.example.com" and "v1.api.example.com", but not "example.com".
func matchHTTPRouteHostname(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
	if pattern == host {
		return true
	}
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := strings.TrimPrefix(pattern, "*")
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	prefix := strings.TrimSuffix(host, suffix)
	return prefix != ""
}
