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

import "time"

// OpenAI-compatible Files / Batches API path segments and object identifiers.
const (
	PathV1Files   = "/v1/files"
	PathFiles     = "/files"
	PathV1Batches = "/v1/batches"
	PathBatches   = "/batches"

	ObjectFile  = "file"
	ObjectList  = "list"
	ObjectBatch = "batch"

	FileIDPrefix  = "file-"
	BatchIDPrefix = "batch_"

	FormFieldFile    = "file"
	FormFieldPurpose = "purpose"

	PurposeBatch       = "batch"
	PurposeBatchOutput = "batch_output"

	QueryPurpose = "purpose"
	QueryLimit   = "limit"
	QueryOrder   = "order"
	QueryAfter   = "after"

	OrderAsc  = "asc"
	OrderDesc = "desc"

	ContentSuffix = "/content"
	CancelSuffix  = "/cancel"

	StatusValidating = "validating"
	StatusFailed     = "failed"
	StatusInProgress = "in_progress"
	StatusFinalizing = "finalizing"
	StatusCompleted  = "completed"
	StatusExpired    = "expired"
	StatusCancelling = "cancelling"
	StatusCancelled  = "cancelled"

	CompletionWindow24h = "24h"

	EndpointChatCompletions = "/v1/chat/completions"
	EndpointCompletions     = "/v1/completions"
)

const (
	EnvFilesDir        = "KTHENA_BATCH_FILES_DIR"
	EnvMaxFileBytes    = "KTHENA_BATCH_MAX_FILE_BYTES"
	EnvBatchTTL        = "KTHENA_BATCH_FILE_TTL"
	EnvMaxConcurrency  = "KTHENA_BATCH_MAX_CONCURRENCY"
	EnvInteractiveBusy = "KTHENA_BATCH_INTERACTIVE_BUSY_THRESHOLD"
)

const (
	DefaultMaxFileBytes             int64 = 200 * 1024 * 1024
	DefaultBatchTTL                       = 30 * 24 * time.Hour
	DefaultCompletionWindow               = 24 * time.Hour
	DefaultMaxConcurrency                 = 4
	DefaultInteractiveBusyThreshold int64 = 32
	DefaultListLimit                      = 10000
	MinListLimit                          = 1
	MaxListLimit                          = 10000
	DefaultMaxRequestsPerBatch            = 50000
)
