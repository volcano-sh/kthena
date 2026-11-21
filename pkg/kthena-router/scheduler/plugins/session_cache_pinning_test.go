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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupSessionPinningRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, client
}

func newSessionPinningManagerForTest(t *testing.T, client *redis.Client, defaultTTL time.Duration) *SessionPinningManager {
	t.Helper()
	return NewSessionPinningManagerWithRedis(client, defaultTTL)
}

func TestSessionControlSignal_Valid(t *testing.T) {
	for _, s := range []SessionControlSignal{SessionSignalOpen, SessionSignalHeartbeat, SessionSignalClose} {
		if !s.Valid() {
			t.Fatalf("%q should be valid", s)
		}
	}
	if SessionControlSignal("weird").Valid() {
		t.Fatalf("garbage should be invalid")
	}
}

func TestSessionPinState_IsPinned(t *testing.T) {
	if (&SessionPinState{}).IsPinned() {
		t.Fatalf("zero state should not be pinned")
	}
	st := &SessionPinState{TTL: 10 * time.Second, OpenedAt: time.Now()}
	if !st.IsPinned() {
		t.Fatalf("should be pinned when TTL > 0 and opened")
	}
}

func TestBuildSessionKeys(t *testing.T) {
	if BuildSessionPinKey("s1") != "matrix:agent:session:s1" {
		t.Fatalf("unexpected session pin key")
	}
	if BuildSessionBlocksKey("s1") != "matrix:agent:session:s1:blocks" {
		t.Fatalf("unexpected session blocks key")
	}
}

// --- Lifecycle open/heartbeat/close --------------------------------------

func TestSessionPinningManager_OpenCloseRoundtrip(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)

	if err := m.Open("", 0, nil); err == nil {
		t.Fatalf("Open should require non-empty session_id")
	}

	if err := m.Open("sess-1", 120*time.Second, []string{"pod-a.ns", "pod-b.ns"}); err != nil {
		t.Fatalf("Open: %v", err)
	}

	key := BuildSessionPinKey("sess-1")
	owners, err := client.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 2 {
		t.Fatalf("want 2 owners, got %d", len(owners))
	}
	if _, ok := owners["pod-a.ns"]; !ok {
		t.Fatalf("pod-a.ns missing")
	}

	ttl, err := client.TTL(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < 115*time.Second || ttl > 125*time.Second {
		t.Fatalf("TTL = %v, expected ~120s", ttl)
	}

	if err := m.Close("sess-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	exists, _ := client.Exists(context.Background(), key).Result()
	if exists != 0 {
		t.Fatalf("key should be deleted after Close")
	}
	blocksExists, _ := client.Exists(context.Background(), BuildSessionBlocksKey("sess-1")).Result()
	if blocksExists != 0 {
		t.Fatalf("blocks key should also be deleted")
	}
}

func TestSessionPinningManager_Open_DefaultTTL(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := NewSessionPinningManagerWithRedis(client, 0) // triggers manager default → 60s
	if err := m.Open("s1", 0, nil); err != nil {
		t.Fatal(err)
	}
	ttl, _ := client.TTL(context.Background(), BuildSessionPinKey("s1")).Result()
	if ttl < 55*time.Second || ttl > 65*time.Second {
		t.Fatalf("default TTL = %v, want ~60s", ttl)
	}
}

func TestSessionPinningManager_Heartbeat_ExtendsTTL(t *testing.T) {
	mr, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)

	m.Open("s1", 30*time.Second, []string{"pod.ns"})
	mr.FastForward(15 * time.Second)

	if err := m.Heartbeat("s1", 0); err != nil {
		t.Fatal(err)
	}

	ttl, _ := client.TTL(context.Background(), BuildSessionPinKey("s1")).Result()
	if ttl < 55*time.Second || ttl > 65*time.Second {
		t.Fatalf("after heartbeat TTL = %v, expect ~60s", ttl)
	}
}

func TestSessionPinningManager_Heartbeat_EmptySessionID(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)
	if err := m.Heartbeat("", 0); err == nil {
		t.Fatalf("expected error for empty session_id")
	}
}

// --- Pinned blocks ------------------------------------------------------

func TestSessionPinningManager_AddPinnedBlocks_IsBlockPinned(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)

	m.Open("s1", 120*time.Second, []string{"pod.ns"})

	added, err := m.AddPinnedBlocks("s1", []uint64{111111, 222222, 333333})
	if err != nil {
		t.Fatalf("AddPinnedBlocks: %v", err)
	}
	if added != 3 {
		t.Fatalf("added = %d, want 3", added)
	}

	if !m.IsBlockPinned(111111, []string{"s1"}) {
		t.Fatalf("block 111111 should be pinned")
	}
	if m.IsBlockPinned(999999, []string{"s1"}) {
		t.Fatalf("block 999999 should NOT be pinned")
	}
	if m.IsBlockPinned(111111, nil) {
		t.Fatalf("nil session IDs should not pin anything")
	}

	added2, _ := m.AddPinnedBlocks("s1", []uint64{111111})
	if added2 != 0 {
		t.Fatalf("duplicate add = %d, want 0", added2)
	}
}

func TestSessionPinningManager_AddPinnedBlocks_EmptyNoop(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)
	added, err := m.AddPinnedBlocks("", []uint64{1})
	if err != nil || added != 0 {
		t.Fatalf("empty session_id should noop (added=%d err=%v)", added, err)
	}
	added, err = m.AddPinnedBlocks("s1", nil)
	if err != nil || added != 0 {
		t.Fatalf("empty hashes should noop")
	}
}

// --- Snapshot / ActiveSessionIDs --------------------------------------

func TestSessionPinningManager_Snapshot(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)

	m.Open("s1", 120*time.Second, []string{"pod-a.ns", "pod-b.ns"})
	m.AddPinnedBlocks("s1", []uint64{1, 2, 3})

	states, err := m.Snapshot(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("want 1 state, got %d", len(states))
	}
	st := states[0]
	if st.SessionID != "s1" {
		t.Fatalf("unexpected SessionID")
	}
	if len(st.PinnedPodIDs) != 2 {
		t.Fatalf("want 2 pod IDs, got %d", len(st.PinnedPodIDs))
	}
	if st.PinnedBlocks != 3 {
		t.Fatalf("PinnedBlocks = %d, want 3", st.PinnedBlocks)
	}
	if !st.IsPinned() {
		t.Fatalf("state should be pinned")
	}
}

func TestSessionPinningManager_ActiveSessionIDs(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)

	m.Open("s1", 60*time.Second, nil)
	m.Open("s2", 60*time.Second, nil)

	ids, err := m.ActiveSessionIDs(context.Background())
	if err != nil {
		t.Fatalf("ActiveSessionIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 active sessions, got %d (%v)", len(ids), ids)
	}
}

// --- HandleSignal dispatch ---------------------------------------------

func TestSessionPinningManager_HandleSignal(t *testing.T) {
	_, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 60*time.Second)

	if err := m.HandleSignal(SessionSignalOpen, "s1", 0, []string{"pod.ns"}); err != nil {
		t.Fatal(err)
	}
	if err := m.HandleSignal(SessionSignalHeartbeat, "s1", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.HandleSignal(SessionSignalClose, "s1", 0, nil); err != nil {
		t.Fatal(err)
	}
	exists, _ := client.Exists(context.Background(), BuildSessionPinKey("s1")).Result()
	if exists != 0 {
		t.Fatalf("closed session key should be gone")
	}

	if err := m.HandleSignal(SessionControlSignal("bogus"), "s1", 0, nil); err == nil {
		t.Fatalf("unknown signal should return error")
	}
}

// --- Nil / no-redis no-ops --------------------------------------------

func TestSessionPinningManager_NilReceiver(t *testing.T) {
	var m *SessionPinningManager
	if err := m.Open("s", 0, nil); err == nil {
		t.Fatalf("nil receiver should error")
	}
}

func TestSessionPinningManager_NoRedis(t *testing.T) {
	m := NewSessionPinningManager(60 * time.Second)
	if err := m.Open("s", 0, nil); err == nil {
		t.Fatalf("expected redis-unavailable error")
	}
	if m.IsBlockPinned(1, nil) {
		t.Fatalf("no redis → not pinned")
	}
}

// --- Expiration semantics ----------------------------------------------

func TestSessionPinningManager_TTLExpiresAfterIdle(t *testing.T) {
	mr, client := setupSessionPinningRedis(t)
	m := newSessionPinningManagerForTest(t, client, 30*time.Second)

	m.Open("s1", 30*time.Second, []string{"pod.ns"})
	mr.FastForward(31 * time.Second)

	exists, _ := client.Exists(context.Background(), BuildSessionPinKey("s1")).Result()
	if exists != 0 {
		t.Fatalf("session key should have expired")
	}
}
