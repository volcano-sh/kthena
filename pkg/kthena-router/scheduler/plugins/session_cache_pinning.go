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

/*
Session Cache Pinning Manager

Implements Issue #1828 §4 — session-aligned cache pinning coordinated through
Redis. Agents send open/heartbeat/close signals, and the router pins a
session's KV blocks during idle windows so they survive LRU eviction while
the agent is blocked on external tool/API calls.

Lifecycle
---------

  open(session_id, ttl_seconds)
      Pin the session's KV blocks with an initial TTL derived from
      agent_hints.session_ttl_hint (default 60s). Writes a Redis HASH whose
      fields map each pinned block hash to the pod owner; the TTL slides on
      every heartbeat.

  heartbeat(session_id)
      Extend the TTL by the configured SessionTTLHint. Idle sessions without
      a heartbeat for the full TTL are automatically unpinned by Redis
      expiry.

  close(session_id)
      Immediately unpins the session — deletes the Redis HASH and frees the
      cache blocks back to the general eviction pool.

Eviction interaction
--------------------

The Runtime Sidecar and Mooncake L2 pool both honour pinned sessions: blocks
whose hashes appear in an active session pin set are skipped during eviction
until the session is closed or its TTL expires.

This file implements the coordination layer; each engine-side reader will
query the same Redis keys to decide which blocks to protect.

Redis layout
------------

  key = "matrix:agent:session:<session_id>"
  value = HASH {
      "<podOwner>": "<unix_timestamp>"   // one field per pod that has pinned blocks for this session
  }
  TTL = sliding — refreshed on open/heartbeat; defaults to AgentHints.SessionTTLHint.

Additionally, a SET of pinned block hashes per session:

  key = "matrix:agent:session:<session_id>:blocks"
  value = SET { <hash64_str>, ... }
  TTL   = same as above

This dual-layout lets the router answer two questions cheaply:

  1. "Which pods are associated with this session?"  → HGETALL session key
  2. "Is this block hash currently pinned?"         → SISMEMBER session:<id>:blocks <hash>

The eviction query path (question 2) is O(1) and can be batch-AMORTISED with
Redis pipelines when the runtime-sidecar scans block candidates.
*/

package plugins

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	"k8s.io/klog/v2"
)

// --- Public key helpers ----------------------------------------------------

const (
	sessionPinKeyPrefix  = "matrix:agent:session:"
	sessionBlocksSuffix  = ":blocks"
	defaultSessionPinTTL = 60 * time.Second
	defaultSessionPinGC  = 5 * time.Minute
	sessionPinTimeout    = 2 * time.Second
	maxActiveSessions    = 100000 // safety cap; enforced on open()
)

// BuildSessionPinKey is exposed so callers (router REST API, tests) can
// compute the Redis key for a given session ID.
func BuildSessionPinKey(sessionID string) string {
	return sessionPinKeyPrefix + sessionID
}

// BuildSessionBlocksKey is the SET key that tracks which block hashes are
// pinned by the session.
func BuildSessionBlocksKey(sessionID string) string {
	return sessionPinKeyPrefix + sessionID + sessionBlocksSuffix
}

// --- SessionPinState ------------------------------------------------------

// SessionControlSignal identifies the lifecycle signal an agent sends through
// the Router API.
type SessionControlSignal string

const (
	SessionSignalOpen      SessionControlSignal = "open"
	SessionSignalHeartbeat SessionControlSignal = "heartbeat"
	SessionSignalClose     SessionControlSignal = "close"
)

// Valid reports whether the signal is one of the supported lifecycle events.
func (s SessionControlSignal) Valid() bool {
	switch s {
	case SessionSignalOpen, SessionSignalHeartbeat, SessionSignalClose:
		return true
	}
	return false
}

// SessionPinState is the router's view of a pinned session. Produced by
// SessionPinningManager.Snapshot and used for observability endpoints.
type SessionPinState struct {
	SessionID     string
	TTL           time.Duration // seconds remaining (0 if closed / expired)
	PinnedPodIDs  []string      // pod cache-owner identifiers
	PinnedBlocks  int           // number of hashes currently pinned
	OpenedAt      time.Time
	LastHeartbeat time.Time
}

// IsPinned reports whether the session still protects cache blocks.
func (s *SessionPinState) IsPinned() bool {
	if s == nil {
		return false
	}
	return s.TTL > 0 && !s.OpenedAt.IsZero()
}

// SessionPinningManager coordinates session-aligned cache pinning through Redis.
// A zero-value manager with nil redis is safe to call — every no-op path is a
// short-circuit that returns a typed error for callers to handle gracefully.
type SessionPinningManager struct {
	redis      *redis.Client
	defaultTTL time.Duration
}

// NewSessionPinningManager constructs the manager. Redis is optional; when nil
// the manager degrades to a no-op and every lifecycle call returns an error
// suitable for the caller to log / ignore.
func NewSessionPinningManager(defaultTTL time.Duration) *SessionPinningManager {
	ttl := defaultTTL
	if ttl <= 0 {
		ttl = defaultSessionPinTTL
	}
	m := &SessionPinningManager{
		redis:      utils.TryGetRedisClient(),
		defaultTTL: ttl,
	}
	if m.redis == nil {
		klog.Warning("SessionPinningManager: Redis client is nil — all session pinning operations will no-op. " +
			"Agent workloads will still run, but cache blocks will not be protected during idle tool calls.")
	}
	return m
}

// NewSessionPinningManagerWithRedis lets callers inject a custom Redis client
// (used in tests and in-process router stacks).
func NewSessionPinningManagerWithRedis(client *redis.Client, defaultTTL time.Duration) *SessionPinningManager {
	ttl := defaultTTL
	if ttl <= 0 {
		ttl = defaultSessionPinTTL
	}
	return &SessionPinningManager{redis: client, defaultTTL: ttl}
}

// --- Lifecycle API ---------------------------------------------------------

// Open pins the session's cache blocks. ttl overrides the manager default;
// pass 0 to use the default. The caller should pass podOwners (podName.namespace
// pairs) if they are known so subsequent heartbeats can extend the right
// HASH fields.
//
// Returns a non-nil error when Redis is unavailable or the session ID is empty.
func (m *SessionPinningManager) Open(sessionID string, ttl time.Duration, podOwners []string) error {
	if m == nil {
		return fmt.Errorf("session pin open: nil manager")
	}
	if sessionID == "" {
		return fmt.Errorf("session pin open: empty session_id")
	}
	if m.redis == nil {
		return errSessionPinRedisUnavailable
	}
	if ttl <= 0 {
		ttl = m.defaultTTL
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionPinTimeout)
	defer cancel()

	now := time.Now().Unix()
	key := BuildSessionPinKey(sessionID)

	// Check whether we're about to blow past the safety cap — the HGETALL is
	// cheap compared to a full OPEN that would silently create thousands of rows.
	if n, err := m.redis.DBSize(ctx).Result(); err == nil && int(n) >= maxActiveSessions {
		return fmt.Errorf("session pin open: max active sessions %d reached", maxActiveSessions)
	}

	pipe := m.redis.Pipeline()
	if len(podOwners) > 0 {
		fieldMap := make(map[string]interface{}, len(podOwners))
		for _, owner := range podOwners {
			fieldMap[owner] = now
		}
		pipe.HSet(ctx, key, fieldMap)
	} else {
		// Create a placeholder field so the key exists and subsequent heartbeats /
		// snapshots have something to work with. Using a sentinel key avoids creating
		// a separate empty-key branch elsewhere.
		pipe.HSet(ctx, key, "_opened_at", now)
	}
	pipe.Expire(ctx, key, ttl)

	// Persist lifecycle metadata as a small HASH on a side key — this is what
	// Snapshot reads back and what metrics scrapers can aggregate.
	metaKey := key + ":meta"
	pipe.HSet(ctx, metaKey, map[string]interface{}{
		"opened_at":      strconv.FormatInt(now, 10),
		"last_heartbeat": strconv.FormatInt(now, 10),
	})
	pipe.Expire(ctx, metaKey, ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("session pin open: %w", err)
	}

	klog.V(4).Infof("SessionPinningManager.Open: session=%q, ttl=%s, owners=%d",
		sessionID, ttl, len(podOwners))
	return nil
}

// Heartbeat extends the session's TTL. Unlike Open, it does NOT add new
// podOwners — that would silently register pods that aren't actually hosting
// this session's blocks. If callers have learned of a new pod during routing
// they should call Open again, which is safe (idempotent) with an existing
// session ID.
func (m *SessionPinningManager) Heartbeat(sessionID string, ttl time.Duration) error {
	if m == nil {
		return fmt.Errorf("session pin heartbeat: nil manager")
	}
	if sessionID == "" {
		return fmt.Errorf("session pin heartbeat: empty session_id")
	}
	if m.redis == nil {
		return errSessionPinRedisUnavailable
	}
	if ttl <= 0 {
		ttl = m.defaultTTL
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionPinTimeout)
	defer cancel()

	now := time.Now().Unix()
	key := BuildSessionPinKey(sessionID)
	metaKey := key + ":meta"

	pipe := m.redis.Pipeline()
	pipe.Expire(ctx, key, ttl)
	pipe.Expire(ctx, BuildSessionBlocksKey(sessionID), ttl)
	pipe.HSet(ctx, metaKey, "last_heartbeat", strconv.FormatInt(now, 10))
	pipe.Expire(ctx, metaKey, ttl)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("session pin heartbeat: %w", err)
	}

	klog.V(4).Infof("SessionPinningManager.Heartbeat: session=%q, ttl=%s", sessionID, ttl)
	return nil
}

// Close immediately unpins the session and deletes both the owners HASH and
// the blocks SET. After Close the session's blocks are back in the general
// eviction pool.
func (m *SessionPinningManager) Close(sessionID string) error {
	if m == nil {
		return fmt.Errorf("session pin close: nil manager")
	}
	if sessionID == "" {
		return fmt.Errorf("session pin close: empty session_id")
	}
	if m.redis == nil {
		return errSessionPinRedisUnavailable
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionPinTimeout)
	defer cancel()

	key := BuildSessionPinKey(sessionID)
	blocksKey := BuildSessionBlocksKey(sessionID)
	metaKey := key + ":meta"

	pipe := m.redis.Pipeline()
	pipe.Del(ctx, key, blocksKey, metaKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("session pin close: %w", err)
	}

	klog.V(4).Infof("SessionPinningManager.Close: session=%q", sessionID)
	return nil
}

// AddPinnedBlocks appends block hashes to the session's pinned-blocks SET.
// Runtime sidecars call this after writing block ownership to Redis so that
// the eviction query path (SISMEMBER) is populated.
func (m *SessionPinningManager) AddPinnedBlocks(sessionID string, hashes []uint64) (int64, error) {
	if sessionID == "" || len(hashes) == 0 {
		return 0, nil
	}
	if m.redis == nil {
		return 0, errSessionPinRedisUnavailable
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionPinTimeout)
	defer cancel()

	members := make([]interface{}, len(hashes))
	for i, h := range hashes {
		members[i] = strconv.FormatUint(h, 10)
	}

	key := BuildSessionBlocksKey(sessionID)
	added, err := m.redis.SAdd(ctx, key, members...).Result()
	if err != nil {
		return 0, fmt.Errorf("session pin add blocks: %w", err)
	}

	// Also refresh the TTL so a session that's actively accumulating blocks
	// keeps its protection window.
	pipe := m.redis.Pipeline()
	pipe.Expire(ctx, key, m.defaultTTL)
	pipe.Expire(ctx, BuildSessionPinKey(sessionID), m.defaultTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		klog.V(4).Infof("SessionPinningManager.AddPinnedBlocks: expire failed: %v", err)
	}

	klog.V(4).Infof("SessionPinningManager.AddPinnedBlocks: session=%q, added=%d", sessionID, added)
	return added, nil
}

// IsBlockPinned checks whether a single block hash is currently protected by
// any active session. The Runtime Sidecar calls this during its L1 eviction
// loop to skip pinned blocks.
//
// Internally this is O(1) per session SET — when multiple sessions are pinned
// the caller should batch with SIsMemberPipeline via BatchIsBlockPinned.
func (m *SessionPinningManager) IsBlockPinned(blockHash uint64, activeSessionIDs []string) bool {
	if m == nil || m.redis == nil || len(activeSessionIDs) == 0 {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionPinTimeout)
	defer cancel()

	target := strconv.FormatUint(blockHash, 10)
	for _, sid := range activeSessionIDs {
		isMember, err := m.redis.SIsMember(ctx, BuildSessionBlocksKey(sid), target).Result()
		if err == nil && isMember {
			return true
		}
	}
	return false
}

// ActiveSessionIDs returns the set of session IDs currently pinned. Returns an
// error when Redis is unavailable; the returned slice is best-effort.
func (m *SessionPinningManager) ActiveSessionIDs(ctx context.Context) ([]string, error) {
	if m == nil || m.redis == nil {
		return nil, errSessionPinRedisUnavailable
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), sessionPinTimeout)
		defer cancel()
	}

	iter := m.redis.Scan(ctx, 0, sessionPinKeyPrefix+"*", 1000).Iterator()
	var ids []string
	for iter.Next(ctx) {
		key := iter.Val()
		suffix := key[len(sessionPinKeyPrefix):]
		// Skip auxiliary keys (meta, blocks) — they are not session roots.
		if strings.HasSuffix(suffix, ":meta") || strings.HasSuffix(suffix, sessionBlocksSuffix) {
			continue
		}
		if suffix != "" {
			ids = append(ids, suffix)
		}
	}
	if err := iter.Err(); err != nil {
		return ids, fmt.Errorf("session pin scan: %w", err)
	}
	return ids, nil
}

// Snapshot returns the current in-Redis state for the given session IDs.
// Callers pass an empty slice to snapshot every known session (best-effort).
func (m *SessionPinningManager) Snapshot(ctx context.Context, sessionIDs ...string) ([]SessionPinState, error) {
	if m == nil || m.redis == nil {
		return nil, errSessionPinRedisUnavailable
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), sessionPinTimeout)
		defer cancel()
	}

	if len(sessionIDs) == 0 {
		ids, err := m.ActiveSessionIDs(ctx)
		if err != nil {
			return nil, err
		}
		sessionIDs = ids
	}

	out := make([]SessionPinState, 0, len(sessionIDs))
	for _, sid := range sessionIDs {
		state := m.snapshotOne(ctx, sid)
		out = append(out, state)
	}
	return out, nil
}

// snapshotOne reads HGETALL + TTL + HLEN for one session — up to 3 round-trips,
// all issued in a Redis pipeline.
func (m *SessionPinningManager) snapshotOne(ctx context.Context, sid string) SessionPinState {
	key := BuildSessionPinKey(sid)
	blocksKey := BuildSessionBlocksKey(sid)
	metaKey := key + ":meta"

	pipe := m.redis.Pipeline()
	ownersCmd := pipe.HGetAll(ctx, key)
	ttlCmd := pipe.TTL(ctx, key)
	metaCmd := pipe.HGetAll(ctx, metaKey)
	blockCountCmd := pipe.SCard(ctx, blocksKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return SessionPinState{SessionID: sid}
	}

	state := SessionPinState{SessionID: sid}
	if ttl, err := ttlCmd.Result(); err == nil {
		state.TTL = ttl
	}
	if owners, err := ownersCmd.Result(); err == nil {
		state.PinnedPodIDs = make([]string, 0, len(owners))
		for owner := range owners {
			state.PinnedPodIDs = append(state.PinnedPodIDs, owner)
		}
	}
	if blocks, err := blockCountCmd.Result(); err == nil {
		state.PinnedBlocks = int(blocks)
	}
	if meta, err := metaCmd.Result(); err == nil {
		if raw, ok := meta["opened_at"]; ok {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				state.OpenedAt = time.Unix(n, 0)
			}
		}
		if raw, ok := meta["last_heartbeat"]; ok {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				state.LastHeartbeat = time.Unix(n, 0)
			}
		}
	}
	return state
}

// HandleSignal dispatches an open/heartbeat/close lifecycle event. Intended as
// a thin bridge from the Router REST API or K8s webhook to the manager.
func (m *SessionPinningManager) HandleSignal(signal SessionControlSignal, sessionID string, ttl time.Duration, podOwners []string) error {
	switch signal {
	case SessionSignalOpen:
		return m.Open(sessionID, ttl, podOwners)
	case SessionSignalHeartbeat:
		return m.Heartbeat(sessionID, ttl)
	case SessionSignalClose:
		return m.Close(sessionID)
	default:
		return fmt.Errorf("unknown session signal %q", signal)
	}
}

// --- Sentinel errors ------------------------------------------------------

var errSessionPinRedisUnavailable = fmt.Errorf("session pin: Redis client is unavailable — pinning disabled")

// SentinelErrSessionPinRedisUnavailable is a public alias so callers can match on it.
var SentinelErrSessionPinRedisUnavailable = errSessionPinRedisUnavailable
