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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestFileStore uses a temporary directory, or KTHENA_TEST_FILE_ROOT to test a real volume.
func newTestFileStore(t *testing.T) (*FSStore, string) {
	t.Helper()
	root := t.TempDir()
	if shared := os.Getenv("KTHENA_TEST_FILE_ROOT"); shared != "" {
		root = filepath.Join(shared, strings.ReplaceAll(t.Name(), "/", "_"))
		require.NoError(t, os.MkdirAll(root, dirMode))
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	return NewFSStore(root), root
}

func readAll(t *testing.T, store *FSStore, tenant, id string) string {
	t.Helper()
	f, err := store.Open(context.Background(), tenant, id)
	require.NoError(t, err)
	defer f.Close()
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(data)
}

func TestFileStoreCreateAndRead(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()

	written, err := store.Create(ctx, "alice", "file-1", strings.NewReader("hello\nworld\n"), 1024)
	require.NoError(t, err)
	assert.Equal(t, int64(12), written)
	assert.Equal(t, "hello\nworld\n", readAll(t, store, "alice", "file-1"))

	size, err := store.Size(ctx, "alice", "file-1")
	require.NoError(t, err)
	assert.Equal(t, int64(12), size)
}

func TestFileStoreKeepsTenantsApart(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()

	_, err := store.Create(ctx, "alice", "file-1", strings.NewReader("alice data"), 1024)
	require.NoError(t, err)
	_, err = store.Create(ctx, "bob", "file-1", strings.NewReader("bob data"), 1024)
	require.NoError(t, err)

	assert.Equal(t, "alice data", readAll(t, store, "alice", "file-1"))
	assert.Equal(t, "bob data", readAll(t, store, "bob", "file-1"), "the same id in another tenant is another file")

	require.NoError(t, store.Remove(ctx, "bob", "file-1"))
	assert.Equal(t, "alice data", readAll(t, store, "alice", "file-1"), "deleting one tenant's file leaves the other")
}

func TestFileStoreRejectsIdsThatCouldEscape(t *testing.T) {
	store, root := newTestFileStore(t)
	ctx := context.Background()

	for _, id := range []string{"../escape", "a/b", "", ".", "..", "file 1", "file\x00", strings.Repeat("x", 129)} {
		_, err := store.Create(ctx, "alice", id, strings.NewReader("x"), 1024)
		assert.Error(t, err, "id %q must be refused", id)
		_, err = store.Open(ctx, "alice", id)
		assert.Error(t, err, "id %q must be refused", id)
		assert.Error(t, store.Remove(ctx, "alice", id), "id %q must be refused", id)
	}

	outside := filepath.Join(filepath.Dir(root), "escape")
	_, err := os.Stat(outside)
	assert.True(t, errors.Is(err, os.ErrNotExist), "nothing is written outside the root")
}

func TestFileStoreEnforcesTheLimit(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()

	_, err := store.Create(ctx, "alice", "file-big", bytes.NewReader(bytes.Repeat([]byte("x"), 100)), 50)
	assert.ErrorIs(t, err, ErrTooLarge)

	_, err = store.Open(ctx, "alice", "file-big")
	assert.ErrorIs(t, err, ErrNotFound, "an oversized upload leaves nothing behind")

	written, err := store.Create(ctx, "alice", "file-exact", bytes.NewReader(bytes.Repeat([]byte("x"), 50)), 50)
	require.NoError(t, err, "a file exactly at the limit is fine")
	assert.Equal(t, int64(50), written)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("network died") }

func TestFileStoreLeavesNothingBehindOnFailure(t *testing.T) {
	store, root := newTestFileStore(t)
	ctx := context.Background()

	require.NoError(t, func() error {
		_, err := store.Create(ctx, "alice", "file-1", strings.NewReader("good"), 1024)
		return err
	}())

	_, err := store.Create(ctx, "alice", "file-2", failingReader{}, 1024)
	assert.Error(t, err)

	_, err = store.Open(ctx, "alice", "file-2")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, "good", readAll(t, store, "alice", "file-1"), "the other file is untouched")

	var leftovers []string
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.Contains(info.Name(), "file-2") {
			leftovers = append(leftovers, path)
		}
		return nil
	}))
	assert.Empty(t, leftovers, "no temp file is left behind")
}

func TestFileStoreReplacesAtomically(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()

	_, err := store.Create(ctx, "alice", "file-1", strings.NewReader("first"), 1024)
	require.NoError(t, err)
	_, err = store.Create(ctx, "alice", "file-1", strings.NewReader("second write"), 1024)
	require.NoError(t, err)
	assert.Equal(t, "second write", readAll(t, store, "alice", "file-1"),
		"finalize rewrites the same file, so a replace must be complete or not at all")
}

func TestFileStoreMissingFile(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()

	_, err := store.Open(ctx, "alice", "file-nope")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.Size(ctx, "alice", "file-nope")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.Remove(ctx, "alice", "file-nope"), ErrNotFound)
}

func TestFileStoreWritesGroupReadableFiles(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()

	_, err := store.Create(ctx, "alice", "file-1", strings.NewReader("data"), 1024)
	require.NoError(t, err)

	path, err := store.path("alice", "file-1")
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
		"replicas share the volume by group, and nobody else may read prompts")

	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), dir.Mode().Perm())
}

// slowReader hands out its data in small pieces, so a reader can catch a rewrite in the act.
type slowReader struct {
	data []byte
	at   int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 4096)], r.data[r.at:])
	r.at += n
	time.Sleep(time.Millisecond)
	return n, nil
}

func TestFileStoreReplaceIsNeverSeenHalfWritten(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()
	const size = 256 << 10
	first := bytes.Repeat([]byte("A"), size)
	second := bytes.Repeat([]byte("B"), size)

	_, err := store.Create(ctx, "alice", "file-1", bytes.NewReader(first), 1<<20)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := store.Create(ctx, "alice", "file-1", &slowReader{data: second}, 1<<20)
		assert.NoError(t, err)
	}()

	reads := 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		f, err := store.Open(ctx, "alice", "file-1")
		require.NoError(t, err, "the file must never disappear during a rewrite")
		data, err := io.ReadAll(f)
		f.Close()
		require.NoError(t, err)
		reads++
		if !bytes.Equal(data, first) && !bytes.Equal(data, second) {
			t.Fatalf("read %d bytes that are neither the old nor the new content", len(data))
		}
	}
	assert.Greater(t, reads, 1, "the reader must have looked while the writer was working")

	f, err := store.Open(ctx, "alice", "file-1")
	require.NoError(t, err)
	defer f.Close()
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(data, second), "the rewrite finished")
}

func TestFileStoreProbe(t *testing.T) {
	store, root := newTestFileStore(t)
	ctx := context.Background()
	require.NoError(t, store.Probe(ctx))

	entries, err := os.ReadDir(filepath.Join(root, reservedDir))
	require.NoError(t, err)
	assert.Empty(t, entries, "the probe removes its own files and nothing else")

	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	readOnly := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(readOnly, 0o500))
	assert.Error(t, NewFSStore(filepath.Join(readOnly, "batch")).Probe(ctx),
		"a volume this pod cannot write to must fail at startup, not at the first upload")
}

func TestFileStoreProbesDoNotCollide(t *testing.T) {
	store, root := newTestFileStore(t)
	const replicas = 24
	failures := make(chan error, replicas)
	for i := 0; i < replicas; i++ {
		go func() { failures <- store.Probe(context.Background()) }()
	}
	for i := 0; i < replicas; i++ {
		assert.NoError(t, <-failures, "replicas probing one volume at once must all pass")
	}
	entries, err := os.ReadDir(filepath.Join(root, reservedDir))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestFileStoreAppend(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()
	for _, chunk := range []string{"one\n", "two\n"} {
		w, err := store.Append(ctx, "alice", "segment-1")
		require.NoError(t, err)
		_, err = w.Write([]byte(chunk))
		require.NoError(t, err)
		require.NoError(t, w.Sync())
		require.NoError(t, w.Close())
	}
	assert.Equal(t, "one\ntwo\n", readAll(t, store, "alice", "segment-1"), "a reopened segment keeps what it had")
	size, err := store.Size(ctx, "alice", "segment-1")
	require.NoError(t, err)
	assert.EqualValues(t, 8, size)
}

func TestFileStoreRejectsUnsafeNames(t *testing.T) {
	store, root := newTestFileStore(t)
	ctx := context.Background()
	for _, tenant := range []string{"", "..", "a/b", reservedDir, "a b"} {
		_, err := store.Create(ctx, tenant, "file-1", strings.NewReader("x"), 0)
		assert.Error(t, err, "tenant %q", tenant)
		_, err = store.Append(ctx, tenant, "file-1")
		assert.Error(t, err, "tenant %q", tenant)
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created for a name that is refused")
}

func TestFileStoreReadsAtAnOffset(t *testing.T) {
	store, _ := newTestFileStore(t)
	ctx := context.Background()
	_, err := store.Create(ctx, "alice", "file-1", strings.NewReader("0123456789"), 0)
	require.NoError(t, err)
	f, err := store.Open(ctx, "alice", "file-1")
	require.NoError(t, err)
	defer f.Close()
	buf := make([]byte, 3)
	_, err = f.ReadAt(buf, 4)
	require.NoError(t, err)
	assert.Equal(t, "456", string(buf))
}
