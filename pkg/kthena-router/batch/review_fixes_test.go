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
	"testing"
)

func TestSafeContentFilename(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`normal.jsonl`, `normal.jsonl`},
		{"evil\"name\n.jsonl", "evilname.jsonl"},
		{"  ", "download"},
		{`"`, "download"},
	}
	for _, tt := range tests {
		if got := safeContentFilename(tt.in); got != tt.want {
			t.Fatalf("safeContentFilename(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
}

func TestMemoryBatchStore_UpdateRequestCountsPreservesStatus(t *testing.T) {
	s := NewMemoryBatchStore()
	b := &BatchObject{
		ID:     BatchIDPrefix + "x",
		Object: ObjectBatch,
		Status: StatusCancelling,
		RequestCounts: RequestCounts{
			Total: 10,
		},
		Metadata: map[string]string{},
	}
	if _, err := s.Create(t.Context(), b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpdateRequestCounts(t.Context(), b.ID, RequestCounts{Total: 10, Completed: 3, Failed: 1}); err != nil {
		t.Fatalf("UpdateRequestCounts: %v", err)
	}
	got, err := s.Get(t.Context(), b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusCancelling {
		t.Fatalf("status clobbered: %s", got.Status)
	}
	if got.RequestCounts.Completed != 3 || got.RequestCounts.Failed != 1 {
		t.Fatalf("counts=%+v", got.RequestCounts)
	}
}
