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
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/connectors"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	klog.InitFlags(nil)
	flag.Set("v", "4")
	flag.Parse()
	exitCode := m.Run()
	os.Exit(exitCode)
}

// setupTestRouter initializes a router and its dependencies for testing.
// It uses a mock HTTP server as the backend, following the community's recommendation
// to avoid hacky dependency injection.
func setupTestRouter(t *testing.T, backendHandler http.Handler) (*Router, datastore.Store, *httptest.Server) {
	gin.SetMode(gin.TestMode)

	backend := httptest.NewServer(backendHandler)
	store := datastore.New()
	router := NewRouter(store, "../scheduler/testdata/configmap.yaml")

	return router, store, backend
}

func TestRouter_HandleHTTPRoute_PathPrefix(t *testing.T) {
	pathType := gatewayv1.PathMatchPathPrefix
	kind := gatewayv1.Kind("Gateway")
	group := gatewayv1.Group("inference.networking.k8s.io")
	backendKind := gatewayv1.Kind("InferencePool")

	tests := []struct {
		name           string
		prefix         string
		path           string
		defaultType    bool
		defaultValue   bool
		expectedMatch  bool
		expectedPrefix string
	}{
		{
			name:           "root matches root",
			prefix:         "/",
			path:           "/",
			expectedMatch:  true,
			expectedPrefix: "/",
		},
		{
			name:           "root matches nested path",
			prefix:         "/",
			path:           "/foo/bar",
			expectedMatch:  true,
			expectedPrefix: "/",
		},
		{
			name:           "prefix matches exact path",
			prefix:         "/foo",
			path:           "/foo",
			expectedMatch:  true,
			expectedPrefix: "/foo",
		},
		{
			name:           "prefix matches path with trailing slash",
			prefix:         "/foo",
			path:           "/foo/",
			expectedMatch:  true,
			expectedPrefix: "/foo",
		},
		{
			name:           "prefix matches nested path element",
			prefix:         "/foo",
			path:           "/foo/bar",
			expectedMatch:  true,
			expectedPrefix: "/foo",
		},
		{
			name:           "trailing slash prefix matches exact path",
			prefix:         "/foo/",
			path:           "/foo",
			expectedMatch:  true,
			expectedPrefix: "/foo",
		},
		{
			name:           "trailing slash prefix matches nested path",
			prefix:         "/foo/",
			path:           "/foo/bar",
			expectedMatch:  true,
			expectedPrefix: "/foo",
		},
		{
			name:          "prefix does not match partial segment",
			prefix:        "/foo",
			path:          "/foobar",
			expectedMatch: false,
		},
		{
			name:          "prefix does not match partial nested segment",
			prefix:        "/foo",
			path:          "/foo-bar/baz",
			expectedMatch: false,
		},
		{
			name:          "prefix with more path elements does not match shorter path",
			prefix:        "/a/b/c",
			path:          "/abc",
			expectedMatch: false,
		},
		{
			name:          "prefix with one path element does not match nested path text",
			prefix:        "/abc",
			path:          "/a/b/c",
			expectedMatch: false,
		},
		{
			name:          "prefix matching is case sensitive",
			prefix:        "/foo",
			path:          "/Foo",
			expectedMatch: false,
		},
		{
			name:           "missing type defaults to path prefix",
			prefix:         "/foo",
			path:           "/foo/bar",
			defaultType:    true,
			expectedMatch:  true,
			expectedPrefix: "/foo",
		},
		{
			name:           "missing value defaults to root",
			path:           "/foo/bar",
			defaultValue:   true,
			expectedMatch:  true,
			expectedPrefix: "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := datastore.New()
			router := &Router{store: store}
			pathMatch := &gatewayv1.HTTPPathMatch{}
			if !tt.defaultType {
				pathMatch.Type = &pathType
			}
			if !tt.defaultValue {
				pathMatch.Value = &tt.prefix
			}
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: v1.ObjectMeta{Name: "route", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Name: "gw",
								Kind: &kind,
							},
						},
					},
					Rules: []gatewayv1.HTTPRouteRule{
						{
							Matches: []gatewayv1.HTTPRouteMatch{
								{
									Path: pathMatch,
								},
							},
							BackendRefs: []gatewayv1.HTTPBackendRef{
								{
									BackendRef: gatewayv1.BackendRef{
										BackendObjectReference: gatewayv1.BackendObjectReference{
											Group: &group,
											Kind:  &backendKind,
											Name:  "pool",
										},
									},
								},
							},
						},
					},
				},
			}
			assert.NoError(t, store.AddOrUpdateHTTPRoute(route))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodPost, tt.path, nil)

			matched, pool := router.handleHTTPRoute(c, "default/gw")
			assert.Equal(t, tt.expectedMatch, matched)
			if !tt.expectedMatch {
				return
			}
			assert.Equal(t, types.NamespacedName{Namespace: "default", Name: "pool"}, pool)
			prefix, exists := c.Get("matchedPrefix")
			assert.True(t, exists)
			assert.Equal(t, tt.expectedPrefix, prefix)
		})
	}
}

func TestRouter_HandlerFunc_AggregatedMode(t *testing.T) {
	// 1. Setup backend mock server
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		var reqBody ModelRequest
		json.Unmarshal(body, &reqBody)
		assert.Equal(t, "test-model-base", reqBody["model"]) // Model name overwritten
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"response-id"}`)
	})
	router, store, backend := setupTestRouter(t, backendHandler)
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
	// 1. Setup backend mock server
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
	router, store, backend := setupTestRouter(t, backendHandler)
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
	router, _, backend := setupTestRouter(t, nil)
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
	router, store, backend := setupTestRouter(t, backendHandler)
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

func TestParseModelRequestValidatesModelName(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "valid model",
			body:    `{"model": "test-model", "prompt": "hello"}`,
			wantErr: false,
		},
		{
			name:    "missing model",
			body:    `{"prompt": "hello"}`,
			wantErr: true,
		},
		{
			name:    "non-string model",
			body:    `{"model": 123, "prompt": "hello"}`,
			wantErr: true,
		},
		{
			name:    "empty model",
			body:    `{"model": "", "prompt": "hello"}`,
			wantErr: true,
		},
		{
			name:    "whitespace model",
			body:    `{"model": "  ", "prompt": "hello"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(tt.body))

			got, err := ParseModelRequest(c)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, got)
				assert.Equal(t, http.StatusNotFound, w.Code)
				assert.Contains(t, w.Body.String(), "model not found")
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, "test-model", got["model"])
		})
	}
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

// TestProxy_RetryBodyNotDrained verifies that when the first pod returns a non-2xx
// response, the retry attempt to the next pod carries the full request body.
// Before the fix, transport.RoundTrip drained the body on the first attempt, so
// every subsequent pod received an empty POST body.
func TestProxy_RetryBodyNotDrained(t *testing.T) {
	var receivedBodies []string

	// Single backend: returns 503 on the first call, 200 on the second.
	// Both pods in the test point to this same server so we can observe both attempts.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBodies = append(receivedBodies, string(body))
		if len(receivedBodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"retry-ok"}`)
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	store := datastore.New()
	router := NewRouter(store, "../scheduler/testdata/configmap.yaml")

	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: v1.ObjectMeta{Name: "ms-retry", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			Model:           func(s string) *string { return &s }("base-model"),
			WorkloadPort:    aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
			InferenceEngine: "vLLM",
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "pod-retry-1", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "pod-retry-2", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	modelRoute := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-retry", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "retry-model",
			Rules: []*aiv1alpha1.Rule{
				{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-retry"}}},
			},
		},
	}

	store.AddOrUpdateModelServer(modelServer, sets.New(
		types.NamespacedName{Name: "pod-retry-1", Namespace: "default"},
		types.NamespacedName{Name: "pod-retry-2", Namespace: "default"},
	))
	store.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdatePod(pod2, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute)

	// Give pod-2 a non-zero waiting count so pod-1 scores higher and is always
	// BestPods[0]. This guarantees the 503 is hit first, forcing a retry to pod-2.
	podInfo2 := store.GetPodInfo(types.NamespacedName{Name: "pod-retry-2", Namespace: "default"})
	assert.NotNil(t, podInfo2)
	podInfo2.RequestWaitingNum = 1

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	reqBody := `{"model": "retry-model", "prompt": "test prompt for retry path"}`
	c.Request, _ = http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	router.HandlerFunc()(c)

	assert.Equal(t, http.StatusOK, w.Code)
	if assert.Len(t, receivedBodies, 2, "expected exactly 2 backend attempts (first 503, then retry)") {
		// The router rewrites model name to the ModelServer's base model before dispatch,
		// so check for the prompt which passes through unchanged.
		assert.Contains(t, receivedBodies[0], "test prompt for retry path", "first attempt body was missing")
		// Regression assertion: before the fix, transport.RoundTrip drained the body on the
		// first attempt, so this would be an empty string.
		assert.Contains(t, receivedBodies[1], "test prompt for retry path", "retry attempt sent empty body (body reuse regression)")
	}
}

func TestRouter_HandlerFunc_ListModels(t *testing.T) {
	router, store, backend := setupTestRouter(t, nil)
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	backendIP := backendURL.Hostname()
	backendPort, _ := strconv.Atoi(backendURL.Port())

	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: v1.ObjectMeta{Name: "ms-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			Model:        func(s string) *string { return &s }("base-model"),
			WorkloadPort: aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "pod-1", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: backendIP, Phase: corev1.PodRunning},
	}
	modelRoute1 := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-1", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "model-alpha",
			Rules: []*aiv1alpha1.Rule{
				{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-1"}}},
			},
		},
	}
	modelRoute2 := &aiv1alpha1.ModelRoute{
		ObjectMeta: v1.ObjectMeta{Name: "mr-2", Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "model-beta",
			Rules: []*aiv1alpha1.Rule{
				{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-1"}}},
			},
		},
	}

	store.AddOrUpdateModelServer(modelServer, sets.New(types.NamespacedName{Name: "pod-1", Namespace: "default"}))
	store.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{modelServer})
	store.AddOrUpdateModelRoute(modelRoute1)
	store.AddOrUpdateModelRoute(modelRoute2)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/v1/models", nil)

	router.HandlerFunc()(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "list", resp.Object)
	assert.Len(t, resp.Data, 2)
	assert.Equal(t, "model-alpha", resp.Data[0].ID)
	assert.Equal(t, "model-beta", resp.Data[1].ID)
	assert.Equal(t, "model", resp.Data[0].Object)
	assert.Equal(t, "kthena", resp.Data[0].OwnedBy)
}

func TestRouter_HandlerFunc_ListModels_Empty(t *testing.T) {
	router, _, backend := setupTestRouter(t, nil)
	defer backend.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/v1/models", nil)

	router.HandlerFunc()(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Object string        `json:"object"`
		Data   []interface{} `json:"data"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "list", resp.Object)
	assert.Empty(t, resp.Data)
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
