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

// Package batch implements the OpenAI-compatible files and batches API of the router.
package batch

import "errors"

// recordVersion is stored on every record so a later release can read older ones.
const recordVersion = "1"

// ErrNotFound means the file or its record does not exist.
var ErrNotFound = errors.New("batch: not found")

// File purposes and statuses from the OpenAI files API.
const (
	PurposeBatch       = "batch"
	PurposeBatchOutput = "batch_output"

	FileUploaded  = "uploaded"
	FileProcessed = "processed"
	FileError     = "error"
)

// File is the record for an uploaded input file or a produced result file.
type File struct {
	ID        string
	Tenant    string
	Filename  string
	Purpose   string
	Status    string
	Bytes     int64
	CreatedAt int64
	ExpiresAt int64
	Deleted   bool
}
