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

package debug

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// ── ListModelRoutes ───────────────────────────────────────────────────────────

func TestListModelRoutes_SkipsInvalidKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	// Key without "/" is silently skipped
	mockStore.On("GetAllModelRoutes").Return(map[string]*aiv1alpha1.ModelRoute{
		"no-slash": {ObjectMeta: metav1.ObjectMeta{Name: "bad-route"}},
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/modelroutes", nil)

	handler.ListModelRoutes(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]ModelRouteResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Empty(t, response["modelroutes"])

	mockStore.AssertExpectations(t)
}

// ── GetModelRoute ─────────────────────────────────────────────────────────────

func TestGetModelRoute_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	mockStore.On("GetModelRoute", "default/missing-route").Return(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/modelroutes/missing-route", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "missing-route"},
	}

	handler.GetModelRoute(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "ModelRoute not found", response["error"])

	mockStore.AssertExpectations(t)
}

func TestGetModelRoute_MissingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces//modelroutes/", nil)
	// c.Params intentionally empty → Param() returns "" for both keys

	handler.GetModelRoute(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "namespace and name parameters are required", response["error"])
}

// ── ListModelServers ──────────────────────────────────────────────────────────

func TestListModelServers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	msKey := types.NamespacedName{Namespace: "default", Name: "llama2-server"}
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "llama2-server", Namespace: "default"},
	}
	associatedPods := []*datastore.PodInfo{
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}}},
	}
	decodePods := []*datastore.PodInfo{
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "decode-1", Namespace: "default"}}},
	}
	prefillPods := []*datastore.PodInfo{
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "prefill-1", Namespace: "default"}}},
	}

	mockStore.On("GetAllModelServers").Return(map[types.NamespacedName]*aiv1alpha1.ModelServer{msKey: modelServer})
	mockStore.On("GetPodsByModelServer", msKey).Return(associatedPods, nil)
	mockStore.On("GetDecodePods", msKey).Return(decodePods, nil)
	mockStore.On("GetPrefillPods", msKey).Return(prefillPods, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/modelservers", nil)

	handler.ListModelServers(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]ModelServerResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

	servers := response["modelservers"]
	assert.Len(t, servers, 1)
	assert.Equal(t, "llama2-server", servers[0].Name)
	assert.Equal(t, "default", servers[0].Namespace)
	assert.Equal(t, []string{"default/pod-a"}, servers[0].AssociatedPods)
	assert.Equal(t, []string{"default/decode-1"}, servers[0].DecodePods)
	assert.Equal(t, []string{"default/prefill-1"}, servers[0].PrefillPods)

	mockStore.AssertExpectations(t)
}

// ── GetModelServer ────────────────────────────────────────────────────────────

func TestGetModelServer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	msKey := types.NamespacedName{Namespace: "default", Name: "llama2-server"}
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "llama2-server", Namespace: "default"},
	}
	pods := []*datastore.PodInfo{
		{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"}}},
	}

	mockStore.On("GetModelServer", msKey).Return(modelServer)
	mockStore.On("GetPodsByModelServer", msKey).Return(pods, nil)
	mockStore.On("GetDecodePods", msKey).Return([]*datastore.PodInfo{}, nil)
	mockStore.On("GetPrefillPods", msKey).Return([]*datastore.PodInfo{}, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/modelservers/llama2-server", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "llama2-server"},
	}

	handler.GetModelServer(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response ModelServerResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "llama2-server", response.Name)
	assert.Equal(t, "default", response.Namespace)
	assert.Equal(t, []string{"default/pod-1"}, response.AssociatedPods)

	mockStore.AssertExpectations(t)
}

func TestGetModelServer_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	mockStore.On("GetModelServer", types.NamespacedName{Namespace: "default", Name: "missing"}).Return(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/modelservers/missing", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "missing"},
	}

	handler.GetModelServer(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "ModelServer not found", response["error"])

	mockStore.AssertExpectations(t)
}

func TestGetModelServer_MissingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces//modelservers/", nil)

	handler.GetModelServer(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "namespace and name parameters are required", response["error"])
}

// ── ListPods ──────────────────────────────────────────────────────────────────

func TestListPods(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	podKey := types.NamespacedName{Namespace: "default", Name: "pod-1"}
	podInfo := &datastore.PodInfo{
		GPUCacheUsage:     0.75,
		RequestWaitingNum: 3,
		RequestRunningNum: 2,
		TPOT:              1.5,
		TTFT:              0.2,
	}

	mockStore.On("GetAllPods").Return(map[types.NamespacedName]*datastore.PodInfo{podKey: podInfo})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/pods", nil)

	handler.ListPods(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]PodResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

	pods := response["pods"]
	assert.Len(t, pods, 1)
	assert.Equal(t, "pod-1", pods[0].Name)
	assert.Equal(t, "default", pods[0].Namespace)
	assert.NotNil(t, pods[0].Metrics)
	assert.Equal(t, 0.75, pods[0].Metrics.GPUCacheUsage)
	assert.Equal(t, float64(3), pods[0].Metrics.RequestWaitingNum)
	assert.Equal(t, float64(2), pods[0].Metrics.RequestRunningNum)
	assert.Equal(t, 1.5, pods[0].Metrics.TPOT)
	assert.Equal(t, 0.2, pods[0].Metrics.TTFT)

	mockStore.AssertExpectations(t)
}

// ── GetPod ────────────────────────────────────────────────────────────────────

func TestGetPod(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	podKey := types.NamespacedName{Namespace: "default", Name: "pod-1"}
	podInfo := &datastore.PodInfo{
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-1",
				Namespace: "default",
				Labels:    map[string]string{"app": "llm"},
			},
			Spec: corev1.PodSpec{NodeName: "node-1"},
			Status: corev1.PodStatus{
				PodIP: "10.0.0.42",
				Phase: corev1.PodRunning,
			},
		},
		GPUCacheUsage:     0.5,
		RequestRunningNum: 1,
	}

	mockStore.On("GetPodInfo", podKey).Return(podInfo)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/pods/pod-1", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "pod-1"},
	}

	handler.GetPod(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response PodResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "pod-1", response.Name)
	assert.Equal(t, "default", response.Namespace)
	assert.NotNil(t, response.PodInfo)
	assert.Equal(t, "10.0.0.42", response.PodInfo.PodIP)
	assert.Equal(t, "node-1", response.PodInfo.NodeName)
	assert.Equal(t, "Running", response.PodInfo.Phase)
	assert.Equal(t, map[string]string{"app": "llm"}, response.PodInfo.Labels)
	assert.Equal(t, 0.5, response.Metrics.GPUCacheUsage)

	mockStore.AssertExpectations(t)
}

func TestGetPod_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	mockStore.On("GetPodInfo", types.NamespacedName{Namespace: "default", Name: "missing"}).Return(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/pods/missing", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "missing"},
	}

	handler.GetPod(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "Pod not found", response["error"])

	mockStore.AssertExpectations(t)
}

func TestGetPod_MissingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces//pods/", nil)

	handler.GetPod(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "namespace and name parameters are required", response["error"])
}

// ── ListGateways ──────────────────────────────────────────────────────────────

func TestListGateways(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
	}

	mockStore.On("GetAllGateways").Return([]*gatewayv1.Gateway{gw})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/gateways", nil)

	handler.ListGateways(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]GatewayResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

	gateways := response["gateways"]
	assert.Len(t, gateways, 1)
	assert.Equal(t, "my-gateway", gateways[0].Name)
	assert.Equal(t, "default", gateways[0].Namespace)

	mockStore.AssertExpectations(t)
}

// ── GetGateway ────────────────────────────────────────────────────────────────

func TestGetGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
	}

	mockStore.On("GetGateway", "default/my-gateway").Return(gw)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/gateways/my-gateway", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "my-gateway"},
	}

	handler.GetGateway(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response GatewayResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "my-gateway", response.Name)
	assert.Equal(t, "default", response.Namespace)

	mockStore.AssertExpectations(t)
}

func TestGetGateway_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	mockStore.On("GetGateway", "default/missing").Return(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/gateways/missing", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "missing"},
	}

	handler.GetGateway(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "Gateway not found", response["error"])

	mockStore.AssertExpectations(t)
}

func TestGetGateway_MissingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces//gateways/", nil)

	handler.GetGateway(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "namespace and name parameters are required", response["error"])
}

// ── ListHTTPRoutes ────────────────────────────────────────────────────────────

func TestListHTTPRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	hr := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-httproute", Namespace: "default"},
	}

	mockStore.On("GetAllHTTPRoutes").Return([]*gatewayv1.HTTPRoute{hr})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/httproutes", nil)

	handler.ListHTTPRoutes(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]HTTPRouteResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

	routes := response["httproutes"]
	assert.Len(t, routes, 1)
	assert.Equal(t, "my-httproute", routes[0].Name)
	assert.Equal(t, "default", routes[0].Namespace)

	mockStore.AssertExpectations(t)
}

func TestListHTTPRoutes_SkipsNilEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	valid := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-route", Namespace: "default"},
	}

	// nil entries should be silently skipped
	mockStore.On("GetAllHTTPRoutes").Return([]*gatewayv1.HTTPRoute{nil, valid, nil})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/httproutes", nil)

	handler.ListHTTPRoutes(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]HTTPRouteResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Len(t, response["httproutes"], 1)
	assert.Equal(t, "valid-route", response["httproutes"][0].Name)

	mockStore.AssertExpectations(t)
}

// ── GetHTTPRoute ──────────────────────────────────────────────────────────────

func TestGetHTTPRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	hr := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-httproute", Namespace: "default"},
	}

	mockStore.On("GetHTTPRoute", "default/my-httproute").Return(hr)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/httproutes/my-httproute", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "my-httproute"},
	}

	handler.GetHTTPRoute(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response HTTPRouteResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "my-httproute", response.Name)
	assert.Equal(t, "default", response.Namespace)

	mockStore.AssertExpectations(t)
}

func TestGetHTTPRoute_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	mockStore.On("GetHTTPRoute", "default/missing").Return(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/httproutes/missing", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "missing"},
	}

	handler.GetHTTPRoute(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "HTTPRoute not found", response["error"])

	mockStore.AssertExpectations(t)
}

func TestGetHTTPRoute_MissingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces//httproutes/", nil)

	handler.GetHTTPRoute(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "namespace and name parameters are required", response["error"])
}

// ── ListInferencePools ────────────────────────────────────────────────────────

func TestListInferencePools(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	ip := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pool", Namespace: "default"},
	}

	mockStore.On("GetAllInferencePools").Return([]*inferencev1.InferencePool{ip})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/inferencepools", nil)

	handler.ListInferencePools(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]InferencePoolResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))

	pools := response["inferencepools"]
	assert.Len(t, pools, 1)
	assert.Equal(t, "my-pool", pools[0].Name)
	assert.Equal(t, "default", pools[0].Namespace)

	mockStore.AssertExpectations(t)
}

func TestListInferencePools_SkipsNilEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	valid := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-pool", Namespace: "default"},
	}

	// nil entries should be silently skipped
	mockStore.On("GetAllInferencePools").Return([]*inferencev1.InferencePool{nil, valid, nil})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/inferencepools", nil)

	handler.ListInferencePools(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]InferencePoolResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Len(t, response["inferencepools"], 1)
	assert.Equal(t, "valid-pool", response["inferencepools"][0].Name)

	mockStore.AssertExpectations(t)
}

// ── GetInferencePool ──────────────────────────────────────────────────────────

func TestGetInferencePool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	ip := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pool", Namespace: "default"},
	}

	mockStore.On("GetInferencePool", "default/my-pool").Return(ip)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/inferencepools/my-pool", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "my-pool"},
	}

	handler.GetInferencePool(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var response InferencePoolResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "my-pool", response.Name)
	assert.Equal(t, "default", response.Namespace)

	mockStore.AssertExpectations(t)
}

func TestGetInferencePool_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	mockStore.On("GetInferencePool", "default/missing").Return(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces/default/inferencepools/missing", nil)
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "missing"},
	}

	handler.GetInferencePool(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "InferencePool not found", response["error"])

	mockStore.AssertExpectations(t)
}

func TestGetInferencePool_MissingParams(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/debug/config_dump/namespaces//inferencepools/", nil)

	handler.GetInferencePool(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]string
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "namespace and name parameters are required", response["error"])
}
