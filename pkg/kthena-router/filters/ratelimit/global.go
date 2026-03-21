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

	"github.com/go-redis/redis/v8"
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
	localTokens map[string]float64 // userID -> tokens
	batchSize   int
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

	return &GlobalRateLimiter{
		client:      client,
		keyPrefix:   keyPrefix,
		modelName:   modelName,
		tokenType:   tokenType,
		limit:       limit,
		unit:        unit,
		burst:       int(limit),
		localTokens: make(map[string]float64),
		batchSize:   batchSize,
	}
}

// AllowN implements Limiter interface
func (g *GlobalRateLimiter) AllowN(now time.Time, n int) bool {
	return g.AllowNWithUser(now, n, "")
}

// AllowNWithUser implements Limiter interface with user-based partitioning and token batching
func (g *GlobalRateLimiter) AllowNWithUser(now time.Time, n int, userID string) bool {
	g.mu.Lock()
	// Check if local tokens are sufficient
	if g.localTokens[userID] >= float64(n) {
		g.localTokens[userID] -= float64(n)
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()

	// If not sufficient, fetch from Redis in batches
	requestedBatch := n
	if n < g.batchSize {
		requestedBatch = g.batchSize
	}

	key := g.getRedisKey(userID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Lua script implemented to handle distributed token bucket with batching support
	luaScript := `
		local key = KEYS[1]
		local requested_tokens = tonumber(ARGV[1])
		local capacity = tonumber(ARGV[2])
		local refill_rate = tonumber(ARGV[3])
		local expire_seconds = tonumber(ARGV[4])
		local min_required = tonumber(ARGV[5])
		
		local time_result = redis.call('time')
		local current_time = tonumber(time_result[1]) + tonumber(time_result[2]) / 1000000
		
		local current_tokens = tonumber(redis.call('hget', key, 'tokens')) or capacity
		local last_update = tonumber(redis.call('hget', key, 'last_update')) or current_time
		
		local time_passed = math.max(0, current_time - last_update)
		local tokens_to_add = time_passed * refill_rate
		
		current_tokens = math.min(capacity, current_tokens + tokens_to_add)
		
		if current_tokens >= requested_tokens then
			current_tokens = current_tokens - requested_tokens
			redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
			redis.call('expire', key, expire_seconds)
			return requested_tokens
		elseif current_tokens >= min_required then
			local tokens_to_return = current_tokens
			current_tokens = 0
			redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
			redis.call('expire', key, expire_seconds)
			return tokens_to_return
		else
			redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
			redis.call('expire', key, expire_seconds)
			return 0
		end
	`

	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, requestedBatch, g.burst, refillRate, expireSeconds, n)

	if result.Err() != nil {
		klog.Errorf("failed to execute token bucket lua script: %v", result.Err())
		return false
	}

	tokensFetched, ok := result.Val().(int64)
	if !ok {
		// Lua might return float if we use division, but here it should be int
		if f, ok := result.Val().(float64); ok {
			tokensFetched = int64(f)
		} else {
			klog.Errorf("unexpected result type from lua script: %T", result.Val())
			return false
		}
	}

	if tokensFetched >= int64(n) {
		g.mu.Lock()
		g.localTokens[userID] += float64(tokensFetched - int64(n))
		g.mu.Unlock()
		return true
	}

	return false
}

func (g *GlobalRateLimiter) getRedisKey(userID string) string {
	if userID == "" {
		return fmt.Sprintf("%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType)
	}
	return fmt.Sprintf("%s:%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType, userID)
}

// Tokens implements Limiter interface
func (g *GlobalRateLimiter) Tokens() float64 {
	return g.TokensWithUser("")
}

// TokensWithUser returns the estimated number of tokens currently available for a specific user
func (g *GlobalRateLimiter) TokensWithUser(userID string) float64 {
	g.mu.Lock()
	local := g.localTokens[userID]
	g.mu.Unlock()

	key := g.getRedisKey(userID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

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
		
		redis.call('hset', key, 'tokens', available_tokens, 'last_update', current_time)
		redis.call('expire', key, expire_seconds)
		
		return available_tokens
	`

	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, g.burst, refillRate, expireSeconds)

	if result.Err() != nil {
		klog.Errorf("failed to execute tokens check lua script: %v", result.Err())
		return local
	}

	tokens, ok := result.Val().(float64)
	if !ok {
		if tokensInt, ok := result.Val().(int64); ok {
			tokens = float64(tokensInt)
		} else {
			klog.Errorf("unexpected result type from tokens lua script: %T", result.Val())
			return local
		}
	}

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
