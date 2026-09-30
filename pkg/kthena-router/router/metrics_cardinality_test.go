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
	"sort"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

func labelValues(t *testing.T, collector prometheus.Collector, label string) []string {
	t.Helper()

	metricsCh := make(chan prometheus.Metric)
	go func() {
		collector.Collect(metricsCh)
		close(metricsCh)
	}()

	seen := make(map[string]struct{})
	for collected := range metricsCh {
		metric := &dto.Metric{}
		if err := collected.Write(metric); err != nil {
			t.Fatalf("Write: %v", err)
		}
		for _, l := range metric.Label {
			if l.GetName() == label {
				seen[l.GetValue()] = struct{}{}
			}
		}
	}

	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func TestRouter_HandlerFunc_BoundsPathLabelCardinality(t *testing.T) {
	router, _, backend := setupTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("backend must not be called for unroutable requests")
	}))
	defer backend.Close()

	requestsBefore := requestCounterValue(
		t, router, metrics.UnknownModel, metrics.UnknownPath, "404", "route_not_found",
	)

	const uniquePaths = 25
	for i := 0; i < uniquePaths; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		requestPath := fmt.Sprintf("/v1/zz-%d", i)
		c.Request, _ = http.NewRequest(http.MethodPost, requestPath, bytes.NewBufferString(`{"model":"zz-unregistered","prompt":"hello"}`))
		c.Request.Header.Set("Content-Type", "application/json")

		router.HandlerFunc()(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.NotContains(t, labelValues(t, &router.metrics.RequestsTotal, metrics.LabelPath), requestPath,
			"a client-chosen request path must not become a label value")
	}

	assert.Equal(t, requestsBefore+uniquePaths, requestCounterValue(
		t, router, metrics.UnknownModel, metrics.UnknownPath, "404", "route_not_found",
	))
}

func TestRouter_HandlerFunc_PathLabelKeepsRouterRoutes(t *testing.T) {
	tests := []struct {
		name        string
		matchType   gatewayv1.PathMatchType
		matchValue  string
		requestPath string
		wantLabel   string
	}{
		{
			name:        "router API endpoint keeps the endpoint path",
			requestPath: "/v1/chat/completions",
			wantLabel:   "/v1/chat/completions",
		},
		{
			name:        "path prefix match reports the route template",
			matchType:   gatewayv1.PathMatchPathPrefix,
			matchValue:  "/custom",
			requestPath: "/custom/zz-1",
			wantLabel:   "/custom",
		},
		{
			name:        "exact match reports the configured path",
			matchType:   gatewayv1.PathMatchExact,
			matchValue:  "/exact-path",
			requestPath: "/exact-path",
			wantLabel:   "/exact-path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, store, backend := setupTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"id":"ok"}`)
			}))
			defer backend.Close()

			backendURL, _ := url.Parse(backend.URL)
			backendPort, _ := strconv.Atoi(backendURL.Port())
			parentKind := gatewayv1.Kind("Gateway")
			poolGroup := inferencePoolBackendGroup
			poolKind := inferencePoolBackendKind

			pool := &inferencev1.InferencePool{
				ObjectMeta: objMeta("pool"),
				Spec: inferencev1.InferencePoolSpec{
					TargetPorts: []inferencev1.Port{{Number: inferencev1.PortNumber(backendPort)}},
					Selector: inferencev1.LabelSelector{MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
						"app": "pool",
					}},
					EndpointPickerRef: inferencev1.EndpointPickerRef{Name: "pool-picker"},
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: objMeta("pool-pod"),
				Status:     corev1.PodStatus{PodIP: backendURL.Hostname(), Phase: corev1.PodRunning},
			}
			pod.Labels = map[string]string{"app": "pool"}

			rules := []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &poolGroup,
						Kind:  &poolKind,
						Name:  "pool",
					},
				}}},
			}}
			if tt.matchType != "" {
				matchType := tt.matchType
				matchValue := tt.matchValue
				rules[0].Matches = []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{
					Type:  &matchType,
					Value: &matchValue,
				}}}
			}
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: objMeta("route"),
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
						Name: "gw",
						Kind: &parentKind,
					}}},
					Rules: rules,
				},
			}

			assert.NoError(t, store.AddOrUpdateInferencePool(pool))
			assert.NoError(t, store.AddOrUpdatePod(pod, nil))
			assert.NoError(t, store.AddOrUpdateHTTPRoute(route))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set(GatewayKey, "default/gw")
			c.Request, _ = http.NewRequest(http.MethodPost, tt.requestPath, bytes.NewBufferString(`{"model":"pool-model","prompt":"hello"}`))
			c.Request.Header.Set("Content-Type", "application/json")

			router.HandlerFunc()(c)

			assert.Equal(t, http.StatusOK, w.Code)
			paths := labelValues(t, &router.metrics.RequestsTotal, metrics.LabelPath)
			assert.Contains(t, paths, tt.wantLabel)
			if tt.requestPath != tt.wantLabel {
				assert.NotContains(t, paths, tt.requestPath,
					"the label reports the route template, not the request path")
			}
		})
	}
}

func TestRouter_HandlerFunc_BoundsUpstreamModelLabelCardinality(t *testing.T) {
	router, store, backend := setupTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok"}`)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendPort, _ := strconv.Atoi(backendURL.Port())
	parentKind := gatewayv1.Kind("Gateway")
	poolGroup := inferencePoolBackendGroup
	poolKind := inferencePoolBackendKind
	pathType := gatewayv1.PathMatchPathPrefix
	pathPrefix := "/v1"

	pool := &inferencev1.InferencePool{
		ObjectMeta: objMeta("pool"),
		Spec: inferencev1.InferencePoolSpec{
			TargetPorts: []inferencev1.Port{{Number: inferencev1.PortNumber(backendPort)}},
			Selector: inferencev1.LabelSelector{MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
				"app": "pool",
			}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{Name: "pool-picker"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "pool"},
		},
		Status: corev1.PodStatus{PodIP: backendURL.Hostname(), Phase: corev1.PodRunning},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: objMeta("pool-route"),
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name: "gw",
				Kind: &parentKind,
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{
					Type:  &pathType,
					Value: &pathPrefix,
				}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &poolGroup,
						Kind:  &poolKind,
						Name:  "pool",
					},
				}}},
			}},
		},
	}

	assert.NoError(t, store.AddOrUpdateInferencePool(pool))
	assert.NoError(t, store.AddOrUpdatePod(pod, nil))
	assert.NoError(t, store.AddOrUpdateHTTPRoute(route))

	const uniqueModels = 25
	for i := 0; i < uniqueModels; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set(GatewayKey, "default/gw")
		modelName := fmt.Sprintf("zz-model-%d", i)
		body := fmt.Sprintf(`{"model":%q,"prompt":"hello"}`, modelName)
		c.Request, _ = http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")

		router.HandlerFunc()(c)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.NotContains(t, labelValues(t, &router.metrics.ActiveUpstreamRequests, metrics.LabelUpstreamModel), modelName,
			"an unregistered requested model must not become a label value")
	}

	assert.Contains(t, labelValues(t, &router.metrics.ActiveUpstreamRequests, metrics.LabelUpstreamModel), metrics.UnknownModel)
}

// TestRouter_HandlerFunc_ModelRoutePrecedenceKeepsPathLabelBounded covers the
// precedence between ModelRoute and HTTPRoute for /v1/ requests: when a
// ModelRoute serves the request, an overlapping HTTPRoute must not supply the
// `path` label, because it did not serve the request.
func TestRouter_HandlerFunc_ModelRoutePrecedenceKeepsPathLabelBounded(t *testing.T) {
	router, store, backend := setupTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok"}`)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: objMeta("ms-path-label"),
		Spec: aiv1alpha1.ModelServerSpec{
			Model:           func(s string) *string { return &s }("test-model-base"),
			WorkloadPort:    aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
			InferenceEngine: "vLLM",
		},
	}
	modelPod := &corev1.Pod{
		ObjectMeta: objMeta("ms-path-label-pod"),
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: objMeta("mr-path-label"),
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "path-label-model",
			Rules: []*aiv1alpha1.Rule{{
				TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-path-label"}},
			}},
		},
	}
	store.AddOrUpdateModelServer(modelServer, sets.New(types.NamespacedName{Name: "ms-path-label-pod", Namespace: "default"}))
	store.AddOrUpdatePod(modelPod, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute)

	// An HTTPRoute that also matches /v1/ paths but does not serve this request.
	parentKind := gatewayv1.Kind("Gateway")
	poolGroup := inferencePoolBackendGroup
	poolKind := inferencePoolBackendKind
	pathType := gatewayv1.PathMatchPathPrefix
	pathPrefix := "/v1"
	pool := &inferencev1.InferencePool{
		ObjectMeta: objMeta("pool-path-label"),
		Spec: inferencev1.InferencePoolSpec{
			TargetPorts: []inferencev1.Port{{Number: inferencev1.PortNumber(backendPort)}},
			Selector: inferencev1.LabelSelector{MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
				"app": "pool-path-label",
			}},
			EndpointPickerRef: inferencev1.EndpointPickerRef{Name: "pool-picker"},
		},
	}
	poolPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-path-label-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "pool-path-label"},
		},
		Status: corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: objMeta("unused-route"),
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name: "gw",
				Kind: &parentKind,
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{
					Type:  &pathType,
					Value: &pathPrefix,
				}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &poolGroup,
						Kind:  &poolKind,
						Name:  "pool-path-label",
					},
				}}},
			}},
		},
	}
	assert.NoError(t, store.AddOrUpdateInferencePool(pool))
	assert.NoError(t, store.AddOrUpdatePod(poolPod, nil))
	assert.NoError(t, store.AddOrUpdateHTTPRoute(httpRoute))

	unknownPathBefore := requestCounterValue(
		t, router, "path-label-model", metrics.UnknownPath, "200", successfulRequestFinishReason,
	)
	unusedRouteLabelBefore := requestCounterValue(
		t, router, "path-label-model", pathPrefix, "200", successfulRequestFinishReason,
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(GatewayKey, "default/gw")
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/custom", bytes.NewBufferString(`{"model":"path-label-model","prompt":"hello"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	router.HandlerFunc()(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, float64(1), requestCounterValue(
		t, router, "path-label-model", metrics.UnknownPath, "200", successfulRequestFinishReason,
	)-unknownPathBefore, "a ModelRoute-served request keeps a bounded path label")
	assert.Equal(t, float64(0), requestCounterValue(
		t, router, "path-label-model", pathPrefix, "200", successfulRequestFinishReason,
	)-unusedRouteLabelBefore, "the HTTPRoute that did not serve the request must not supply the path label")
}

func objMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "default"}
}
