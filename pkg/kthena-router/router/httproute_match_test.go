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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type orderedHTTPRouteStore struct {
	datastore.Store
	routes []*gatewayv1.HTTPRoute
}

func (s *orderedHTTPRouteStore) GetHTTPRoutesByGateway(string) []*gatewayv1.HTTPRoute {
	return s.routes
}

func TestRouter_FindHTTPRouteMatch(t *testing.T) {
	pathType := gatewayv1.PathMatchPathPrefix
	kind := gatewayv1.Kind("Gateway")
	group := inferencePoolBackendGroup
	backendKind := inferencePoolBackendKind
	backendRefs := func(name string) []gatewayv1.HTTPBackendRef {
		return []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &group,
						Kind:  &backendKind,
						Name:  gatewayv1.ObjectName(name),
					},
				},
			},
		}
	}
	matchRule := func(match gatewayv1.HTTPRouteMatch, backend string) gatewayv1.HTTPRouteRule {
		return gatewayv1.HTTPRouteRule{
			Matches:     []gatewayv1.HTTPRouteMatch{match},
			BackendRefs: backendRefs(backend),
		}
	}
	pathMatch := func(prefix string) gatewayv1.HTTPRouteMatch {
		return gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{
				Type:  &pathType,
				Value: &prefix,
			},
		}
	}
	pathRule := func(prefix, backend string) gatewayv1.HTTPRouteRule {
		return matchRule(pathMatch(prefix), backend)
	}
	route := func(name string, hostnames []gatewayv1.Hostname, rules []gatewayv1.HTTPRouteRule) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: v1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{
							Name: "gw",
							Kind: &kind,
						},
					},
				},
				Hostnames: hostnames,
				Rules:     rules,
			},
		}
	}
	method := gatewayv1.HTTPMethodGet
	routeForListener := func(name, listenerName string, listenerPort gatewayv1.PortNumber, rules []gatewayv1.HTTPRouteRule) *gatewayv1.HTTPRoute {
		httpRoute := route(name, nil, rules)
		sectionName := gatewayv1.SectionName(listenerName)
		httpRoute.Spec.ParentRefs[0].SectionName = &sectionName
		httpRoute.Spec.ParentRefs[0].Port = &listenerPort
		return httpRoute
	}

	tests := []struct {
		name           string
		routes         []*gatewayv1.HTTPRoute
		host           string
		path           string
		listenerName   string
		listenerPort   int
		expectedRoute  string
		expectedPool   string
		expectedPrefix string
	}{
		{
			name: "matches route accepted by selected listener",
			routes: []*gatewayv1.HTTPRoute{
				routeForListener("route", "public", 8080, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat",
			listenerName:   "public",
			listenerPort:   8080,
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "matches route attached to selected listener",
			routes: []*gatewayv1.HTTPRoute{
				routeForListener("route", "private", 8081, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat",
			listenerName:   "private",
			listenerPort:   8081,
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "skips route attached to another listener",
			routes: []*gatewayv1.HTTPRoute{
				routeForListener("route", "private", 8081, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host:         "api.example.com",
			path:         "/chat",
			listenerName: "public",
			listenerPort: 8080,
		},
		{
			name: "skips route attached to another port",
			routes: []*gatewayv1.HTTPRoute{
				routeForListener("route", "http", 8081, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host:         "api.example.com",
			path:         "/chat",
			listenerName: "http",
			listenerPort: 8080,
		},
		{
			name: "matches route without sectionName and port against active listener",
			routes: []*gatewayv1.HTTPRoute{
				route("route", nil, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat",
			listenerName:   "public",
			listenerPort:   8080,
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "matches route with sectionName only against matching listener",
			routes: []*gatewayv1.HTTPRoute{
				func() *gatewayv1.HTTPRoute {
					r := route("route", nil, []gatewayv1.HTTPRouteRule{
						pathRule("/chat", "pool"),
					})
					section := gatewayv1.SectionName("public")
					r.Spec.ParentRefs[0].SectionName = &section
					return r
				}(),
			},
			host:           "api.example.com",
			path:           "/chat",
			listenerName:   "public",
			listenerPort:   8080,
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "skips route with sectionName only against different listener",
			routes: []*gatewayv1.HTTPRoute{
				func() *gatewayv1.HTTPRoute {
					r := route("route", nil, []gatewayv1.HTTPRouteRule{
						pathRule("/chat", "pool"),
					})
					section := gatewayv1.SectionName("private")
					r.Spec.ParentRefs[0].SectionName = &section
					return r
				}(),
			},
			host:         "api.example.com",
			path:         "/chat",
			listenerName: "public",
			listenerPort: 8080,
		},
		{
			name: "matches route with port only against matching port",
			routes: []*gatewayv1.HTTPRoute{
				func() *gatewayv1.HTTPRoute {
					r := route("route", nil, []gatewayv1.HTTPRouteRule{
						pathRule("/chat", "pool"),
					})
					port := gatewayv1.PortNumber(8080)
					r.Spec.ParentRefs[0].Port = &port
					return r
				}(),
			},
			host:           "api.example.com",
			path:           "/chat",
			listenerName:   "public",
			listenerPort:   8080,
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "skips route with port only against different port",
			routes: []*gatewayv1.HTTPRoute{
				func() *gatewayv1.HTTPRoute {
					r := route("route", nil, []gatewayv1.HTTPRouteRule{
						pathRule("/chat", "pool"),
					})
					port := gatewayv1.PortNumber(9090)
					r.Spec.ParentRefs[0].Port = &port
					return r
				}(),
			},
			host:         "api.example.com",
			path:         "/chat",
			listenerName: "public",
			listenerPort: 8080,
		},
		{
			name: "matches route with multiple parentRefs when second parentRef matches",
			routes: []*gatewayv1.HTTPRoute{
				func() *gatewayv1.HTTPRoute {
					r := route("route", nil, []gatewayv1.HTTPRouteRule{
						pathRule("/chat", "pool"),
					})
					otherGw := gatewayv1.ObjectName("other-gw")
					sectionPrivate := gatewayv1.SectionName("private")
					sectionPublic := gatewayv1.SectionName("public")
					r.Spec.ParentRefs = []gatewayv1.ParentReference{
						{
							Name:        otherGw,
							Kind:        &kind,
							SectionName: &sectionPrivate,
						},
						{
							Name:        "gw",
							Kind:        &kind,
							SectionName: &sectionPublic,
						},
					}
					return r
				}(),
			},
			host:           "api.example.com",
			path:           "/chat",
			listenerName:   "public",
			listenerPort:   8080,
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "skips listener-scoped route without listener context",
			routes: []*gatewayv1.HTTPRoute{
				routeForListener("route", "private", 8081, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host: "api.example.com",
			path: "/chat",
		},
		{
			name: "matches multiple parentRefs without listener context when one has no section/port",
			routes: []*gatewayv1.HTTPRoute{
				func() *gatewayv1.HTTPRoute {
					r := route("route", nil, []gatewayv1.HTTPRouteRule{
						pathRule("/chat", "pool"),
					})
					sectionPrivate := gatewayv1.SectionName("private")
					portPrivate := gatewayv1.PortNumber(8081)
					r.Spec.ParentRefs = []gatewayv1.ParentReference{
						{
							Name:        "gw",
							Kind:        &kind,
							SectionName: &sectionPrivate,
							Port:        &portPrivate,
						},
						{
							Name: "gw",
							Kind: &kind,
						},
					}
					return r
				}(),
			},
			host:           "api.example.com",
			path:           "/chat",
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "prefers longest prefix in a single route",
			routes: []*gatewayv1.HTTPRoute{
				route("route", nil, []gatewayv1.HTTPRouteRule{
					pathRule("/", "pool-root"),
					pathRule("/chat", "pool-chat"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat/completions",
			expectedRoute:  "route",
			expectedPool:   "pool-chat",
			expectedPrefix: "/chat",
		},
		{
			name: "matches hostname before selecting a rule",
			routes: []*gatewayv1.HTTPRoute{
				route("route", []gatewayv1.Hostname{"api.example.com"}, []gatewayv1.HTTPRouteRule{
					pathRule("/chat", "pool"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat",
			expectedRoute:  "route",
			expectedPool:   "pool",
			expectedPrefix: "/chat",
		},
		{
			name: "returns nil when no rule matches",
			routes: []*gatewayv1.HTTPRoute{
				route("route", nil, []gatewayv1.HTTPRouteRule{
					pathRule("/api", "pool"),
				}),
			},
			host: "api.example.com",
			path: "/chat",
		},
		{
			name: "skips unsupported method match instead of treating it as path only",
			routes: []*gatewayv1.HTTPRoute{
				route("route", nil, []gatewayv1.HTTPRouteRule{
					matchRule(gatewayv1.HTTPRouteMatch{
						Path:   pathMatch("/chat").Path,
						Method: &method,
					}, "pool-method"),
					pathRule("/", "pool-root"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat/completions",
			expectedRoute:  "route",
			expectedPool:   "pool-root",
			expectedPrefix: "/",
		},
		{
			name: "skips unsupported header match instead of treating it as path only",
			routes: []*gatewayv1.HTTPRoute{
				route("route", nil, []gatewayv1.HTTPRouteRule{
					matchRule(gatewayv1.HTTPRouteMatch{
						Path: pathMatch("/chat").Path,
						Headers: []gatewayv1.HTTPHeaderMatch{
							{Name: "x-model", Value: "v1"},
						},
					}, "pool-header"),
					pathRule("/", "pool-root"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat/completions",
			expectedRoute:  "route",
			expectedPool:   "pool-root",
			expectedPrefix: "/",
		},
		{
			name: "skips unsupported query param match instead of treating it as path only",
			routes: []*gatewayv1.HTTPRoute{
				route("route", nil, []gatewayv1.HTTPRouteRule{
					matchRule(gatewayv1.HTTPRouteMatch{
						Path: pathMatch("/chat").Path,
						QueryParams: []gatewayv1.HTTPQueryParamMatch{
							{Name: "version", Value: "v1"},
						},
					}, "pool-query"),
					pathRule("/", "pool-root"),
				}),
			},
			host:           "api.example.com",
			path:           "/chat/completions",
			expectedRoute:  "route",
			expectedPool:   "pool-root",
			expectedPrefix: "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := datastore.New()
			router := &Router{store: store}
			for _, route := range tt.routes {
				assert.NoError(t, store.AddOrUpdateHTTPRoute(route))
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodPost, tt.path, nil)
			c.Request.Host = tt.host
			if tt.listenerName != "" {
				c.Set(GatewayListenerNameKey, tt.listenerName)
				c.Set(GatewayListenerPortKey, tt.listenerPort)
			}

			result, matched := router.findHTTPRouteMatch(c, "default/gw")
			if tt.expectedRoute == "" {
				assert.False(t, matched)
				return
			}
			assert.True(t, matched)
			assert.Equal(t, tt.expectedRoute, result.route.Name)
			assert.Equal(t, tt.expectedPrefix, result.matchedPrefix)
			pool, found := inferencePoolFromHTTPRouteRule(result.route, result.rule)
			assert.True(t, found)
			assert.Equal(t, types.NamespacedName{Namespace: "default", Name: tt.expectedPool}, pool)
		})
	}
}

func TestRouter_FindHTTPRouteMatchCrossRoutePrecedence(t *testing.T) {
	pathType := gatewayv1.PathMatchPathPrefix
	gatewayKind := gatewayv1.Kind("Gateway")
	gatewayNamespace := gatewayv1.Namespace("default")
	route := func(namespace, name, prefix string, createdAt time.Time) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: v1.ObjectMeta{
				Namespace:         namespace,
				Name:              name,
				CreationTimestamp: v1.NewTime(createdAt),
			},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{{
						Name:      "gateway",
						Namespace: &gatewayNamespace,
						Kind:      &gatewayKind,
					}},
				},
				Rules: []gatewayv1.HTTPRouteRule{{
					Matches: []gatewayv1.HTTPRouteMatch{{
						Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &prefix},
					}},
				}},
			},
		}
	}
	withHostname := func(httpRoute *gatewayv1.HTTPRoute, hostname gatewayv1.Hostname) *gatewayv1.HTTPRoute {
		httpRoute.Spec.Hostnames = []gatewayv1.Hostname{hostname}
		return httpRoute
	}
	newer := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)

	tests := []struct {
		name          string
		routes        []*gatewayv1.HTTPRoute
		host          string
		expectedRoute types.NamespacedName
	}{
		{
			name: "longest matching prefix wins regardless of route order",
			routes: []*gatewayv1.HTTPRoute{
				route("default", "root-route", "/", older),
				route("default", "chat-route", "/chat", newer),
			},
			expectedRoute: types.NamespacedName{Namespace: "default", Name: "chat-route"},
		},
		{
			name: "exact hostname wins before path specificity",
			routes: []*gatewayv1.HTTPRoute{
				withHostname(route("default", "wildcard-route", "/chat", newer), "*.example.com"),
				withHostname(route("default", "exact-route", "/", older), "api.example.com"),
			},
			host:          "api.example.com",
			expectedRoute: types.NamespacedName{Namespace: "default", Name: "exact-route"},
		},
		{
			name: "oldest route wins equal path specificity regardless of route order",
			routes: []*gatewayv1.HTTPRoute{
				route("default", "new-route", "/chat", newer),
				route("default", "old-route", "/chat", older),
			},
			expectedRoute: types.NamespacedName{Namespace: "default", Name: "old-route"},
		},
		{
			name: "namespace and name resolve equal timestamp ties",
			routes: []*gatewayv1.HTTPRoute{
				route("team-b", "a-route", "/chat", older),
				route("team-a", "z-route", "/chat", older),
			},
			expectedRoute: types.NamespacedName{Namespace: "team-a", Name: "z-route"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := &Router{store: &orderedHTTPRouteStore{routes: tt.routes}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodPost, "/chat/completions", nil)
			c.Request.Host = tt.host

			match, found := router.findHTTPRouteMatch(c, "default/gateway")
			if !found {
				t.Fatal("expected a matching HTTPRoute")
			}
			assert.Equal(t, tt.expectedRoute, types.NamespacedName{
				Namespace: match.route.Namespace,
				Name:      match.route.Name,
			})
		})
	}
}

func TestMatchHTTPRouteHostname(t *testing.T) {
	tests := []struct {
		name          string
		pattern       string
		host          string
		expectedMatch bool
	}{
		{
			name:          "exact hostname",
			pattern:       "api.example.com",
			host:          "api.example.com",
			expectedMatch: true,
		},
		{
			name:          "exact pattern is normalized",
			pattern:       "API.EXAMPLE.COM.",
			host:          "api.example.com",
			expectedMatch: true,
		},
		{
			name:          "wildcard suffix",
			pattern:       "*.example.com",
			host:          "api.example.com",
			expectedMatch: true,
		},
		{
			name:          "wildcard suffix matches nested subdomain",
			pattern:       "*.example.com",
			host:          "v1.api.example.com",
			expectedMatch: true,
		},
		{
			name:          "wildcard does not match apex",
			pattern:       "*.example.com",
			host:          "example.com",
			expectedMatch: false,
		},
		{
			name:          "wildcard suffix mismatch",
			pattern:       "*.example.com",
			host:          "api.example.net",
			expectedMatch: false,
		},
		{
			name:          "exact mismatch",
			pattern:       "api.example.com",
			host:          "other.example.com",
			expectedMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched := matchHTTPRouteHostname(tt.pattern, tt.host)
			assert.Equal(t, tt.expectedMatch, matched)
		})
	}
}

func TestInferencePoolFromHTTPRouteRuleWeights(t *testing.T) {
	group := inferencePoolBackendGroup
	kind := inferencePoolBackendKind
	zero := int32(0)
	one := int32(1)

	backendRef := func(name string, weight *int32) gatewayv1.HTTPBackendRef {
		return gatewayv1.HTTPBackendRef{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: &group,
					Kind:  &kind,
					Name:  gatewayv1.ObjectName(name),
				},
				Weight: weight,
			},
		}
	}

	tests := []struct {
		name        string
		backendRefs []gatewayv1.HTTPBackendRef
		expected    types.NamespacedName
		found       bool
	}{
		{
			name: "skips a zero-weight backend",
			backendRefs: []gatewayv1.HTTPBackendRef{
				backendRef("pool-zero", &zero),
				backendRef("pool-live", &one),
			},
			expected: types.NamespacedName{Namespace: "default", Name: "pool-live"},
			found:    true,
		},
		{
			name: "uses the default weight when omitted",
			backendRefs: []gatewayv1.HTTPBackendRef{
				backendRef("pool-default", nil),
			},
			expected: types.NamespacedName{Namespace: "default", Name: "pool-default"},
			found:    true,
		},
		{
			name: "returns no backend when all weights are zero",
			backendRefs: []gatewayv1.HTTPBackendRef{
				backendRef("pool-zero", &zero),
			},
			found: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := &gatewayv1.HTTPRoute{ObjectMeta: v1.ObjectMeta{Namespace: "default"}}
			rule := &gatewayv1.HTTPRouteRule{BackendRefs: tt.backendRefs}

			pool, found := inferencePoolFromHTTPRouteRule(route, rule)
			assert.Equal(t, tt.found, found)
			assert.Equal(t, tt.expected, pool)
		})
	}
}

func TestSelectWeightedInferencePool(t *testing.T) {
	pools := []weightedInferencePool{
		{name: types.NamespacedName{Namespace: "default", Name: "pool-a"}, weight: 3},
		{name: types.NamespacedName{Namespace: "default", Name: "pool-b"}, weight: 1},
	}

	tests := []struct {
		name      string
		selection int
		expected  string
	}{
		{name: "first weight slot", selection: 0, expected: "pool-a"},
		{name: "last first-pool weight slot", selection: 2, expected: "pool-a"},
		{name: "second-pool weight slot", selection: 3, expected: "pool-b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, found := selectWeightedInferencePool(pools, tt.selection)
			assert.True(t, found)
			assert.Equal(t, tt.expected, pool.Name)
		})
	}
}
