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
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestLocalFileStore_CreateGetListDeleteContent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalFileStore(Config{
		FilesDir:     dir,
		MaxFileBytes: 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLocalFileStore: %v", err)
	}

	ctx := context.Background()
	content := []byte(`{"custom_id":"1","method":"POST","url":"/v1/chat/completions","body":{}}`)
	obj, err := store.Create(ctx, "requests.jsonl", PurposeBatch, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(obj.ID, FileIDPrefix) {
		t.Fatalf("unexpected id prefix: %s", obj.ID)
	}
	if obj.Object != ObjectFile || obj.Purpose != PurposeBatch {
		t.Fatalf("unexpected object fields: %+v", obj)
	}
	if obj.Bytes != int64(len(content)) {
		t.Fatalf("bytes=%d want %d", obj.Bytes, len(content))
	}
	if obj.ExpiresAt == nil {
		t.Fatal("expected expires_at")
	}

	got, err := store.Get(ctx, obj.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != obj.ID {
		t.Fatalf("Get id=%s want %s", got.ID, obj.ID)
	}

	listed, err := store.List(ctx, ListOptions{Purpose: PurposeBatch, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List len=%d want 1", len(listed))
	}

	rc, meta, err := store.Open(ctx, obj.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("content mismatch: got %q", body)
	}
	if meta.Filename != "requests.jsonl" {
		t.Fatalf("filename=%s", meta.Filename)
	}

	del, err := store.Delete(ctx, obj.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !del.Deleted || del.ID != obj.ID {
		t.Fatalf("unexpected delete response: %+v", del)
	}

	if _, err := store.Get(ctx, obj.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: err=%v", err)
	}
}

func TestLocalFileStore_RejectsUnsupportedPurposeAndOversize(t *testing.T) {
	store, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 8,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLocalFileStore: %v", err)
	}
	ctx := context.Background()

	_, err = store.Create(ctx, "a.jsonl", "assistants", strings.NewReader("hello"))
	if !errors.Is(err, ErrInvalidPurpose) {
		t.Fatalf("purpose err=%v want ErrInvalidPurpose", err)
	}

	_, err = store.Create(ctx, "a.jsonl", PurposeBatch, strings.NewReader("0123456789"))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size err=%v want ErrTooLarge", err)
	}

	_, err = store.Create(ctx, "a.jsonl", PurposeBatch, strings.NewReader(""))
	if !errors.Is(err, ErrEmptyFile) {
		t.Fatalf("empty err=%v want ErrEmptyFile", err)
	}
}

func TestLocalFileStore_ListPaginationAndOrder(t *testing.T) {
	store, err := NewLocalFileStore(Config{
		FilesDir:     t.TempDir(),
		MaxFileBytes: 1024,
		BatchTTL:     time.Hour,
	})
	if err != nil {
		t.Fatalf("NewLocalFileStore: %v", err)
	}
	ctx := context.Background()

	var ids []string
	for i := 0; i < 3; i++ {
		obj, err := store.Create(ctx, "f.jsonl", PurposeBatch, strings.NewReader(strings.Repeat("x", i+1)))
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, obj.ID)
		time.Sleep(2 * time.Millisecond)
	}

	asc, err := store.List(ctx, ListOptions{Order: OrderAsc, Limit: 10})
	if err != nil {
		t.Fatalf("List asc: %v", err)
	}
	if len(asc) != 3 || asc[0].CreatedAt > asc[2].CreatedAt {
		t.Fatalf("unexpected asc order: %+v", asc)
	}

	page, err := store.List(ctx, ListOptions{Order: OrderAsc, Limit: 1, After: asc[0].ID})
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(page) != 1 || page[0].ID != asc[1].ID {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestLoadConfigFromEnv_DefaultsAndOverrides(t *testing.T) {
	t.Setenv(EnvFilesDir, "")
	t.Setenv(EnvMaxFileBytes, "")
	t.Setenv(EnvBatchTTL, "")

	cfg := LoadConfigFromEnv()
	if cfg.Enabled() {
		t.Fatal("expected disabled when FilesDir empty")
	}
	if cfg.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("MaxFileBytes=%d", cfg.MaxFileBytes)
	}
	if cfg.BatchTTL != DefaultBatchTTL {
		t.Fatalf("BatchTTL=%v", cfg.BatchTTL)
	}

	t.Setenv(EnvFilesDir, "/data/batch")
	t.Setenv(EnvMaxFileBytes, "4096")
	t.Setenv(EnvBatchTTL, "1h")
	cfg = LoadConfigFromEnv()
	if !cfg.Enabled() || cfg.FilesDir != "/data/batch" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.MaxFileBytes != 4096 {
		t.Fatalf("MaxFileBytes=%d", cfg.MaxFileBytes)
	}
	if cfg.BatchTTL != time.Hour {
		t.Fatalf("BatchTTL=%v", cfg.BatchTTL)
	}
}

func TestFilesPathSuffix(t *testing.T) {
	tests := []struct {
		path    string
		wantRel string
		wantOK  bool
	}{
		{PathV1Files, "", true},
		{PathV1Files + "/file-1", "/file-1", true},
		{PathV1Files + "/file-1/content", "/file-1/content", true},
		{PathFiles, "", true},
		{PathFiles + "/file-1", "/file-1", true},
		{"/v1/chat/completions", "", false},
		{"/v1/models", "", false},
	}
	for _, tt := range tests {
		rel, ok := filesPathSuffix(tt.path)
		if ok != tt.wantOK || rel != tt.wantRel {
			t.Fatalf("path=%s got (%q,%v) want (%q,%v)", tt.path, rel, ok, tt.wantRel, tt.wantOK)
		}
	}
}
