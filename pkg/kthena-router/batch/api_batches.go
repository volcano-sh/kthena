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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

const (
	batchesPath = "/v1/batches"
	// chatCompletions is the only endpoint a batch can target so far.
	chatCompletions  = "/v1/chat/completions"
	completionWindow = "24h"
	batchWindow      = 24 * time.Hour
	maxCreateBody    = 1 << 20
	maxMetadata      = 16
	maxMetadataKey   = 64
	maxMetadataValue = 512
)

type createBatchRequest struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata"`
}

func (s *Service) serveBatches(c *gin.Context) {
	method := c.Request.Method
	rest := strings.Trim(strings.TrimPrefix(c.Request.URL.Path, batchesPath), "/")
	switch {
	case rest == "" && method == http.MethodPost:
		s.createBatch(c)
	case rest != "" && !strings.Contains(rest, "/") && method == http.MethodGet:
		s.getBatch(c, rest)
	default:
		fail(c, http.StatusNotFound, errInvalidRequest, "", "unknown route "+method+" "+c.Request.URL.Path)
	}
}

func (s *Service) createBatch(c *gin.Context) {
	request, param, message, status := readCreateRequest(c)
	if message == "" {
		param, message = checkCreateRequest(request)
		status = http.StatusBadRequest
	}
	if message != "" {
		fail(c, status, errInvalidRequest, param, message)
		return
	}
	now := s.now()
	job := &Job{
		ID:               s.newID("batch_"),
		Tenant:           tenantOf(c),
		Subject:          c.GetString(common.UserIdKey),
		Status:           StatusValidating,
		Endpoint:         request.Endpoint,
		InputFileID:      request.InputFileID,
		CompletionWindow: request.CompletionWindow,
		Metadata:         request.Metadata,
		Origin:           s.originOf(c),
		CreatedAt:        now.Unix(),
		ExpiresAt:        now.Add(batchWindow).Unix(),
	}
	err := s.createJob(c.Request.Context(), job)
	switch {
	case errors.Is(err, ErrNoInputFile):
		fail(c, http.StatusBadRequest, errInvalidRequest, "input_file_id", "no such file: "+request.InputFileID)
	case errors.Is(err, ErrNotBatchFile):
		fail(c, http.StatusBadRequest, errInvalidRequest, "input_file_id", "the input file must have purpose "+PurposeBatch)
	case err != nil:
		unavailable(c, "cannot record the batch", err)
	default:
		c.JSON(http.StatusOK, batchObject(job))
	}
}

// createJob records the batch; a write whose answer was lost is looked up, so a retry by the client does not run it twice.
func (s *Service) createJob(ctx context.Context, job *Job) error {
	err := s.store.CreateJob(ctx, job)
	if err == nil || errors.Is(err, ErrNoInputFile) || errors.Is(err, ErrNotBatchFile) {
		return err
	}
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if stored, lookupErr := s.store.GetJob(settle, job.ID); lookupErr == nil && stored.Tenant == job.Tenant {
		return nil
	}
	return err
}

// readCreateRequest decodes exactly one JSON object with known parameters, and says what is wrong otherwise.
func readCreateRequest(c *gin.Context) (*createBatchRequest, string, string, int) {
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxCreateBody)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	request := &createBatchRequest{}
	err := decoder.Decode(request)
	if err == nil && decoder.Decode(&struct{}{}) != io.EOF {
		err = errors.New("the body holds more than one JSON value")
	}
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return request, "", "", 0
	case errors.As(err, &tooBig):
		return nil, "", "the request body is larger than " + strconv.Itoa(maxCreateBody) + " bytes", http.StatusRequestEntityTooLarge
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return nil, name, "unknown parameter: " + name, http.StatusBadRequest
	default:
		return nil, "", "the request body must be one JSON object with the batch parameters: " + err.Error(), http.StatusBadRequest
	}
}

// checkCreateRequest applies the OpenAI limits; metadata is counted in characters, as the OpenAI API states it.
func checkCreateRequest(request *createBatchRequest) (string, string) {
	switch {
	case request.InputFileID == "":
		return "input_file_id", "input_file_id is required"
	case request.Endpoint != chatCompletions:
		return "endpoint", "endpoint must be " + chatCompletions + ", the only endpoint batches support so far"
	case request.CompletionWindow != completionWindow:
		return "completion_window", "completion_window must be " + completionWindow
	case len(request.Metadata) > maxMetadata:
		return "metadata", "metadata may hold at most " + strconv.Itoa(maxMetadata) + " pairs"
	}
	for key, value := range request.Metadata {
		if utf8.RuneCountInString(key) > maxMetadataKey {
			return "metadata", "metadata keys may be at most " + strconv.Itoa(maxMetadataKey) + " characters"
		}
		if utf8.RuneCountInString(value) > maxMetadataValue {
			return "metadata", "metadata values may be at most " + strconv.Itoa(maxMetadataValue) + " characters"
		}
	}
	return "", ""
}

// originOf records how the request reached the router, because a batch line has no listener or host of its own.
func (s *Service) originOf(c *gin.Context) Origin {
	origin := Origin{Host: c.Request.Host}
	if s.config.Gateway != nil {
		origin.Gateway, origin.Listener, origin.Port = s.config.Gateway(c)
	}
	return origin
}

func (s *Service) getBatch(c *gin.Context, id string) {
	job, err := s.store.GetJob(c.Request.Context(), id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		unavailable(c, "cannot read the batch", err)
		return
	}
	if err != nil || job.Tenant != tenantOf(c) {
		fail(c, http.StatusNotFound, errInvalidRequest, "id", "no such batch: "+id)
		return
	}
	c.JSON(http.StatusOK, batchObject(job))
}

// batchObject is the OpenAI batch object; a field that is not set yet is null rather than missing.
func batchObject(job *Job) gin.H {
	out := gin.H{
		"id":                job.ID,
		"object":            "batch",
		"endpoint":          job.Endpoint,
		"model":             nil,
		"errors":            nil,
		"input_file_id":     job.InputFileID,
		"completion_window": job.CompletionWindow,
		"status":            string(job.Status),
		"output_file_id":    orNull(job.OutputFileID),
		"error_file_id":     orNull(job.ErrorFileID),
		"created_at":        job.CreatedAt,
		"in_progress_at":    orNull(job.InProgressAt),
		"expires_at":        orNull(job.ExpiresAt),
		"finalizing_at":     orNull(job.FinalizingAt),
		"completed_at":      orNull(job.CompletedAt),
		"failed_at":         nil,
		"expired_at":        nil,
		"cancelling_at":     nil,
		"cancelled_at":      nil,
		"request_counts":    gin.H{"total": job.Total, "completed": job.Completed, "failed": job.Failed},
		"usage":             nil,
		"metadata":          nil,
	}
	if len(job.Metadata) > 0 {
		out["metadata"] = job.Metadata
	}
	return out
}

// orNull turns the zero value of a field into null.
func orNull[T comparable](value T) any {
	var zero T
	if value == zero {
		return nil
	}
	return value
}
