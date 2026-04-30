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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// NIXLConnector implements high-performance distributed in-memory KV cache using NIXL
type NIXLConnector struct {
	name              string
	prefillRequest    *http.Request
	decodeRequestBody map[string]interface{}
}

// NewNIXLConnector creates a new NIXL connector
func NewNIXLConnector() KVConnector {
	return &NIXLConnector{
		name: "nixl",
	}
}

// Name returns the connector type name
func (n *NIXLConnector) Name() string {
	return n.name
}

// Proxy executes the complete prefill-decode flow using NIXL for high-performance KV transfer
func (n *NIXLConnector) Proxy(c *gin.Context, reqBody map[string]interface{}, prefillAddr, decodeAddr string) (int, error) {
	// Get metrics recorder from context
	var metricsRecorder *metrics.RequestMetricsRecorder
	if recorder, exists := c.Get("metricsRecorder"); exists {
		if rec, ok := recorder.(*metrics.RequestMetricsRecorder); ok {
			metricsRecorder = rec
		}
	}

	req := c.Request
	prefillBody := cloneReqBody(reqBody)
	n.prefillRequest = n.buildPrefillRequest(req, prefillBody)
	n.decodeRequestBody = addTokenUsage(c, reqBody)

	// Start prefill phase metrics and increment upstream request
	if metricsRecorder != nil {
		metricsRecorder.StartPrefillPhase()
		metricsRecorder.IncActiveUpstreamRequests()
	}

	// 1. send prefill request
	kvTransferParams, err := n.prefill(n.prefillRequest, prefillAddr)

	// End prefill phase metrics and handle upstream requests
	if metricsRecorder != nil {
		statusCode := "200" // Default status code for successful prefill
		if err != nil {
			statusCode = "500"
		}
		metricsRecorder.FinishPrefillPhase(statusCode)
		metricsRecorder.DecActiveUpstreamRequests()

		if err == nil {
			metricsRecorder.StartDecodePhase()
			metricsRecorder.IncActiveUpstreamRequests()
		}
	}

	if err != nil {
		return 0, err
	}

	// 2. send decode request
	decodeReq := n.buildDecodeRequest(c, n.decodeRequestBody, kvTransferParams)
	result, decodeErr := n.decode(c, decodeReq, decodeAddr)

	// End decode phase metrics and decrement upstream request
	if metricsRecorder != nil {
		statusCode := "200" // Default status code, will be updated by response
		if decodeErr != nil {
			statusCode = "500"
		}
		metricsRecorder.FinishDecodePhase(statusCode)
		metricsRecorder.DecActiveUpstreamRequests()
	}

	return result, decodeErr
}

// prefill send prefill request, returns kv_transfer_params
func (n *NIXLConnector) prefill(req *http.Request, prefillAddr string) (interface{}, error) {
	req.URL.Host = prefillAddr
	req.URL.Scheme = "http"
	klog.V(4).Infof("%s prefill: sending to %s", n.name, req.URL.String())

	// Send prefill request
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prefill request failed with status %d", resp.StatusCode)
	}

	// Parse prefill response
	var buf strings.Builder
	_, err = io.Copy(&buf, resp.Body)
	if err != nil {
		return nil, err
	}
	var prefillerResponse map[string]interface{}
	if err := json.Unmarshal([]byte(buf.String()), &prefillerResponse); err != nil {
		return nil, err
	}
	kvTransferParams, ok := prefillerResponse["kv_transfer_params"]
	if !ok {
		klog.Warning("NIXL: missing 'kv_transfer_params' in prefill response")
	}
	return kvTransferParams, nil
}

func (n *NIXLConnector) buildDecodeRequest(c *gin.Context, reqBody map[string]interface{}, kvTransferParams interface{}) *http.Request {
	reqBody["kv_transfer_params"] = kvTransferParams
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil
	}

	// build request
	reqCopy := c.Request.Clone(c.Request.Context())
	reqCopy.URL.Scheme = "http"
	reqCopy.Body = io.NopCloser(bytes.NewBuffer(body))
	reqCopy.ContentLength = int64(len(body))

	return reqCopy
}

// decode send decode request with streaming response
func (n *NIXLConnector) decode(c *gin.Context, req *http.Request, decodeAddr string) (int, error) {
	// Set kv_transfer_params from prefill response
	req.URL.Host = decodeAddr
	req.URL.Scheme = "http"

	klog.V(4).Infof("%s decode: sending to %s", n.name, req.URL.String())

	// Use decoderProxy to handle the decode response with proper streaming
	return decoderProxy(c, req)
}

func cloneReqBody(reqBody map[string]interface{}) map[string]interface{} {
	// Create a deep copy of the request body
	clone := make(map[string]interface{})
	maps.Copy(clone, reqBody)
	return clone
}

func (n *NIXLConnector) buildPrefillRequest(req *http.Request, reqBody map[string]interface{}) *http.Request {
	// Prepare the body for a generic prefill request.
	preparePrefillBody(reqBody)

	// Add NIXL-specific parameters for KV cache transfer.
	reqBody["kv_transfer_params"] = &KVTransferParams{
		DoRemoteDecode:  true,
		DoRemotePrefill: false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		klog.Errorf("Failed to marshal prefill request body: %v", err)
		return nil
	}

	prefillReq := req.Clone(req.Context())
	prefillReq.URL.Scheme = "http"
	prefillReq.Body = io.NopCloser(bytes.NewBuffer(body))
	prefillReq.ContentLength = int64(len(body))

	return prefillReq
}
