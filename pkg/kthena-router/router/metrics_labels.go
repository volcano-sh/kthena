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
	"github.com/gin-gonic/gin"

	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// routerAPIPaths are the request paths the router serves itself. Keep the list
// in sync with the handlers that consume these paths.
var routerAPIPaths = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/completions":      {},
	"/v1/responses":        {},
	"/v1/messages":         {},
	"/v1/models":           {},
	"/models":              {},
}

// requestPathLabel returns the initial `path` metric label value for a request:
// a router-owned API path, or metrics.UnknownPath. Requests served by an
// HTTPRoute are attributed to the path template of the route that served them
// by setRequestPathLabel once the routing decision is made.
func requestPathLabel(c *gin.Context) string {
	if requestPath := c.Request.URL.Path; isRouterAPIPath(requestPath) {
		return requestPath
	}
	return metrics.UnknownPath
}

// isRouterAPIPath reports whether path is an API endpoint the router serves
// itself, which is the only request-derived value allowed in the `path` label.
func isRouterAPIPath(path string) bool {
	_, isRouterAPI := routerAPIPaths[path]
	return isRouterAPI
}

// setRequestPathLabel attributes the request to the route that serves it: the
// HTTPRoute path template or the `uri` match value of the matched ModelRoute
// rule. Callers pass the match they actually used, so a route that did not
// serve the request cannot change the label. Requests to a router-owned API
// endpoint keep the endpoint path, which is more specific than a route value.
func setRequestPathLabel(c *gin.Context, pathTemplate string) {
	if pathTemplate == "" || isRouterAPIPath(c.Request.URL.Path) {
		return
	}
	recorder, exists := c.Get("metricsRecorder")
	if !exists {
		return
	}
	if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
		rec.SetPathLabel(pathTemplate)
	}
}

// modelRouteURIMatchValue returns the `path` metric label value for a request
// served by a ModelRoute: the `uri` match value of the rule that matched,
// bounded by the configured ModelRoutes. It returns "" when the rule matches
// without a `uri` condition, in which case the request keeps its default label.
func modelRouteURIMatchValue(rule *v1alpha1.Rule) string {
	if rule == nil || rule.ModelMatch == nil || rule.ModelMatch.Uri == nil {
		return ""
	}
	switch uri := rule.ModelMatch.Uri; {
	case uri.Exact != nil:
		return *uri.Exact
	case uri.Prefix != nil:
		return *uri.Prefix
	case uri.Regex != nil:
		return *uri.Regex
	}
	return ""
}

// upstreamModelLabelForMetrics returns the `upstream_model` metric label value:
// the ModelServer model override, the registered model, or metrics.UnknownModel.
func upstreamModelLabelForMetrics(modelServer *v1alpha1.ModelServer, modelName string, isLora, registeredModel bool) string {
	if modelServer != nil && modelServer.Spec.Model != nil && !isLora {
		return *modelServer.Spec.Model
	}
	if registeredModel {
		return modelName
	}
	return metrics.UnknownModel
}
