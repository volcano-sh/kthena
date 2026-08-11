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

package batch

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

// Handler serves OpenAI-compatible /v1/files endpoints.
// It is analogous to Router.ListModels: a dedicated control-plane handler that
// does not enter ParseModelRequest / doLoadbalance.
type Handler struct {
	store FileStore
}

// NewHandler returns a Files API handler. store may be nil (feature disabled).
func NewHandler(store FileStore) *Handler {
	return &Handler{store: store}
}

// ServeHTTP dispatches by method and path suffix under /v1/files or /files.
func (h *Handler) ServeHTTP(c *gin.Context) {
	if h == nil || h.store == nil {
		abortJSON(c, http.StatusServiceUnavailable, "server_error", "files_disabled",
			fmt.Sprintf("batch files API is disabled; set %s to enable", EnvFilesDir))
		return
	}

	rel, ok := filesPathSuffix(c.Request.URL.Path)
	if !ok {
		abortJSON(c, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
		return
	}

	switch {
	case c.Request.Method == http.MethodPost && rel == "":
		h.upload(c)
	case c.Request.Method == http.MethodGet && rel == "":
		h.list(c)
	case c.Request.Method == http.MethodGet && strings.HasSuffix(rel, ContentSuffix):
		id := strings.TrimSuffix(rel, ContentSuffix)
		id = strings.TrimPrefix(id, "/")
		h.content(c, id)
	case c.Request.Method == http.MethodGet && rel != "":
		h.get(c, strings.TrimPrefix(rel, "/"))
	case c.Request.Method == http.MethodDelete && rel != "":
		h.delete(c, strings.TrimPrefix(rel, "/"))
	default:
		abortJSON(c, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed",
			fmt.Sprintf("method %s not allowed", c.Request.Method))
	}
}

// IsFilesPath reports whether path is an OpenAI Files API path.
func IsFilesPath(path string) bool {
	_, ok := filesPathSuffix(path)
	return ok
}

// filesPathSuffix returns the path after /v1/files or /files.
// Examples:
//
//	"/v1/files"           -> "", true
//	"/v1/files/file-1"    -> "/file-1", true
//	"/files/file-1/content" -> "/file-1/content", true
//	"/v1/chat/completions" -> "", false
func filesPathSuffix(path string) (string, bool) {
	switch {
	case path == PathV1Files || strings.HasPrefix(path, PathV1Files+"/"):
		return strings.TrimPrefix(path, PathV1Files), true
	case path == PathFiles || strings.HasPrefix(path, PathFiles+"/"):
		return strings.TrimPrefix(path, PathFiles), true
	default:
		return "", false
	}
}

func (h *Handler) upload(c *gin.Context) {
	purpose := c.PostForm(FormFieldPurpose)
	if purpose == "" {
		abortFromStoreError(c, ErrMissingPurpose)
		return
	}
	if !IsUploadPurposeAllowed(purpose) {
		abortFromStoreError(c, fmt.Errorf("%w: %q (supported: %s)", ErrInvalidPurpose, purpose, PurposeBatch))
		return
	}

	fileHeader, err := c.FormFile(FormFieldFile)
	if err != nil {
		abortFromStoreError(c, ErrMissingFile)
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		abortJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_request",
			fmt.Sprintf("failed to open uploaded file: %v", err))
		return
	}
	defer src.Close()

	obj, err := h.store.Create(c.Request.Context(), fileHeader.Filename, purpose, src)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}

	klog.V(4).Infof("uploaded batch file id=%s filename=%s bytes=%d", obj.ID, obj.Filename, obj.Bytes)
	c.JSON(http.StatusOK, obj)
}

func (h *Handler) list(c *gin.Context) {
	opts := ListOptions{
		Purpose: c.Query(QueryPurpose),
		Order:   c.Query(QueryOrder),
		After:   c.Query(QueryAfter),
		Limit:   DefaultListLimit,
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

	items, err := h.store.List(c.Request.Context(), opts)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	if items == nil {
		items = []FileObject{}
	}

	c.JSON(http.StatusOK, FileList{
		Object: ObjectList,
		Data:   items,
	})
}

func (h *Handler) get(c *gin.Context, id string) {
	if id == "" {
		abortJSON(c, http.StatusNotFound, "invalid_request_error", "file_not_found", ErrNotFound.Error())
		return
	}
	obj, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, obj)
}

func (h *Handler) content(c *gin.Context, id string) {
	if id == "" {
		abortJSON(c, http.StatusNotFound, "invalid_request_error", "file_not_found", ErrNotFound.Error())
		return
	}
	rc, obj, err := h.store.Open(c.Request.Context(), id)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	defer rc.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, obj.Filename))
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, rc); err != nil {
		klog.Errorf("failed to stream file content id=%s: %v", id, err)
	}
}

func (h *Handler) delete(c *gin.Context, id string) {
	if id == "" {
		abortJSON(c, http.StatusNotFound, "invalid_request_error", "file_not_found", ErrNotFound.Error())
		return
	}
	resp, err := h.store.Delete(c.Request.Context(), id)
	if err != nil {
		abortFromStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}
