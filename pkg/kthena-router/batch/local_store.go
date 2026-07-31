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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// LocalFileStore keeps metadata in memory and content on disk under FilesDir.
type LocalFileStore struct {
	dir          string
	maxFileBytes int64
	batchTTL     time.Duration

	mu    sync.RWMutex
	files map[string]*storedFile
}

type storedFile struct {
	meta     FileObject
	diskPath string
}

// NewLocalFileStore creates the storage directory and an empty in-memory index.
func NewLocalFileStore(cfg Config) (*LocalFileStore, error) {
	if cfg.FilesDir == "" {
		return nil, fmt.Errorf("%w: %s is empty", ErrDisabled, EnvFilesDir)
	}
	if err := os.MkdirAll(cfg.FilesDir, 0o750); err != nil {
		return nil, fmt.Errorf("create batch files dir %q: %w", cfg.FilesDir, err)
	}

	maxBytes := cfg.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	ttl := cfg.BatchTTL
	if ttl <= 0 {
		ttl = DefaultBatchTTL
	}

	klog.Infof("batch files store enabled: dir=%s maxBytes=%d ttl=%s", cfg.FilesDir, maxBytes, ttl)
	return &LocalFileStore{
		dir:          cfg.FilesDir,
		maxFileBytes: maxBytes,
		batchTTL:     ttl,
		files:        make(map[string]*storedFile),
	}, nil
}

func (s *LocalFileStore) Create(ctx context.Context, filename, purpose string, r io.Reader) (*FileObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filename == "" {
		filename = "upload"
	}
	if !IsStoredPurposeAllowed(purpose) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPurpose, purpose)
	}

	id := FileIDPrefix + uuid.New().String()
	diskPath := filepath.Join(s.dir, id)

	f, err := os.OpenFile(diskPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create file on disk: %w", err)
	}

	limited := io.LimitReader(r, s.maxFileBytes+1)
	n, copyErr := io.Copy(f, limited)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(diskPath)
		return nil, fmt.Errorf("write file content: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(diskPath)
		return nil, fmt.Errorf("close file content: %w", closeErr)
	}
	if n == 0 {
		_ = os.Remove(diskPath)
		return nil, ErrEmptyFile
	}
	if n > s.maxFileBytes {
		_ = os.Remove(diskPath)
		return nil, fmt.Errorf("%w: max %d bytes", ErrTooLarge, s.maxFileBytes)
	}

	now := time.Now().Unix()
	expiresAt := now + int64(s.batchTTL.Seconds())
	meta := FileObject{
		ID:        id,
		Object:    ObjectFile,
		Bytes:     n,
		CreatedAt: now,
		Filename:  filename,
		Purpose:   purpose,
		ExpiresAt: &expiresAt,
	}

	s.mu.Lock()
	s.files[id] = &storedFile{meta: meta, diskPath: diskPath}
	s.mu.Unlock()

	out := meta
	return &out, nil
}

func (s *LocalFileStore) Get(ctx context.Context, id string) (*FileObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sf, ok := s.files[id]
	if !ok {
		return nil, ErrNotFound
	}
	out := sf.meta
	return &out, nil
}

func (s *LocalFileStore) List(ctx context.Context, opts ListOptions) ([]FileObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit < MinListLimit {
		limit = MinListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	order := opts.Order
	if order == "" {
		order = OrderDesc
	}

	s.mu.RLock()
	items := make([]FileObject, 0, len(s.files))
	for _, sf := range s.files {
		if opts.Purpose != "" && sf.meta.Purpose != opts.Purpose {
			continue
		}
		items = append(items, sf.meta)
	}
	s.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		if order == OrderAsc {
			if items[i].CreatedAt == items[j].CreatedAt {
				return items[i].ID < items[j].ID
			}
			return items[i].CreatedAt < items[j].CreatedAt
		}
		if items[i].CreatedAt == items[j].CreatedAt {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})

	if opts.After != "" {
		idx := -1
		for i, item := range items {
			if item.ID == opts.After {
				idx = i
				break
			}
		}
		if idx >= 0 {
			items = items[idx+1:]
		}
	}

	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *LocalFileStore) Delete(ctx context.Context, id string) (*DeleteFileResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	sf, ok := s.files[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	delete(s.files, id)
	diskPath := sf.diskPath
	s.mu.Unlock()

	if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("failed to remove batch file %s from disk: %v", id, err)
	}

	return &DeleteFileResponse{
		ID:      id,
		Object:  ObjectFile,
		Deleted: true,
	}, nil
}

func (s *LocalFileStore) Open(ctx context.Context, id string) (io.ReadCloser, *FileObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	s.mu.RLock()
	sf, ok := s.files[id]
	if !ok {
		s.mu.RUnlock()
		return nil, nil, ErrNotFound
	}
	meta := sf.meta
	diskPath := sf.diskPath
	s.mu.RUnlock()

	f, err := os.Open(diskPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("open file content: %w", err)
	}
	return f, &meta, nil
}
