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

import "context"

// Store keeps the batch records shared between router replicas.
type Store interface {
	// CreateFile stores the record for an uploaded or produced file.
	CreateFile(ctx context.Context, file *File) error
	// GetFile returns one file record, or ErrNotFound.
	GetFile(ctx context.Context, id string) (*File, error)
	// DeleteFile marks a file deleted and reports whether its bytes can go now.
	DeleteFile(ctx context.Context, id string) (bool, error)
	// ListFiles returns one page of a tenant's files and whether more follow.
	ListFiles(ctx context.Context, query FileQuery) ([]*File, bool, error)
}

// FileQuery selects one page of a tenant's files.
type FileQuery struct {
	Tenant    string
	Purpose   string
	After     string
	Limit     int
	Ascending bool
}
