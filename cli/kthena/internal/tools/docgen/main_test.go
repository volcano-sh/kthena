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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFiles creates the given files under dir. Keys are slash-separated paths
// relative to dir, and parent directories are created as needed.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// readFiles returns every file under dir, keyed by its slash-separated path
// relative to dir.
func readFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	require.NoError(t, err)
	return files
}

func TestClearDirectory(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  map[string]string
	}{
		{
			name:  "removes all markdown files",
			files: map[string]string{"a.md": "a", "b.md": "b"},
			want:  map[string]string{},
		},
		{
			name:  "keeps files with other extensions",
			files: map[string]string{"a.md": "a", "notes.txt": "notes"},
			want:  map[string]string{"notes.txt": "notes"},
		},
		{
			name:  "keeps a directory named like a markdown file",
			files: map[string]string{"a.md": "a", "dir.md/nested.md": "nested"},
			want:  map[string]string{"dir.md/nested.md": "nested"},
		},
		{
			name:  "empty directory",
			files: map[string]string{},
			want:  map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tt.files)

			require.NoError(t, clearDirectory(dir))

			assert.Equal(t, tt.want, readFiles(t, dir))
		})
	}
}

func TestClearDirectoryMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")

	require.NoError(t, clearDirectory(dir))

	assert.NoDirExists(t, dir)
}

func TestClearDirectoryNotADirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"file": "content"})

	err := clearDirectory(filepath.Join(dir, "file"))

	assert.ErrorContains(t, err, "error reading directory")
}

func TestAddFrontMatter(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  map[string]string
	}{
		{
			name:  "prepends front matter to markdown files",
			files: map[string]string{"a.md": "# Hello", "b.md": "# World"},
			want: map[string]string{
				"a.md": "---\ntitle: Kthena CLI\n---\n# Hello",
				"b.md": "---\ntitle: Kthena CLI\n---\n# World",
			},
		},
		{
			name:  "keeps existing front matter",
			files: map[string]string{"existing.md": "---\ntitle: Existing\n---\n# Existing\n"},
			want:  map[string]string{"existing.md": "---\ntitle: Existing\n---\n# Existing\n"},
		},
		{
			name:  "skips files with other extensions",
			files: map[string]string{"notes.txt": "plain text\n"},
			want:  map[string]string{"notes.txt": "plain text\n"},
		},
		{
			name:  "skips a directory named like a markdown file",
			files: map[string]string{"dir.md/nested.md": "# Nested"},
			want:  map[string]string{"dir.md/nested.md": "# Nested"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tt.files)

			require.NoError(t, addFrontMatter(dir))

			assert.Equal(t, tt.want, readFiles(t, dir))
		})
	}
}

func TestAddFrontMatterIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.md": "# Hello"})

	require.NoError(t, addFrontMatter(dir))
	require.NoError(t, addFrontMatter(dir))

	want := map[string]string{"a.md": "---\ntitle: Kthena CLI\n---\n# Hello"}
	assert.Equal(t, want, readFiles(t, dir))
}

func TestAddFrontMatterMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")

	err := addFrontMatter(dir)

	assert.ErrorContains(t, err, "error reading directory")
}
