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

	// Force non-streaming so responses can be captured as a single body.
	modelRequest["stream"] = false
	rewritten, err := json.Marshal(modelRequest)
	if err != nil {
		return 0, nil, "", err
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
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
		if recorder.Code == 0 || recorder.Code == http.StatusOK {
			return http.StatusInternalServerError, recorder.Body.Bytes(), requestID, err
		}
		return recorder.Code, recorder.Body.Bytes(), requestID, nil
	}
	status := recorder.Code
	if status == 0 {
		status = http.StatusOK
	}
	return status, recorder.Body.Bytes(), requestID, nil
}
