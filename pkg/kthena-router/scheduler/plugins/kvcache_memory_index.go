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
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

// The in-memory KV index ("memory" index mode) replaces the shared Redis
// block matrix with a per-router-process index kept in sync by push:
//
//  1. The runtime sidecar next to each inference engine subscribes to the
//     engine's KV cache events, converts engine block hashes into
//     standardized token-block hashes, and keeps the authoritative
//     per-model block set for its pod
//     (python/kthena/runtime/memory_kv_manager.py).
//  2. Every router periodically registers with each runtime sidecar
//     (kvcache_registration.go), announcing its push endpoint, a TTL, and a
//     per-process generation.
//  3. Sidecars push incremental stored/removed/cleared deltas to all
//     registered routers, and push a full replace-style snapshot to a router
//     whose process is new (generation change or expired registration) or
//     whose previous push failed, so a router can always rebuild its index.
//  4. Each router applies these events to its KVBlockMemoryIndex and scores
//     pods by longest prefix match exactly like the Redis backend; a
//     periodic GC drops entries not refreshed within the freshness window.
//
// KV event types pushed by the runtime sidecar in memory sync mode.
// Kept in sync by hand with python/kthena/runtime/memory_kv_manager.py.
const (
	kvEventStored   = "stored"
	kvEventRemoved  = "removed"
	kvEventCleared  = "cleared"
	kvEventSnapshot = "snapshot"
)

// KVEvent is a single KV cache event carried in a push payload. All block
// hashes are standardized hashes computed by the runtime sidecar.
type KVEvent struct {
	Type        string   `json:"type"`
	BlockHashes []uint64 `json:"block_hashes,omitempty"`
	Timestamp   int64    `json:"timestamp,omitempty"`
	// Timestamps optionally carries one unix-second store time per entry of
	// BlockHashes. Snapshots use it to preserve original store times instead
	// of re-stamping every block at snapshot time, which would defeat the
	// engine-restart freshness filter.
	Timestamps []int64 `json:"timestamps,omitempty"`
}

// KVEventPayload is the body of POST /kvcache/events pushed by a runtime sidecar.
type KVEventPayload struct {
	PodIdentifier string    `json:"pod_identifier"`
	ModelName     string    `json:"model_name"`
	Events        []KVEvent `json:"events"`
}

// An owner is the pod identifier of the inference engine pod that holds a
// block in its KV cache, in the "<podName>.<namespace>" form — the same
// identifier the Redis backend stores as hash field names, so lookup results
// flow through the shared scoring path unchanged.
//
// ownerModelKey indexes per-owner-per-model block sets for removal and snapshots.
type ownerModelKey struct {
	owner string
	model string
}

// KVBlockMemoryIndex is an in-memory replacement for the Redis block matrix.
// It stores which pod (owner) currently caches which standardized token block
// hash, per model, together with the time the block was last stored.
type KVBlockMemoryIndex struct {
	mu sync.RWMutex
	// model -> block hash -> owner(podName.namespace) -> unix seconds of last store
	blocks map[string]map[uint64]map[string]int64
	// reverse index for cleared/snapshot events
	ownerBlocks map[ownerModelKey]map[uint64]struct{}
}

func NewKVBlockMemoryIndex() *KVBlockMemoryIndex {
	return &KVBlockMemoryIndex{
		blocks:      make(map[string]map[uint64]map[string]int64),
		ownerBlocks: make(map[ownerModelKey]map[uint64]struct{}),
	}
}

// Apply updates the index with a pushed event payload.
func (idx *KVBlockMemoryIndex) Apply(payload *KVEventPayload) error {
	if payload.PodIdentifier == "" || payload.ModelName == "" {
		return fmt.Errorf("pod_identifier and model_name are required")
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	for _, event := range payload.Events {
		if len(event.Timestamps) > 0 && len(event.Timestamps) != len(event.BlockHashes) {
			return fmt.Errorf("timestamps length %d does not match block_hashes length %d",
				len(event.Timestamps), len(event.BlockHashes))
		}
		switch event.Type {
		case kvEventStored:
			idx.storeLocked(payload.ModelName, payload.PodIdentifier, event.BlockHashes, event.Timestamp, event.Timestamps)
		case kvEventRemoved:
			idx.removeLocked(payload.ModelName, payload.PodIdentifier, event.BlockHashes)
		case kvEventCleared:
			idx.clearLocked(payload.ModelName, payload.PodIdentifier)
		case kvEventSnapshot:
			// A snapshot replaces everything known about this owner+model.
			idx.clearLocked(payload.ModelName, payload.PodIdentifier)
			idx.storeLocked(payload.ModelName, payload.PodIdentifier, event.BlockHashes, event.Timestamp, event.Timestamps)
		default:
			return fmt.Errorf("unknown event type %q", event.Type)
		}
	}
	return nil
}

func (idx *KVBlockMemoryIndex) storeLocked(model, owner string, hashes []uint64, timestamp int64, timestamps []int64) {
	if len(hashes) == 0 {
		return
	}
	if timestamp <= 0 {
		timestamp = time.Now().Unix()
	}
	modelBlocks, ok := idx.blocks[model]
	if !ok {
		modelBlocks = make(map[uint64]map[string]int64)
		idx.blocks[model] = modelBlocks
	}
	key := ownerModelKey{owner: owner, model: model}
	owned, ok := idx.ownerBlocks[key]
	if !ok {
		owned = make(map[uint64]struct{})
		idx.ownerBlocks[key] = owned
	}
	for i, hash := range hashes {
		owners, ok := modelBlocks[hash]
		if !ok {
			owners = make(map[string]int64)
			modelBlocks[hash] = owners
		}
		ts := timestamp
		if len(timestamps) > 0 && timestamps[i] > 0 {
			ts = timestamps[i]
		}
		owners[owner] = ts
		owned[hash] = struct{}{}
	}
}

func (idx *KVBlockMemoryIndex) removeLocked(model, owner string, hashes []uint64) {
	modelBlocks := idx.blocks[model]
	owned := idx.ownerBlocks[ownerModelKey{owner: owner, model: model}]
	for _, hash := range hashes {
		if owners, ok := modelBlocks[hash]; ok {
			delete(owners, owner)
			if len(owners) == 0 {
				delete(modelBlocks, hash)
			}
		}
		delete(owned, hash)
	}
}

func (idx *KVBlockMemoryIndex) clearLocked(model, owner string) {
	key := ownerModelKey{owner: owner, model: model}
	modelBlocks := idx.blocks[model]
	for hash := range idx.ownerBlocks[key] {
		if owners, ok := modelBlocks[hash]; ok {
			delete(owners, owner)
			if len(owners) == 0 {
				delete(modelBlocks, hash)
			}
		}
	}
	delete(idx.ownerBlocks, key)
}

// GetBlockOwners returns, for each requested block hash that is present, the
// owners and the unix-second timestamps of their last store. The timestamps
// are returned as strings so the result can flow through the same freshness
// filtering (freshOwners) as the Redis backend.
func (idx *KVBlockMemoryIndex) GetBlockOwners(model string, hashes []uint64) map[uint64]map[string]string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	result := make(map[uint64]map[string]string)
	modelBlocks, ok := idx.blocks[model]
	if !ok {
		return result
	}
	for _, hash := range hashes {
		owners, ok := modelBlocks[hash]
		if !ok || len(owners) == 0 {
			continue
		}
		entries := make(map[string]string, len(owners))
		for owner, ts := range owners {
			entries[owner] = strconv.FormatInt(ts, 10)
		}
		result[hash] = entries
	}
	return result
}

// RemoveOwner drops every block owned by the given pod identifier across all
// models, e.g. when the pod is deleted.
func (idx *KVBlockMemoryIndex) RemoveOwner(owner string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for key := range idx.ownerBlocks {
		if key.owner == owner {
			idx.clearLocked(key.model, key.owner)
		}
	}
}

// gcStaleEntries drops ownership entries older than freshDuration, mirroring
// the Redis backend GC so restarts or missed removal events cannot leave dead
// entries behind forever.
//
// The full map is traversed while holding the write lock, blocking concurrent
// lookups for the duration of the sweep. This is a deliberate simplicity
// trade-off: the index size is bounded by the aggregate engine KV cache
// capacity (one entry per cached block per pod), so a sweep is pure in-memory
// work finishing in milliseconds even at millions of entries, and it runs
// only once per kvCacheGCInterval (hourly). If the pause ever becomes a
// problem, the sweep can be made incremental, like the cursor-based SCAN GC
// of the Redis backend.
func (idx *KVBlockMemoryIndex) gcStaleEntries(freshDuration time.Duration) {
	cutoff := time.Now().Add(-freshDuration).Unix()

	idx.mu.Lock()
	defer idx.mu.Unlock()

	for model, modelBlocks := range idx.blocks {
		for hash, owners := range modelBlocks {
			for owner, ts := range owners {
				if ts < cutoff {
					delete(owners, owner)
					delete(idx.ownerBlocks[ownerModelKey{owner: owner, model: model}], hash)
				}
			}
			if len(owners) == 0 {
				delete(modelBlocks, hash)
			}
		}
	}
}

// runGC periodically removes stale entries until ctx is done.
func (idx *KVBlockMemoryIndex) runGC(ctx context.Context, interval, freshDuration time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			idx.gcStaleEntries(freshDuration)
		}
	}
}

// kvEventsHandler handles POST /kvcache/events pushed by runtime sidecars.
func kvEventsHandler(index *KVBlockMemoryIndex) gin.HandlerFunc {
	return func(c *gin.Context) {
		var payload KVEventPayload
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload: " + err.Error()})
			return
		}
		if err := index.Apply(&payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		klog.V(4).Infof("KVCacheAware: applied %d pushed events from pod=%s model=%s",
			len(payload.Events), payload.PodIdentifier, payload.ModelName)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// startKVEventsServer starts the HTTP listener that receives pushed KV events
// from runtime sidecars when the plugin runs in memory index mode.
func startKVEventsServer(port int, index *KVBlockMemoryIndex) {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.POST("/kvcache/events", kvEventsHandler(index))

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: engine.Handler(),
	}
	go func() {
		klog.Infof("KVCacheAware: starting KV events server on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Errorf("KVCacheAware: KV events server failed: %v", err)
		}
	}()
}
