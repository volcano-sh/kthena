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
	"fmt"
)

var createJobScript = mustLoadScript("create_job.lua")

func (s *RedisStore) jobKey(id string) string { return s.prefix + "job:" + id }
func (s *RedisStore) queueKey() string        { return s.prefix + "queue" }

// CreateJob holds the input file, queues the batch and writes its record in one step; a retry of the same ID changes nothing.
func (s *RedisStore) CreateJob(ctx context.Context, job *Job) error {
	if job == nil || job.ID == "" {
		return errors.New("batch: job id is required")
	}
	fields, err := job.toHash()
	if err != nil {
		return err
	}
	keys := []string{s.fileKey(job.InputFileID), s.fileRefsKey(job.InputFileID), s.jobKey(job.ID), s.queueKey()}
	args := append([]any{job.Tenant, job.ID, job.CreatedAt}, fields...)
	res, err := createJobScript.Run(ctx, s.client, keys, args...).Text()
	if err != nil {
		return err
	}
	switch res {
	case "OK", "EXISTS":
		return nil
	case "NO_FILE":
		return ErrNoInputFile
	case "NOT_BATCH":
		return ErrNotBatchFile
	}
	return fmt.Errorf("batch: unexpected reply %q when creating %s", res, job.ID)
}

// GetJob returns one batch record.
func (s *RedisStore) GetJob(ctx context.Context, id string) (*Job, error) {
	raw, err := s.client.HGetAll(ctx, s.jobKey(id)).Result()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNotFound
	}
	return jobFromHash(raw)
}

func (j *Job) toHash() ([]any, error) {
	metadata := ""
	if len(j.Metadata) > 0 {
		encoded, err := json.Marshal(j.Metadata)
		if err != nil {
			return nil, err
		}
		metadata = string(encoded)
	}
	origin, err := json.Marshal(j.Origin)
	if err != nil {
		return nil, err
	}
	return []any{
		"v", recordVersion,
		"id", j.ID,
		"tenant", j.Tenant,
		"subject", j.Subject,
		"status", string(j.Status),
		"endpoint", j.Endpoint,
		"input_file_id", j.InputFileID,
		"completion_window", j.CompletionWindow,
		"metadata", metadata,
		"origin", string(origin),
		"output_file_id", j.OutputFileID,
		"error_file_id", j.ErrorFileID,
		"total", j.Total,
		"completed", j.Completed,
		"failed", j.Failed,
		"created_at", j.CreatedAt,
		"expires_at", j.ExpiresAt,
		"in_progress_at", j.InProgressAt,
		"finalizing_at", j.FinalizingAt,
		"completed_at", j.CompletedAt,
		"holder", j.Holder,
		"fence", j.Fence,
	}, nil
}

func jobFromHash(raw map[string]string) (*Job, error) {
	job := &Job{
		ID:               raw["id"],
		Tenant:           raw["tenant"],
		Subject:          raw["subject"],
		Status:           Status(raw["status"]),
		Endpoint:         raw["endpoint"],
		InputFileID:      raw["input_file_id"],
		CompletionWindow: raw["completion_window"],
		OutputFileID:     raw["output_file_id"],
		ErrorFileID:      raw["error_file_id"],
		Total:            parseInt(raw["total"]),
		Completed:        parseInt(raw["completed"]),
		Failed:           parseInt(raw["failed"]),
		CreatedAt:        parseInt(raw["created_at"]),
		ExpiresAt:        parseInt(raw["expires_at"]),
		InProgressAt:     parseInt(raw["in_progress_at"]),
		FinalizingAt:     parseInt(raw["finalizing_at"]),
		CompletedAt:      parseInt(raw["completed_at"]),
		Holder:           raw["holder"],
		Fence:            raw["fence"],
	}
	if value := raw["metadata"]; value != "" {
		if err := json.Unmarshal([]byte(value), &job.Metadata); err != nil {
			return nil, fmt.Errorf("batch: the metadata of %s cannot be read: %w", job.ID, err)
		}
	}
	if value := raw["origin"]; value != "" {
		if err := json.Unmarshal([]byte(value), &job.Origin); err != nil {
			return nil, fmt.Errorf("batch: the origin of %s cannot be read: %w", job.ID, err)
		}
	}
	return job, nil
}
