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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// MockStore implements the datastore.Store interface for testing.
type MockStore struct {
	mock.Mock
}

func (m *MockStore) AddOrUpdateModelServer(modelServer *aiv1alpha1.ModelServer, pods sets.Set[types.NamespacedName]) error {
	args := m.Called(modelServer, pods)
	return args.Error(0)
}

func (m *MockStore) DeleteModelServer(name types.NamespacedName) error {
	args := m.Called(name)
	return args.Error(0)
}

func (m *MockStore) GetModelServer(name types.NamespacedName) *aiv1alpha1.ModelServer {
	args := m.Called(name)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*aiv1alpha1.ModelServer)
}

func (m *MockStore) GetPodsByModelServer(name types.NamespacedName) ([]*datastore.PodInfo, error) {
	args := m.Called(name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*datastore.PodInfo), args.Error(1)
}

func (m *MockStore) AddOrUpdatePod(pod *corev1.Pod, modelServer []*aiv1alpha1.ModelServer) error {
	args := m.Called(pod, modelServer)
	return args.Error(0)
}

func (m *MockStore) AppendModelServerToPod(pod *corev1.Pod, modelServers []*aiv1alpha1.ModelServer) error {
	args := m.Called(pod, modelServers)
	return args.Error(0)
}

func (m *MockStore) DeletePod(podName types.NamespacedName) error {
	args := m.Called(podName)
	return args.Error(0)
}

func (m *MockStore) MatchModelServer(modelName string, request *http.Request, gatewayKey string) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error) {
	args := m.Called(modelName, request, gatewayKey)
	var modelRoute *aiv1alpha1.ModelRoute
	if args.Get(2) != nil {
		modelRoute = args.Get(2).(*aiv1alpha1.ModelRoute)
	}
	return args.Get(0).(types.NamespacedName), args.Bool(1), modelRoute, args.Error(3)
}

func (m *MockStore) AddOrUpdateModelRoute(mr *aiv1alpha1.ModelRoute) error {
	args := m.Called(mr)
	return args.Error(0)
}

func (m *MockStore) DeleteModelRoute(namespacedName string) error {
	args := m.Called(namespacedName)
	return args.Error(0)
}

func (m *MockStore) GetDecodePods(modelServerName types.NamespacedName) ([]*datastore.PodInfo, error) {
	args := m.Called(modelServerName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*datastore.PodInfo), args.Error(1)
}

func (m *MockStore) GetPrefillPods(modelServerName types.NamespacedName) ([]*datastore.PodInfo, error) {
	args := m.Called(modelServerName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*datastore.PodInfo), args.Error(1)
}

func (m *MockStore) GetPrefillPodsForDecodeGroup(modelServerName types.NamespacedName, decodePodName types.NamespacedName) ([]*datastore.PodInfo, error) {
	args := m.Called(modelServerName, decodePodName)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*datastore.PodInfo), args.Error(1)
}

func (m *MockStore) RegisterCallback(kind string, callback datastore.CallbackFunc) {
	m.Called(kind, callback)
}

func (m *MockStore) Run(ctx context.Context) {
	m.Called(ctx)
}

func (m *MockStore) HasSynced() bool {
	args := m.Called()
	return args.Bool(0)
}

func (m *MockStore) GetPodInfo(podName types.NamespacedName) *datastore.PodInfo {
	args := m.Called(podName)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*datastore.PodInfo)
}

func (m *MockStore) GetTokenCount(userId, modelName string) (float64, error) {
	args := m.Called(userId, modelName)
	return args.Get(0).(float64), args.Error(1)
}

func (m *MockStore) UpdateTokenCount(userId, modelName string, inputTokens, outputTokens float64) error {
	args := m.Called(userId, modelName, inputTokens, outputTokens)
	return args.Error(0)
}

func (m *MockStore) GetRequestCount(userId, modelName string) (int, error) {
	args := m.Called(userId, modelName)
	return args.Int(0), args.Error(1)
}

func (m *MockStore) Enqueue(req *datastore.Request) error {
	args := m.Called(req)
	return args.Error(0)
}

func (m *MockStore) GetRequestWaitingQueueStats() []datastore.QueueStat {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]datastore.QueueStat)
}

func (m *MockStore) GetAllModelRoutes() map[string]*aiv1alpha1.ModelRoute {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(map[string]*aiv1alpha1.ModelRoute)
}

func (m *MockStore) GetAllModelServers() map[types.NamespacedName]*aiv1alpha1.ModelServer {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(map[types.NamespacedName]*aiv1alpha1.ModelServer)
}

func (m *MockStore) GetAllPods() map[types.NamespacedName]*datastore.PodInfo {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(map[types.NamespacedName]*datastore.PodInfo)
}

func (m *MockStore) GetModelRoute(namespacedName string) *aiv1alpha1.ModelRoute {
	args := m.Called(namespacedName)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*aiv1alpha1.ModelRoute)
}

func (m *MockStore) AddOrUpdateGateway(gateway *gatewayv1.Gateway) error {
	args := m.Called(gateway)
	return args.Error(0)
}

func (m *MockStore) DeleteGateway(key string) error {
	args := m.Called(key)
	return args.Error(0)
}

func (m *MockStore) GetGateway(key string) *gatewayv1.Gateway {
	args := m.Called(key)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*gatewayv1.Gateway)
}

func (m *MockStore) GetGatewaysByNamespace(namespace string) []*gatewayv1.Gateway {
	args := m.Called(namespace)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*gatewayv1.Gateway)
}

func (m *MockStore) GetAllGateways() []*gatewayv1.Gateway {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*gatewayv1.Gateway)
}

func (m *MockStore) AddOrUpdateInferencePool(inferencePool *inferencev1.InferencePool) error {
	args := m.Called(inferencePool)
	return args.Error(0)
}

func (m *MockStore) DeleteInferencePool(key string) error {
	args := m.Called(key)
	return args.Error(0)
}

func (m *MockStore) GetInferencePool(key string) *inferencev1.InferencePool {
	args := m.Called(key)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*inferencev1.InferencePool)
}

func (m *MockStore) GetPodsByInferencePool(name types.NamespacedName) ([]*datastore.PodInfo, error) {
	args := m.Called(name)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*datastore.PodInfo), args.Error(1)
}

func (m *MockStore) AddOrUpdateHTTPRoute(httpRoute *gatewayv1.HTTPRoute) error {
	args := m.Called(httpRoute)
	return args.Error(0)
}

func (m *MockStore) DeleteHTTPRoute(key string) error {
	args := m.Called(key)
	return args.Error(0)
}

func (m *MockStore) GetHTTPRoute(key string) *gatewayv1.HTTPRoute {
	args := m.Called(key)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*gatewayv1.HTTPRoute)
}

func (m *MockStore) GetHTTPRoutesByGateway(gatewayKey string) []*gatewayv1.HTTPRoute {
	args := m.Called(gatewayKey)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*gatewayv1.HTTPRoute)
}

func (m *MockStore) GetModelRoutesByGateway(gatewayKey string) []*aiv1alpha1.ModelRoute {
	args := m.Called(gatewayKey)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*aiv1alpha1.ModelRoute)
}

func (m *MockStore) GetAllHTTPRoutes() []*gatewayv1.HTTPRoute {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*gatewayv1.HTTPRoute)
}

func (m *MockStore) GetAllInferencePools() []*inferencev1.InferencePool {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]*inferencev1.InferencePool)
}

func newTestContext(params gin.Params) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/", nil)
	c.Params = params
	return c, w
}

func nsName(namespace, name string) gin.Params {
	return gin.Params{
		{Key: "namespace", Value: namespace},
		{Key: "name", Value: name},
	}
}

type handlerCase struct {
	name           string
	params         gin.Params
	setup          func(*MockStore)
	expectedStatus int
	check          func(*testing.T, []byte)
}

func runHandlerCases(t *testing.T, cases []handlerCase, invoke func(*DebugHandler, *gin.Context)) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockStore := &MockStore{}
			if tc.setup != nil {
				tc.setup(mockStore)
			}
			handler := NewDebugHandler(mockStore)

			c, w := newTestContext(tc.params)
			invoke(handler, c)

			assert.Equal(t, tc.expectedStatus, w.Code)
			if tc.check != nil {
				tc.check(t, w.Body.Bytes())
			}
			mockStore.AssertExpectations(t)
		})
	}
}

func errorBodyCheck(message string) func(*testing.T, []byte) {
	return func(t *testing.T, body []byte) {
		var response map[string]string
		require.NoError(t, json.Unmarshal(body, &response))
		assert.Equal(t, message, response["error"])
	}
}

func TestListModelRoutes(t *testing.T) {
	cases := []handlerCase{
		{
			name: "success",
			setup: func(m *MockStore) {
				m.On("GetAllModelRoutes").Return(map[string]*aiv1alpha1.ModelRoute{
					"default/llama2-route": {
						ObjectMeta: metav1.ObjectMeta{Name: "llama2-route", Namespace: "default"},
						Spec: aiv1alpha1.ModelRouteSpec{
							ModelName:    "llama2-7b",
							LoraAdapters: []string{"lora-adapter-1", "lora-adapter-2"},
						},
					},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]ModelRouteResponse
				require.NoError(t, json.Unmarshal(body, &response))
				routes := response["modelroutes"]
				require.Len(t, routes, 1)
				assert.Equal(t, "llama2-route", routes[0].Name)
				assert.Equal(t, "default", routes[0].Namespace)
				assert.Equal(t, "llama2-7b", routes[0].Spec.ModelName)
			},
		},
		{
			// Keys without "/" are silently skipped.
			name: "skips invalid key without slash",
			setup: func(m *MockStore) {
				m.On("GetAllModelRoutes").Return(map[string]*aiv1alpha1.ModelRoute{
					"no-slash": {ObjectMeta: metav1.ObjectMeta{Name: "bad-route"}},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]ModelRouteResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Empty(t, response["modelroutes"])
			},
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.ListModelRoutes(c) })
}

func TestGetModelRoute(t *testing.T) {
	cases := []handlerCase{
		{
			name:   "found",
			params: nsName("default", "llama2-route"),
			setup: func(m *MockStore) {
				m.On("GetModelRoute", "default/llama2-route").Return(&aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "llama2-route", Namespace: "default"},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName:    "llama2-7b",
						LoraAdapters: []string{"lora-adapter-1"},
					},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response ModelRouteResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Equal(t, "llama2-route", response.Name)
				assert.Equal(t, "default", response.Namespace)
				assert.Equal(t, "llama2-7b", response.Spec.ModelName)
			},
		},
		{
			name:   "not found",
			params: nsName("default", "missing-route"),
			setup: func(m *MockStore) {
				m.On("GetModelRoute", "default/missing-route").Return(nil)
			},
			expectedStatus: http.StatusNotFound,
			check:          errorBodyCheck("ModelRoute not found"),
		},
		{
			name:           "missing params",
			params:         gin.Params{},
			expectedStatus: http.StatusBadRequest,
			check:          errorBodyCheck("namespace and name parameters are required"),
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.GetModelRoute(c) })
}

func TestListModelServers(t *testing.T) {
	msKey := types.NamespacedName{Namespace: "default", Name: "llama2-server"}
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "llama2-server", Namespace: "default"},
	}
	pod := func(name string) *datastore.PodInfo {
		return &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}}
	}

	cases := []handlerCase{
		{
			name: "all queries succeed",
			setup: func(m *MockStore) {
				m.On("GetAllModelServers").Return(map[types.NamespacedName]*aiv1alpha1.ModelServer{msKey: modelServer})
				m.On("GetPodsByModelServer", msKey).Return([]*datastore.PodInfo{pod("pod-a")}, nil)
				m.On("GetDecodePods", msKey).Return([]*datastore.PodInfo{pod("decode-1")}, nil)
				m.On("GetPrefillPods", msKey).Return([]*datastore.PodInfo{pod("prefill-1")}, nil)
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]ModelServerResponse
				require.NoError(t, json.Unmarshal(body, &response))
				servers := response["modelservers"]
				require.Len(t, servers, 1)
				assert.Equal(t, []string{"default/pod-a"}, servers[0].AssociatedPods)
				assert.Equal(t, []string{"default/decode-1"}, servers[0].DecodePods)
				assert.Equal(t, []string{"default/prefill-1"}, servers[0].PrefillPods)
			},
		},
		{
			name: "GetPodsByModelServer error leaves AssociatedPods nil",
			setup: func(m *MockStore) {
				m.On("GetAllModelServers").Return(map[types.NamespacedName]*aiv1alpha1.ModelServer{msKey: modelServer})
				m.On("GetPodsByModelServer", msKey).Return(nil, errors.New("pods unavailable"))
				m.On("GetDecodePods", msKey).Return([]*datastore.PodInfo{pod("decode-1")}, nil)
				m.On("GetPrefillPods", msKey).Return([]*datastore.PodInfo{pod("prefill-1")}, nil)
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]ModelServerResponse
				require.NoError(t, json.Unmarshal(body, &response))
				servers := response["modelservers"]
				require.Len(t, servers, 1)
				assert.Nil(t, servers[0].AssociatedPods)
				assert.Equal(t, []string{"default/decode-1"}, servers[0].DecodePods)
				assert.Equal(t, []string{"default/prefill-1"}, servers[0].PrefillPods)
			},
		},
		{
			name: "GetDecodePods error leaves DecodePods nil",
			setup: func(m *MockStore) {
				m.On("GetAllModelServers").Return(map[types.NamespacedName]*aiv1alpha1.ModelServer{msKey: modelServer})
				m.On("GetPodsByModelServer", msKey).Return([]*datastore.PodInfo{pod("pod-a")}, nil)
				m.On("GetDecodePods", msKey).Return(nil, errors.New("decode pods unavailable"))
				m.On("GetPrefillPods", msKey).Return([]*datastore.PodInfo{pod("prefill-1")}, nil)
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]ModelServerResponse
				require.NoError(t, json.Unmarshal(body, &response))
				servers := response["modelservers"]
				require.Len(t, servers, 1)
				assert.Equal(t, []string{"default/pod-a"}, servers[0].AssociatedPods)
				assert.Nil(t, servers[0].DecodePods)
				assert.Equal(t, []string{"default/prefill-1"}, servers[0].PrefillPods)
			},
		},
		{
			name: "GetPrefillPods error leaves PrefillPods nil",
			setup: func(m *MockStore) {
				m.On("GetAllModelServers").Return(map[types.NamespacedName]*aiv1alpha1.ModelServer{msKey: modelServer})
				m.On("GetPodsByModelServer", msKey).Return([]*datastore.PodInfo{pod("pod-a")}, nil)
				m.On("GetDecodePods", msKey).Return([]*datastore.PodInfo{pod("decode-1")}, nil)
				m.On("GetPrefillPods", msKey).Return(nil, errors.New("prefill pods unavailable"))
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]ModelServerResponse
				require.NoError(t, json.Unmarshal(body, &response))
				servers := response["modelservers"]
				require.Len(t, servers, 1)
				assert.Equal(t, []string{"default/pod-a"}, servers[0].AssociatedPods)
				assert.Equal(t, []string{"default/decode-1"}, servers[0].DecodePods)
				assert.Nil(t, servers[0].PrefillPods)
			},
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.ListModelServers(c) })
}

func TestGetModelServer(t *testing.T) {
	msKey := types.NamespacedName{Namespace: "default", Name: "llama2-server"}
	pod := func(name string) *datastore.PodInfo {
		return &datastore.PodInfo{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}}
	}

	cases := []handlerCase{
		{
			name:   "found",
			params: nsName("default", "llama2-server"),
			setup: func(m *MockStore) {
				m.On("GetModelServer", msKey).Return(&aiv1alpha1.ModelServer{
					ObjectMeta: metav1.ObjectMeta{Name: "llama2-server", Namespace: "default"},
				})
				m.On("GetPodsByModelServer", msKey).Return([]*datastore.PodInfo{pod("pod-1")}, nil)
				m.On("GetDecodePods", msKey).Return([]*datastore.PodInfo{pod("decode-1")}, nil)
				m.On("GetPrefillPods", msKey).Return([]*datastore.PodInfo{pod("prefill-1")}, nil)
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response ModelServerResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Equal(t, "llama2-server", response.Name)
				assert.Equal(t, "default", response.Namespace)
				assert.Equal(t, []string{"default/pod-1"}, response.AssociatedPods)
				assert.Equal(t, []string{"default/decode-1"}, response.DecodePods)
				assert.Equal(t, []string{"default/prefill-1"}, response.PrefillPods)
			},
		},
		{
			name:   "not found",
			params: nsName("default", "missing"),
			setup: func(m *MockStore) {
				m.On("GetModelServer", types.NamespacedName{Namespace: "default", Name: "missing"}).Return(nil)
			},
			expectedStatus: http.StatusNotFound,
			check:          errorBodyCheck("ModelServer not found"),
		},
		{
			name:           "missing params",
			params:         gin.Params{},
			expectedStatus: http.StatusBadRequest,
			check:          errorBodyCheck("namespace and name parameters are required"),
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.GetModelServer(c) })
}

func TestListPods(t *testing.T) {
	podKey := types.NamespacedName{Namespace: "default", Name: "pod-1"}
	podInfo := &datastore.PodInfo{
		GPUCacheUsage:     0.75,
		RequestWaitingNum: 3,
		RequestRunningNum: 2,
		TPOT:              1.5,
		TTFT:              0.2,
	}

	cases := []handlerCase{
		{
			name: "success",
			setup: func(m *MockStore) {
				m.On("GetAllPods").Return(map[types.NamespacedName]*datastore.PodInfo{podKey: podInfo})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]PodResponse
				require.NoError(t, json.Unmarshal(body, &response))
				pods := response["pods"]
				require.Len(t, pods, 1)
				assert.Equal(t, "pod-1", pods[0].Name)
				assert.Equal(t, "default", pods[0].Namespace)
				require.NotNil(t, pods[0].Metrics)
				assert.Equal(t, 0.75, pods[0].Metrics.GPUCacheUsage)
				assert.Equal(t, float64(3), pods[0].Metrics.RequestWaitingNum)
				assert.Equal(t, float64(2), pods[0].Metrics.RequestRunningNum)
				assert.Equal(t, 1.5, pods[0].Metrics.TPOT)
				assert.Equal(t, 0.2, pods[0].Metrics.TTFT)
			},
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.ListPods(c) })
}

func TestGetPod(t *testing.T) {
	podKey := types.NamespacedName{Namespace: "default", Name: "pod-1"}
	startTime := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	makePodInfo := func() *datastore.PodInfo {
		info := &datastore.PodInfo{
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-1",
					Namespace: "default",
					Labels:    map[string]string{"app": "llm"},
				},
				Spec: corev1.PodSpec{NodeName: "node-1"},
				Status: corev1.PodStatus{
					PodIP:     "10.0.0.42",
					Phase:     corev1.PodRunning,
					StartTime: &startTime,
				},
			},
			GPUCacheUsage:     0.5,
			RequestRunningNum: 1,
		}
		// Attach a model server so convertPodInfoToResponse exercises the
		// GetModelServersList loop body.
		info.AddModelServer(types.NamespacedName{Namespace: "default", Name: "llama2-server"})
		return info
	}

	cases := []handlerCase{
		{
			name:   "found",
			params: nsName("default", "pod-1"),
			setup: func(m *MockStore) {
				m.On("GetPodInfo", podKey).Return(makePodInfo())
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response PodResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Equal(t, "pod-1", response.Name)
				assert.Equal(t, "default", response.Namespace)
				require.NotNil(t, response.PodInfo)
				assert.Equal(t, "10.0.0.42", response.PodInfo.PodIP)
				assert.Equal(t, "node-1", response.PodInfo.NodeName)
				assert.Equal(t, "Running", response.PodInfo.Phase)
				assert.Equal(t, map[string]string{"app": "llm"}, response.PodInfo.Labels)
				assert.Equal(t, "2026-01-02T03:04:05Z", response.PodInfo.StartTime)
				assert.Equal(t, 0.5, response.Metrics.GPUCacheUsage)
				assert.Equal(t, []string{"default/llama2-server"}, response.ModelServers)
			},
		},
		{
			name:   "not found",
			params: nsName("default", "missing"),
			setup: func(m *MockStore) {
				m.On("GetPodInfo", types.NamespacedName{Namespace: "default", Name: "missing"}).Return(nil)
			},
			expectedStatus: http.StatusNotFound,
			check:          errorBodyCheck("Pod not found"),
		},
		{
			name:           "missing params",
			params:         gin.Params{},
			expectedStatus: http.StatusBadRequest,
			check:          errorBodyCheck("namespace and name parameters are required"),
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.GetPod(c) })
}

func TestListGateways(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
	}

	cases := []handlerCase{
		{
			name: "success",
			setup: func(m *MockStore) {
				m.On("GetAllGateways").Return([]*gatewayv1.Gateway{gw})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]GatewayResponse
				require.NoError(t, json.Unmarshal(body, &response))
				gateways := response["gateways"]
				require.Len(t, gateways, 1)
				assert.Equal(t, "my-gateway", gateways[0].Name)
				assert.Equal(t, "default", gateways[0].Namespace)
			},
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.ListGateways(c) })
}

func TestGetGateway(t *testing.T) {
	cases := []handlerCase{
		{
			name:   "found",
			params: nsName("default", "my-gateway"),
			setup: func(m *MockStore) {
				m.On("GetGateway", "default/my-gateway").Return(&gatewayv1.Gateway{
					ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response GatewayResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Equal(t, "my-gateway", response.Name)
				assert.Equal(t, "default", response.Namespace)
			},
		},
		{
			name:   "not found",
			params: nsName("default", "missing"),
			setup: func(m *MockStore) {
				m.On("GetGateway", "default/missing").Return(nil)
			},
			expectedStatus: http.StatusNotFound,
			check:          errorBodyCheck("Gateway not found"),
		},
		{
			name:           "missing params",
			params:         gin.Params{},
			expectedStatus: http.StatusBadRequest,
			check:          errorBodyCheck("namespace and name parameters are required"),
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.GetGateway(c) })
}

func TestListHTTPRoutes(t *testing.T) {
	cases := []handlerCase{
		{
			name: "success",
			setup: func(m *MockStore) {
				m.On("GetAllHTTPRoutes").Return([]*gatewayv1.HTTPRoute{
					{ObjectMeta: metav1.ObjectMeta{Name: "my-httproute", Namespace: "default"}},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]HTTPRouteResponse
				require.NoError(t, json.Unmarshal(body, &response))
				routes := response["httproutes"]
				require.Len(t, routes, 1)
				assert.Equal(t, "my-httproute", routes[0].Name)
				assert.Equal(t, "default", routes[0].Namespace)
			},
		},
		{
			// nil entries should be silently skipped.
			name: "skips nil entries",
			setup: func(m *MockStore) {
				valid := &gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "valid-route", Namespace: "default"},
				}
				m.On("GetAllHTTPRoutes").Return([]*gatewayv1.HTTPRoute{nil, valid, nil})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]HTTPRouteResponse
				require.NoError(t, json.Unmarshal(body, &response))
				require.Len(t, response["httproutes"], 1)
				assert.Equal(t, "valid-route", response["httproutes"][0].Name)
			},
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.ListHTTPRoutes(c) })
}

func TestGetHTTPRoute(t *testing.T) {
	cases := []handlerCase{
		{
			name:   "found",
			params: nsName("default", "my-httproute"),
			setup: func(m *MockStore) {
				m.On("GetHTTPRoute", "default/my-httproute").Return(&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "my-httproute", Namespace: "default"},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response HTTPRouteResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Equal(t, "my-httproute", response.Name)
				assert.Equal(t, "default", response.Namespace)
			},
		},
		{
			name:   "not found",
			params: nsName("default", "missing"),
			setup: func(m *MockStore) {
				m.On("GetHTTPRoute", "default/missing").Return(nil)
			},
			expectedStatus: http.StatusNotFound,
			check:          errorBodyCheck("HTTPRoute not found"),
		},
		{
			name:           "missing params",
			params:         gin.Params{},
			expectedStatus: http.StatusBadRequest,
			check:          errorBodyCheck("namespace and name parameters are required"),
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.GetHTTPRoute(c) })
}

func TestListInferencePools(t *testing.T) {
	cases := []handlerCase{
		{
			name: "success",
			setup: func(m *MockStore) {
				m.On("GetAllInferencePools").Return([]*inferencev1.InferencePool{
					{ObjectMeta: metav1.ObjectMeta{Name: "my-pool", Namespace: "default"}},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]InferencePoolResponse
				require.NoError(t, json.Unmarshal(body, &response))
				pools := response["inferencepools"]
				require.Len(t, pools, 1)
				assert.Equal(t, "my-pool", pools[0].Name)
				assert.Equal(t, "default", pools[0].Namespace)
			},
		},
		{
			// nil entries should be silently skipped.
			name: "skips nil entries",
			setup: func(m *MockStore) {
				valid := &inferencev1.InferencePool{
					ObjectMeta: metav1.ObjectMeta{Name: "valid-pool", Namespace: "default"},
				}
				m.On("GetAllInferencePools").Return([]*inferencev1.InferencePool{nil, valid, nil})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response map[string][]InferencePoolResponse
				require.NoError(t, json.Unmarshal(body, &response))
				require.Len(t, response["inferencepools"], 1)
				assert.Equal(t, "valid-pool", response["inferencepools"][0].Name)
			},
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.ListInferencePools(c) })
}

func TestGetInferencePool(t *testing.T) {
	cases := []handlerCase{
		{
			name:   "found",
			params: nsName("default", "my-pool"),
			setup: func(m *MockStore) {
				m.On("GetInferencePool", "default/my-pool").Return(&inferencev1.InferencePool{
					ObjectMeta: metav1.ObjectMeta{Name: "my-pool", Namespace: "default"},
				})
			},
			expectedStatus: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var response InferencePoolResponse
				require.NoError(t, json.Unmarshal(body, &response))
				assert.Equal(t, "my-pool", response.Name)
				assert.Equal(t, "default", response.Namespace)
			},
		},
		{
			name:   "not found",
			params: nsName("default", "missing"),
			setup: func(m *MockStore) {
				m.On("GetInferencePool", "default/missing").Return(nil)
			},
			expectedStatus: http.StatusNotFound,
			check:          errorBodyCheck("InferencePool not found"),
		},
		{
			name:           "missing params",
			params:         gin.Params{},
			expectedStatus: http.StatusBadRequest,
			check:          errorBodyCheck("namespace and name parameters are required"),
		},
	}
	runHandlerCases(t, cases, func(h *DebugHandler, c *gin.Context) { h.GetInferencePool(c) })
}
