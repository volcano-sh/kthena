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

// Store persists session key → upstream Pod name with TTL.
type Store interface {
	Get(ctx context.Context, key string) (podName string, ok bool)
	Delete(ctx context.Context, key string)
	// Commit sets or refreshes binding to podName; returns the canonical pod name
	// (may differ when another replica wins under Redis).
	Commit(ctx context.Context, key, podName string, ttl time.Duration) (string, error)
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
	pod   string
	until time.Time
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

func (s *MemoryStore) Get(_ context.Context, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[key]
	if !ok || !e.until.After(time.Now()) {
		return "", false
	}
	return e.pod, true
}

func (s *MemoryStore) Delete(_ context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}

func (s *MemoryStore) Commit(_ context.Context, key, podName string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Second
	}
	now := time.Now()
	until := now.Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.m[key]
	if ok && cur.until.After(now) {
		if cur.pod != podName {
			return cur.pod, nil
		}
		s.m[key] = memoryEntry{pod: podName, until: until}
		return podName, nil
	}
	s.m[key] = memoryEntry{pod: podName, until: until}
	return podName, nil
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

// RedisStore uses Redis with compare-and-refresh semantics in a Lua script.
type RedisStore struct {
	rdb *redis.Client
}

// stickyCommitScript: set if missing; refresh TTL if same pod; otherwise return existing pod.
const stickyCommitScript = `
local cur = redis.call('GET', KEYS[1])
if cur == false then
  redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
  return ARGV[1]
elseif cur == ARGV[1] then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
  return ARGV[1]
else
  return cur
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

func (s *RedisStore) Get(ctx context.Context, key string) (string, bool) {
	v, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil || v == "" {
		return "", false
	}
	if err != nil {
		klog.Errorf("session sticky redis GET: %v", err)
		return "", false
	}
	return v, true
}

func (s *RedisStore) Delete(ctx context.Context, key string) {
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		klog.Errorf("session sticky redis DEL: %v", err)
	}
}

func (s *RedisStore) Commit(ctx context.Context, key, podName string, ttl time.Duration) (string, error) {
	sec := int(ttl / time.Second)
	if sec < 1 {
		sec = 1
	}
	res, err := s.rdb.Eval(ctx, stickyCommitScript, []string{key}, podName, sec).Result()
	if err != nil {
		return podName, err
	}
	out, _ := res.(string)
	if out == "" {
		return podName, nil
	}
	return out, nil
}

func (s *RedisStore) Close() error {
	return s.rdb.Close()
}
