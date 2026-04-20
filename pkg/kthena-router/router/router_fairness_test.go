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
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type captureEnqueueStore struct {
	datastore.Store
	tokenCount float64
	lastReq    *datastore.Request
}

func (s *captureEnqueueStore) GetTokenCount(userId, modelName string) (float64, error) {
	return s.tokenCount, nil
}

func (s *captureEnqueueStore) Enqueue(req *datastore.Request) error {
	s.lastReq = req
	return fmt.Errorf("enqueue blocked for test")
}

func newFairnessTestContext(t *testing.T, userID string, inputTokens int) *gin.Context {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	req, err := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	c.Request = req
	c.Set(common.UserIdKey, userID)
	c.Set(accesslog.AccessLogContextKey, accesslog.NewAccessLogContext("test-request", "POST", req.URL.Path, req.Proto, "m"))
	accesslog.SetTokenCounts(c, inputTokens, 0)

	return c
}

func TestGetRequestedOutputTokens(t *testing.T) {
	tests := []struct {
		name     string
		request  ModelRequest
		expected float64
	}{
		{
			name: "uses max_completion_tokens when present",
			request: ModelRequest{
				"max_completion_tokens": float64(24),
				"max_tokens":            float64(300),
			},
			expected: 24,
		},
		{
			name: "falls back to max_tokens",
			request: ModelRequest{
				"max_tokens": float64(32),
			},
			expected: 32,
		},
		{
			name: "supports int output budget",
			request: ModelRequest{
				"max_tokens": 48,
			},
			expected: 48,
		},
		{
			name: "supports int64 output budget",
			request: ModelRequest{
				"max_completion_tokens": int64(64),
			},
			expected: 64,
		},
		{
			name: "missing output budget returns zero",
			request: ModelRequest{
				"prompt": "hello",
			},
			expected: 0,
		},
		{
			name: "invalid non-numeric budget returns zero",
			request: ModelRequest{
				"max_tokens": "128",
			},
			expected: 0,
		},
		{
			name: "negative budget returns zero",
			request: ModelRequest{
				"max_completion_tokens": float64(-20),
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := getRequestedOutputTokens(tt.request)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestCalculateRequestPriority_UsesHistoricalAndEstimatedCost(t *testing.T) {
	r := &Router{
		store:             &captureEnqueueStore{Store: datastore.New(), tokenCount: 15},
		inputTokenWeight:  1.5,
		outputTokenWeight: 2.5,
	}

	priority := r.calculateRequestPriority("u1", "m1", 10, ModelRequest{"max_tokens": float64(4)})

	assert.Equal(t, 15.0, priority.HistoricalUsage)
	assert.Equal(t, 4.0, priority.RequestedOutputTokens)
	assert.Equal(t, 25.0, priority.EstimatedRequestCost)
	assert.Equal(t, 40.0, priority.FinalPriority)
}

func TestCalculateRequestPriority_ZeroOutputBudgetFallsBackToInputOnly(t *testing.T) {
	r := &Router{
		store:             &captureEnqueueStore{Store: datastore.New(), tokenCount: 7},
		inputTokenWeight:  2.0,
		outputTokenWeight: 3.0,
	}

	priority := r.calculateRequestPriority("u1", "m1", 9, ModelRequest{"prompt": "hello"})

	assert.Equal(t, 0.0, priority.RequestedOutputTokens)
	assert.Equal(t, 18.0, priority.EstimatedRequestCost)
	assert.Equal(t, 25.0, priority.FinalPriority)
}

func TestHandleFairnessScheduling_PriorityChangesWithRequestSize(t *testing.T) {
	store := &captureEnqueueStore{Store: datastore.New(), tokenCount: 20}
	r := &Router{
		store:             store,
		fairnessTimeout:   time.Second,
		inputTokenWeight:  1.0,
		outputTokenWeight: 2.0,
	}

	smallCtx := newFairnessTestContext(t, "user-a", 10)
	err := r.handleFairnessScheduling(smallCtx, ModelRequest{"max_tokens": float64(10)}, "req-small", "model-a")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to enqueue request")
	smallPriority := store.lastReq.Priority

	largeCtx := newFairnessTestContext(t, "user-a", 100)
	err = r.handleFairnessScheduling(largeCtx, ModelRequest{"max_tokens": float64(200)}, "req-large", "model-a")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to enqueue request")
	largePriority := store.lastReq.Priority

	assert.Greater(t, largePriority, smallPriority)
}

func TestHandleFairnessScheduling_HistoricalUsageIncludedInPriority(t *testing.T) {
	store := &captureEnqueueStore{Store: datastore.New(), tokenCount: 75}
	r := &Router{
		store:             store,
		fairnessTimeout:   time.Second,
		inputTokenWeight:  1.0,
		outputTokenWeight: 2.0,
	}

	c := newFairnessTestContext(t, "user-a", 20)
	err := r.handleFairnessScheduling(c, ModelRequest{"max_tokens": float64(10)}, "req-1", "model-a")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to enqueue request")

	// Estimated request cost = 1*20 + 2*10 = 40, final should include +75 historical usage.
	assert.Equal(t, 115.0, store.lastReq.Priority)
}

func TestHandleFairnessScheduling_LogVerbosityDoesNotChangeBehavior(t *testing.T) {
	store := &captureEnqueueStore{Store: datastore.New(), tokenCount: 10}
	r := &Router{
		store:             store,
		fairnessTimeout:   time.Second,
		inputTokenWeight:  1.0,
		outputTokenWeight: 2.0,
	}

	vFlag := flag.Lookup("v")
	if vFlag == nil {
		t.Fatal("klog verbosity flag is not initialized")
	}
	originalV := vFlag.Value.String()
	defer func() {
		_ = flag.Set("v", originalV)
	}()

	_ = flag.Set("v", "0")
	c1 := newFairnessTestContext(t, "user-a", 12)
	err := r.handleFairnessScheduling(c1, ModelRequest{"max_completion_tokens": float64(8)}, "req-v0", "model-a")
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "failed to enqueue request"))
	priorityAtV0 := store.lastReq.Priority

	_ = flag.Set("v", "4")
	c2 := newFairnessTestContext(t, "user-a", 12)
	err = r.handleFairnessScheduling(c2, ModelRequest{"max_completion_tokens": float64(8)}, "req-v4", "model-a")
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "failed to enqueue request"))
	priorityAtV4 := store.lastReq.Priority

	assert.Equal(t, priorityAtV0, priorityAtV4)
}
