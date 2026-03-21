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

package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cespare/xxhash"
	"github.com/go-redis/redis/v8"
	"github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
	"k8s.io/klog/v2"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// GlobalRateLimiter provides distributed rate limiting capabilities across multiple
// router instances using Redis as a centralized coordination layer.
type GlobalRateLimiter struct {
	client    *redis.Client
	keyPrefix string
	modelName string
	tokenType string
	limit     uint32
	unit      networkingv1alpha1.RateLimitUnit
	burst     int

	// Performance optimization: local token batching
	mu          sync.Mutex
	localTokens *lru.Cache[string, float64]
	batchSize   int
	sf          singleflight.Group
}

// NewGlobalRateLimiter creates a new GlobalRateLimiter instance
func NewGlobalRateLimiter(client *redis.Client, keyPrefix, modelName, tokenType string, limit uint32, unit networkingv1alpha1.RateLimitUnit) *GlobalRateLimiter {
	// Set batch size to 10% of the limit, with a minimum of 1 and maximum of 100
	batchSize := int(limit / 10)
	if batchSize < 1 {
		batchSize = 1
	}
	if batchSize > 100 {
		batchSize = 100
	}

	// Create LRU cache for local tokens to prevent unbounded memory growth from unique user IDs.
	// Capacity of 10000 users should be sufficient for most deployments.
	cache, _ := lru.New[string, float64](10000)

	return &GlobalRateLimiter{
		client:      client,
		keyPrefix:   keyPrefix,
		modelName:   modelName,
		tokenType:   tokenType,
		limit:       limit,
		unit:        unit,
		burst:       int(limit),
		localTokens: cache,
		batchSize:   batchSize,
	}
}

// AllowN implements Limiter interface
func (g *GlobalRateLimiter) AllowN(now time.Time, n int) bool {
	return g.AllowNWithUser(now, n, "")
}

// AllowNWithUser implements Limiter interface with user-based partitioning and token batching.
// It uses singleflight to prevent the "thundering herd" problem when the local cache is empty.
func (g *GlobalRateLimiter) AllowNWithUser(now time.Time, n int, userID string) bool {
	g.mu.Lock()
	if tokens, ok := g.localTokens.Get(userID); ok && tokens >= float64(n) {
		g.localTokens.Add(userID, tokens-float64(n))
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()

	// Use singleflight to ensure only one batch fetch happens concurrently for the same user
	res, _, _ := g.sf.Do(userID, func() (interface{}, error) {
		return g.fetchBatchFromRedis(n, userID), nil
	})

	if res.(bool) {
		// After a successful batch fetch by another goroutine, we should check our local cache again
		g.mu.Lock()
		defer g.mu.Unlock()
		if tokens, ok := g.localTokens.Get(userID); ok && tokens >= float64(n) {
			g.localTokens.Add(userID, tokens-float64(n))
			return true
		}
	}

	return false
}

func (g *GlobalRateLimiter) fetchBatchFromRedis(n int, userID string) bool {
	requestedBatch := n
	if n < g.batchSize {
		requestedBatch = g.batchSize
	}

	key := g.getRedisKey(userID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Lua script implements a distributed token bucket with batching support.
	// KEYS[1]: Redis key for the bucket
	// ARGV[1]: requested_tokens (the batch size we want to fetch)
	// ARGV[2]: capacity (burst limit)
	// ARGV[3]: refill_rate (tokens per second)
	// ARGV[4]: expire_seconds (TTL for the bucket state)
	// ARGV[5]: min_required (the absolute minimum tokens needed for the current request)
	luaScript := `
		local key = KEYS[1]
		local requested_tokens = tonumber(ARGV[1])
		local capacity = tonumber(ARGV[2])
		local refill_rate = tonumber(ARGV[3])
		local expire_seconds = tonumber(ARGV[4])
		local min_required = tonumber(ARGV[5])
		
		-- Use Redis server time for consistent time-based refill across multiple router instances
		local time_result = redis.call('time')
		local current_time = tonumber(time_result[1]) + tonumber(time_result[2]) / 1000000
		
		-- Load existing state or initialize with full capacity
		local current_tokens = tonumber(redis.call('hget', key, 'tokens')) or capacity
		local last_update = tonumber(redis.call('hget', key, 'last_update')) or current_time
		
		-- Refill tokens based on elapsed time
		local time_passed = math.max(0, current_time - last_update)
		local tokens_to_add = time_passed * refill_rate
		current_tokens = math.min(capacity, current_tokens + tokens_to_add)
		
		local tokens_returned = 0
		if current_tokens >= requested_tokens then
			-- Case 1: Full batch available
			tokens_returned = requested_tokens
			current_tokens = current_tokens - requested_tokens
		elseif current_tokens >= min_required then
			-- Case 2: Partial batch available (but enough for the original request)
			tokens_returned = current_tokens
			current_tokens = 0
		else
			-- Case 3: Not even enough for the minimum required tokens
			tokens_returned = 0
		end
		
		-- Persist state back to Redis
		redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
		redis.call('expire', key, expire_seconds)
		
		return tostring(tokens_returned)
	`

	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, requestedBatch, g.burst, refillRate, expireSeconds, n)

	if result.Err() != nil {
		klog.Errorf("failed to execute token bucket lua script: %v", result.Err())
		return false
	}

	// Parse as string to avoid precision loss or type confusion between int/float in Redis/Lua
	valStr, ok := result.Val().(string)
	if !ok {
		klog.Errorf("unexpected internal result type from lua script: %T", result.Val())
		return false
	}

	var tokensFetched float64
	fmt.Sscanf(valStr, "%f", &tokensFetched)

	if tokensFetched >= float64(n) {
		g.mu.Lock()
		current, _ := g.localTokens.Get(userID)
		g.localTokens.Add(userID, current+tokensFetched)
		g.mu.Unlock()
		return true
	}

	return false
}

func (g *GlobalRateLimiter) getRedisKey(userID string) string {
	if userID == "" {
		return fmt.Sprintf("%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType)
	}
	// Use xxhash to normalize userID and prevent injection/unbounded key names
	h := xxhash.New()
	h.Write([]byte(userID))
	userHash := fmt.Sprintf("%x", h.Sum64())
	return fmt.Sprintf("%s:%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType, userHash)
}

// Tokens implements Limiter interface
func (g *GlobalRateLimiter) Tokens() float64 {
	return g.TokensWithUser("")
}

// TokensWithUser returns the estimated number of tokens currently available for a specific user
func (g *GlobalRateLimiter) TokensWithUser(userID string) float64 {
	g.mu.Lock()
	local, _ := g.localTokens.Get(userID)
	g.mu.Unlock()

	key := g.getRedisKey(userID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ARGV[1]: capacity
	// ARGV[2]: refill_rate
	// ARGV[3]: expire_seconds
	luaScript := `
		local key = KEYS[1]
		local capacity = tonumber(ARGV[1])
		local refill_rate = tonumber(ARGV[2])
		local expire_seconds = tonumber(ARGV[3])
		
		local time_result = redis.call('time')
		local current_time = tonumber(time_result[1]) + tonumber(time_result[2]) / 1000000
		
		local current_tokens = tonumber(redis.call('hget', key, 'tokens')) or capacity
		local last_update = tonumber(redis.call('hget', key, 'last_update')) or current_time
		
		local time_passed = math.max(0, current_time - last_update)
		local tokens_to_add = time_passed * refill_rate
		
		local available_tokens = math.min(capacity, current_tokens + tokens_to_add)
		
		-- Update state to ensure precision for subsequent calls
		redis.call('hset', key, 'tokens', available_tokens, 'last_update', current_time)
		redis.call('expire', key, expire_seconds)
		
		return tostring(available_tokens)
	`

	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, g.burst, refillRate, expireSeconds)

	if result.Err() != nil {
		klog.Errorf("failed to execute tokens check lua script: %v", result.Err())
		return local
	}

	valStr, ok := result.Val().(string)
	if !ok {
		klog.Errorf("unexpected internal result type from tokens lua script: %T", result.Val())
		return local
	}

	var tokens float64
	fmt.Sscanf(valStr, "%f", &tokens)

	return local + tokens
}

// getRefillRate calculates the token refill rate per second
func (g *GlobalRateLimiter) getRefillRate() float64 {
	duration := getTimeUnitDuration(g.unit)
	return float64(g.limit) / duration.Seconds()
}

// getExpireSeconds calculates appropriate expire time based on rate limit unit
func (g *GlobalRateLimiter) getExpireSeconds() int {
	duration := getTimeUnitDuration(g.unit)
	expireSeconds := int(duration.Seconds() * 3)

	if expireSeconds < 600 {
		expireSeconds = 600
	} else if expireSeconds > 7776000 {
		expireSeconds = 7776000
	}

	return expireSeconds
}
