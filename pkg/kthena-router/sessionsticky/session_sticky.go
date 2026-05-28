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

package sessionsticky

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

const (
	StoreMemory = "memory"
	StoreRedis  = "redis"
)

const defaultSessionAffinitySeconds int32 = 600
const redisOperationTimeout = 2 * time.Second
const memoryCleanupInterval = time.Minute

type StoreConfig struct {
	Type          string
	RedisAddress  string
	RedisPassword string
}

type Store interface {
	Get(routeKey, sessionKey string) (types.NamespacedName, bool)
	Set(routeKey, sessionKey string, pod types.NamespacedName, ttl time.Duration)
	Delete(routeKey, sessionKey string)
	Close() error
}

type ClaimExtractor interface {
	ExtractStringClaim(c *gin.Context, claimName string) (string, error)
}

type sessionBinding struct {
	pod       types.NamespacedName
	expiresAt time.Time
}

type MemoryStore struct {
	mu              sync.Mutex
	stopOnce        sync.Once
	stopCh          chan struct{}
	now             func() time.Time
	cleanupInterval time.Duration
	nextCleanup     time.Time
	bindings        map[string]sessionBinding
}

func NewStore(config StoreConfig) (Store, error) {
	storeType := strings.ToLower(strings.TrimSpace(config.Type))
	if storeType == "" {
		storeType = StoreMemory
	}
	switch storeType {
	case StoreMemory:
		return NewMemoryStore(), nil
	case StoreRedis:
		return NewRedisStore(config.RedisAddress, config.RedisPassword)
	default:
		return nil, fmt.Errorf("invalid session sticky store type %q", config.Type)
	}
}

func NewMemoryStore() *MemoryStore {
	now := time.Now()
	store := &MemoryStore{
		stopCh:          make(chan struct{}),
		now:             time.Now,
		cleanupInterval: memoryCleanupInterval,
		nextCleanup:     now.Add(memoryCleanupInterval),
		bindings:        make(map[string]sessionBinding),
	}
	go store.runCleanupLoop()
	return store
}

func (s *MemoryStore) Get(routeKey, sessionKey string) (types.NamespacedName, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := storeKey(routeKey, sessionKey)
	binding, ok := s.bindings[key]
	if !ok {
		return types.NamespacedName{}, false
	}
	if !binding.expiresAt.After(s.now()) {
		delete(s.bindings, key)
		return types.NamespacedName{}, false
	}
	return binding.pod, true
}

func (s *MemoryStore) Set(routeKey, sessionKey string, pod types.NamespacedName, ttl time.Duration) {
	if routeKey == "" || sessionKey == "" || pod.Name == "" || ttl <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.cleanupExpiredLocked(now)
	key := storeKey(routeKey, sessionKey)
	if existing, ok := s.bindings[key]; ok && existing.expiresAt.After(now) {
		if existing.pod == pod {
			existing.expiresAt = now.Add(ttl)
			s.bindings[key] = existing
		} else {
			klog.V(4).Infof("session sticky binding for route %s session %s already points to another pod; keeping existing binding",
				routeKey, HashSessionKey(sessionKey))
		}
		return
	}
	s.bindings[key] = sessionBinding{
		pod:       pod,
		expiresAt: now.Add(ttl),
	}
}

func (s *MemoryStore) Delete(routeKey, sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, storeKey(routeKey, sessionKey))
}

func (s *MemoryStore) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	return nil
}

func (s *MemoryStore) runCleanupLoop() {
	ticker := time.NewTicker(memoryCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cleanupExpired()
		case <-s.stopCh:
			return
		}
	}
}

func (s *MemoryStore) cleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(s.now())
}

func (s *MemoryStore) cleanupExpiredLocked(now time.Time) {
	if s.cleanupInterval <= 0 || now.Before(s.nextCleanup) {
		return
	}
	for key, binding := range s.bindings {
		if !binding.expiresAt.After(now) {
			delete(s.bindings, key)
		}
	}
	s.nextCleanup = now.Add(s.cleanupInterval)
}

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(address, password string) (*RedisStore, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("redis address is required")
	}
	client := redis.NewClient(&redis.Options{
		Addr:         address,
		Password:     password,
		PoolSize:     32,
		MinIdleConns: 4,
		IdleTimeout:  5 * time.Minute,
	})
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}
	return &RedisStore{client: client}, nil
}

func (s *RedisStore) Get(routeKey, sessionKey string) (types.NamespacedName, bool) {
	if s == nil || s.client == nil {
		return types.NamespacedName{}, false
	}
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	value, err := s.client.Get(ctx, redisStoreKey(routeKey, sessionKey)).Result()
	if err == redis.Nil {
		return types.NamespacedName{}, false
	}
	if err != nil {
		klog.Errorf("failed to get session sticky binding from redis: %v", err)
		return types.NamespacedName{}, false
	}
	namespace, name, ok := strings.Cut(value, "/")
	if !ok || namespace == "" || name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, true
}

func (s *RedisStore) Set(routeKey, sessionKey string, pod types.NamespacedName, ttl time.Duration) {
	if s == nil || s.client == nil || routeKey == "" || sessionKey == "" || pod.Name == "" || ttl <= 0 {
		return
	}
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	value := pod.Namespace + "/" + pod.Name

	if err := s.client.Set(ctx, redisStoreKey(routeKey, sessionKey), value, ttl).Err(); err != nil {
		klog.Errorf("failed to set session sticky binding in redis: %v", err)
	}
}

func (s *RedisStore) Delete(routeKey, sessionKey string) {
	if s == nil || s.client == nil {
		return
	}
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	if err := s.client.Del(ctx, redisStoreKey(routeKey, sessionKey)).Err(); err != nil {
		klog.Errorf("failed to delete session sticky binding from redis: %v", err)
	}
}

func (s *RedisStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func storeKey(routeKey, sessionKey string) string {
	return routeKey + "\x00" + HashSessionKey(sessionKey)
}

func redisStoreKey(routeKey, sessionKey string) string {
	return "kthena:router:sessionsticky:" + routeKey + ":" + HashSessionKey(sessionKey)
}

func contextWithRedisTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), redisOperationTimeout)
}

// HashSessionKey keeps raw session material out of in-memory keys, Redis keys,
// and logs because session keys may come from headers, cookies, or JWT claims.
func HashSessionKey(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:])
}

func RouteKey(route *v1alpha1.ModelRoute) string {
	if route == nil {
		return ""
	}
	return route.Namespace + "/" + route.Name
}

func TTL(spec *v1alpha1.SessionSticky) time.Duration {
	seconds := defaultSessionAffinitySeconds
	if spec != nil && spec.SessionAffinitySeconds != nil {
		seconds = *spec.SessionAffinitySeconds
	}
	if seconds < 1 {
		seconds = defaultSessionAffinitySeconds
	}
	return time.Duration(seconds) * time.Second
}

func ExtractKey(c *gin.Context, spec *v1alpha1.SessionSticky, extractor ClaimExtractor) string {
	if c == nil || c.Request == nil || spec == nil || len(spec.Sources) == 0 {
		return ""
	}
	for _, source := range spec.Sources {
		value := extractFromSource(c, source, extractor)
		if value != "" {
			return value
		}
	}
	return ""
}

func extractFromSource(c *gin.Context, source v1alpha1.SessionKeySource, extractor ClaimExtractor) string {
	switch source.Type {
	case v1alpha1.SessionKeySourceHeader:
		return strings.TrimSpace(c.Request.Header.Get(source.Name))
	case v1alpha1.SessionKeySourceQuery:
		return strings.TrimSpace(c.Request.URL.Query().Get(source.Name))
	case v1alpha1.SessionKeySourceCookie:
		cookie, err := c.Request.Cookie(source.Name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(cookie.Value)
	case v1alpha1.SessionKeySourceJWTClaim:
		if extractor == nil {
			return ""
		}
		value, err := extractor.ExtractStringClaim(c, source.Name)
		if err != nil {
			klog.V(4).Infof("failed to extract JWT session claim %q: %v", source.Name, err)
			return ""
		}
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func IndexPodsByName(pods []*datastore.PodInfo) map[types.NamespacedName]*datastore.PodInfo {
	index := make(map[types.NamespacedName]*datastore.PodInfo, len(pods))
	for _, candidate := range pods {
		if candidate == nil || candidate.Pod == nil {
			continue
		}
		index[types.NamespacedName{Namespace: candidate.Pod.Namespace, Name: candidate.Pod.Name}] = candidate
	}
	return index
}
