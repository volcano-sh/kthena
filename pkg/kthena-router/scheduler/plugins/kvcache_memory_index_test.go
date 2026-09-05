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

package plugins

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestKVBlockMemoryIndex_Apply(t *testing.T) {
	now := time.Now().Unix()

	tests := []struct {
		name        string
		payloads    []KVEventPayload
		wantErr     bool
		queryModel  string
		queryHashes []uint64
		wantOwners  map[uint64][]string
	}{
		{
			name: "stored blocks are queryable",
			payloads: []KVEventPayload{{
				PodIdentifier: "pod-1.default",
				ModelName:     "qwen",
				Events: []KVEvent{
					{Type: kvEventStored, BlockHashes: []uint64{1, 2}, Timestamp: now},
				},
			}},
			queryModel:  "qwen",
			queryHashes: []uint64{1, 2, 3},
			wantOwners: map[uint64][]string{
				1: {"pod-1.default"},
				2: {"pod-1.default"},
			},
		},
		{
			name: "removed blocks disappear",
			payloads: []KVEventPayload{
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events: []KVEvent{
						{Type: kvEventStored, BlockHashes: []uint64{1, 2}, Timestamp: now},
						{Type: kvEventRemoved, BlockHashes: []uint64{1}},
					},
				},
			},
			queryModel:  "qwen",
			queryHashes: []uint64{1, 2},
			wantOwners: map[uint64][]string{
				2: {"pod-1.default"},
			},
		},
		{
			name: "cleared drops only the clearing owner",
			payloads: []KVEventPayload{
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1}, Timestamp: now}},
				},
				{
					PodIdentifier: "pod-2.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1}, Timestamp: now}},
				},
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventCleared}},
				},
			},
			queryModel:  "qwen",
			queryHashes: []uint64{1},
			wantOwners: map[uint64][]string{
				1: {"pod-2.default"},
			},
		},
		{
			name: "snapshot replaces previous owner state",
			payloads: []KVEventPayload{
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1, 2}, Timestamp: now}},
				},
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventSnapshot, BlockHashes: []uint64{3}, Timestamp: now}},
				},
			},
			queryModel:  "qwen",
			queryHashes: []uint64{1, 2, 3},
			wantOwners: map[uint64][]string{
				3: {"pod-1.default"},
			},
		},
		{
			name: "empty snapshot clears previous owner state",
			payloads: []KVEventPayload{
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1, 2}, Timestamp: now}},
				},
				{
					PodIdentifier: "pod-1.default",
					ModelName:     "qwen",
					Events:        []KVEvent{{Type: kvEventSnapshot, Timestamp: now}},
				},
			},
			queryModel:  "qwen",
			queryHashes: []uint64{1, 2},
			wantOwners:  map[uint64][]string{},
		},
		{
			name: "mismatched timestamps length is rejected",
			payloads: []KVEventPayload{{
				PodIdentifier: "pod-1.default",
				ModelName:     "qwen",
				Events: []KVEvent{{
					Type:        kvEventSnapshot,
					BlockHashes: []uint64{1, 2},
					Timestamps:  []int64{now},
				}},
			}},
			wantErr:     true,
			queryModel:  "qwen",
			queryHashes: []uint64{1, 2},
			wantOwners:  map[uint64][]string{},
		},
		{
			name: "models are isolated",
			payloads: []KVEventPayload{{
				PodIdentifier: "pod-1.default",
				ModelName:     "qwen",
				Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1}, Timestamp: now}},
			}},
			queryModel:  "other-model",
			queryHashes: []uint64{1},
			wantOwners:  map[uint64][]string{},
		},
		{
			name: "missing pod identifier is rejected",
			payloads: []KVEventPayload{{
				ModelName: "qwen",
				Events:    []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1}}},
			}},
			wantErr:     true,
			queryModel:  "qwen",
			queryHashes: []uint64{1},
			wantOwners:  map[uint64][]string{},
		},
		{
			name: "unknown event type is rejected",
			payloads: []KVEventPayload{{
				PodIdentifier: "pod-1.default",
				ModelName:     "qwen",
				Events:        []KVEvent{{Type: "bogus", BlockHashes: []uint64{1}}},
			}},
			wantErr:     true,
			queryModel:  "qwen",
			queryHashes: []uint64{1},
			wantOwners:  map[uint64][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := NewKVBlockMemoryIndex()
			var gotErr error
			for i := range tt.payloads {
				if err := idx.Apply(&tt.payloads[i]); err != nil {
					gotErr = err
				}
			}
			if (gotErr != nil) != tt.wantErr {
				t.Fatalf("Apply() error = %v, wantErr = %v", gotErr, tt.wantErr)
			}

			got := idx.GetBlockOwners(tt.queryModel, tt.queryHashes)
			if len(got) != len(tt.wantOwners) {
				t.Fatalf("GetBlockOwners() = %v, want owners for %v", got, tt.wantOwners)
			}
			for hash, wantPods := range tt.wantOwners {
				entries, ok := got[hash]
				if !ok {
					t.Fatalf("expected hash %d in result, got %v", hash, got)
				}
				if len(entries) != len(wantPods) {
					t.Fatalf("hash %d: got owners %v, want %v", hash, entries, wantPods)
				}
				for _, pod := range wantPods {
					if _, ok := entries[pod]; !ok {
						t.Errorf("hash %d: expected owner %s, got %v", hash, pod, entries)
					}
				}
			}
		})
	}
}

func TestKVBlockMemoryIndex_SnapshotPreservesPerBlockTimestamps(t *testing.T) {
	idx := NewKVBlockMemoryIndex()
	tsOld := time.Now().Add(-2 * time.Hour).Unix()
	tsNew := time.Now().Unix()

	payload := KVEventPayload{
		PodIdentifier: "pod-1.default",
		ModelName:     "qwen",
		Events: []KVEvent{{
			Type:        kvEventSnapshot,
			BlockHashes: []uint64{1, 2},
			Timestamps:  []int64{tsOld, tsNew},
		}},
	}
	if err := idx.Apply(&payload); err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}

	got := idx.GetBlockOwners("qwen", []uint64{1, 2})
	if want := strconv.FormatInt(tsOld, 10); got[1]["pod-1.default"] != want {
		t.Errorf("hash 1 timestamp = %s, want %s", got[1]["pod-1.default"], want)
	}
	if want := strconv.FormatInt(tsNew, 10); got[2]["pod-1.default"] != want {
		t.Errorf("hash 2 timestamp = %s, want %s", got[2]["pod-1.default"], want)
	}
}

func TestKVBlockMemoryIndex_RemoveOwner(t *testing.T) {
	idx := NewKVBlockMemoryIndex()
	now := time.Now().Unix()
	for _, payload := range []KVEventPayload{
		{
			PodIdentifier: "pod-1.default",
			ModelName:     "qwen",
			Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1, 2}, Timestamp: now}},
		},
		{
			PodIdentifier: "pod-1.default",
			ModelName:     "llama",
			Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{9}, Timestamp: now}},
		},
		{
			PodIdentifier: "pod-2.default",
			ModelName:     "qwen",
			Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1}, Timestamp: now}},
		},
	} {
		p := payload
		if err := idx.Apply(&p); err != nil {
			t.Fatalf("Apply() failed: %v", err)
		}
	}

	idx.RemoveOwner("pod-1.default")

	qwen := idx.GetBlockOwners("qwen", []uint64{1, 2})
	if len(qwen) != 1 {
		t.Fatalf("expected only hash 1 to remain for qwen, got %v", qwen)
	}
	if _, ok := qwen[1]["pod-2.default"]; !ok {
		t.Errorf("expected pod-2.default to remain owner of hash 1, got %v", qwen)
	}
	if llama := idx.GetBlockOwners("llama", []uint64{9}); len(llama) != 0 {
		t.Errorf("expected llama blocks of pod-1 to be gone, got %v", llama)
	}
}

func TestKVBlockMemoryIndex_GCStaleEntries(t *testing.T) {
	idx := NewKVBlockMemoryIndex()
	fresh := time.Now().Unix()
	stale := time.Now().Add(-25 * time.Hour).Unix()

	for _, payload := range []KVEventPayload{
		{
			PodIdentifier: "pod-fresh.default",
			ModelName:     "qwen",
			Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1}, Timestamp: fresh}},
		},
		{
			PodIdentifier: "pod-stale.default",
			ModelName:     "qwen",
			Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{1, 2}, Timestamp: stale}},
		},
	} {
		p := payload
		if err := idx.Apply(&p); err != nil {
			t.Fatalf("Apply() failed: %v", err)
		}
	}

	idx.gcStaleEntries(kvCacheFieldFreshDuration)

	got := idx.GetBlockOwners("qwen", []uint64{1, 2})
	if len(got) != 1 {
		t.Fatalf("expected only hash 1 to survive GC, got %v", got)
	}
	if _, ok := got[1]["pod-fresh.default"]; !ok {
		t.Errorf("expected fresh owner to survive GC, got %v", got)
	}
	if _, ok := got[1]["pod-stale.default"]; ok {
		t.Errorf("expected stale owner to be removed, got %v", got)
	}
}

func TestKVCacheAware_QueryMemoryForBlocks(t *testing.T) {
	idx := NewKVBlockMemoryIndex()
	now := time.Now().Unix()
	payload := KVEventPayload{
		PodIdentifier: "pod-1.default",
		ModelName:     "qwen",
		Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{111}, Timestamp: now}},
	}
	if err := idx.Apply(&payload); err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}

	plugin := &KVCacheAware{
		indexMode:   kvCacheIndexModeMemory,
		memoryIndex: idx,
	}

	result, err := plugin.queryBlocks([]uint64{111, 222}, "qwen", nil)
	if err != nil {
		t.Fatalf("queryBlocks returned error: %v", err)
	}
	if len(result) != 1 || len(result[111]) != 1 || result[111][0] != "pod-1.default" {
		t.Fatalf("unexpected result: %v", result)
	}

	// Ownership written before the owning pod's containers started is dropped.
	result, err = plugin.queryBlocks([]uint64{111}, "qwen",
		map[string]int64{"pod-1.default": now + 10})
	if err != nil {
		t.Fatalf("queryBlocks returned error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected stale ownership to be dropped, got %v", result)
	}
}

func TestKVEventsHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	idx := NewKVBlockMemoryIndex()
	engine := gin.New()
	engine.POST("/kvcache/events", kvEventsHandler(idx))

	tests := []struct {
		name       string
		body       any
		rawBody    string
		wantStatus int
	}{
		{
			name: "valid payload",
			body: KVEventPayload{
				PodIdentifier: "pod-1.default",
				ModelName:     "qwen",
				Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{42}, Timestamp: time.Now().Unix()}},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid json",
			rawBody:    "{not-json",
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "missing model name",
			body: KVEventPayload{
				PodIdentifier: "pod-1.default",
				Events:        []KVEvent{{Type: kvEventStored, BlockHashes: []uint64{42}}},
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body []byte
			if tt.rawBody != "" {
				body = []byte(tt.rawBody)
			} else {
				var err error
				body, err = json.Marshal(tt.body)
				if err != nil {
					t.Fatalf("failed to marshal body: %v", err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/kvcache/events", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}

	got := idx.GetBlockOwners("qwen", []uint64{42})
	if len(got) != 1 {
		t.Fatalf("expected stored block from valid payload, got %v", got)
	}
}
