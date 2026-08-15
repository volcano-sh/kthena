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
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHandler_UploadListGetContentDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLocalFileStore: %v", err)
	}
	h := NewHandler(store)

	payload := `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"m"}}`
	obj := uploadFile(t, h, "requests.jsonl", PurposeBatch, payload)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, PathV1Files, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var list FileList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if list.Object != ObjectList || len(list.Data) != 1 || list.Data[0].ID != obj.ID {
		t.Fatalf("unexpected list: %+v", list)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, PathV1Files+"/"+obj.ID, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, PathV1Files+"/"+obj.ID+ContentSuffix, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("content status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != payload {
		t.Fatalf("content=%q", w.Body.String())
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, PathV1Files+"/"+obj.ID, nil)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_RejectsUnsupportedPurposeAndDisabledStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLocalFileStore: %v", err)
	}

	h := NewHandler(store)
	body, contentType := multipartBody(t, "a.jsonl", "assistants", "data")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, PathV1Files, body)
	c.Request.Header.Set("Content-Type", contentType)
	h.ServeHTTP(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported purpose status=%d", w.Code)
	}

	disabled := NewHandler(nil)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, PathV1Files, nil)
	disabled.ServeHTTP(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled status=%d", w.Code)
	}
}

func uploadFile(t *testing.T, h *Handler, filename, purpose, content string) FileObject {
	t.Helper()
	body, contentType := multipartBody(t, filename, purpose, content)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, PathV1Files, body)
	c.Request.Header.Set("Content-Type", contentType)
	h.ServeHTTP(c)
	if w.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", w.Code, w.Body.String())
	}
	var obj FileObject
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		t.Fatalf("upload json: %v", err)
	}
	return obj
}

func multipartBody(t *testing.T, filename, purpose, content string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField(FormFieldPurpose, purpose); err != nil {
		t.Fatalf("WriteField purpose: %v", err)
	}
	part, err := w.CreateFormFile(FormFieldFile, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.Copy(part, strings.NewReader(content)); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return &buf, w.FormDataContentType()
}
