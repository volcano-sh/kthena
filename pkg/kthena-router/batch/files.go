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
	io.Seeker
	io.Closer
}

// FileStore keeps file bytes; it is an interface so object storage can be added later.
type FileStore interface {
	// Create stores r under id, at most limit bytes (0 is no limit), and returns once durable.
	Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error)
	// Open returns the file for reading, or ErrNotFound.
	Open(ctx context.Context, tenant, id string) (ReadFile, error)
	// Remove deletes the bytes, or returns ErrNotFound.
	Remove(ctx context.Context, tenant, id string) error
	// Probe checks that this pod can write, sync and rename on the volume.
	Probe(ctx context.Context) error
	// VolumeID returns the ID of the volume, writing a new one the first time.
	VolumeID(ctx context.Context) (string, error)
	// NewWriter starts a file that is written bit by bit and appears under id only once committed.
	NewWriter(ctx context.Context, tenant, id string) (FileWriter, error)
}

// FileWriter is a file being written; nobody can read it under its ID until Commit returns.
type FileWriter interface {
	io.Writer
	// Commit makes the file durable under its ID and returns its size.
	Commit() (int64, error)
	// Abort throws away what was written.
	Abort()
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
	err := os.Mkdir(dir, dirMode)
	if err == nil {
		// The umask can take away bits that the volume's group needs, so a new directory gets them back.
		err = os.Chmod(dir, dirMode)
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// Create writes the file through NewWriter, so it appears only once it is durable; a failure leaves nothing.
func (s *FSStore) Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error) {
	writer, err := s.NewWriter(ctx, tenant, id)
	if err != nil {
		return 0, err
	}
	written, err := fill(ctx, writer, r, limit)
	if err != nil {
		writer.Abort()
		return 0, err
	}
	if _, err := writer.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

func fill(ctx context.Context, w io.Writer, r io.Reader, limit int64) (int64, error) {
	if limit > 0 {
		r = io.LimitReader(r, limit+1)
	}
	written, err := io.Copy(w, r)
	if err != nil {
		return 0, err
	}
	if limit > 0 && written > limit {
		return 0, ErrTooLarge
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return written, nil
}

// NewWriter starts a temporary file in the tenant's directory, which Commit syncs and renames into place.
func (s *FSStore) NewWriter(_ context.Context, tenant, id string) (FileWriter, error) {
	final, err := s.path(tenant, id)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(final)
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(dir, "."+id+".*")
	if err != nil {
		return nil, err
	}
	if err := temp.Chmod(fileMode); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return nil, err
	}
	return &fsWriter{temp: temp, final: final, dir: dir}, nil
}

// fsWriter is a file on the volume that is still under its temporary name.
type fsWriter struct {
	temp    *os.File
	final   string
	dir     string
	written int64
}

func (w *fsWriter) Write(p []byte) (int, error) {
	n, err := w.temp.Write(p)
	w.written += int64(n)
	return n, err
}

// Commit syncs the file, renames it into place and syncs the directory; on a failure nothing is left.
func (w *fsWriter) Commit() (int64, error) {
	err := w.temp.Sync()
	if closeErr := w.temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(w.temp.Name(), w.final)
	}
	if err != nil {
		_ = os.Remove(w.temp.Name())
		return 0, err
	}
	if err := syncDir(w.dir); err != nil {
		_ = os.Remove(w.final)
		return 0, err
	}
	return w.written, nil
}

// Abort removes the temporary file.
func (w *fsWriter) Abort() {
	_ = w.temp.Close()
	_ = os.Remove(w.temp.Name())
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
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("batch: %s is removed, but syncing its directory failed: %w", path, err)
	}
	return nil
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
