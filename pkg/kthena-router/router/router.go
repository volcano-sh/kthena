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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/connectors"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/auth"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/ratelimit"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/tokenizer"
	"github.com/volcano-sh/kthena/pkg/kthena-router/handlers"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

const (
	// Context keys for gin context
	GatewayKey = "gatewayKey"
	PromptKey  = "promptKey" // store parsed ChatMessage, which will be reused
)

func getEnvBool(key string, fallback bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return fallback
}

// EnableFairnessScheduling enables the router's per-model user-fairness queue,
// which orders requests by each user's recent token usage. EnableSessionBoost
// enables session-aware boosting to maximize prefix cache reuse. The two are
// mutually exclusive scheduling strategies; enable at most one.
var EnableFairnessScheduling = getEnvBool("ENABLE_FAIRNESS_SCHEDULING", false)
var EnableSessionBoost = getEnvBool("ENABLE_SESSION_BOOST", false)

type Router struct {
	scheduler       scheduler.Scheduler
	authenticator   *auth.JWTAuthenticator
	store           datastore.Store
	loadRateLimiter *ratelimit.TokenRateLimiter
	accessLogger    accesslog.AccessLogger
	metrics         *metrics.Metrics
	tokenizer       tokenizer.Tokenizer

	// KV Connector management
	connectorFactory *connectors.Factory

	// Priority queue configuration
	queueTimeout     time.Duration
	tokenWeight      float64 // Weight for token-based priority in the fairness strategy (default 1.0)
	requestNumWeight float64 // Weight for request-count-based priority in the fairness strategy (default 0.0)
}

// ActiveRequestCount returns the number of requests currently being handled by the router.
func (r *Router) ActiveRequestCount() int64 {
	return r.metrics.ActiveRequestsCount()
}

func NewRouter(store datastore.Store, routerConfigPath string) *Router {
	// User fairness and session boost are mutually exclusive scheduling strategies.
	// Enabling both is a configuration error.
	if EnableFairnessScheduling && EnableSessionBoost {
		klog.Fatalf("ENABLE_FAIRNESS_SCHEDULING and ENABLE_SESSION_BOOST are mutually exclusive; enable only one")
	}

	// Create a unified rate limiter for all models
	loadRateLimiter := ratelimit.NewTokenRateLimiter()

	// Use global metrics instance
	metricsInstance := metrics.DefaultMetrics

	// Initialize tokenizer
	tokenizerInstance := tokenizer.NewSimpleEstimateTokenizer()

	store.RegisterCallback("ModelRoute", func(data datastore.EventData) {
		switch data.EventType {
		case datastore.EventAdd, datastore.EventUpdate:
			if data.ModelRoute == nil || data.ModelRoute.Spec.RateLimit == nil {
				return
			}
			klog.Infof("add or update rate limit for model %s", data.ModelName)

			// Configure the unified rate limiter for this model
			if err := loadRateLimiter.AddOrUpdateLimiter(data.ModelName, data.ModelRoute.Spec.RateLimit); err != nil {
				klog.Errorf("failed to configure rate limiter for model %s: %v", data.ModelName, err)
			}

		case datastore.EventDelete:
			klog.Infof("delete rate limit for model %s", data.ModelName)
			loadRateLimiter.DeleteLimiter(data.ModelName)
		}
	})

	routerConfig, err := conf.ParseRouterConfig(routerConfigPath)
	if err != nil {
		klog.Fatalf("failed to parse router config: %v", err)
	}

	// Initialize access logger with configuration from environment variables
	accessLogConfig := &accesslog.AccessLoggerConfig{
		Enabled: true,
		Format:  accesslog.FormatText,
		Output:  "stdout",
	}

	// Read access log configuration from environment variables
	if enabled := os.Getenv("ACCESS_LOG_ENABLED"); enabled != "" {
		if enabledBool, err := strconv.ParseBool(enabled); err == nil {
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

	accessLogger, err := accesslog.NewAccessLogger(accessLogConfig)
	if err != nil {
		klog.Fatalf("failed to create access logger: %v", err)
	}

	return &Router{
		store:            store,
		scheduler:        scheduler.NewScheduler(store, routerConfig),
		authenticator:    auth.NewJWTAuthenticator(routerConfig),
		loadRateLimiter:  loadRateLimiter,
		accessLogger:     accessLogger,
		metrics:          metricsInstance,
		tokenizer:        tokenizerInstance,
		connectorFactory: connectors.NewDefaultFactory(),
		queueTimeout:     parseQueueTimeout(),
		tokenWeight:      parseEnvFloat("FAIRNESS_PRIORITY_TOKEN_WEIGHT", 1.0),
		requestNumWeight: parseEnvFloat("FAIRNESS_PRIORITY_REQUEST_NUM_WEIGHT", 0.0),
	}
}

const defaultQueueTimeout = 60 * time.Second

func parseQueueTimeout() time.Duration {
	if s, ok := os.LookupEnv("FAIRNESS_QUEUE_TIMEOUT"); ok {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		klog.Warningf("Invalid FAIRNESS_QUEUE_TIMEOUT %q, using default %v", s, defaultQueueTimeout)
	}
	return defaultQueueTimeout
}

func parseEnvFloat(key string, fallback float64) float64 {
	if s, ok := os.LookupEnv(key); ok {
		if v, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 {
			return v
		}
		klog.Warningf("Invalid %s %q, using default %v", key, s, fallback)
	}
	return fallback
}

func (r *Router) calculateRequestPriority(userID, modelName string) float64 {
	priority, err := datastore.CalculateFairnessPriority(r.store, userID, modelName, r.tokenWeight, r.requestNumWeight)
	if err != nil {
		klog.Warningf("failed to calculate fairness priority for user=%s model=%s: %v", userID, modelName, err)
		return 0
	}
	return priority
}

type ModelRequest map[string]interface{}

func (r *Router) HandlerFunc() gin.HandlerFunc {
	return func(c *gin.Context) {
		r.metrics.IncActiveRequests()
		defer r.metrics.DecActiveRequests()

		// Handle /v1/models endpoint (OpenAI-compatible model listing)
		if c.Request.Method == http.MethodGet &&
			(c.Request.URL.Path == "/v1/models" || c.Request.URL.Path == "/models") {
			r.ListModels(c)
			return
		}

		// Step 1: Parse and validate request
		modelRequest, err := ParseModelRequest(c)
		if err != nil {
			accesslog.SetError(c, "request_parsing", err.Error())
			return
		}

		// step 2: Detection of rate limit
		modelName := modelRequest["model"].(string)

		// Set model name in access log
		accesslog.SetModelName(c, modelName)

		// Store model name in context for metrics middleware
		c.Set("model", modelName)

		// Create metrics recorder for this request
		path := c.Request.URL.Path
		metricsRecorder := metrics.NewRequestMetricsRecorder(r.metrics, modelName, path)

		// Increment downstream request count at request start
		r.metrics.IncActiveDownstreamRequests(modelName)
		defer func() {
			// Decrement downstream request count when request completes
			r.metrics.DecActiveDownstreamRequests(modelName)
			if metricsRecorder != nil {
				statusCode := strconv.Itoa(c.Writer.Status())
				reason := "successful_request"
				if r, exists := c.Get("finishReason"); exists {
					reason = r.(string)
				}
				metricsRecorder.Finish(statusCode, reason)
			}
		}()

		prompt, err := utils.ParsePrompt(modelRequest)
		if err != nil {
			accesslog.SetError(c, "prompt_parsing", "prompt not found")
			c.AbortWithStatusJSON(http.StatusNotFound, "prompt not found")
			c.Set("finishReason", "prompt_parsing")
			return
		}
		// Store parsed prompt to avoid re-parsing in doLoadbalance.
		c.Set(PromptKey, prompt)
		promptStr := utils.GetPromptString(prompt)

		// Calculate input tokens for metrics using tokenizer
		inputTokens, err := r.tokenizer.CalculateTokenNum(promptStr)
		if err != nil {
			klog.Errorf("failed to calculate token number: %v", err)
			inputTokens = len(promptStr) / 4 // fallback estimation
		}

		// Calculate and set input tokens for access log
		accesslog.SetTokenCounts(c, inputTokens, 0)

		// Mark end of request processing phase
		accesslog.MarkRequestProcessingEnd(c)

		// Record input tokens immediately
		metricsRecorder.RecordInputTokens(inputTokens)

		// Apply rate limiting using the unified rate limiter
		if err := r.loadRateLimiter.RateLimit(modelName, promptStr); err != nil {
			var errorMsg string
			var errorType string
			var tokenType string
			switch err.(type) {
			case *ratelimit.InputRateLimitExceededError:
				errorMsg = "input token rate limit exceeded"
				errorType = "input_rate_limit"
				tokenType = metrics.LimitTypeInputTokens
			case *ratelimit.OutputRateLimitExceededError:
				errorMsg = "output token rate limit exceeded"
				errorType = "output_rate_limit"
				tokenType = metrics.LimitTypeOutputTokens
			default:
				errorMsg = "token usage exceeds rate limit"
				errorType = "rate_limit"
				tokenType = metrics.LimitTypeRequests
			}
			accesslog.SetError(c, errorType, errorMsg)

			// Record rate limit exceeded
			metricsRecorder.RecordRateLimitExceeded(tokenType)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, errorMsg)
			c.Set("finishReason", "rate_limit")
			return
		}

		requestID := uuid.New().String()
		if c.Request.Header.Get("x-request-id") == "" {
			c.Request.Header.Set("x-request-id", requestID)
		}

		// Store metrics recorder in context for use in other functions
		c.Set("metricsRecorder", metricsRecorder)

		// step 3.1: direct load balancing when neither fairness scheduling nor
		// session boost is enabled.
		if !EnableFairnessScheduling && !EnableSessionBoost {
			_ = r.doLoadbalance(c, modelRequest)
			return
		}

		// step 3.2: queue scheduling. The queue orders requests by the active
		// strategy: per-user fairness or session boost (mutually exclusive).
		if err := r.handleFairnessScheduling(c, modelRequest, requestID, modelName); err != nil {
			accesslog.SetError(c, "scheduling", err.Error())
			c.Set("finishReason", "scheduling")
			return
		}
	}
}

func (r *Router) doLoadbalance(c *gin.Context, modelRequest ModelRequest) error {
	modelName := modelRequest["model"].(string)

	// Check if this is an InferencePool request from HTTPRoute
	var pods []*datastore.PodInfo
	var port int32
	var modelServerName types.NamespacedName
	var modelRoute *v1alpha1.ModelRoute
	var modelServer *v1alpha1.ModelServer

	// Get gateway key from context if available (set by Gateway listener)
	var gatewayKey string
	if key, exists := c.Get(GatewayKey); exists {
		if k, ok := key.(string); ok {
			gatewayKey = k
		}
	}
	if gatewayKey != "" {
		accesslog.SetGatewayAPIInfo(c, gatewayKey, "", "")
	}

	var isLora bool
	var err error
	// Try to match ModelRoute first
	modelServerName, isLora, modelRoute, err = r.store.MatchModelServer(modelName, c.Request, gatewayKey)
	if err != nil {
		accesslog.SetError(c, "model_server_matching", fmt.Sprintf("can't find corresponding model server: %v", err))
	}

	if err == nil && strings.HasPrefix(c.Request.URL.Path, "/v1/") {
		// Regular ModelServer request
		// step 3: Find pods and model server details
		klog.V(4).Infof("modelServer is %v, is_lora: %v", modelServerName, isLora)

		pods, modelServer, err = r.getPodsAndServer(modelServerName)
		if err != nil || len(pods) == 0 {
			klog.Errorf("failed to get pods and model server: %v, %v", modelServerName, err)
			accesslog.SetError(c, "pod_discovery", fmt.Sprintf("can't find model server: %v", modelServerName))
			c.AbortWithStatusJSON(http.StatusNotFound, fmt.Sprintf("can't find model server: %v", modelServerName))
			return fmt.Errorf("can't find model server: %v", modelServerName)
		}

		model := modelServer.Spec.Model
		if model != nil && !isLora {
			modelRequest["model"] = *model
		}

		port = modelServer.Spec.WorkloadPort.Port
	} else if matched, inferencePoolName := r.handleHTTPRoute(c, gatewayKey); matched {
		// If ModelRoute is not matched, try to match HTTPRoute

		// Get InferencePool from store
		inferencePoolKey := fmt.Sprintf("%s/%s", inferencePoolName.Namespace, inferencePoolName.Name)
		inferencePool := r.store.GetInferencePool(inferencePoolKey)
		if inferencePool == nil {
			klog.Errorf("failed to get inference pool: %v", inferencePoolName)
			accesslog.SetError(c, "inference_pool_discovery", fmt.Sprintf("can't find inference pool: %v", inferencePoolName))
			c.AbortWithStatusJSON(http.StatusNotFound, fmt.Sprintf("can't find inference pool: %v", inferencePoolName))
			return fmt.Errorf("can't find inference pool: %v", inferencePoolName)
		}

		// Get pods from InferencePool
		pods, err = r.store.GetPodsByInferencePool(inferencePoolName)
		if err != nil || len(pods) == 0 {
			klog.Errorf("failed to get pods for inference pool: %v, %v", inferencePoolName, err)
			accesslog.SetError(c, "pod_discovery", fmt.Sprintf("can't find pods for inference pool: %v", inferencePoolName))
			c.AbortWithStatusJSON(http.StatusNotFound, fmt.Sprintf("can't find pods for inference pool: %v", inferencePoolName))
			return fmt.Errorf("can't find pods for inference pool: %v", inferencePoolName)
		}

		// Get target port from InferencePool
		if len(inferencePool.Spec.TargetPorts) == 0 {
			klog.Errorf("inference pool %v has no target ports", inferencePoolName)
			accesslog.SetError(c, "port_discovery", fmt.Sprintf("inference pool %v has no target ports", inferencePoolName))
			c.AbortWithStatusJSON(http.StatusBadRequest, fmt.Sprintf("inference pool %v has no target ports", inferencePoolName))
			return fmt.Errorf("inference pool %v has no target ports", inferencePoolName)
		}
		// Use the first target port
		port = int32(inferencePool.Spec.TargetPorts[0].Number)

		klog.V(4).Infof("InferencePool is %v, pods count: %d, port: %d", inferencePoolName, len(pods), port)
	} else {
		accesslog.SetError(c, "route_not_found", "route not found")
		c.AbortWithStatusJSON(http.StatusNotFound, "route not found")
		return fmt.Errorf("route not found")
	}

	// Common scheduling logic for both ModelServer and InferencePool
	var prompt *common.ChatMessage
	if cached, exists := c.Get(PromptKey); exists {
		var ok bool
		if prompt, ok = cached.(*common.ChatMessage); !ok {
			accesslog.SetError(c, "prompt_parsing", "internal error: invalid prompt type")
			c.AbortWithStatusJSON(http.StatusInternalServerError, "internal error")
			return fmt.Errorf("invalid prompt type")
		}
	} else {
		accesslog.SetError(c, "prompt_parsing", "prompt not found")
		c.AbortWithStatusJSON(http.StatusNotFound, "prompt not found")
		return fmt.Errorf("prompt not found")
	}

	// Get metrics recorder from gin context
	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	// Get PDGroup if available (only for ModelServer)
	var pdGroup *v1alpha1.PDGroup
	if modelServer != nil && modelServer.Spec.WorkloadSelector != nil {
		pdGroup = modelServer.Spec.WorkloadSelector.PDGroup
	}

	sessionHeader := r.store.GetSessionIDHeader()
	var sessionID string
	if sessionHeader != "" {
		sessionID = c.Request.Header.Get(sessionHeader)
	}

	ctx := &framework.Context{
		Model:           modelName,
		Prompt:          prompt,
		SessionID:       sessionID,
		ModelServerName: modelServerName,
		PDGroup:         pdGroup,
		MetricsRecorder: metricsRecorder,
	}

	err = r.scheduler.Schedule(ctx, pods)
	if err != nil {
		accesslog.SetError(c, "scheduling", fmt.Sprintf("can't schedule to target pod: %v", err))
		c.AbortWithStatusJSON(http.StatusBadRequest, fmt.Sprintf("can't schedule to target pod: %v", err))
		return fmt.Errorf("can't schedule to target pod: %v", err)
	}

	// Set complete request routing information in access log
	modelServerFullName := fmt.Sprintf("%s/%s", modelServerName.Namespace, modelServerName.Name)
	modelRouteName := ""
	if modelRoute != nil {
		modelRouteName = fmt.Sprintf("%s/%s", modelRoute.Namespace, modelRoute.Name)
		// Set the model route name in context for upstream connections
		c.Set("modelRouteName", modelRouteName)
	}

	if len(ctx.BestPods) > 0 {
		selectedPod := ctx.BestPods[0].GetPodNamespacedName().Name
		accesslog.SetRequestRouting(c, modelRouteName, modelServerFullName, selectedPod)
	} else {
		// Set routing info even if no pod is selected (for error cases)
		accesslog.SetRequestRouting(c, modelRouteName, modelServerFullName, "")
	}

	req := c.Request
	if err := r.proxyModelEndpoint(c, req, ctx, modelRequest, port); err != nil {
		klog.Errorf("request failed reqID: %s: %v", c.Request.Header.Get("x-request-id"), err)
		accesslog.SetError(c, "proxy", "request processing failed")
		c.AbortWithStatusJSON(http.StatusInternalServerError, "request processing failed")
		return err
	}
	return nil
}

func ParseModelRequest(c *gin.Context) (ModelRequest, error) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, err)
		return nil, err
	}
	var modelRequest ModelRequest
	if err := json.Unmarshal(bodyBytes, &modelRequest); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, err)
		return nil, err
	}

	modelName, ok := modelRequest["model"].(string)
	if !ok || strings.TrimSpace(modelName) == "" {
		c.AbortWithStatusJSON(http.StatusNotFound, "model not found")
		return nil, fmt.Errorf("model not found")
	}
	klog.V(4).Infof("model name is %v", modelName)

	return modelRequest, nil
}

func (r *Router) getPodsAndServer(modelServerName types.NamespacedName) ([]*datastore.PodInfo, *v1alpha1.ModelServer, error) {
	pods, err := r.store.GetPodsByModelServer(modelServerName)
	if err != nil || len(pods) == 0 {
		return nil, nil, fmt.Errorf("can't find target pods of model server: %v, err: %v", modelServerName, err)
	}
	modelServer := r.store.GetModelServer(modelServerName)
	if modelServer == nil {
		return nil, nil, fmt.Errorf("can't find model server: %v", modelServerName)
	}
	return pods, modelServer, nil
}

// handleHTTPRoute handles HTTPRoute matching for non-/v1/ paths
// Returns true if HTTPRoute was matched and request is being handled, false otherwise
// Also returns the InferencePool NamespacedName if found
func (r *Router) handleHTTPRoute(c *gin.Context, gatewayKey string) (bool, types.NamespacedName) {
	matchResult, matched := r.findHTTPRouteMatch(c, gatewayKey)
	if !matched {
		return false, types.NamespacedName{}
	}

	// Record Gateway API match into access log (gatewayKey is already "namespace/name").
	httpRouteKey := fmt.Sprintf("%s/%s", matchResult.route.Namespace, matchResult.route.Name)
	accesslog.SetGatewayAPIInfo(c, gatewayKey, httpRouteKey, "")

	// Store the matched prefix in context for URL rewriting
	if matchResult.matchedPrefix != "" {
		c.Set("matchedPrefix", matchResult.matchedPrefix)
	}

	inferencePoolName, found := inferencePoolFromHTTPRouteRule(matchResult.route, matchResult.rule)
	if !found {
		return false, types.NamespacedName{}
	}

	// Record InferencePool match into access log.
	inferencePoolKey := fmt.Sprintf("%s/%s", inferencePoolName.Namespace, inferencePoolName.Name)
	accesslog.SetGatewayAPIInfo(c, "", "", inferencePoolKey)

	// Apply HTTPURLRewriteFilter from the same rule that matched the request.
	if matchResult.rule.Filters != nil {
		for _, filter := range matchResult.rule.Filters {
			if filter.Type == gatewayv1.HTTPRouteFilterURLRewrite && filter.URLRewrite != nil {
				r.applyURLRewrite(c, filter.URLRewrite)
			}
		}
	}

	return true, inferencePoolName
}

// applyURLRewrite applies HTTPURLRewriteFilter to the request
func (r *Router) applyURLRewrite(c *gin.Context, urlRewrite *gatewayv1.HTTPURLRewriteFilter) {
	// Apply hostname rewrite
	if urlRewrite.Hostname != nil {
		newHostname := string(*urlRewrite.Hostname)
		c.Request.Host = newHostname
		klog.V(4).Infof("Rewrote hostname to: %s", newHostname)
	}

	// Apply path rewrite
	if urlRewrite.Path != nil {
		originalPath := c.Request.URL.Path
		newPath := originalPath

		switch urlRewrite.Path.Type {
		case gatewayv1.FullPathHTTPPathModifier:
			// Replace the full path
			if urlRewrite.Path.ReplaceFullPath != nil {
				newPath = *urlRewrite.Path.ReplaceFullPath
				klog.V(4).Infof("Rewrote full path from %s to %s", originalPath, newPath)
			}

		case gatewayv1.PrefixMatchHTTPPathModifier:
			// Replace the matched prefix with the specified replacement
			if urlRewrite.Path.ReplacePrefixMatch != nil {
				// Get the matched prefix from context
				prefix, exists := c.Get("matchedPrefix")
				if !exists {
					klog.Errorf("matchedPrefix not found in context for path rewrite")
					break
				}
				matchedPrefix, ok := prefix.(string)
				if !ok || matchedPrefix == "" {
					klog.Errorf("matchedPrefix is not a valid string in context")
					break
				}
				// Replace the matched prefix
				replacement := *urlRewrite.Path.ReplacePrefixMatch
				newPath = replacement + strings.TrimPrefix(originalPath, matchedPrefix)
				klog.V(4).Infof("Rewrote path prefix from %s to %s (matched prefix: %s)", originalPath, newPath, matchedPrefix)
			}
		}

		// Update the request path
		c.Request.URL.Path = newPath
		// Also update the raw path to maintain consistency
		c.Request.URL.RawPath = ""
	}
}

func (r *Router) proxy(
	c *gin.Context,
	req *http.Request,
	ctx *framework.Context,
	stream bool,
	port int32,
	onUsage func(u handlers.OpenAIResponse),
) error {
	modelServerName := fmt.Sprintf("%s/%s", ctx.ModelServerName.Namespace, ctx.ModelServerName.Name)

	// Get model route name from context
	var modelRouteName string
	if routeName, exists := c.Get("modelRouteName"); exists {
		if name, ok := routeName.(string); ok {
			modelRouteName = name
		}
	}

	// Capture body bytes once so each retry attempt gets a fresh reader.
	// transport.RoundTrip drains req.Body on every call, so reusing the same
	// request across loop iterations sends an empty body to subsequent pods.
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to read request body: %w", err)
		}
		bodyBytes = b
	}

	for i := 0; i < len(ctx.BestPods); i++ {
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		pod := ctx.BestPods[i]
		podObj := pod.GetPod()
		podName := types.NamespacedName{Namespace: podObj.Namespace, Name: podObj.Name}

		// Track this request as in-flight to the chosen pod.
		r.store.IncrPodOnFlightRequests(podName)

		// Increment upstream request count with both modelServer and modelRoute
		r.metrics.IncActiveUpstreamRequests(modelServerName, modelRouteName)

		// Request dispatched to the pod.
		err := proxyRequest(c, req, podObj.Status.PodIP, port, stream, onUsage)

		// Decrement upstream request count when request completes
		r.metrics.DecActiveUpstreamRequests(modelServerName, modelRouteName)

		// Request is complete (success or failure) — decrement on-flight counter.
		r.store.DecrPodOnFlightRequests(podName)

		if err != nil {
			klog.Errorf(" pod request error: %v", err)
			if c.Writer.Written() {
				return err
			}
			continue
		}
		// record in prefix cache
		r.scheduler.RunPostHooks(ctx, i)
		return nil
	}
	c.AbortWithStatusJSON(http.StatusNotFound, "request to all pods failed")
	return fmt.Errorf("request to all pods failed")
}

func (r *Router) proxyModelEndpoint(
	c *gin.Context,
	req *http.Request,
	ctx *framework.Context,
	modelRequest ModelRequest,
	port int32,
) error {
	// Mark start of upstream processing
	accesslog.MarkUpstreamStart(c)

	// Get metrics recorder from context
	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	// proxy to pd aggregated pod
	if ctx.BestPods != nil {
		// build request
		decodeRequest := connectors.BuildDecodeRequest(c, req, modelRequest)
		stream := isStreaming(modelRequest)
		modelName := ctx.Model
		userID := c.GetString(common.UserIdKey)
		err := r.proxy(c, decodeRequest, ctx, stream, port, func(resp handlers.OpenAIResponse) {
			if resp.Usage.TotalTokens <= 0 {
				return
			}
			// Record output tokens for rate limiting
			if r.loadRateLimiter != nil {
				r.loadRateLimiter.RecordOutputTokens(modelName, resp.Usage.CompletionTokens)
			}
			// Update access log with output tokens
			if accessCtx := accesslog.GetAccessLogContext(c); accessCtx != nil {
				accessCtx.SetTokenCounts(accessCtx.InputTokens, resp.Usage.CompletionTokens)
			}

			// Record output token metrics
			if metricsRecorder != nil {
				// Record output tokens
				metricsRecorder.RecordOutputTokens(resp.Usage.CompletionTokens)
			}
			if userID == "" || modelName == "" {
				return
			}
			_ = r.store.UpdateTokenCount(userID, modelName, float64(resp.Usage.PromptTokens), float64(resp.Usage.CompletionTokens))
		})

		// Mark end of upstream processing
		accesslog.MarkUpstreamEnd(c)
		return err
	}

	// Get appropriate connector for this model server
	kvConnector, err := r.getKVConnector(ctx.ModelServerName)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, fmt.Sprintf("failed to get KV connector: %v", err))
		return fmt.Errorf("failed to get KV connector: %w", err)
	}

	// PD disaggregated mode - use KV connector
	return r.proxyToPDDisaggregated(c, req, ctx, kvConnector, modelRequest, port)
}

func (r *Router) GetModelServer(modelName string, req *http.Request) (*v1alpha1.ModelServer, error) {
	modelServerName, isLora, _, err := r.store.MatchModelServer(modelName, req, "")
	if err != nil {
		return nil, fmt.Errorf("can't find corresponding model server: %v", err)
	}
	klog.V(4).Infof("modelServer is %v, is_lora: %v", modelServerName, isLora)

	pods, modelServer, err := r.getPodsAndServer(modelServerName)
	if err != nil || len(pods) == 0 {
		klog.Errorf("failed to get pods and model server: %v, %v", modelServerName, err)
		return nil, fmt.Errorf("can't find model server: %v", modelServerName)
	}

	return modelServer, nil
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelsResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// ListModels implements the OpenAI-compatible GET /v1/models endpoint.
// It returns all model names registered via ModelRoutes.
func (r *Router) ListModels(c *gin.Context) {
	modelNames := r.store.GetModelNames()

	data := make([]modelObject, 0, len(modelNames))
	for _, name := range modelNames {
		data = append(data, modelObject{
			ID:      name,
			Object:  "model",
			Created: 0,
			OwnedBy: "kthena",
		})
	}

	c.JSON(http.StatusOK, modelsResponse{
		Object: "list",
		Data:   data,
	})
}

func (r *Router) Auth() gin.HandlerFunc {
	return r.authenticator.Authenticate()
}

func (r *Router) AccessLog() gin.HandlerFunc {
	return accesslog.AccessLogMiddleware(r.accessLogger)
}

// proxyRequest proxies the request to the model server pods, returns response to downstream.
func proxyRequest(
	c *gin.Context,
	req *http.Request,
	podIP string,
	port int32,
	stream bool,
	onUsage func(u handlers.OpenAIResponse),
) error {
	resp, err := doRequest(req, podIP, port)
	if err != nil {
		return fmt.Errorf("decode request error: %w", err)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			c.Header(k, v)
		}
	}
	defer resp.Body.Close()

	c.Status(resp.StatusCode)

	if stream {
		// If the request is a streaming request, we need to stream the response body.
		// Stream response: read and forward each event (line) one by one, and parse usage if present
		c.Status(resp.StatusCode)
		reader := bufio.NewReader(resp.Body)
		var streamErr error
		c.Stream(func(w io.Writer) bool {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				// Try to parse usage from this line, assuming it's a data line
				parsed := handlers.ParseStreamRespForUsage(string(line))
				if parsed.Usage.CompletionTokens > 0 {
					klog.V(4).Infof("Parsed usage: %+v", parsed.Usage)

					// Always call onUsage callback to record output tokens
					if onUsage != nil {
						onUsage(parsed)
					}

					// The token usage is set by router, so remove it before sending to downstream
					if v, ok := c.Get(common.TokenUsageKey); ok && v.(bool) {
						return true
					}
				}
				// Forward to downstream
				_, _ = w.Write(line)
			}
			if err != nil {
				if err != io.EOF {
					klog.Errorf("error reading stream body: %v", err)
					streamErr = err
				}
				return false
			}
			return true
		})
		return streamErr
	} else {
		// Non-stream: efficiently stream response while capturing for parsing
		var buf bytes.Buffer
		ttee := io.TeeReader(resp.Body, &buf)

		_, err := io.Copy(c.Writer, ttee)
		if err != nil {
			klog.Errorf("copy response to downstream failed: %v", err)
			return err
		}

		// Parse usage if present
		parsed, _ := handlers.ParseOpenAIResponseBody(buf.Bytes())
		if parsed != nil && parsed.Usage.CompletionTokens > 0 {
			klog.V(4).Infof("Parsed usage: %+v", parsed.Usage)
			if onUsage != nil {
				onUsage(*parsed)
			}
		}
	}

	return nil
}

func doRequest(
	req *http.Request,
	podIP string,
	port int32,
) (*http.Response, error) {
	// step 1: change request URL to prefill pod URL.
	req.URL.Host = net.JoinHostPort(podIP, strconv.Itoa(int(port)))

	// step 2: use http.Transport to do request to prefill pod.
	transport := http.DefaultTransport
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("http resp error, http code is %d", resp.StatusCode)
	}
	return resp, nil
}

// isStreaming checks if the given model request has streaming enabled
func isStreaming(modelRequest ModelRequest) bool {
	if v, ok := modelRequest["stream"]; ok {
		if stream, isBool := v.(bool); isBool && stream {
			return true
		}
	}
	return false
}

// getKVConnector gets the appropriate KV connector for a model server
func (r *Router) getKVConnector(modelServerName types.NamespacedName) (connectors.KVConnector, error) {
	modelServer := r.store.GetModelServer(modelServerName)
	if modelServer == nil {
		return nil, fmt.Errorf("model server %s not found", modelServerName)
	}

	// Determine connector type from ModelServer CRD.
	// If kvConnector is explicitly set, use it; otherwise infer from inferenceEngine.
	connectorType := v1alpha1.ConnectorTypeHTTP
	if modelServer.Spec.KVConnector != nil && modelServer.Spec.KVConnector.Type != "" {
		connectorType = modelServer.Spec.KVConnector.Type
	} else if modelServer.Spec.InferenceEngine == v1alpha1.SGLang {
		connectorType = connectors.ConnectorTypeSGLang
	}

	connector := r.connectorFactory.GetConnector(connectorType)
	if connector == nil {
		return nil, fmt.Errorf("failed to get connector %s", connectorType)
	}

	return connector, nil
}

// proxyToPDDisaggregated handles PD disaggregated routing using KV connectors
func (r *Router) proxyToPDDisaggregated(
	c *gin.Context,
	req *http.Request,
	ctx *framework.Context,
	kvConnector connectors.KVConnector,
	modelRequest ModelRequest,
	port int32,
) error {
	// Get metrics recorder from context
	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	modelServerName := fmt.Sprintf("%s/%s", ctx.ModelServerName.Namespace, ctx.ModelServerName.Name)

	// Get model route name from context
	var modelRouteName string
	if routeName, exists := c.Get("modelRouteName"); exists {
		if name, ok := routeName.(string); ok {
			modelRouteName = name
		}
	}

	// Set upstream connection info in metrics recorder
	if metricsRecorder != nil {
		metricsRecorder.SetUpstreamConnectionInfo(modelServerName, modelRouteName)
	}

	// Try multiple prefill/decode pairs
	maxRetry := len(ctx.DecodePods)
	if len(ctx.PrefillPods) < maxRetry {
		maxRetry = len(ctx.PrefillPods)
	}

	for i := 0; i < maxRetry; i++ {
		if ctx.PrefillPods[i] == nil || ctx.DecodePods[i] == nil {
			continue
		}
		prefillPod := ctx.PrefillPods[i].GetPod()
		decodePod := ctx.DecodePods[i].GetPod()

		// Build addresses for prefill and decode pods
		prefillAddr := net.JoinHostPort(prefillPod.Status.PodIP, strconv.Itoa(int(port)))
		decodeAddr := net.JoinHostPort(decodePod.Status.PodIP, strconv.Itoa(int(port)))

		klog.V(4).Infof("Attempting PD disaggregated request: prefill=%s, decode=%s", prefillAddr, decodeAddr)

		// Build on-flight hooks so the connector can update the per-pod counters
		// at the precise point each phase starts and ends.
		prefillPodName := types.NamespacedName{Namespace: prefillPod.Namespace, Name: prefillPod.Name}
		decodePodName := types.NamespacedName{Namespace: decodePod.Namespace, Name: decodePod.Name}
		hooks := &connectors.OnFlightHooks{
			IncrPrefill: func() { r.store.IncrPodOnFlightRequests(prefillPodName) },
			DecrPrefill: func() { r.store.DecrPodOnFlightRequests(prefillPodName) },
			IncrDecode:  func() { r.store.IncrPodOnFlightRequests(decodePodName) },
			DecrDecode:  func() { r.store.DecrPodOnFlightRequests(decodePodName) },
		}

		// Execute the PD disaggregated proxy operation
		outputTokens, err := kvConnector.Proxy(c, modelRequest, prefillAddr, decodeAddr, hooks)

		if err != nil {
			klog.Errorf("proxy failed for prefill pod %s, decode pod %s: %v",
				prefillPod.Name, decodePod.Name, err)
			continue
		}

		// Record output tokens for rate limiting
		if outputTokens > 0 && r.loadRateLimiter != nil {
			r.loadRateLimiter.RecordOutputTokens(ctx.Model, outputTokens)
		}

		// Record output token metrics
		if metricsRecorder != nil {
			metricsRecorder.RecordOutputTokens(outputTokens)
		}

		// Record successful operation in cache
		r.scheduler.RunPostHooks(ctx, i)

		klog.V(4).Infof("kv connector run successful for prefill pod %s, decode pod %s, output tokens: %d",
			prefillPod.Name, decodePod.Name, outputTokens)

		return nil
	}

	c.AbortWithStatusJSON(http.StatusInternalServerError, "all prefill/decode attempts failed")
	return fmt.Errorf("all prefill/decode attempts failed")
}

// handleFairnessScheduling handles the fairness scheduling flow for requests.
func (r *Router) handleFairnessScheduling(c *gin.Context, modelRequest ModelRequest, requestID string, modelName string) error {
	// Extract session ID from HTTP header for multi-turn conversation tracking.
	sessionHeader := r.store.GetSessionIDHeader()
	var sessionID string
	if sessionHeader != "" {
		sessionID = c.Request.Header.Get(sessionHeader)
	}
	// Use the request ID from header if available, otherwise fall back to the generated one
	if headerReqID := c.Request.Header.Get("X-Request-ID"); headerReqID != "" {
		requestID = headerReqID
	}

	var userId string
	if userIdVal, ok := c.Get(common.UserIdKey); ok {
		if s, ok := userIdVal.(string); ok {
			userId = s
		}
	}
	if userId == "" {
		klog.Warningf("user ID not found in request %s", requestID)
	}

	klog.V(4).Infof("[FairnessScheduling] incoming request: reqID=%s user=%s model=%s",
		requestID, userId, modelName)

	// Create request-scoped context that unifies client disconnect and server timeout
	reqCtx, cancel := context.WithTimeout(c.Request.Context(), r.queueTimeout)
	defer cancel()

	var pri float64
	if EnableSessionBoost {
		// In session-boost mode the queue orders by session boost, not per-user
		// priority, so skip the token-tracker priority computation entirely.
		pri = 0
	} else if userId != "" {
		pri = r.calculateRequestPriority(userId, modelName)
	} else {
		// Assign lowest priority to unauthenticated requests so they don't
		// starve authenticated users (lower value = higher priority).
		pri = math.MaxFloat64
	}
	queueReq := &datastore.Request{
		UserID:      userId,
		ModelName:   modelName,
		SessionID:   sessionID,
		Priority:    pri,
		RequestTime: time.Now(),
		NotifyChan:  make(chan struct{}),
		CancelCh:    reqCtx.Done(),
	}

	if err := r.store.Enqueue(queueReq); err != nil {
		klog.Errorf("[FairnessScheduling] failed to enqueue: reqID=%s sessionID=%s user=%s model=%s err=%v",
			requestID, sessionID, userId, modelName, err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, fmt.Sprintf("failed to enqueue request: %v", err))
		return fmt.Errorf("failed to enqueue request: %v", err)
	}

	select {
	case <-queueReq.NotifyChan:
		if queueReq.Release != nil {
			defer queueReq.Release()
		}
		klog.V(4).Infof("[FairnessScheduling] request dequeued: reqID=%s user=%s model=%s sessionBoost=%v waitTime=%v",
			requestID, userId, modelName, queueReq.SessionBoost, time.Since(queueReq.RequestTime))
		lbErr := r.doLoadbalance(c, modelRequest)

		// After a successful proxy, mark the session request as completed so follow-up
		// requests from the same session get priority boost for prefix cache. Skip on
		// failure: a failed request did not warm any backend prefix cache.
		if lbErr == nil && sessionID != "" {
			r.store.MarkSessionRequestCompleted(modelName, sessionID)
		}
		return nil
	case <-reqCtx.Done():
		if queueReq.Release != nil {
			queueReq.Release()
		}
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			klog.Errorf("[FairnessScheduling] request timed out in queue: reqID=%s sessionID=%s user=%s model=%s timeout=%v",
				requestID, sessionID, userId, modelName, r.queueTimeout)
			c.AbortWithStatusJSON(http.StatusGatewayTimeout, "Request processing timed out")
			return fmt.Errorf("request processing timed out in fairness queue")
		}
		klog.V(4).Infof("[FairnessScheduling] request cancelled (client disconnected): reqID=%s sessionID=%s user=%s model=%s",
			requestID, sessionID, userId, modelName)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, "Client disconnected while waiting in fairness queue")
		return fmt.Errorf("client disconnected while waiting in fairness queue")
	}
}
