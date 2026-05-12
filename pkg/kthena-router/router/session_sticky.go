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

package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/auth"
)

const defaultSessionAffinitySeconds int32 = 10800
const redisOperationTimeout = 2 * time.Second

type sessionBinding struct {
	pod       types.NamespacedName
	expiresAt time.Time
}

type sessionStickyStore interface {
	Get(routeKey, sessionKey string) (types.NamespacedName, bool)
	Set(routeKey, sessionKey string, pod types.NamespacedName, ttl time.Duration)
	Delete(routeKey, sessionKey string)
	Close() error
}

type memorySessionStickyStore struct {
	mu       sync.Mutex
	now      func() time.Time
	bindings map[string]sessionBinding
}

func newMemorySessionStickyStore() *memorySessionStickyStore {
	return &memorySessionStickyStore{
		now:      time.Now,
		bindings: make(map[string]sessionBinding),
	}
}

func (s *memorySessionStickyStore) Get(routeKey, sessionKey string) (types.NamespacedName, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := sessionStoreKey(routeKey, sessionKey)
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

func (s *memorySessionStickyStore) Set(routeKey, sessionKey string, pod types.NamespacedName, ttl time.Duration) {
	if routeKey == "" || sessionKey == "" || pod.Name == "" || ttl <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for key, binding := range s.bindings {
		if !binding.expiresAt.After(now) {
			delete(s.bindings, key)
		}
	}
	s.bindings[sessionStoreKey(routeKey, sessionKey)] = sessionBinding{
		pod:       pod,
		expiresAt: now.Add(ttl),
	}
}

func (s *memorySessionStickyStore) Delete(routeKey, sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, sessionStoreKey(routeKey, sessionKey))
}

func (s *memorySessionStickyStore) Close() error {
	return nil
}

type redisSessionStickyStore struct {
	client *redis.Client
}

func newRedisSessionStickyStore(address, password string) (*redisSessionStickyStore, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("redis address is required")
	}
	client := redis.NewClient(&redis.Options{
		Addr:     address,
		Password: password,
	})
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}
	return &redisSessionStickyStore{client: client}, nil
}

func (s *redisSessionStickyStore) Get(routeKey, sessionKey string) (types.NamespacedName, bool) {
	if s == nil || s.client == nil {
		return types.NamespacedName{}, false
	}
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	value, err := s.client.Get(ctx, redisSessionStoreKey(routeKey, sessionKey)).Result()
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

func (s *redisSessionStickyStore) Set(routeKey, sessionKey string, pod types.NamespacedName, ttl time.Duration) {
	if s == nil || s.client == nil || ttl <= 0 {
		return
	}
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	value := pod.Namespace + "/" + pod.Name
	if err := s.client.Set(ctx, redisSessionStoreKey(routeKey, sessionKey), value, ttl).Err(); err != nil {
		klog.Errorf("failed to set session sticky binding in redis: %v", err)
	}
}

func (s *redisSessionStickyStore) Delete(routeKey, sessionKey string) {
	if s == nil || s.client == nil {
		return
	}
	ctx, cancel := contextWithRedisTimeout()
	defer cancel()
	if err := s.client.Del(ctx, redisSessionStoreKey(routeKey, sessionKey)).Err(); err != nil {
		klog.Errorf("failed to delete session sticky binding from redis: %v", err)
	}
}

func (s *redisSessionStickyStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func sessionStoreKey(routeKey, sessionKey string) string {
	return routeKey + "\x00" + hashSessionKey(sessionKey)
}

func redisSessionStoreKey(routeKey, sessionKey string) string {
	return "kthena:router:sessionsticky:" + routeKey + ":" + hashSessionKey(sessionKey)
}

func contextWithRedisTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), redisOperationTimeout)
}

func hashSessionKey(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:])
}

func routeSessionKey(route *v1alpha1.ModelRoute) string {
	if route == nil {
		return ""
	}
	return route.Namespace + "/" + route.Name
}

func sessionAffinityTTL(spec *v1alpha1.SessionSticky) time.Duration {
	seconds := defaultSessionAffinitySeconds
	if spec != nil && spec.SessionAffinitySeconds != nil {
		seconds = *spec.SessionAffinitySeconds
	}
	if seconds < 1 {
		seconds = defaultSessionAffinitySeconds
	}
	return time.Duration(seconds) * time.Second
}

func extractSessionKey(req *http.Request, spec *v1alpha1.SessionSticky, authenticator *auth.JWTAuthenticator) string {
	if req == nil || spec == nil || len(spec.Sources) == 0 {
		return ""
	}
	for _, source := range spec.Sources {
		value := extractSessionKeyFromSource(req, source, authenticator)
		if value != "" {
			return value
		}
	}
	return ""
}

func extractSessionKeyFromSource(req *http.Request, source v1alpha1.SessionKeySource, authenticator *auth.JWTAuthenticator) string {
	switch source.Type {
	case v1alpha1.SessionKeySourceHeader:
		return strings.TrimSpace(req.Header.Get(source.Name))
	case v1alpha1.SessionKeySourceQuery:
		return strings.TrimSpace(req.URL.Query().Get(source.Name))
	case v1alpha1.SessionKeySourceCookie:
		cookie, err := req.Cookie(source.Name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(cookie.Value)
	case v1alpha1.SessionKeySourceJWTClaim:
		if authenticator == nil {
			return ""
		}
		value, err := authenticator.ExtractStringClaim(req, source.Name)
		if err != nil {
			klog.V(4).Infof("failed to extract JWT session claim %q: %v", source.Name, err)
			return ""
		}
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func findPodByName(pods []*datastore.PodInfo, pod types.NamespacedName) (*datastore.PodInfo, bool) {
	for _, candidate := range pods {
		if candidate == nil || candidate.Pod == nil {
			continue
		}
		if candidate.Pod.Namespace == pod.Namespace && candidate.Pod.Name == pod.Name {
			return candidate, true
		}
	}
	return nil, false
}
