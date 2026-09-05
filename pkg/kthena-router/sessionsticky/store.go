/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package sessionsticky

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

// Store persists session key → ModelServer+Pod binding with TTL.
type Store interface {
	Get(ctx context.Context, key string) (Binding, bool)
	Delete(ctx context.Context, key string)
	// Commit sets or refreshes binding; returns the canonical binding
	// (may differ when another replica wins under Redis).
	Commit(ctx context.Context, key string, binding Binding, ttl time.Duration) (Binding, error)
	Close() error
}

const (
	backendMemory       = "memory"
	backendRedis        = "redis"
	memorySweepInterval = time.Minute
)

// NewStore builds a sticky store from router configuration.
func NewStore(cfg *conf.SessionStickyConfig) (Store, error) {
	if cfg == nil || strings.EqualFold(cfg.Backend, "") || strings.EqualFold(cfg.Backend, backendMemory) {
		return NewMemoryStore(), nil
	}
	if strings.EqualFold(cfg.Backend, backendRedis) {
		addr := ""
		if cfg.Redis != nil {
			addr = strings.TrimSpace(cfg.Redis.Address)
		}
		if addr == "" {
			return nil, fmt.Errorf("sessionSticky.redis.address is required when backend is redis")
		}
		return NewRedisStore(addr)
	}
	return nil, fmt.Errorf("sessionSticky.backend %q is invalid (use memory or redis)", cfg.Backend)
}

type memoryEntry struct {
	binding Binding
	until   time.Time
}

// MemoryStore is a process-local TTL map with a background sweeper.
type MemoryStore struct {
	mu     sync.RWMutex
	m      map[string]memoryEntry
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{
		m:      make(map[string]memoryEntry),
		stopCh: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.sweepLoop()
	return s
}

func (s *MemoryStore) sweepLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(memorySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.sweepExpired()
		}
	}
}

func (s *MemoryStore) sweepExpired() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if !e.until.After(now) {
			klog.InfoS("session sticky: binding expired", "key", k)
			delete(s.m, k)
		}
	}
}

func (s *MemoryStore) Get(_ context.Context, key string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[key]
	if !ok || !e.until.After(time.Now()) || !e.binding.Valid() {
		return Binding{}, false
	}
	return e.binding, true
}

func (s *MemoryStore) Delete(_ context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

func (s *MemoryStore) Commit(_ context.Context, key string, binding Binding, ttl time.Duration) (Binding, error) {
	if !binding.Valid() {
		return Binding{}, fmt.Errorf("invalid session sticky binding")
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	now := time.Now()
	until := now.Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.m[key]
	if ok && cur.until.After(now) && cur.binding.Valid() {
		if !cur.binding.Equal(binding) {
			return cur.binding, nil
		}
		s.m[key] = memoryEntry{binding: binding, until: until}
		return binding, nil
	}
	s.m[key] = memoryEntry{binding: binding, until: until}
	return binding, nil
}

func (s *MemoryStore) Close() error {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
		s.wg.Wait()
	}
	return nil
}

// RedisStore uses a Redis hash (modelServer/pod fields) with compare-and-refresh semantics.
type RedisStore struct {
	rdb *redis.Client
}

// stickyCommitScript: create hash if missing; refresh TTL if same binding; otherwise return existing fields.
// Returns {modelServer, pod}.
const stickyCommitScript = `
local ms = redis.call('HGET', KEYS[1], 'modelServer')
local pod = redis.call('HGET', KEYS[1], 'pod')
if (not ms) or (not pod) then
  redis.call('HSET', KEYS[1], 'modelServer', ARGV[1], 'pod', ARGV[2])
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
  return {ARGV[1], ARGV[2]}
elseif ms == ARGV[1] and pod == ARGV[2] then
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
  return {ARGV[1], ARGV[2]}
else
  return {ms, pod}
end
`

func NewRedisStore(addr string) (*RedisStore, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping for session sticky: %w", err)
	}
	klog.InfoS("session sticky store using Redis", "address", addr)
	return &RedisStore{rdb: rdb}, nil
}

func (s *RedisStore) Get(ctx context.Context, key string) (Binding, bool) {
	vals, err := s.rdb.HMGet(ctx, key, redisFieldModelServer, redisFieldPod).Result()
	if err == redis.Nil {
		return Binding{}, false
	}
	if err != nil {
		klog.Errorf("session sticky redis HMGET: %v", err)
		return Binding{}, false
	}
	b, ok := bindingFromRedisFields(vals)
	if !ok {
		return Binding{}, false
	}
	return b, true
}

func (s *RedisStore) Delete(ctx context.Context, key string) {
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		klog.Errorf("session sticky redis DEL: %v", err)
	}
}

func (s *RedisStore) Commit(ctx context.Context, key string, binding Binding, ttl time.Duration) (Binding, error) {
	if !binding.Valid() {
		return Binding{}, fmt.Errorf("invalid session sticky binding")
	}
	sec := int(ttl / time.Second)
	if sec < 1 {
		sec = 1
	}
	res, err := s.rdb.Eval(ctx, stickyCommitScript, []string{key}, binding.ModelServer, binding.Pod, sec).Result()
	if err != nil {
		return binding, err
	}
	out, ok := bindingFromLuaResult(res)
	if !ok {
		return binding, nil
	}
	return out, nil
}

func (s *RedisStore) Close() error {
	return s.rdb.Close()
}

func bindingFromRedisFields(vals []interface{}) (Binding, bool) {
	if len(vals) < 2 {
		return Binding{}, false
	}
	ms, _ := vals[0].(string)
	pod, _ := vals[1].(string)
	b := Binding{ModelServer: ms, Pod: pod}
	if !b.Valid() {
		return Binding{}, false
	}
	return b, true
}

func bindingFromLuaResult(res interface{}) (Binding, bool) {
	arr, ok := res.([]interface{})
	if !ok {
		return Binding{}, false
	}
	return bindingFromRedisFields(arr)
}
