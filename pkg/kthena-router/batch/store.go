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
	"io"
)

// FileStore persists OpenAI-compatible file metadata and content.
// Implementations must be safe for concurrent use.
type FileStore interface {
	Create(ctx context.Context, filename, purpose string, r io.Reader) (*FileObject, error)
	Get(ctx context.Context, id string) (*FileObject, error)
	List(ctx context.Context, opts ListOptions) ([]FileObject, error)
	Delete(ctx context.Context, id string) (*DeleteFileResponse, error)
	Open(ctx context.Context, id string) (io.ReadCloser, *FileObject, error)
}

// NewFileStoreFromConfig constructs a local file store when enabled.
// Returns nil when FilesDir is unset so callers can treat the feature as off.
func NewFileStoreFromConfig(cfg Config) (FileStore, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	return NewLocalFileStore(cfg)
}
