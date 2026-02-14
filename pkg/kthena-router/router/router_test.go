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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/connectors"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	klog.InitFlags(nil)
	// Set klog verbosity level to 4
	flag.Set("v", "4")
	flag.Parse() // Parse flags to apply the klog level
	routerConfig, _ := conf.ParseRouterConfig("../scheduler/testdata/configmap.yaml")
	patch1 := gomonkey.ApplyFunc(conf.ParseRouterConfig, func(configMapPath string) (*conf.RouterConfiguration, error) {
		return routerConfig, nil
	})
	defer patch1.Reset()

	pluginsWeight, plugins, pluginConfig, _ := conf.LoadSchedulerConfig(&routerConfig.Scheduler)
	patch2 := gomonkey.ApplyFunc(conf.LoadSchedulerConfig, func() (map[string]int, []string, map[string]runtime.RawExtension, error) {
		return pluginsWeight, plugins, pluginConfig, nil
	})
	defer patch2.Reset()

	// Run the tests
	exitCode := m.Run()
	// Exit with the appropriate code
	os.Exit(exitCode)
}

func buildPodInfo(name string, ip string) *datastore.PodInfo {
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: name,
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}

	return &datastore.PodInfo{
		Pod: pod,
	}
}

func TestProxyModelEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest("POST", "/", nil)
	modelReq := ModelRequest{"model": "test"}
	r := NewRouter(datastore.New(), "testdata/comfigmap.yaml")
	hookPatch := gomonkey.ApplyMethod(r.scheduler, "RunPostHooks", func(s scheduler.Scheduler, ctx *framework.Context, index int) {})
	defer hookPatch.Reset()

	tests := []struct {
		name       string
		ctx        *framework.Context
		proxyPatch func() *gomonkey.Patches
		wantErr    error
	}{
		{
			name: "BestPods are set, aggregated mode success",
			ctx: &framework.Context{
				Model:    "test",
				Prompt:   common.ChatMessage{Text: "test"},
				BestPods: []*datastore.PodInfo{buildPodInfo("decode1", "1.1.1.1")},
			},
			proxyPatch: func() *gomonkey.Patches {
				patches := gomonkey.ApplyFunc(connectors.BuildDecodeRequest, func(c *gin.Context, req *http.Request, modelRequest ModelRequest) *http.Request {
					return req
				})
				patches.ApplyFunc(isStreaming, func(modelRequest ModelRequest) bool {
					return false
				})
				patches.ApplyFunc(proxyRequest, func(c *gin.Context, req *http.Request, podIP string, port int32, stream bool) error {
					return nil
				})
				return patches
			},
			wantErr: nil,
		},
		{
			name: "BestPods proxy returns error",
			ctx: &framework.Context{
				Model:    "test",
				Prompt:   common.ChatMessage{Text: "test"},
				BestPods: []*datastore.PodInfo{buildPodInfo("decode1", "1.1.1.1")},
			},
			proxyPatch: func() *gomonkey.Patches {
				patches := gomonkey.ApplyFunc(connectors.BuildDecodeRequest, func(c *gin.Context, req *http.Request, modelRequest ModelRequest) *http.Request {
					return req
				})
				patches.ApplyFunc(isStreaming, func(modelRequest ModelRequest) bool {
					return false
				})
				patches.ApplyFunc(proxyRequest, func(c *gin.Context, req *http.Request, podIP string, port int32, stream bool) error {
					return errors.New("proxy error")
				})
				return patches
			},
			wantErr: errors.New("request to all pods failed"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var patch *gomonkey.Patches
			if tt.proxyPatch != nil {
				patch = tt.proxyPatch()
				defer patch.Reset()
			}
			err := r.proxyModelEndpoint(c, req, tt.ctx, modelReq, int32(8080))
			if tt.wantErr != nil {
				assert.Error(t, err)
				assert.Equal(t, tt.wantErr.Error(), err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// setupTestRouter initializes a router and its dependencies for testing.
func setupTestRouter(backendHandler http.Handler) (*Router, datastore.Store, *httptest.Server) {
	gin.SetMode(gin.TestMode)

	backend := httptest.NewServer(backendHandler)
	store := datastore.New()
	router := NewRouter(store, "")

	return router, store, backend
}

func TestRouter_HandlerFunc_AggregatedMode(t *testing.T) {
	// 1. Setup backend mock
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		var reqBody ModelRequest
		json.Unmarshal(body, &reqBody)
		assert.Equal(t, "test-model-base", reqBody["model"]) // Model name overwritten
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"response-id"}`)
	})
	router, store, backend := setupTestRouter(backendHandler)
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	// 2. Populate store
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: v1.ObjectMeta{Name: "ms-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			Model:           func(s string) *string { return &s }("test-model-base"),
			WorkloadPort:    aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
			InferenceEngine: "vLLM",
			// No WorkloadSelector means aggregated mode
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "test-model",
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{ModelServerName: "ms-1"},
					},
				},
			},
		},
	}

	store.AddOrUpdateModelServer(modelServer, sets.New(types.NamespacedName{Name: "pod-1", Namespace: "default"}))
	store.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute)

	// 3. Create request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	reqBody := `{"model": "test-model", "prompt": "hello"}`
	c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	// 4. Execute handler
	router.HandlerFunc()(c)

	// 5. Assertions
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"id":"response-id"`)
}

func TestRouter_HandlerFunc_DisaggregatedMode(t *testing.T) {
	// 1. Setup backend mock
	prefillReqs := 0
	decodeReqs := 0
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqBody ModelRequest
		json.Unmarshal(body, &reqBody)

		// Check if this is a prefill request (stream key removed) or decode request (stream key present)
		if _, hasStream := reqBody["stream"]; !hasStream {
			// Prefill request - stream key is deleted
			prefillReqs++
			assert.Equal(t, "test-model-base", reqBody["model"])
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"prefill-resp"}`)
		} else {
			// Decode request - stream key is present
			decodeReqs++
			assert.Equal(t, "test-model-base", reqBody["model"])
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `data: {"id":"decode-resp"}`)
		}
	})
	router, store, backend := setupTestRouter(backendHandler)
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	// 2. Populate store
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: v1.ObjectMeta{Name: "ms-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			Model:           func(s string) *string { return &s }("test-model-base"),
			WorkloadPort:    aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
			InferenceEngine: "vLLM",
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				PDGroup: &aiv1alpha1.PDGroup{
					GroupKey:      "group",
					DecodeLabels:  map[string]string{"app": "decode"},
					PrefillLabels: map[string]string{"app": "prefill"},
				},
			},
		},
	}
	decodePod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "decode-pod-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "decode", "group": "test-group"},
		},
		Status: corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	prefillPod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "prefill-pod-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "prefill", "group": "test-group"},
		},
		Status: corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}

	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "test-model",
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{ModelServerName: "ms-1"},
					},
				},
			},
		},
	}

	store.AddOrUpdateModelServer(modelServer, sets.New(
		types.NamespacedName{Name: "decode-pod-1", Namespace: "default"},
		types.NamespacedName{Name: "prefill-pod-1", Namespace: "default"},
	))
	store.AddOrUpdatePod(decodePod, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdatePod(prefillPod, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute)

	// 3. Create request
	w := connectors.CreateTestResponseRecorder()
	c, _ := gin.CreateTestContext(w)

	reqBody := `{"model": "test-model", "prompt": "hello", "stream": true}`
	c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	// 4. Execute handler
	router.HandlerFunc()(c)

	// 5. Assertions
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, prefillReqs)
	assert.Equal(t, 1, decodeReqs)
	assert.Contains(t, w.Body.String(), `data: {"id":"decode-resp"}`)
}

func TestRouter_HandlerFunc_ModelNotFound(t *testing.T) {
	router, _, backend := setupTestRouter(nil)
	defer backend.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	reqBody := `{"model": "non-existent-model", "prompt": "hello"}`
	c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	router.HandlerFunc()(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "route not found")
}

func TestRouter_HandlerFunc_ScheduleFailure(t *testing.T) {
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This should not be called
		t.Error("backend should not be called on schedule failure")
	})
	router, store, backend := setupTestRouter(backendHandler)
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	// 2. Populate store
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: v1.ObjectMeta{Name: "ms-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			Model:        func(s string) *string { return &s }("test-model-base"),
			WorkloadPort: aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "test-model",
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{ModelServerName: "ms-1"},
					},
				},
			},
		},
	}

	store.AddOrUpdateModelServer(modelServer, sets.New(types.NamespacedName{Name: "pod-1", Namespace: "default"}))
	store.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute)

	podInfo := store.GetPodInfo(types.NamespacedName{Name: "pod-1", Namespace: "default"})
	assert.NotNil(t, podInfo)
	podInfo.RequestWaitingNum = 20 // default max is 10, so this should be filtered out

	// 3. Create request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	reqBody := `{"model": "test-model", "prompt": "hello"}`
	c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	// 4. Execute handler
	router.HandlerFunc()(c)

	// 5. Assertions
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "can't schedule to target pod")
}

func TestRouter_HandlerFunc_AccessLogRoutingInfo(t *testing.T) {
	// 1. Setup backend mock
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"response-id"}`)
	})
	router, store, backend := setupTestRouter(backendHandler)
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	// 2. Populate store
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: v1.ObjectMeta{Name: "ms-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			Model:           func(s string) *string { return &s }("test-model-base"),
			WorkloadPort:    aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
			InferenceEngine: "vLLM",
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	// Create Gateway and add to store (required for ModelRoute with parentRefs to match)
	gateway := &gatewayv1.Gateway{
		ObjectMeta: v1.ObjectMeta{Name: "test-gateway", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}
	gatewayKey := "default/test-gateway"
	store.AddOrUpdateGateway(gateway)

	// Create ModelRoute with parentRefs pointing to the Gateway
	gatewayKind := gatewayv1.Kind("Gateway")
	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "test-model",
			ParentRefs: []gatewayv1.ParentReference{
				{
					Name:  gatewayv1.ObjectName("test-gateway"),
					Kind:  &gatewayKind,
					Group: func() *gatewayv1.Group { g := gatewayv1.Group("gateway.networking.k8s.io"); return &g }(),
				},
			},
			Rules: []*aiv1alpha1.Rule{
				{
					TargetModels: []*aiv1alpha1.TargetModel{
						{ModelServerName: "ms-1"},
					},
				},
			},
		},
	}

	store.AddOrUpdateModelServer(modelServer, sets.New(types.NamespacedName{Name: "pod-1", Namespace: "default"}))
	store.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute)

	// 3. Create request with Gateway API info in context
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	reqBody := `{"model": "test-model", "prompt": "hello"}`
	c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	// Set Gateway API info in gin.Context
	// We set GatewayKey because ModelRoute has parentRefs that match the Gateway
	c.Set(GatewayKey, gatewayKey)
	c.Set(HTTPRouteKey, types.NamespacedName{Namespace: "default", Name: "test-httproute"})
	c.Set(InferencePoolKey, types.NamespacedName{Namespace: "default", Name: "test-inferencepool"})

	// 4. Execute access log middleware first to create access log context
	router.AccessLog()(c)

	// 5. Execute handler
	router.HandlerFunc()(c)

	// 6. Verify request succeeded
	assert.Equal(t, http.StatusOK, w.Code, "Request should succeed to test routing info setting")

	// 7. Verify access log context has all routing information
	accessCtx := accesslog.GetAccessLogContext(c)
	require.NotNil(t, accessCtx, "Access log context should be set")

	// Verify AI-specific routing information
	assert.Equal(t, "test-model", accessCtx.ModelName, "ModelName should be set from request")
	assert.Equal(t, "default/mr-1", accessCtx.ModelRoute, "ModelRoute should be set from matched ModelRoute")
	assert.Equal(t, "default/ms-1", accessCtx.ModelServer, "ModelServer should be set from matched ModelServer")
	assert.Equal(t, "pod-1", accessCtx.SelectedPod, "SelectedPod should be set from scheduled pod")
	assert.Equal(t, accessCtx.RequestID, c.Request.Header.Get("x-request-id"), "RequestID should match request header")

	// Verify Gateway API information
	assert.Equal(t, gatewayKey, accessCtx.Gateway, "Gateway should be set from gin.Context GatewayKey")
	assert.Equal(t, "default/test-httproute", accessCtx.HTTPRoute, "HTTPRoute should be set from gin.Context")
	assert.Equal(t, "default/test-inferencepool", accessCtx.InferencePool, "InferencePool should be set from gin.Context")
}

func TestAccessLogConfigurationFromEnv(t *testing.T) {
	// Save original environment variables
	originalEnabled := os.Getenv("ACCESS_LOG_ENABLED")
	originalFormat := os.Getenv("ACCESS_LOG_FORMAT")
	originalOutput := os.Getenv("ACCESS_LOG_OUTPUT")

	// Clean up after test
	defer func() {
		os.Setenv("ACCESS_LOG_ENABLED", originalEnabled)
		os.Setenv("ACCESS_LOG_FORMAT", originalFormat)
		os.Setenv("ACCESS_LOG_OUTPUT", originalOutput)
	}()

	tests := []struct {
		name            string
		envEnabled      string
		envFormat       string
		envOutput       string
		expectedEnabled bool
		expectedFormat  accesslog.LogFormat
		expectedOutput  string
	}{
		{
			name:            "default configuration",
			envEnabled:      "",
			envFormat:       "",
			envOutput:       "",
			expectedEnabled: true,
			expectedFormat:  accesslog.FormatText,
			expectedOutput:  "stdout",
		},
		{
			name:            "JSON format configuration",
			envEnabled:      "true",
			envFormat:       "json",
			envOutput:       "stdout",
			expectedEnabled: true,
			expectedFormat:  accesslog.FormatJSON,
			expectedOutput:  "stdout",
		},
		{
			name:            "text format with file output",
			envEnabled:      "true",
			envFormat:       "text",
			envOutput:       "/tmp/access.log",
			expectedEnabled: true,
			expectedFormat:  accesslog.FormatText,
			expectedOutput:  "/tmp/access.log",
		},
		{
			name:            "disabled access log",
			envEnabled:      "false",
			envFormat:       "json",
			envOutput:       "stdout",
			expectedEnabled: false,
			expectedFormat:  accesslog.FormatJSON,
			expectedOutput:  "stdout",
		},
		{
			name:            "stderr output",
			envEnabled:      "true",
			envFormat:       "text",
			envOutput:       "stderr",
			expectedEnabled: true,
			expectedFormat:  accesslog.FormatText,
			expectedOutput:  "stderr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Handle file output case - create a temporary file
			envOutput := tt.envOutput
			var tempFile *os.File
			if tt.envOutput == "/tmp/access.log" {
				var err error
				tempFile, err = os.CreateTemp("", "access_log_test_*.log")
				assert.NoError(t, err)
				envOutput = tempFile.Name()
				defer func() {
					tempFile.Close()
					os.Remove(tempFile.Name())
				}()
			}

			// Set environment variables
			os.Setenv("ACCESS_LOG_ENABLED", tt.envEnabled)
			os.Setenv("ACCESS_LOG_FORMAT", tt.envFormat)
			os.Setenv("ACCESS_LOG_OUTPUT", envOutput)

			// Create access log configuration (simulating the logic in NewRouter)
			accessLogConfig := &accesslog.AccessLoggerConfig{
				Enabled: true,
				Format:  accesslog.FormatText,
				Output:  "stdout",
			}

			// Apply environment variable overrides
			if enabled := os.Getenv("ACCESS_LOG_ENABLED"); enabled != "" {
				if enabledBool, err := parseBool(enabled); err == nil {
					accessLogConfig.Enabled = enabledBool
				}
			}

			if format := os.Getenv("ACCESS_LOG_FORMAT"); format != "" {
				if format == "json" {
					accessLogConfig.Format = accesslog.FormatJSON
				} else if format == "text" {
					accessLogConfig.Format = accesslog.FormatText
				}
			}

			if output := os.Getenv("ACCESS_LOG_OUTPUT"); output != "" {
				accessLogConfig.Output = output
			}

			// Verify configuration
			assert.Equal(t, tt.expectedEnabled, accessLogConfig.Enabled)
			assert.Equal(t, tt.expectedFormat, accessLogConfig.Format)
			// For file output, we check that the config output matches the actual temp file path
			if tt.envOutput == "/tmp/access.log" {
				assert.Equal(t, envOutput, accessLogConfig.Output)
			} else {
				assert.Equal(t, tt.expectedOutput, accessLogConfig.Output)
			}

			// Test that logger can be created with this configuration
			logger, err := accesslog.NewAccessLogger(accessLogConfig)
			assert.NoError(t, err)
			assert.NotNil(t, logger)

			// Clean up
			logger.Close()
		})
	}
}

// Helper function to parse boolean (same logic as strconv.ParseBool but simpler for test)
func parseBool(str string) (bool, error) {
	switch str {
	case "1", "t", "T", "true", "TRUE", "True":
		return true, nil
	case "0", "f", "F", "false", "FALSE", "False":
		return false, nil
	}
	return false, &strconv.NumError{Func: "ParseBool", Num: str, Err: strconv.ErrSyntax}
}
