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

package batch

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// BatchesHandler serves OpenAI-compatible /v1/batches endpoints.
type BatchesHandler struct {
	files   FileStore
	batches BatchStore
	enqueue func(batchID string)
}

// NewBatchesHandler returns a batches HTTP handler.
// enqueue is called after a batch is created (nil-safe).
func NewBatchesHandler(files FileStore, batches BatchStore, enqueue func(batchID string)) *BatchesHandler {
	return &BatchesHandler{files: files, batches: batches, enqueue: enqueue}
}

// ServeHTTP dispatches by method and path under /v1/batches or /batches.
func (h *BatchesHandler) ServeHTTP(c *gin.Context) {
	if h == nil || h.files == nil || h.batches == nil {
		abortJSON(c, http.StatusServiceUnavailable, "server_error", "batch_disabled",
			fmt.Sprintf("batch API is disabled; set %s to enable", EnvFilesDir))
		return
	}

	rel, ok := batchesPathSuffix(c.Request.URL.Path)
	if !ok {
		abortJSON(c, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
		return
	}

	switch {
	case c.Request.Method == http.MethodPost && rel == "":
		h.create(c)
	case c.Request.Method == http.MethodGet && rel == "":
		h.list(c)
	case c.Request.Method == http.MethodPost && strings.HasSuffix(rel, CancelSuffix):
		id := strings.TrimSuffix(rel, CancelSuffix)
		id = strings.TrimPrefix(id, "/")
		h.cancel(c, id)
	case c.Request.Method == http.MethodGet && rel != "":
		h.get(c, strings.TrimPrefix(rel, "/"))
	default:
		abortJSON(c, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed",
			fmt.Sprintf("method %s not allowed", c.Request.Method))
	}
}

// IsBatchesPath reports whether path is an OpenAI Batches API path.
func IsBatchesPath(path string) bool {
	_, ok := batchesPathSuffix(path)
	return ok
}

func batchesPathSuffix(path string) (string, bool) {
	switch {
	case path == PathV1Batches || strings.HasPrefix(path, PathV1Batches+"/"):
		return strings.TrimPrefix(path, PathV1Batches), true
	case path == PathBatches || strings.HasPrefix(path, PathBatches+"/"):
		return strings.TrimPrefix(path, PathBatches), true
	default:
		return "", false
	}
}

func (h *BatchesHandler) create(c *gin.Context) {
	var req CreateBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		abortJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_request",
			fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if req.InputFileID == "" {
		abortFromStoreError(c, fmt.Errorf("%w: input_file_id is required", ErrInvalidInputFile))
		return
	}
	if req.CompletionWindow == "" {
		req.CompletionWindow = CompletionWindow24h
	}
	if req.CompletionWindow != CompletionWindow24h {
		abortFromStoreError(c, fmt.Errorf("%w: %q", ErrInvalidWindow, req.CompletionWindow))
		return
	}
	if !IsBatchEndpointAllowed(req.Endpoint) {
		abortFromStoreError(c, fmt.Errorf("%w: %q", ErrInvalidEndpoint, req.Endpoint))
		return
	}

	fileMeta, err := h.files.Get(c.Request.Context(), req.InputFileID)
	if err != nil {
		abortFromStoreError(c, fmt.Errorf("%w: %v", ErrInvalidInputFile, err))
		return
	}
	if fileMeta.Purpose != PurposeBatch {
		abortFromStoreError(c, fmt.Errorf("%w: file purpose must be %q", ErrInvalidInputFile, PurposeBatch))
		return
	}

	now := time.Now().Unix()
	expiresAt := now + int64(DefaultCompletionWindow.Seconds())
	batch := &BatchObject{
		ID:               BatchIDPrefix + uuid.New().String(),
		Object:           ObjectBatch,
		Endpoint:         req.Endpoint,
		Errors:           nil,
		InputFileID:      req.InputFileID,
		CompletionWindow: req.CompletionWindow,
		Status:           StatusValidating,
		CreatedAt:        now,
		ExpiresAt:        &expiresAt,
		RequestCounts:    RequestCounts{},
		Metadata:         req.Metadata,
	}
	if batch.Metadata == nil {
		batch.Metadata = map[string]string{}
	}

	created, err := h.batches.Create(c.Request.Context(), batch)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}

	klog.V(4).Infof("created batch id=%s input_file_id=%s endpoint=%s", created.ID, created.InputFileID, created.Endpoint)
	if h.enqueue != nil {
		h.enqueue(created.ID)
	}
	c.JSON(http.StatusOK, created)
}

func (h *BatchesHandler) get(c *gin.Context, id string) {
	if id == "" {
		abortFromStoreError(c, ErrBatchNotFound)
		return
	}
	obj, err := h.batches.Get(c.Request.Context(), id)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, obj)
}

func (h *BatchesHandler) list(c *gin.Context) {
	opts := ListOptions{
		Order: c.Query(QueryOrder),
		After: c.Query(QueryAfter),
		Limit: DefaultListLimit,
	}
	if raw := c.Query(QueryLimit); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < MinListLimit || n > MaxListLimit {
			abortJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_limit",
				fmt.Sprintf("limit must be between %d and %d", MinListLimit, MaxListLimit))
			return
		}
		opts.Limit = n
	}
	if opts.Order != "" && opts.Order != OrderAsc && opts.Order != OrderDesc {
		abortJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_order",
			fmt.Sprintf("order must be %q or %q", OrderAsc, OrderDesc))
		return
	}

	// Fetch one extra to compute has_more.
	fetchLimit := opts.Limit
	opts.Limit = fetchLimit + 1
	items, err := h.batches.List(c.Request.Context(), opts)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	hasMore := len(items) > fetchLimit
	if hasMore {
		items = items[:fetchLimit]
	}
	if items == nil {
		items = []BatchObject{}
	}

	resp := BatchList{
		Object:  ObjectList,
		Data:    items,
		HasMore: hasMore,
	}
	if len(items) > 0 {
		resp.FirstID = items[0].ID
		resp.LastID = items[len(items)-1].ID
	}
	c.JSON(http.StatusOK, resp)
}

func (h *BatchesHandler) cancel(c *gin.Context, id string) {
	if id == "" {
		abortFromStoreError(c, ErrBatchNotFound)
		return
	}
	obj, err := h.batches.Get(c.Request.Context(), id)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}

	switch obj.Status {
	case StatusCompleted, StatusFailed, StatusExpired, StatusCancelled:
		abortFromStoreError(c, fmt.Errorf("%w: status=%s", ErrNotCancellable, obj.Status))
		return
	case StatusCancelling:
		c.JSON(http.StatusOK, obj)
		return
	}

	now := time.Now().Unix()
	obj.Status = StatusCancelling
	obj.CancellingAt = &now
	updated, err := h.batches.Update(c.Request.Context(), obj)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	if h.enqueue != nil {
		h.enqueue(updated.ID)
	}
	c.JSON(http.StatusOK, updated)
}
