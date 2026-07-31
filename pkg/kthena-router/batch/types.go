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

// ListOptions controls filtering and pagination for FileStore.List.
type ListOptions struct {
	Purpose string
	Limit   int
	Order   string
	After   string
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
