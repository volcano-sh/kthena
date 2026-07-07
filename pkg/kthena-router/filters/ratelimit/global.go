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
	"time"

	"github.com/go-redis/redis/v8"
	"k8s.io/klog/v2"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// GlobalRateLimiter Architecture and Algorithm Overview
//
// The GlobalRateLimiter provides distributed rate limiting capabilities across multiple
// router instances using Redis as a centralized coordination layer. This ensures
// consistent rate limiting behavior regardless of which router instance handles
// a particular request.
//
// Architecture:
// ┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
// │   Router-1     │    │   Router-2     │    │   Router-N     │
// │ ┌─────────────┐ │    │ ┌─────────────┐ │    │ ┌─────────────┐ │
// │ │GlobalRateL..│ │    │ │GlobalRateL..│ │    │ │GlobalRateL..│ │
// │ └─────────────┘ │    │ └─────────────┘ │    │ └─────────────┘ │
// └─────────┬───────┘    └─────────┬───────┘    └─────────┬───────┘
//           │                      │                      │
//           └──────────────────────┼──────────────────────┘
//                                  │
//                         ┌─────────▼───────┐
//                         │   Redis Cluster  │
//                         │  (Token Buckets) │
//                         └─────────────────┘
//
// Algorithm: Token Bucket
// The implementation uses the Token Bucket algorithm with the following characteristics:
//
// 1. Bucket Capacity: Maximum number of tokens the bucket can hold (burst allowance)
// 2. Refill Rate: Rate at which tokens are added to the bucket (tokens per second)
// 3. Token Consumption: Each request consumes a specified number of tokens
// 4. Atomicity: All operations use Redis Lua scripts for atomic execution
//
// Key Features:
// - Distributed consistency: All router instances share the same token bucket state
// - Time-based refill: Tokens are added based on elapsed time since last access
// - Burst handling: Allows temporary traffic spikes up to bucket capacity
// - Memory efficiency: Unused buckets automatically expire to save Redis memory
// - Atomic operations: Lua scripts prevent race conditions in concurrent access
//
// Token Bucket Formula:
//   available_tokens = min(capacity, current_tokens + (time_elapsed * refill_rate))
//   if available_tokens >= requested_tokens:
//       consume tokens and allow request
//   else:
//       reject request (rate limited)

// GlobalRateLimiter implements Limiter interface using Redis
type GlobalRateLimiter struct {
	client    *redis.Client
	keyPrefix string
	modelName string
	tokenType string
	limit     uint32
	unit      networkingv1alpha1.RateLimitUnit
	burst     int
}

// NewGlobalRateLimiter creates a new GlobalRateLimiter instance
func NewGlobalRateLimiter(client *redis.Client, keyPrefix, modelName, tokenType string, limit uint32, unit networkingv1alpha1.RateLimitUnit) *GlobalRateLimiter {
	return &GlobalRateLimiter{
		client:    client,
		keyPrefix: keyPrefix,
		modelName: modelName,
		tokenType: tokenType,
		limit:     limit,
		unit:      unit,
		burst:     int(limit),
	}
}

// AllowN implements Limiter interface using token bucket algorithm
func (g *GlobalRateLimiter) AllowN(now time.Time, n int) bool {
	key := fmt.Sprintf("%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Use Redis Lua script for atomic token bucket rate limiting operations
	// This script implements the classic Token Bucket algorithm, ensuring rate limiting
	// accuracy in distributed environments
	//
	// How it works:
	// 1. Token bucket generates tokens at a fixed rate (refill_rate), storing up to capacity tokens
	// 2. Each request consumes requested_tokens number of tokens
	// 3. If insufficient tokens in bucket, request is rejected; otherwise tokens are deducted and request is allowed
	// 4. All operations are executed atomically in Redis to avoid concurrency issues
	luaScript := `
		-- Input parameters
		local key = KEYS[1]                           -- Redis key name for storing token bucket state
		local requested_tokens = tonumber(ARGV[1])    -- Number of tokens to consume for this request
		local capacity = tonumber(ARGV[2])            -- Maximum capacity of the token bucket
		local refill_rate = tonumber(ARGV[3])         -- Token refill rate (tokens per second)
		local expire_seconds = tonumber(ARGV[4])      -- Expiration time for Redis key (seconds)
		
		-- Get current time from Redis for consistency across distributed systems
		local time_result = redis.call('time')
		local current_time = tonumber(time_result[1]) + tonumber(time_result[2]) / 1000000
		
		-- Get current token bucket state
		-- If first access, use default values: tokens=capacity, last_update=current_time
		local current_tokens = tonumber(redis.call('hget', key, 'tokens')) or capacity
		local last_update = tonumber(redis.call('hget', key, 'last_update')) or current_time
		
		-- Calculate tokens to add based on time elapsed
		-- time_passed: seconds elapsed since last update
		-- tokens_to_add: new tokens calculated based on refill rate
		local time_passed = math.max(0, current_time - last_update)
		local tokens_to_add = time_passed * refill_rate
		
		-- Update current token count, ensuring it doesn't exceed bucket capacity
		current_tokens = math.min(capacity, current_tokens + tokens_to_add)
		
		-- Check if current tokens are sufficient for this request
		if current_tokens >= requested_tokens then
			-- Sufficient tokens: deduct required tokens
			current_tokens = current_tokens - requested_tokens
			
			-- Atomically update token bucket state in Redis
			redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
			redis.call('expire', key, expire_seconds)
			
			return 1 -- Return 1 to indicate request is allowed
		else
			-- Insufficient tokens: reject request, but still update bucket state for time synchronization
			redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
			redis.call('expire', key, expire_seconds)
			
			return 0 -- Return 0 to indicate request is rate limited
		end
	`

	// Calculate refill rate (tokens per second) and expire time
	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, n, g.burst, refillRate, expireSeconds)

	if result.Err() != nil {
		klog.Errorf("failed to execute token bucket lua script: %v", result.Err())
		return false
	}

	allowed, ok := result.Val().(int64)
	if !ok {
		klog.Errorf("unexpected result type from lua script: %T", result.Val())
		return false
	}

	return allowed == 1
}

// getRefillRate calculates the token refill rate per second
func (g *GlobalRateLimiter) getRefillRate() float64 {
	duration := getTimeUnitDuration(g.unit)
	return float64(g.limit) / duration.Seconds()
}

// getExpireSeconds calculates appropriate expire time based on rate limit unit
//
// Redis Key Expiration Strategy:
//
// Why we need expiration:
// 1. Memory efficiency: Prevents Redis from accumulating unused token bucket keys indefinitely
// 2. Automatic cleanup: Removes stale rate limiting state for inactive models/users
// 3. Resource management: Ensures Redis memory usage stays bounded in production
//
// Expiration time calculation principle:
// - Base duration: 3x the rate limit time unit (e.g., if unit is "minute", expire in 3 minutes)
// - Rationale: Allows sufficient time for token bucket refill cycles while ensuring cleanup
//
// Why 3x multiplier:
// - 1x would be too aggressive, might expire active buckets during token refill
// - 2x provides minimal safety margin
// - 3x ensures bucket survives temporary traffic gaps while still enabling timely cleanup
// - Strikes balance between memory efficiency and functional correctness
//
// Boundary considerations:
// - Minimum 10 minutes: Prevents excessive Redis operations for very short time units
// - Maximum 90 days: Avoids extremely long-lived keys for very long time units
// - These bounds ensure reasonable Redis memory management regardless of configuration
func (g *GlobalRateLimiter) getExpireSeconds() int {
	duration := getTimeUnitDuration(g.unit)
	// Set expire time to 3x the rate limit unit duration, with reasonable bounds
	expireSeconds := int(duration.Seconds() * 3)

	// Set reasonable bounds
	if expireSeconds < 600 { // Minimum 10 minutes
		expireSeconds = 600
	} else if expireSeconds > 7776000 { // Maximum 90 days
		expireSeconds = 7776000
	}

	return expireSeconds
}

// Tokens returns the estimated number of tokens currently available
func (g *GlobalRateLimiter) Tokens() float64 {
	key := fmt.Sprintf("%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Lua script to get current available tokens without consuming any
	// This script implements a read-only query for the token bucket state, updating
	// the bucket's time-based refill but not consuming tokens
	//
	// Use case: Check available tokens for monitoring, debugging, or deciding
	// whether to make a request before actually attempting it
	luaScript := `
		-- Input parameters
		local key = KEYS[1]                           -- Redis key name for the token bucket
		local capacity = tonumber(ARGV[1])            -- Maximum capacity of the token bucket
		local refill_rate = tonumber(ARGV[2])         -- Token refill rate (tokens per second)
		local expire_seconds = tonumber(ARGV[3])      -- Expiration time for Redis key (seconds)
		
		-- Get current time from Redis for consistency across distributed systems
		local time_result = redis.call('time')
		local current_time = tonumber(time_result[1]) + tonumber(time_result[2]) / 1000000
		
		-- Get current token bucket state from Redis
		-- If bucket doesn't exist (first access), use default values
		local current_tokens = tonumber(redis.call('hget', key, 'tokens')) or capacity
		local last_update = tonumber(redis.call('hget', key, 'last_update')) or current_time
		
		-- Calculate tokens to add based on elapsed time since last update
		-- This ensures the bucket reflects the proper state even for read-only operations
		local time_passed = math.max(0, current_time - last_update)
		local tokens_to_add = time_passed * refill_rate
		
		-- Calculate current available tokens, ensuring we don't exceed bucket capacity
		local available_tokens = math.min(capacity, current_tokens + tokens_to_add)
		
		-- Update bucket state to maintain accurate timing for subsequent operations
		-- Even though this is a read-only query, we update the state to keep the
		-- token bucket synchronized with real time
		redis.call('hset', key, 'tokens', available_tokens, 'last_update', current_time)
		redis.call('expire', key, expire_seconds)
		
		-- Return the number of tokens currently available in the bucket
		return available_tokens
	`

	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, g.burst, refillRate, expireSeconds)

	if result.Err() != nil {
		klog.Errorf("failed to execute tokens check lua script: %v", result.Err())
		return 0
	}

	tokens, ok := result.Val().(float64)
	if !ok {
		// Try to convert from int64 to float64
		if tokensInt, ok := result.Val().(int64); ok {
			return float64(tokensInt)
		}
		klog.Errorf("unexpected result type from tokens lua script: %T", result.Val())
		return 0
	}

	return tokens
}

func (g *GlobalRateLimiter) ReconcileN(now time.Time, delta int) (bool, error) {
	key := fmt.Sprintf("%s:%s:%s", g.keyPrefix, g.modelName, g.tokenType)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	luaScript := `
		local key = KEYS[1]
		local delta = tonumber(ARGV[1])         -- can be negative (refund) or positive (extra debit)
		local capacity = tonumber(ARGV[2])
		local refill_rate = tonumber(ARGV[3])
		local expire_seconds = tonumber(ARGV[4])

		local time_result = redis.call('time')
		local current_time = tonumber(time_result[1]) + tonumber(time_result[2]) / 1000000

		local current_tokens = tonumber(redis.call('hget', key, 'tokens')) or capacity
		local last_update = tonumber(redis.call('hget', key, 'last_update')) or current_time

		local time_passed = math.max(0, current_time - last_update)
		local tokens_to_add = time_passed * refill_rate
		current_tokens = math.min(capacity, current_tokens + tokens_to_add)

		-- apply the true-up; cap at capacity, allow negative for temporary debt
		current_tokens = math.min(capacity, current_tokens - delta)

		redis.call('hset', key, 'tokens', current_tokens, 'last_update', current_time)
		redis.call('expire', key, expire_seconds)

		return current_tokens
	`

	refillRate := g.getRefillRate()
	expireSeconds := g.getExpireSeconds()

	result := g.client.Eval(ctx, luaScript, []string{key}, delta, g.burst, refillRate, expireSeconds)
	if result.Err() != nil {
		return false, fmt.Errorf("failed to reconcile tokens: %w", result.Err())
	}
	return true, nil
}
