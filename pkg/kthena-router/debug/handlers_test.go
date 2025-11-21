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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

func (m *MockStore) DeletePod(podName types.NamespacedName) error {
	args := m.Called(podName)
	return args.Error(0)
}

func (m *MockStore) MatchModelServer(modelName string, request *http.Request) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error) {
	args := m.Called(modelName, request)
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
func (m *MockStore) AddOrUpdateGateway(gateway interface{}) error {
	args := m.Called(gateway)
	return args.Error(0)
}

func (m *MockStore) DeleteGateway(key string) error {
	args := m.Called(key)
	return args.Error(0)
}

func (m *MockStore) GetGateway(key string) interface{} {
	args := m.Called(key)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0)
}

func (m *MockStore) GetGatewaysByNamespace(namespace string) []interface{} {
	args := m.Called(namespace)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]interface{})
}

func (m *MockStore) GetAllGateways() []interface{} {
	args := m.Called()
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).([]interface{})
}

// GatewayClass methods (using standard Gateway API)
func (m *MockStore) AddOrUpdateGatewayClass(gatewayClass interface{}) error {
	args := m.Called(gatewayClass)
	return args.Error(0)
}

func (m *MockStore) DeleteGatewayClass(key string) error {
	args := m.Called(key)
	return args.Error(0)
}

func (m *MockStore) GetGatewayClass(key string) interface{} {
	args := m.Called(key)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0)
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
