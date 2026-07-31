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
	"strings"
	"testing"
	"time"
)

func TestWorker_CompletesBatch(t *testing.T) {
	files, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 1024 * 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	jobs := NewMemoryBatchStore()

	dispatch := func(ctx context.Context, endpoint string, body json.RawMessage) (int, []byte, string, error) {
		resp := map[string]interface{}{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
		}
		b, _ := json.Marshal(resp)
		return 200, b, "req-1", nil
	}

	w := NewWorker(files, jobs, dispatch, func() int64 { return 0 }, WorkerConfig{Concurrency: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer func() {
		cancel()
		w.Stop()
	}()

	line := `{"custom_id":"req-a","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"hi"}]}}`
	fileObj, err := files.Create(context.Background(), "in.jsonl", PurposeBatch, strings.NewReader(line+"\n"))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	now := time.Now().Unix()
	expires := now + 3600
	batch := &BatchObject{
		ID:               BatchIDPrefix + "test1",
		Object:           ObjectBatch,
		Endpoint:         EndpointChatCompletions,
		InputFileID:      fileObj.ID,
		CompletionWindow: CompletionWindow24h,
		Status:           StatusValidating,
		CreatedAt:        now,
		ExpiresAt:        &expires,
		Metadata:         map[string]string{},
	}
	if _, err := jobs.Create(context.Background(), batch); err != nil {
		t.Fatalf("create batch: %v", err)
	}

	w.Enqueue(context.Background(), batch.ID)

	deadline := time.Now().Add(5 * time.Second)
	var got *BatchObject
	for time.Now().Before(deadline) {
		got, err = jobs.Get(context.Background(), batch.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status == StatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status=%s counts=%+v errors=%+v", got.Status, got.RequestCounts, got.Errors)
	}
	if got.OutputFileID == nil {
		t.Fatal("expected output_file_id")
	}
	if got.RequestCounts.Completed != 1 || got.RequestCounts.Total != 1 {
		t.Fatalf("counts=%+v", got.RequestCounts)
	}
}

func TestWorker_FailsInvalidJSONL(t *testing.T) {
	files, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 1024 * 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	jobs := NewMemoryBatchStore()
	w := NewWorker(files, jobs, nil, nil, WorkerConfig{Concurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer func() {
		cancel()
		w.Stop()
	}()

	fileObj, err := files.Create(context.Background(), "bad.jsonl", PurposeBatch, strings.NewReader("not-json\n"))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	now := time.Now().Unix()
	expires := now + 3600
	batch := &BatchObject{
		ID:               BatchIDPrefix + "bad",
		Object:           ObjectBatch,
		Endpoint:         EndpointChatCompletions,
		InputFileID:      fileObj.ID,
		CompletionWindow: CompletionWindow24h,
		Status:           StatusValidating,
		CreatedAt:        now,
		ExpiresAt:        &expires,
		Metadata:         map[string]string{},
	}
	_, _ = jobs.Create(context.Background(), batch)
	w.Enqueue(context.Background(), batch.ID)

	deadline := time.Now().Add(5 * time.Second)
	var got *BatchObject
	for time.Now().Before(deadline) {
		got, _ = jobs.Get(context.Background(), batch.ID)
		if got.Status == StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status=%s", got.Status)
	}
}
