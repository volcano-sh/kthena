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

// ErrRefused means a write surely stored nothing: the store turned it down, or it never left this pod.
var ErrRefused = errors.New("batch: the store refused the write")

// File purposes and statuses from the OpenAI files API.
const (
	PurposeBatch       = "batch"
	PurposeBatchOutput = "batch_output"

	FileUploaded  = "uploaded"
	FileProcessed = "processed"
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
	// IndexedAt is the write time in microseconds that orders the listing.
	IndexedAt int64
	Deleted   bool
}

// expired reports whether the file is past its expires_at, given now in Unix seconds; it is gone from then on.
func (f *File) expired(now int64) bool {
	return f.ExpiresAt != 0 && now >= f.ExpiresAt
}

// ErrNoInputFile means a batch names an input file that is missing, deleted, expired or another tenant's.
var ErrNoInputFile = errors.New("batch: no such input file")

// ErrNotBatchFile means a batch names an input file that was not uploaded with purpose batch.
var ErrNotBatchFile = errors.New("batch: the input file does not have purpose batch")

// Status is a batch status from the OpenAI batch object.
type Status string

// Batch statuses from the OpenAI batch object.
const (
	StatusValidating Status = "validating"
	StatusInProgress Status = "in_progress"
	StatusFinalizing Status = "finalizing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusExpired    Status = "expired"
	StatusCancelling Status = "cancelling"
	StatusCancelled  Status = "cancelled"
)

// terminal reports whether nothing more happens to a batch in this status.
func (s Status) terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusExpired || s == StatusCancelled
}

// stampField is the batch object field that records when the batch entered this status.
func (s Status) stampField() string {
	switch s {
	case StatusInProgress, StatusFinalizing, StatusCompleted:
		return string(s) + "_at"
	}
	return ""
}

// ErrNotOwner means another run owns the batch now, so this run must not change it.
var ErrNotOwner = errors.New("batch: this run no longer owns the batch")

// ErrStatusChanged means the batch is not in the status this run expected.
var ErrStatusChanged = errors.New("batch: the batch status changed")

// Lease is one run's claim on a batch; every claim gets a new random fence.
type Lease struct {
	JobID  string
	Holder string
	Fence  string
}

// Origin is how the create request reached the router; every line of the batch is sent the same way.
type Origin struct {
	Host     string `json:"host,omitempty"`
	Gateway  string `json:"gateway,omitempty"`
	Listener string `json:"listener,omitempty"`
	Port     int    `json:"port,omitempty"`
}

// Job is the stored record of one batch.
type Job struct {
	ID     string
	Tenant string
	// Subject is the token subject the lines run as, so fairness counts them for their owner.
	Subject          string
	Status           Status
	Endpoint         string
	InputFileID      string
	CompletionWindow string
	Metadata         map[string]string
	Origin           Origin
	OutputFileID     string
	ErrorFileID      string
	Total            int64
	Completed        int64
	Failed           int64
	CreatedAt        int64
	ExpiresAt        int64
	InProgressAt     int64
	FinalizingAt     int64
	CompletedAt      int64
	// Holder and Fence name the run that owns the batch; every write of that run checks the fence.
	Holder string
	Fence  string
}
