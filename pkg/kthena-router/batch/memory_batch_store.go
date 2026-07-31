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
	"context"
	"sort"
	"sync"
)

// MemoryBatchStore keeps batch metadata in memory.
type MemoryBatchStore struct {
	mu      sync.RWMutex
	batches map[string]*BatchObject
}

// NewMemoryBatchStore returns an empty in-memory BatchStore.
func NewMemoryBatchStore() *MemoryBatchStore {
	return &MemoryBatchStore{batches: make(map[string]*BatchObject)}
}

func (s *MemoryBatchStore) Create(ctx context.Context, batch *BatchObject) (*BatchObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batch == nil || batch.ID == "" {
		return nil, ErrInvalidInputFile
	}
	cp := cloneBatch(batch)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches[cp.ID] = cp
	return cloneBatch(cp), nil
}

func (s *MemoryBatchStore) Get(ctx context.Context, id string) (*BatchObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.batches[id]
	if !ok {
		return nil, ErrBatchNotFound
	}
	return cloneBatch(b), nil
}

func (s *MemoryBatchStore) List(ctx context.Context, opts ListOptions) ([]BatchObject, error) {
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
	items := make([]BatchObject, 0, len(s.batches))
	for _, b := range s.batches {
		items = append(items, *cloneBatch(b))
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

func (s *MemoryBatchStore) Update(ctx context.Context, batch *BatchObject) (*BatchObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batch == nil || batch.ID == "" {
		return nil, ErrBatchNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.batches[batch.ID]; !ok {
		return nil, ErrBatchNotFound
	}
	cp := cloneBatch(batch)
	s.batches[cp.ID] = cp
	return cloneBatch(cp), nil
}

func cloneBatch(in *BatchObject) *BatchObject {
	if in == nil {
		return nil
	}
	out := *in
	if in.Metadata != nil {
		out.Metadata = make(map[string]string, len(in.Metadata))
		for k, v := range in.Metadata {
			out.Metadata[k] = v
		}
	}
	if in.Errors != nil {
		errs := *in.Errors
		errs.Data = append([]BatchError(nil), in.Errors.Data...)
		out.Errors = &errs
	}
	if in.OutputFileID != nil {
		v := *in.OutputFileID
		out.OutputFileID = &v
	}
	if in.ErrorFileID != nil {
		v := *in.ErrorFileID
		out.ErrorFileID = &v
	}
	out.InProgressAt = cloneInt64(in.InProgressAt)
	out.ExpiresAt = cloneInt64(in.ExpiresAt)
	out.FinalizingAt = cloneInt64(in.FinalizingAt)
	out.CompletedAt = cloneInt64(in.CompletedAt)
	out.FailedAt = cloneInt64(in.FailedAt)
	out.ExpiredAt = cloneInt64(in.ExpiredAt)
	out.CancellingAt = cloneInt64(in.CancellingAt)
	out.CancelledAt = cloneInt64(in.CancelledAt)
	return &out
}

func cloneInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
