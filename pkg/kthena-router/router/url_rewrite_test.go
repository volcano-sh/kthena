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
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// TestRouter_HandlerFunc_URLRewriteReplacePrefixMatch checks the path the
// backend receives for ReplacePrefixMatch. The spec rows are the examples on
// HTTPPathModifier.ReplacePrefixMatch in the Gateway API (v1.4.0).
func TestRouter_HandlerFunc_URLRewriteReplacePrefixMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		prefix      string
		replacement string
		requestPath string
		wantPath    string
	}{
		{name: "spec: /foo to /xyz", prefix: "/foo", replacement: "/xyz", requestPath: "/foo/bar", wantPath: "/xyz/bar"},
		{name: "spec: /foo to /xyz/", prefix: "/foo", replacement: "/xyz/", requestPath: "/foo/bar", wantPath: "/xyz/bar"},
		{name: "spec: /foo/ to /xyz", prefix: "/foo/", replacement: "/xyz", requestPath: "/foo/bar", wantPath: "/xyz/bar"},
		{name: "spec: /foo/ to /xyz/", prefix: "/foo/", replacement: "/xyz/", requestPath: "/foo/bar", wantPath: "/xyz/bar"},
		{name: "spec: exact prefix to /xyz", prefix: "/foo", replacement: "/xyz", requestPath: "/foo", wantPath: "/xyz"},
		{name: "spec: prefix with trailing slash to /xyz", prefix: "/foo", replacement: "/xyz", requestPath: "/foo/", wantPath: "/xyz/"},
		{name: "spec: /foo/bar to empty", prefix: "/foo", replacement: "", requestPath: "/foo/bar", wantPath: "/bar"},
		{name: "spec: /foo/ to empty", prefix: "/foo", replacement: "", requestPath: "/foo/", wantPath: "/"},
		{name: "spec: /foo to empty", prefix: "/foo", replacement: "", requestPath: "/foo", wantPath: "/"},
		{name: "spec: /foo/ to /", prefix: "/foo", replacement: "/", requestPath: "/foo/", wantPath: "/"},
		{name: "spec: /foo to /", prefix: "/foo", replacement: "/", requestPath: "/foo", wantPath: "/"},
		{name: "strip a public prefix", prefix: "/openai", replacement: "/", requestPath: "/openai/v1/chat/completions", wantPath: "/v1/chat/completions"},
		{name: "add /v1 under a root prefix", prefix: "/", replacement: "/v1", requestPath: "/chat/completions", wantPath: "/v1/chat/completions"},
		{name: "root path to /v1", prefix: "/", replacement: "/v1", requestPath: "/", wantPath: "/v1"},
		{name: "root prefix does not add a separator the path already has", prefix: "/", replacement: "/v1", requestPath: "//chat/completions", wantPath: "/v1/chat/completions"},
		{name: "exact match keeps the replacement's trailing slash", prefix: "/foo", replacement: "/xyz/", requestPath: "/foo", wantPath: "/xyz/"},
		{name: "remainder after a non-root prefix is kept as sent", prefix: "/foo", replacement: "/xyz", requestPath: "/foo//bar", wantPath: "/xyz//bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu           sync.Mutex
				receivedPath string
			)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					// The router also polls the pod, e.g. GET /metrics.
					return
				}
				mu.Lock()
				receivedPath = r.URL.Path
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"ok"}`)
			}))
			defer backend.Close()
			backendURL, err := url.Parse(backend.URL)
			require.NoError(t, err)
			backendPort, err := strconv.Atoi(backendURL.Port())
			require.NoError(t, err)

			store := datastore.New()
			router := NewRouter(store, "../scheduler/testdata/configmap.yaml", common.NewTransportRegistry())
			pool := &inferencev1.InferencePool{
				ObjectMeta: v1.ObjectMeta{Name: "rewrite-pool", Namespace: "default"},
				Spec: inferencev1.InferencePoolSpec{
					TargetPorts: []inferencev1.Port{{Number: inferencev1.PortNumber(backendPort)}},
					Selector: inferencev1.LabelSelector{MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
						"app": "rewrite-pool",
					}},
					EndpointPickerRef: inferencev1.EndpointPickerRef{Name: "picker"},
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{Name: "rewrite-pool-pod", Namespace: "default", Labels: map[string]string{"app": "rewrite-pool"}},
				Status:     corev1.PodStatus{PodIP: backendURL.Hostname(), Phase: corev1.PodRunning},
			}
			pathType := gatewayv1.PathMatchPathPrefix
			parentKind := gatewayv1.Kind("Gateway")
			backendGroup := inferencePoolBackendGroup
			backendKind := inferencePoolBackendKind
			prefix, replacement := tt.prefix, tt.replacement
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: v1.ObjectMeta{Name: "rewrite-route", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gw", Kind: &parentKind}}},
					Rules: []gatewayv1.HTTPRouteRule{{
						Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &prefix}}},
						Filters: []gatewayv1.HTTPRouteFilter{{
							Type: gatewayv1.HTTPRouteFilterURLRewrite,
							URLRewrite: &gatewayv1.HTTPURLRewriteFilter{Path: &gatewayv1.HTTPPathModifier{
								Type:               gatewayv1.PrefixMatchHTTPPathModifier,
								ReplacePrefixMatch: &replacement,
							}},
						}},
						BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
							Group: &backendGroup, Kind: &backendKind, Name: "rewrite-pool",
						}}}},
					}},
				},
			}
			require.NoError(t, store.AddOrUpdateInferencePool(pool))
			require.NoError(t, store.AddOrUpdatePod(pod, nil))
			require.NoError(t, store.AddOrUpdateHTTPRoute(route))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodPost, "http://router"+tt.requestPath, bytes.NewBufferString(`{"model":"pool-model","prompt":"hi"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(GatewayKey, "default/gw")

			router.HandlerFunc()(c)

			assert.Equal(t, http.StatusOK, w.Code)
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tt.wantPath, receivedPath)
		})
	}
}
