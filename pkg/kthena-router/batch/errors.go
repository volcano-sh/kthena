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
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
)

// Sentinel errors for store and handler mapping.
var (
	ErrNotFound      = errors.New("file not found")
	ErrDisabled      = errors.New("batch files API is disabled")
	ErrInvalidPurpose = errors.New("unsupported file purpose")
	ErrMissingFile   = errors.New("missing file upload")
	ErrMissingPurpose = errors.New("missing purpose")
	ErrTooLarge      = errors.New("file exceeds maximum size")
	ErrEmptyFile     = errors.New("file is empty")
)

// abortJSON writes an OpenAI-shaped error and records it on the access log.
func abortJSON(c *gin.Context, status int, errType, code, message string) {
	accesslog.SetError(c, errType, message)
	c.AbortWithStatusJSON(status, ErrorBody{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

func abortFromStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		abortJSON(c, http.StatusNotFound, "invalid_request_error", "file_not_found", err.Error())
	case errors.Is(err, ErrDisabled):
		abortJSON(c, http.StatusServiceUnavailable, "server_error", "files_disabled", err.Error())
	case errors.Is(err, ErrInvalidPurpose):
		abortJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_purpose", err.Error())
	case errors.Is(err, ErrMissingFile), errors.Is(err, ErrMissingPurpose), errors.Is(err, ErrEmptyFile):
		abortJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
	case errors.Is(err, ErrTooLarge):
		abortJSON(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large", err.Error())
	default:
		abortJSON(c, http.StatusInternalServerError, "server_error", "internal_error", fmt.Sprintf("internal error: %v", err))
	}
}

// IsUploadPurposeAllowed reports whether purpose is accepted on POST /v1/files.
func IsUploadPurposeAllowed(purpose string) bool {
	return purpose == PurposeBatch
}
