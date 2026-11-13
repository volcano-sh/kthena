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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// ErrTooLarge means an upload went over the size limit.
var ErrTooLarge = errors.New("batch: file is too large")

const (
	fileMode = 0o640
	dirMode  = 0o750
	// reservedDir holds the store's own files and can never be a tenant directory.
	reservedDir = ".kthena"
)

// namePattern is what a tenant ID or file ID may look like, so neither can leave the root.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ReadFile is a stored file opened for reading.
type ReadFile interface {
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Closer
}

// SegmentWriter appends to a file; Sync returns once the bytes written so far are durable.
type SegmentWriter interface {
	io.Writer
	Sync() error
	Close() error
}

// FileStore keeps file bytes; it is an interface so object storage can be added later.
type FileStore interface {
	// Create stores r under id, at most limit bytes (0 is no limit), and returns once durable.
	Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error)
	// Open returns the file for reading, or ErrNotFound.
	Open(ctx context.Context, tenant, id string) (ReadFile, error)
	// Append opens a file for appending, creating it durably when it is new.
	Append(ctx context.Context, tenant, id string) (SegmentWriter, error)
	// Remove deletes the bytes, or returns ErrNotFound.
	Remove(ctx context.Context, tenant, id string) error
	// Probe checks that this pod can write, sync and rename on the volume.
	Probe(ctx context.Context) error
}

// FSStore keeps files on a mounted volume, one directory per tenant.
type FSStore struct {
	root string
}

var _ FileStore = (*FSStore)(nil)

// NewFSStore roots a file store at the mounted volume.
func NewFSStore(root string) *FSStore {
	return &FSStore{root: root}
}

func (s *FSStore) path(tenant, id string) (string, error) {
	if !namePattern.MatchString(tenant) {
		return "", fmt.Errorf("batch: invalid tenant %q", tenant)
	}
	if !namePattern.MatchString(id) {
		return "", fmt.Errorf("batch: invalid file id %q", id)
	}
	return filepath.Join(s.root, tenant, id), nil
}

// ensureDir creates dir and syncs its parent every time, as another caller may have just created it.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// Create writes a synced temporary file, renames it into place and syncs the directory; a failure leaves nothing.
func (s *FSStore) Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error) {
	final, err := s.path(tenant, id)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(final)
	if err := ensureDir(dir); err != nil {
		return 0, err
	}
	temp, err := os.CreateTemp(dir, "."+id+".*")
	if err != nil {
		return 0, err
	}
	var written int64
	if err = temp.Chmod(fileMode); err == nil {
		written, err = fill(ctx, temp, r, limit)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), final)
	}
	if err != nil {
		_ = os.Remove(temp.Name())
		return 0, err
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(final)
		return 0, err
	}
	return written, nil
}

func fill(ctx context.Context, f *os.File, r io.Reader, limit int64) (int64, error) {
	if limit > 0 {
		r = io.LimitReader(r, limit+1)
	}
	written, err := io.Copy(f, r)
	if err != nil {
		return 0, err
	}
	if limit > 0 && written > limit {
		return 0, ErrTooLarge
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return written, f.Sync()
}

// Open returns the file for reading.
func (s *FSStore) Open(_ context.Context, tenant, id string) (ReadFile, error) {
	path, err := s.path(tenant, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Append opens a file for appending and syncs the directory so a new file survives a crash.
func (s *FSStore) Append(_ context.Context, tenant, id string) (SegmentWriter, error) {
	path, err := s.path(tenant, id)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Remove deletes the bytes of a file.
func (s *FSStore) Remove(_ context.Context, tenant, id string) error {
	path, err := s.path(tenant, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Probe writes, syncs, renames and removes a file of its own, so replicas never collide.
func (s *FSStore) Probe(_ context.Context) error {
	dir := filepath.Join(s.root, reservedDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("batch: storage %s is not writable: %w", s.root, err)
	}
	temp, err := os.CreateTemp(dir, "probe-*")
	if err != nil {
		return fmt.Errorf("batch: storage %s is not writable: %w", s.root, err)
	}
	name := temp.Name()
	defer os.Remove(name)
	_, err = temp.WriteString("kthena")
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("batch: storage %s cannot be written and synced: %w", s.root, err)
	}
	moved := name + ".moved"
	if err := os.Rename(name, moved); err != nil {
		return fmt.Errorf("batch: storage %s does not support rename: %w", s.root, err)
	}
	return os.Remove(moved)
}

// syncDir makes a directory entry durable; filesystems that cannot sync a directory are tolerated.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
