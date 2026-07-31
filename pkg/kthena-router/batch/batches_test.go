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

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestBatchesHandler_CreateGetListCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	files, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 1024 * 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLocalFileStore: %v", err)
	}
	jobs := NewMemoryBatchStore()

	var enqueued []string
	h := NewBatchesHandler(files, jobs, func(id string) { enqueued = append(enqueued, id) })

	line := `{"custom_id":"1","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"hi"}]}}`
	fileObj, err := files.Create(context.Background(), "in.jsonl", PurposeBatch, strings.NewReader(line+"\n"))
	if err != nil {
		t.Fatalf("Create file: %v", err)
	}

	body, _ := json.Marshal(CreateBatchRequest{
		InputFileID:      fileObj.ID,
		Endpoint:         EndpointChatCompletions,
		CompletionWindow: CompletionWindow24h,
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, PathV1Batches, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created BatchObject
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("create json: %v", err)
	}
	if created.Status != StatusValidating || !strings.HasPrefix(created.ID, BatchIDPrefix) {
		t.Fatalf("unexpected batch: %+v", created)
	}
	if len(enqueued) != 1 || enqueued[0] != created.ID {
		t.Fatalf("enqueue=%v", enqueued)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, PathV1Batches+"/"+created.ID, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d", w.Code)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, PathV1Batches, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d", w.Code)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, PathV1Batches+"/"+created.ID+CancelSuffix, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", w.Code, w.Body.String())
	}
	var cancelled BatchObject
	_ = json.Unmarshal(w.Body.Bytes(), &cancelled)
	if cancelled.Status != StatusCancelling {
		t.Fatalf("status=%s", cancelled.Status)
	}
}

func TestBatchesPathSuffix(t *testing.T) {
	tests := []struct {
		path   string
		wantOK bool
	}{
		{PathV1Batches, true},
		{PathV1Batches + "/batch_1", true},
		{PathV1Batches + "/batch_1/cancel", true},
		{PathBatches, true},
		{"/v1/files", false},
	}
	for _, tt := range tests {
		if _, ok := batchesPathSuffix(tt.path); ok != tt.wantOK {
			t.Fatalf("path=%s ok=%v want %v", tt.path, ok, tt.wantOK)
		}
	}
}
