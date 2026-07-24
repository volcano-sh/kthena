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

package connectors

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// HTTPConnector implements simple HTTP-based KV transfer
// Many kv connectors like LMCache, MoonCakeStore can use this
type HTTPConnector struct {
	prefillRequest *http.Request
	decodeRequest  *http.Request
}

// NewHTTPConnector creates a new HTTP connector with default configuration
func NewHTTPConnector() KVConnector {
	return &HTTPConnector{}
}

// Name returns the connector type name
func (h *HTTPConnector) Name() string {
	return "default"
}

// prefill executes prefill request
func (h *HTTPConnector) prefill(req *http.Request, prefillAddr string) error {
	req.URL.Host = prefillAddr
	req.URL.Scheme = "http"

	klog.V(4).Infof("Sending prefill request to %s", prefillAddr)
	return prefillerProxy(nil, req)
}

// decode executes decode request and streams response
func (h *HTTPConnector) decode(c *gin.Context, req *http.Request, decodeAddr string) (int, error) {
	req.URL.Host = decodeAddr
	req.URL.Scheme = "http"

	klog.V(4).Infof("Sending decode request to %s", decodeAddr)
	return decoderProxy(c, req)
}

// Proxy executes the complete prefill-decode flow for HTTP connector
func (h *HTTPConnector) Proxy(c *gin.Context, reqBody map[string]interface{}, prefillAddr, decodeAddr string, hooks *OnFlightHooks) (int, error) {
	// Get metrics recorder from context
	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	decodeBody := cloneReqBody(reqBody)
	h.decodeRequest = BuildDecodeRequest(c, c.Request, decodeBody)

	prefillBody := cloneReqBody(reqBody)
	h.prefillRequest = buildPrefillRequest(c.Request, prefillBody)

	// --- Prefill phase ---
	if metricsRecorder != nil {
		metricsRecorder.StartPrefillPhase()
		metricsRecorder.IncActiveUpstreamRequests()
	}
	if hooks != nil && hooks.IncrPrefill != nil {
		hooks.IncrPrefill()
	}

	err := h.prefill(h.prefillRequest, prefillAddr)

	if hooks != nil && hooks.DecrPrefill != nil {
		hooks.DecrPrefill()
	}
	if metricsRecorder != nil {
		statusCode := "200"
		if err != nil {
			statusCode = "500"
		}
		metricsRecorder.FinishPrefillPhase(statusCode)
		metricsRecorder.DecActiveUpstreamRequests()
	}

	if err != nil {
		return 0, err
	}

	// --- Decode phase ---
	if metricsRecorder != nil {
		metricsRecorder.StartDecodePhase()
		metricsRecorder.IncActiveUpstreamRequests()
	}
	if hooks != nil && hooks.IncrDecode != nil {
		hooks.IncrDecode()
	}

	result, decodeErr := h.decode(c, h.decodeRequest, decodeAddr)

	if hooks != nil && hooks.DecrDecode != nil {
		hooks.DecrDecode()
	}
	if metricsRecorder != nil {
		statusCode := "200"
		if decodeErr != nil {
			statusCode = "500"
		}
		metricsRecorder.FinishDecodePhase(statusCode)
		metricsRecorder.DecActiveUpstreamRequests()
	}

	return result, decodeErr
}
