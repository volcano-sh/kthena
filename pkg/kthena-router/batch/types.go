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

import "encoding/json"

// FileObject is the OpenAI-compatible file resource returned by /v1/files.
type FileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

// FileList is the OpenAI-compatible list envelope for GET /v1/files.
type FileList struct {
	Object string       `json:"object"`
	Data   []FileObject `json:"data"`
}

// DeleteFileResponse is returned by DELETE /v1/files/{id}.
type DeleteFileResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

// ListOptions controls filtering and pagination for list APIs.
type ListOptions struct {
	Purpose string
	Limit   int
	Order   string
	After   string
}

// CreateBatchRequest is the body for POST /v1/batches.
type CreateBatchRequest struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// RequestCounts tracks per-batch progress (OpenAI Batch object).
type RequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// BatchError is an OpenAI-shaped batch-level error entry.
type BatchError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
	Line    *int   `json:"line,omitempty"`
}

// BatchErrors wraps batch validation/runtime errors.
type BatchErrors struct {
	Object string       `json:"object"`
	Data   []BatchError `json:"data"`
}

// BatchObject is the OpenAI-compatible batch resource.
type BatchObject struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Endpoint         string            `json:"endpoint"`
	Errors           *BatchErrors      `json:"errors"`
	InputFileID      string            `json:"input_file_id"`
	CompletionWindow string            `json:"completion_window"`
	Status           string            `json:"status"`
	OutputFileID     *string           `json:"output_file_id"`
	ErrorFileID      *string           `json:"error_file_id"`
	CreatedAt        int64             `json:"created_at"`
	InProgressAt     *int64            `json:"in_progress_at"`
	ExpiresAt        *int64            `json:"expires_at"`
	FinalizingAt     *int64            `json:"finalizing_at"`
	CompletedAt      *int64            `json:"completed_at"`
	FailedAt         *int64            `json:"failed_at"`
	ExpiredAt        *int64            `json:"expired_at"`
	CancellingAt     *int64            `json:"cancelling_at"`
	CancelledAt      *int64            `json:"cancelled_at"`
	RequestCounts    RequestCounts     `json:"request_counts"`
	Metadata         map[string]string `json:"metadata"`
}

// BatchList is the list envelope for GET /v1/batches.
type BatchList struct {
	Object  string        `json:"object"`
	Data    []BatchObject `json:"data"`
	FirstID string        `json:"first_id,omitempty"`
	LastID  string        `json:"last_id,omitempty"`
	HasMore bool          `json:"has_more"`
}

// InputLine is one JSONL request in a batch input file.
type InputLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// OutputLine is one JSONL response line in a batch output/error file.
type OutputLine struct {
	ID       string        `json:"id"`
	CustomID string        `json:"custom_id"`
	Response *LineResponse `json:"response"`
	Error    *LineError    `json:"error"`
}

// LineResponse wraps an upstream HTTP response for a batch line.
type LineResponse struct {
	StatusCode int             `json:"status_code"`
	RequestID  string          `json:"request_id,omitempty"`
	Body       json.RawMessage `json:"body"`
}

// LineError is a per-request error in output/error JSONL.
type LineError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorBody is a small JSON error payload consistent with other router handlers.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries a machine-readable type and human-readable message.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
