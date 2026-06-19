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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

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

	tests := []struct {
		name           string
		routes         []*gatewayv1.HTTPRoute
		host           string
		path           string
		expectedRoute  string
		expectedPool   string
		expectedPrefix string
	}{
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
