/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// ExecuteBatchLine runs one batch JSONL body through the router's load-balancing
// path (same scheduling/proxy as interactive traffic) and captures the response.
// It intentionally skips fairness/session queues so batch has its own concurrency.
func (r *Router) ExecuteBatchLine(ctx context.Context, endpoint string, body json.RawMessage) (int, []byte, string, error) {
	if endpoint == "" {
		return 0, nil, "", fmt.Errorf("endpoint is required")
	}
	if len(body) == 0 {
		return 0, nil, "", fmt.Errorf("body is required")
	}

	var modelRequest ModelRequest
	if err := json.Unmarshal(body, &modelRequest); err != nil {
		return http.StatusBadRequest, nil, "", fmt.Errorf("invalid body: %w", err)
	}
	modelName, ok := modelRequest["model"].(string)
	if !ok || modelName == "" {
		return http.StatusBadRequest, nil, "", fmt.Errorf("model not found in body")
	}

	// Force non-streaming for batch capture.
	modelRequest["stream"] = false
	rewritten, err := json.Marshal(modelRequest)
	if err != nil {
		return 0, nil, "", err
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rewritten))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	requestID := uuid.New().String()
	req.Header.Set("x-request-id", requestID)
	c.Request = req

	prompt, err := utils.ParsePrompt(modelRequest)
	if err != nil {
		return http.StatusBadRequest, nil, requestID, fmt.Errorf("prompt not found")
	}
	c.Set(PromptKey, prompt)
	c.Set("model", modelName)
	c.Set("metricsRecorder", metrics.NewRequestMetricsRecorder(r.metrics, modelName, endpoint))

	if err := r.doLoadbalance(c, modelRequest); err != nil {
		if w.Code == 0 || w.Code == http.StatusOK {
			return http.StatusInternalServerError, w.Body.Bytes(), requestID, err
		}
		return w.Code, w.Body.Bytes(), requestID, nil
	}
	status := w.Code
	if status == 0 {
		status = http.StatusOK
	}
	return status, w.Body.Bytes(), requestID, nil
}
