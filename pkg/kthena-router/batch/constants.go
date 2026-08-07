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

import "time"

// OpenAI-compatible Files API path segments and object identifiers.
const (
	// PathV1Files is the full path used when the catch-all listener sees the
	// complete URL (gateway mode). PathFiles is used when Gin mounts under
	// Group("/v1") and the remaining path is "/files" (default listener).
	PathV1Files = "/v1/files"
	PathFiles   = "/files"

	ObjectFile = "file"
	ObjectList = "list"

	FileIDPrefix = "file-"

	// Multipart form field names for POST /v1/files.
	FormFieldFile    = "file"
	FormFieldPurpose = "purpose"

	// Supported upload purposes for this slice. batch_output is reserved for
	// files produced by the future batch worker (not accepted on upload).
	PurposeBatch       = "batch"
	PurposeBatchOutput = "batch_output"

	// Query parameter names for GET /v1/files.
	QueryPurpose = "purpose"
	QueryLimit   = "limit"
	QueryOrder   = "order"
	QueryAfter   = "after"

	OrderAsc  = "asc"
	OrderDesc = "desc"

	ContentSuffix = "/content"
)

// Environment variable keys for batch file storage configuration.
const (
	EnvFilesDir     = "KTHENA_BATCH_FILES_DIR"
	EnvMaxFileBytes = "KTHENA_BATCH_MAX_FILE_BYTES"
	EnvBatchTTL     = "KTHENA_BATCH_FILE_TTL"
)

// Named defaults. Values mirror OpenAI Batch Files limits where applicable
// (JSONL inputs up to 200 MiB; batch files expire after 30 days).
const (
	DefaultMaxFileBytes int64 = 200 * 1024 * 1024
	DefaultBatchTTL           = 30 * 24 * time.Hour
	DefaultListLimit          = 10000
	MinListLimit              = 1
	MaxListLimit              = 10000
)
