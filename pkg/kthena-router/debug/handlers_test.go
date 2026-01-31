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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

// MockStore implements the datastore.Store interface for testing
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

// Debug interface methods
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

// Gateway methods (using standard Gateway API)
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

func (m *MockStore) SetListenerStatus(gatewayKey, listenerName string, err error) {
	m.Called(gatewayKey, listenerName, err)
}

func (m *MockStore) GetListenerStatus(gatewayKey, listenerName string) error {
	args := m.Called(gatewayKey, listenerName)
	return args.Error(0)
}

func (m *MockStore) RemoveListenerStatus(gatewayKey, listenerName string) {
	m.Called(gatewayKey, listenerName)
}

func TestListModelRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	// Mock data
	modelRoutes := map[string]*aiv1alpha1.ModelRoute{
		"default/llama2-route": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama2-route",
				Namespace: "default",
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName:    "llama2-7b",
				LoraAdapters: []string{"lora-adapter-1", "lora-adapter-2"},
			},
		},
	}

	mockStore.On("GetAllModelRoutes").Return(modelRoutes)

	// Create request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest("GET", "/debug/config_dump/modelroutes", nil)
	c.Request = req

	// Call handler
	handler.ListModelRoutes(c)

	// Check response
	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string][]ModelRouteResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	routes := response["modelroutes"]
	assert.Len(t, routes, 1)
	assert.Equal(t, "llama2-route", routes[0].Name)
	assert.Equal(t, "default", routes[0].Namespace)
	assert.Equal(t, "llama2-7b", routes[0].Spec.ModelName)

	mockStore.AssertExpectations(t)
}

func TestGetModelRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockStore := &MockStore{}
	handler := NewDebugHandler(mockStore)

	// Mock data
	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llama2-route",
			Namespace: "default",
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName:    "llama2-7b",
			LoraAdapters: []string{"lora-adapter-1"},
		},
	}

	mockStore.On("GetModelRoute", "default/llama2-route").Return(modelRoute)

	// Create request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest("GET", "/debug/config_dump/namespaces/default/modelroutes/llama2-route", nil)
	c.Request = req
	c.Params = gin.Params{
		{Key: "namespace", Value: "default"},
		{Key: "name", Value: "llama2-route"},
	}

	// Call handler
	handler.GetModelRoute(c)

	// Check response
	assert.Equal(t, http.StatusOK, w.Code)

	var response ModelRouteResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	assert.Equal(t, "llama2-route", response.Name)
	assert.Equal(t, "default", response.Namespace)
	assert.Equal(t, "llama2-7b", response.Spec.ModelName)

	mockStore.AssertExpectations(t)
}

// TestDebugServerIntegration tests the debug server as a whole, including server startup and endpoint accessibility
func TestDebugServerIntegration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Find an available port
	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	debugPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	store := datastore.New()
	handler := NewDebugHandler(store)

	// Setup debug server routes (mimicking startDebugServer logic)
	engine := gin.New()
	engine.Use(gin.Recovery())
	debugGroup := engine.Group("/debug/config_dump")
	{
		// List resources
		debugGroup.GET("/modelroutes", handler.ListModelRoutes)
		debugGroup.GET("/modelservers", handler.ListModelServers)
		debugGroup.GET("/pods", handler.ListPods)
		debugGroup.GET("/gateways", handler.ListGateways)
		debugGroup.GET("/httproutes", handler.ListHTTPRoutes)
		debugGroup.GET("/inferencepools", handler.ListInferencePools)

		// Get specific resources
		debugGroup.GET("/namespaces/:namespace/modelroutes/:name", handler.GetModelRoute)
		debugGroup.GET("/namespaces/:namespace/modelservers/:name", handler.GetModelServer)
		debugGroup.GET("/namespaces/:namespace/pods/:name", handler.GetPod)
		debugGroup.GET("/namespaces/:namespace/gateways/:name", handler.GetGateway)
		debugGroup.GET("/namespaces/:namespace/httproutes/:name", handler.GetHTTPRoute)
		debugGroup.GET("/namespaces/:namespace/inferencepools/:name", handler.GetInferencePool)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf("localhost:%d", debugPort),
		Handler: engine.Handler(),
	}

	// Start server in goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Errorf("Debug server listen failed: %v", err)
		}
	}()

	// Wait for server to start by retrying connection
	expectedAddr := fmt.Sprintf("localhost:%d", debugPort)
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", expectedAddr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 5*time.Second, 100*time.Millisecond, "Debug server should be listening on %s", expectedAddr)

	// Test that debug endpoints are accessible
	url := fmt.Sprintf("http://localhost:%d/debug/config_dump/modelroutes", debugPort)
	var resp *http.Response
	require.Eventually(t, func() bool {
		var err error
		resp, err = http.Get(url)
		if err != nil {
			return false
		}
		// Close response body immediately in each attempt
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 100*time.Millisecond, "Debug endpoint should be accessible at %s", url)

	// Verify response status (resp is from the last successful attempt)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Debug endpoint should return 200 OK")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Logf("Warning: Server shutdown failed: %v", err)
	}

	// Wait for server to shut down by verifying the endpoint becomes unavailable
	require.Eventually(t, func() bool {
		_, err := http.Get(url)
		return err != nil // Server should be closed, so connection should fail
	}, 2*time.Second, 50*time.Millisecond, "Debug server should shut down after shutdown")
}
