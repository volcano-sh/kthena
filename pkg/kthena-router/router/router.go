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
	"encoding/json"
	"fmt"
	"io"
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
)

func getEnvBool(key string, fallback bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return fallback
}

var EnableFairnessScheduling = getEnvBool("ENABLE_FAIRNESS_SCHEDULING", false)

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
}

func NewRouter(store datastore.Store, routerConfigPath string) *Router {
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
	}
}

type ModelRequest map[string]interface{}

func (r *Router) HandlerFunc() gin.HandlerFunc {
	return func(c *gin.Context) {
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
		}()

		prompt, err := utils.ParsePrompt(modelRequest)
		if err != nil {
			accesslog.SetError(c, "prompt_parsing", "prompt not found")
			c.AbortWithStatusJSON(http.StatusNotFound, "prompt not found")
			metricsRecorder.Finish(strconv.Itoa(http.StatusNotFound), "prompt_parsing")
			return
		}
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
			metricsRecorder.Finish(strconv.Itoa(http.StatusTooManyRequests), "rate_limit")
			return
		}

		requestID := uuid.New().String()
		if c.Request.Header.Get("x-request-id") == "" {
			c.Request.Header.Set("x-request-id", requestID)
		}

		// Store metrics recorder in context for use in other functions
		c.Set("metricsRecorder", metricsRecorder)

		// step 3.1: load balancing
		if !EnableFairnessScheduling {
			r.doLoadbalance(c, modelRequest)
			return
		}

		// step 3.2: load balancing for Fairness scheduling enabled case
		if err := r.handleFairnessScheduling(c, modelRequest, requestID, modelName); err != nil {
			accesslog.SetError(c, "scheduling", err.Error())
			metricsRecorder.Finish(strconv.Itoa(c.Writer.Status()), "scheduling")
			return
		}
	}
}

func (r *Router) doLoadbalance(c *gin.Context, modelRequest ModelRequest) {
	modelName := modelRequest["model"].(string)

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

	var isLora bool
	var err error
	// Try to match ModelRoute (this now handles both native ModelRoutes and translated HTTPRoutes)
	modelServerName, isLora, modelRoute, err = r.store.MatchModelServer(modelName, c.Request, gatewayKey)
	if err != nil {
		klog.Errorf("failed to match model server for model %s: %v", modelName, err)
		accesslog.SetError(c, "route_not_found", fmt.Sprintf("route not found: %v", err))
		c.AbortWithStatusJSON(http.StatusNotFound, "route not found")
		return
	}

	// step 3: Find pods and model server details
	klog.V(4).Infof("matched modelServer is %v, is_lora: %v", modelServerName, isLora)

	// If this route was generated from an HTTPRoute, handle potential URL rewrites
	if modelRoute != nil && modelRoute.Labels != nil && modelRoute.Labels["kthena.serving.volcano.sh/generated-from"] == "HTTPRoute" {
		hrKey := fmt.Sprintf("%s/%s", modelRoute.Namespace, modelRoute.Name)
		if hr := r.store.GetHTTPRoute(hrKey); hr != nil {
			r.applyURLRewriteFromHTTPRoute(c, hr)
		}
	}

	pods, modelServer, err = r.getPodsAndServer(modelServerName)
	if err != nil || len(pods) == 0 {
		klog.Errorf("failed to get pods and model server: %v, %v", modelServerName, err)
		accesslog.SetError(c, "pod_discovery", fmt.Sprintf("can't find model server: %v", modelServerName))
		c.AbortWithStatusJSON(http.StatusNotFound, fmt.Sprintf("can't find model server: %v", modelServerName))
		return
	}

	// If it's a regular /v1/ request, update the model name in request if needed
	if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
		model := modelServer.Spec.Model
		if model != nil && !isLora {
			modelRequest["model"] = *model
		}
	}

	port = modelServer.Spec.WorkloadPort.Port

	// Common scheduling logic
	prompt, err := utils.ParsePrompt(modelRequest)
	if err != nil {
		accesslog.SetError(c, "prompt_parsing", "prompt not found")
		c.AbortWithStatusJSON(http.StatusNotFound, "prompt not found")
		return
	}

	// Get metrics recorder from gin context
	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	// Get PDGroup if available
	var pdGroup *v1alpha1.PDGroup
	if modelServer != nil && modelServer.Spec.WorkloadSelector != nil {
		pdGroup = modelServer.Spec.WorkloadSelector.PDGroup
	}

	ctx := &framework.Context{
		Model:           modelName,
		Prompt:          prompt,
		ModelServerName: modelServerName,
		PDGroup:         pdGroup,
		MetricsRecorder: metricsRecorder,
	}

	err = r.scheduler.Schedule(ctx, pods)
	if err != nil {
		accesslog.SetError(c, "scheduling", fmt.Sprintf("can't schedule to target pod: %v", err))
		c.AbortWithStatusJSON(http.StatusBadRequest, fmt.Sprintf("can't schedule to target pod: %v", err))
		return
	}

	// Set complete request routing information in access log
	modelServerFullName := fmt.Sprintf("%s/%s", modelServerName.Namespace, modelServerName.Name)
	modelRouteName := ""
	if modelRoute != nil {
		modelRouteName = fmt.Sprintf("%s/%s", modelRoute.Namespace, modelRoute.Name)
		// Set the model route name in context for upstream connections
		c.Set("modelRouteName", modelRouteName)
	}

	if len(ctx.BestPods) > 0 && ctx.BestPods[0].Pod != nil {
		selectedPod := ctx.BestPods[0].Pod.Name
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
	}
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
	if !ok {
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

// applyURLRewriteFromHTTPRoute applies HTTPURLRewriteFilter from an HTTPRoute to the request
func (r *Router) applyURLRewriteFromHTTPRoute(c *gin.Context, hr *gatewayv1.HTTPRoute) {
	for _, rule := range hr.Spec.Rules {
		// Simple heuristic: apply rewrites from the first rule that has them
		// Ideally we should match the rule that was used for translation,
		// but since we only care about filters here, we just look for any rewrite filter.
		matchedPrefix := ""
		for _, match := range rule.Matches {
			if match.Path != nil && match.Path.Type != nil && *match.Path.Type == gatewayv1.PathMatchPathPrefix {
				if strings.HasPrefix(c.Request.URL.Path, *match.Path.Value) {
					matchedPrefix = *match.Path.Value
					break
				}
			}
		}

		if rule.Filters != nil {
			for _, filter := range rule.Filters {
				if filter.Type == gatewayv1.HTTPRouteFilterURLRewrite && filter.URLRewrite != nil {
					r.applyURLRewrite(c, filter.URLRewrite, matchedPrefix)
				}
			}
		}
	}
}

// applyURLRewrite applies HTTPURLRewriteFilter to the request
func (r *Router) applyURLRewrite(c *gin.Context, urlRewrite *gatewayv1.HTTPURLRewriteFilter, matchedPrefix string) {
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
				if matchedPrefix == "" {
					klog.Errorf("matchedPrefix not found for path rewrite")
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

	for i := 0; i < len(ctx.BestPods); i++ {
		// Increment upstream request count with both modelServer and modelRoute
		r.metrics.IncActiveUpstreamRequests(modelServerName, modelRouteName)

		// Request dispatched to the pod.
		err := proxyRequest(c, req, ctx.BestPods[i].Pod.Status.PodIP, port, stream, onUsage)

		// Decrement upstream request count when request completes
		r.metrics.DecActiveUpstreamRequests(modelServerName, modelRouteName)

		if err != nil {
			klog.Errorf(" pod request error: %v", err)
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
		decodeRequest := connectors.BuildDecodeRequest(c, req, modelRequest)
		// build request
		stream := isStreaming(modelRequest)
		userID := ""
		if v, ok := modelRequest["userId"].(string); ok {
			userID = v
		}
		modelName := ctx.Model
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
		c.Stream(func(w io.Writer) bool {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				// Try to parse usage from this line, assuming it's a data line
				parsed := handlers.ParseStreamRespForUsage(string(line))
				if parsed.Usage.CompletionTokens > 0 {
					klog.V(4).Infof("Parsed usage: %+v", parsed.Usage)

					// The token usage is set by router, so remove it before sending to downstream
					if v, ok := c.Get(common.TokenUsageKey); ok && v.(bool) {
						return true
					}
					if onUsage != nil {
						onUsage(parsed)
					}
				}
				// Forward to downstream
				_, _ = w.Write(line)
			}
			if err != nil {
				if err != io.EOF {
					klog.Errorf("error reading stream body: %v", err)
				}
				return false
			}
			return true
		})
	} else {
		// Non-stream: efficiently stream response while capturing for parsing
		var buf bytes.Buffer
		ttee := io.TeeReader(resp.Body, &buf)

		_, err := io.Copy(c.Writer, ttee)
		if err != nil {
			klog.Errorf("copy response to downstream failed: %v", err)
			return nil
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
	req.URL.Host = fmt.Sprintf("%s:%d", podIP, port)

	// step 2: use http.Transport to do request to prefill pod.
	transport := http.DefaultTransport
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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

	// Determine connector type from ModelServer CRD
	connectorType := v1alpha1.ConnectorTypeHTTP
	if modelServer.Spec.KVConnector != nil && modelServer.Spec.KVConnector.Type != "" {
		connectorType = modelServer.Spec.KVConnector.Type
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

		// Build addresses for prefill and decode pods
		prefillAddr := fmt.Sprintf("%s:%d", ctx.PrefillPods[i].Pod.Status.PodIP, port)
		decodeAddr := fmt.Sprintf("%s:%d", ctx.DecodePods[i].Pod.Status.PodIP, port)

		klog.V(4).Infof("Attempting PD disaggregated request: prefill=%s, decode=%s", prefillAddr, decodeAddr)

		// Execute the PD disaggregated proxy operation
		outputTokens, err := kvConnector.Proxy(c, modelRequest, prefillAddr, decodeAddr)

		if err != nil {
			klog.Errorf("proxy failed for prefill pod %s, decode pod %s: %v",
				ctx.PrefillPods[i].Pod.Name, ctx.DecodePods[i].Pod.Name, err)
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
			ctx.PrefillPods[i].Pod.Name, ctx.DecodePods[i].Pod.Name, outputTokens)

		return nil
	}

	c.AbortWithStatusJSON(http.StatusInternalServerError, "all prefill/decode attempts failed")
	return fmt.Errorf("all prefill/decode attempts failed")
}

// handleFairnessScheduling handles the fairness scheduling flow for requests
func (r *Router) handleFairnessScheduling(c *gin.Context, modelRequest ModelRequest, requestID string, modelName string) error {
	userIdVal, ok := c.Get(common.UserIdKey)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, "missing userId in request body")
		return fmt.Errorf("missing userId in request body")
	}
	userId, ok := userIdVal.(string)
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, "userId is not a string")
		return fmt.Errorf("userId is not a string")
	}

	// TODO: better cal priority based on input and output token count
	pri, _ := r.store.GetTokenCount(userId, modelName)
	queueReq := &datastore.Request{
		ReqID:       requestID,
		UserID:      userId,
		ModelName:   modelName,
		Priority:    pri,
		RequestTime: time.Now(),
		NotifyChan:  make(chan struct{}),
	}

	if err := r.store.Enqueue(queueReq); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, fmt.Sprintf("failed to enqueue request: %v", err))
		return fmt.Errorf("failed to enqueue request: %v", err)
	}

	select {
	case <-queueReq.NotifyChan:
		r.doLoadbalance(c, modelRequest)
		return nil
	case <-time.After(60 * time.Second):
		// avoid blocking indefinitely
		klog.Errorf("request %s processing timed out after 60 seconds", requestID)
		c.AbortWithStatusJSON(http.StatusGatewayTimeout, "Request processing timed out")
		return fmt.Errorf("request processing timed out")
	}
}
