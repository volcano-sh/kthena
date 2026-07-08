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
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func setupMiniRedis(t *testing.T) (*miniredis.Miniredis, *networkingv1alpha1.RedisConfig) {
	mr, err := miniredis.Run()
	require.NoError(t, err)

	config := &networkingv1alpha1.RedisConfig{
		Address: mr.Addr(),
	}

	return mr, config
}

func TestTokenRateLimiter_Global(t *testing.T) {
	mr, redisConfig := setupMiniRedis(t)
	defer mr.Close()

	rl := NewTokenRateLimiter()
	model := "test-model"
	prompt := "hello world" // Should be ~3 tokens
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	// Configure global rate limiting
	globalConfig := &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
		Global: &networkingv1alpha1.GlobalRateLimit{
			Redis: redisConfig,
		},
	}

	err := rl.AddOrUpdateLimiter(model, globalConfig)
	require.NoError(t, err)

	// Should allow multiple requests within limit
	for i := 0; i < 3; i++ {
		err := rl.RateLimit(model, prompt)
		assert.NoError(t, err, "Request %d should be allowed", i)
	}

	// Should be rate limited after exceeding limit
	err = rl.RateLimit(model, prompt)
	assert.Error(t, err, "Should be rate limited after exceeding limit")
	assert.IsType(t, &InputRateLimitExceededError{}, err)
}

func TestTokenRateLimiter_LocalVsGlobal(t *testing.T) {
	mr, redisConfig := setupMiniRedis(t)
	defer mr.Close()

	rl := NewTokenRateLimiter()
	localModel := "local-model"
	globalModel := "global-model"
	prompt := "test prompt"
	tokens := uint32(5)
	unit := networkingv1alpha1.Second

	// Configure local rate limiting
	localConfig := &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
		// No Global field = local rate limiting
	}

	// Configure global rate limiting
	globalConfig := &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
		Global: &networkingv1alpha1.GlobalRateLimit{
			Redis: redisConfig,
		},
	}

	err := rl.AddOrUpdateLimiter(localModel, localConfig)
	require.NoError(t, err)

	err = rl.AddOrUpdateLimiter(globalModel, globalConfig)
	require.NoError(t, err)

	// Both should allow initial requests
	err = rl.RateLimit(localModel, prompt)
	assert.NoError(t, err)

	err = rl.RateLimit(globalModel, prompt)
	assert.NoError(t, err)

	// Use up local tokens
	err = rl.RateLimit(localModel, prompt)
	assert.Error(t, err, "Local model should be rate limited")

	// Use up global tokens
	err = rl.RateLimit(globalModel, prompt)
	assert.Error(t, err, "Global model should be rate limited")
}

func TestTokenRateLimiter_OutputTokens(t *testing.T) {
	mr, redisConfig := setupMiniRedis(t)
	defer mr.Close()

	rl := NewTokenRateLimiter()
	model := "test-model"
	outputTokens := uint32(50)
	unit := networkingv1alpha1.Second

	// Configure output token limiting
	config := &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
		Global: &networkingv1alpha1.GlobalRateLimit{
			Redis: redisConfig,
		},
	}

	err := rl.AddOrUpdateLimiter(model, config)
	require.NoError(t, err)

	// Record output tokens (should not block since it's async)
	rl.RecordOutputTokens(model, 25)
	rl.RecordOutputTokens(model, 30) // Total: 55, over limit

	// Give some time for async recording
	time.Sleep(100 * time.Millisecond)

	// Verify the tokens were recorded in Redis
	key := "kthena:ratelimit:test-model:output"
	exists := mr.Exists(key)
	assert.True(t, exists)
}

func TestTokenRateLimiter_GlobalDeleteLimiter(t *testing.T) {
	mr, redisConfig := setupMiniRedis(t)
	defer mr.Close()

	rl := NewTokenRateLimiter()
	model := "test-model"
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	// Add a rate limiter
	config := &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
		Global: &networkingv1alpha1.GlobalRateLimit{
			Redis: redisConfig,
		},
	}

	err := rl.AddOrUpdateLimiter(model, config)
	require.NoError(t, err)

	// Verify it works
	err = rl.RateLimit(model, "test")
	assert.NoError(t, err)

	// Delete the limiter
	rl.DeleteLimiter(model)

	// Should now allow unlimited requests (no limiter configured)
	for i := 0; i < 10; i++ {
		err = rl.RateLimit(model, "test")
		assert.NoError(t, err, "Request %d should be allowed after deletion", i)
	}
}

func TestTokenRateLimiter_RedisConnectionFailure(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	// Configure with non-existent Redis
	config := &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
		Global: &networkingv1alpha1.GlobalRateLimit{
			Redis: &networkingv1alpha1.RedisConfig{
				Address: "localhost:9999", // Non-existent Redis
			},
		},
	}

	err := rl.AddOrUpdateLimiter(model, config)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to redis")
}

// newTestGlobalRateLimiter creates a GlobalRateLimiter directly with a miniredis-backed client.
func newTestGlobalRateLimiter(t *testing.T, mr *miniredis.Miniredis, modelName, tokenType string, limit uint32, unit networkingv1alpha1.RateLimitUnit) *GlobalRateLimiter {
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
	t.Cleanup(func() { client.Close() })
	return NewGlobalRateLimiter(client, "kthena:ratelimit", modelName, tokenType, limit, unit)
}

func TestGlobalRateLimiter_Tokens_InitialCapacity(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 100, networkingv1alpha1.Second)

	// First call to Tokens() should return the full bucket capacity
	tokens := limiter.Tokens()
	assert.Equal(t, float64(100), tokens, "Tokens() should return full capacity on first call")
}

func TestGlobalRateLimiter_Tokens_AfterConsumption(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 100, networkingv1alpha1.Second)

	// Consume 30 tokens
	allowed := limiter.AllowN(time.Now(), 30)
	assert.True(t, allowed, "AllowN(30) should be allowed from capacity of 100")

	// Tokens() should reflect the consumed tokens (approximately 70)
	tokens := limiter.Tokens()
	assert.InDelta(t, 70, tokens, 5, "Tokens() should be approximately 70 after consuming 30 out of 100")
}

func TestGlobalRateLimiter_Tokens_AfterFullConsumption(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 10, networkingv1alpha1.Second)

	// Consume all tokens
	allowed := limiter.AllowN(time.Now(), 10)
	assert.True(t, allowed)

	// Tokens() should be near 0
	tokens := limiter.Tokens()
	assert.InDelta(t, 0, tokens, 2, "Tokens() should be near 0 after consuming all tokens")

	// Trying to consume more should fail
	allowed = limiter.AllowN(time.Now(), 1)
	assert.False(t, allowed, "AllowN(1) should fail after all tokens consumed")
}

func TestGlobalRateLimiter_Tokens_CorrectArgsPassed(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 50, networkingv1alpha1.Second)

	// Call Tokens() which executes the Lua script
	tokens := limiter.Tokens()
	assert.Equal(t, float64(50), tokens, "Tokens() should return capacity when bucket is full")

	// Verify the Redis key TTL is set to a reasonable expire value (not a huge timestamp).
	// The bug fix removed the unused currentTime argument, which was being consumed
	// as expire_seconds in the Lua script. With the bug, TTL would be ~1.7 billion seconds
	// (the Unix timestamp). With the fix, TTL should be the actual expire value (600 seconds minimum).
	key := "kthena:ratelimit:test-model:input"
	ttl := mr.TTL(key)
	assert.True(t, ttl > 0, "Redis key should have a TTL set")
	assert.LessOrEqual(t, ttl, 90*24*time.Hour, "Redis key TTL should not exceed 90 days maximum")
	// For Second unit, expire is 600 seconds (minimum bound)
	assert.InDelta(t, 600, ttl.Seconds(), 10, "TTL should be approximately 600 seconds for Second unit")
}

func TestGlobalRateLimiter_Tokens_ExpireSecondsPerUnit(t *testing.T) {
	tests := []struct {
		name           string
		unit           networkingv1alpha1.RateLimitUnit
		expectedExpire int // expected expire in seconds
	}{
		{
			name:           "Second unit uses minimum 600s",
			unit:           networkingv1alpha1.Second,
			expectedExpire: 600, // 1*3 = 3s, but minimum is 600
		},
		{
			name:           "Minute unit uses minimum 600s",
			unit:           networkingv1alpha1.Minute,
			expectedExpire: 600, // 60*3 = 180s, but minimum is 600
		},
		{
			name:           "Hour unit uses 3x duration",
			unit:           networkingv1alpha1.Hour,
			expectedExpire: 10800, // 3600*3 = 10800s
		},
		{
			name:           "Day unit uses 3x duration",
			unit:           networkingv1alpha1.Day,
			expectedExpire: 259200, // 86400*3 = 259200s
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, err := miniredis.Run()
			require.NoError(t, err)
			defer mr.Close()

			limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 100, tt.unit)

			// Call Tokens() to trigger the Lua script
			tokens := limiter.Tokens()
			assert.Equal(t, float64(100), tokens)

			// Verify TTL matches expected expire seconds
			key := "kthena:ratelimit:test-model:input"
			ttl := mr.TTL(key)
			assert.InDelta(t, tt.expectedExpire, ttl.Seconds(), 10,
				"TTL should be approximately %d seconds for %s unit", tt.expectedExpire, tt.name)
		})
	}
}

func TestGlobalRateLimiter_Tokens_ConnectionFailure(t *testing.T) {
	// Create a limiter with a non-existent Redis
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:9999",
	})
	defer client.Close()

	limiter := NewGlobalRateLimiter(client, "kthena:ratelimit", "model", "input", 10, networkingv1alpha1.Second)

	// Tokens() should return 0 on connection failure
	tokens := limiter.Tokens()
	assert.Equal(t, float64(0), tokens, "Tokens() should return 0 when Redis is unreachable")
}

func TestGlobalRateLimiter_Tokens_MultipleCallsConsistent(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 100, networkingv1alpha1.Second)

	// Multiple Tokens() calls without any consumption should return consistent values
	tokens1 := limiter.Tokens()
	tokens2 := limiter.Tokens()
	tokens3 := limiter.Tokens()

	assert.Equal(t, float64(100), tokens1)
	// Tokens may refill slightly between calls due to time passing, but should stay at capacity
	assert.InDelta(t, 100, tokens2, 1)
	assert.InDelta(t, 100, tokens3, 1)
}

func TestGlobalRateLimiter_Tokens_AllowNThenTokens(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 50, networkingv1alpha1.Second)

	// Consume tokens in multiple steps and verify Tokens() after each
	steps := []struct {
		consume       int
		expectedApprx float64
	}{
		{10, 40},
		{15, 25},
		{20, 5},
	}

	for i, step := range steps {
		allowed := limiter.AllowN(time.Now(), step.consume)
		assert.True(t, allowed, "step %d: AllowN(%d) should be allowed", i, step.consume)

		tokens := limiter.Tokens()
		assert.InDelta(t, step.expectedApprx, tokens, 5,
			"step %d: Tokens() should be approximately %.0f after consuming %d",
			i, step.expectedApprx, step.consume)
	}
}

func TestGlobalRateLimiter_Tokens_SharedState(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Create two limiter instances pointing to the same Redis key
	limiter1 := newTestGlobalRateLimiter(t, mr, "shared-model", "input", 100, networkingv1alpha1.Second)
	limiter2 := newTestGlobalRateLimiter(t, mr, "shared-model", "input", 100, networkingv1alpha1.Second)

	// limiter1 consumes tokens
	allowed := limiter1.AllowN(time.Now(), 60)
	assert.True(t, allowed)

	// limiter2 should see reduced tokens (distributed consistency)
	tokens := limiter2.Tokens()
	assert.InDelta(t, 40, tokens, 5, "Second limiter should see reduced tokens due to shared Redis state")
}

func TestGlobalRateLimiter_Tokens_DifferentModels(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiterA := newTestGlobalRateLimiter(t, mr, "model-a", "input", 100, networkingv1alpha1.Second)
	limiterB := newTestGlobalRateLimiter(t, mr, "model-b", "input", 50, networkingv1alpha1.Second)

	// Consume tokens from model A
	limiterA.AllowN(time.Now(), 80)

	// model B should still have full capacity
	tokensB := limiterB.Tokens()
	assert.Equal(t, float64(50), tokensB, "Model B should have full capacity, independent of model A")

	// model A should have reduced tokens
	tokensA := limiterA.Tokens()
	assert.InDelta(t, 20, tokensA, 5, "Model A should have reduced tokens after consumption")
}

func TestGlobalRateLimiter_Tokens_KeyFormat(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "my-model", "output", 100, networkingv1alpha1.Second)

	// Call Tokens() to create the key
	limiter.Tokens()

	// Verify the Redis key follows the expected format
	key := "kthena:ratelimit:my-model:output"
	assert.True(t, mr.Exists(key),
		"Redis key should be in format keyPrefix:modelName:tokenType")

	// Verify hash fields exist
	hkeys, err := mr.HKeys(key)
	require.NoError(t, err)
	assert.Contains(t, hkeys, "tokens", "Hash should contain 'tokens' field")
	assert.Contains(t, hkeys, "last_update", "Hash should contain 'last_update' field")
}

func TestGlobalRateLimiter_AllowN_CorrectExpireSeconds(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 50, networkingv1alpha1.Hour)

	// Call AllowN to create the key
	allowed := limiter.AllowN(time.Now(), 1)
	assert.True(t, allowed)

	// Verify the Redis key TTL matches the expected expire seconds for Hour unit
	key := "kthena:ratelimit:test-model:input"
	ttl := mr.TTL(key)
	// For Hour unit: 3600 * 3 = 10800 seconds
	assert.InDelta(t, 10800, ttl.Seconds(), 10,
		"AllowN should set correct TTL for Hour unit")
}

func TestGlobalRateLimiter_GetRefillRate(t *testing.T) {
	tests := []struct {
		name     string
		limit    uint32
		unit     networkingv1alpha1.RateLimitUnit
		expected float64
	}{
		{"10 per second", 10, networkingv1alpha1.Second, 10.0},
		{"60 per minute", 60, networkingv1alpha1.Minute, 1.0},
		{"3600 per hour", 3600, networkingv1alpha1.Hour, 1.0},
		{"100 per day", 100, networkingv1alpha1.Day, 100.0 / 86400.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := &GlobalRateLimiter{
				limit: tt.limit,
				unit:  tt.unit,
			}
			rate := limiter.getRefillRate()
			assert.InDelta(t, tt.expected, rate, 0.0001,
				"refill rate should match expected value")
		})
	}
}

func TestGlobalRateLimiter_GetExpireSeconds(t *testing.T) {
	tests := []struct {
		name     string
		unit     networkingv1alpha1.RateLimitUnit
		expected int
	}{
		{"Second - uses minimum 600", networkingv1alpha1.Second, 600},
		{"Minute - uses minimum 600", networkingv1alpha1.Minute, 600},
		{"Hour - 3x = 10800", networkingv1alpha1.Hour, 10800},
		{"Day - 3x = 259200", networkingv1alpha1.Day, 259200},
		{"Month - 3x = 7776000", networkingv1alpha1.Month, 7776000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := &GlobalRateLimiter{
				unit: tt.unit,
			}
			expireSeconds := limiter.getExpireSeconds()
			assert.Equal(t, tt.expected, expireSeconds,
				"expire seconds should match expected value")
		})
	}
}

func TestGlobalRateLimiter_TokensAndAllowN_ConsistentArgs(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 100, networkingv1alpha1.Second)

	// Call Tokens() first to initialize the bucket via the Tokens Lua script
	tokens := limiter.Tokens()
	assert.Equal(t, float64(100), tokens)

	key := "kthena:ratelimit:test-model:input"
	ttlAfterTokens := mr.TTL(key)

	// Call AllowN() which uses a separate Lua script
	allowed := limiter.AllowN(time.Now(), 10)
	assert.True(t, allowed)

	ttlAfterAllowN := mr.TTL(key)

	// Both should set similar TTLs (both use getExpireSeconds())
	assert.InDelta(t, ttlAfterTokens.Seconds(), ttlAfterAllowN.Seconds(), 10,
		"Tokens() and AllowN() should set consistent TTLs")

	// Verify TTL is reasonable (not a Unix timestamp)
	assert.Less(t, ttlAfterTokens.Seconds(), float64(7776001),
		"TTL should not be a Unix timestamp (should be ≤ 90 days)")
	assert.Less(t, ttlAfterAllowN.Seconds(), float64(7776001),
		"TTL should not be a Unix timestamp (should be ≤ 90 days)")
}

func TestGlobalRateLimiter_Tokens_IntAndFloatReturn(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Use integer capacity to test both int64 and float64 return paths
	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 50, networkingv1alpha1.Second)

	tokens := limiter.Tokens()
	// Whether the Lua script returns int or float, Tokens() should handle it correctly
	assert.True(t, tokens >= 0, "Tokens() should return a non-negative value")
	assert.InDelta(t, 50, tokens, 1, "Tokens() should return approximately the capacity")
}

func TestGlobalRateLimiter_NewGlobalRateLimiter(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer client.Close()

	limiter := NewGlobalRateLimiter(client, "prefix", "model", "input", 100, networkingv1alpha1.Second)

	assert.NotNil(t, limiter)
	assert.Equal(t, "prefix", limiter.keyPrefix)
	assert.Equal(t, "model", limiter.modelName)
	assert.Equal(t, "input", limiter.tokenType)
	assert.Equal(t, uint32(100), limiter.limit)
	assert.Equal(t, networkingv1alpha1.Second, limiter.unit)
	assert.Equal(t, 100, limiter.burst, "burst should equal limit")
}

func TestGlobalRateLimiter_AllowN_ExceedCapacity(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	limiter := newTestGlobalRateLimiter(t, mr, "test-model", "input", 10, networkingv1alpha1.Second)

	// Try to consume more tokens than capacity
	allowed := limiter.AllowN(time.Now(), 20)
	assert.False(t, allowed, "AllowN(20) should fail when capacity is 10")

	// Tokens should still be at capacity (request was rejected)
	tokens := limiter.Tokens()
	assert.InDelta(t, 10, tokens, 2, "Tokens should remain at capacity after rejected request")
}

func TestGlobalRateLimiter_AllowN_ConnectionFailure(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:9999",
	})
	defer client.Close()

	limiter := NewGlobalRateLimiter(client, "kthena:ratelimit", "model", "input", 10, networkingv1alpha1.Second)

	// AllowN should return false on connection failure
	allowed := limiter.AllowN(time.Now(), 1)
	assert.False(t, allowed, "AllowN should return false when Redis is unreachable")
}

func TestGlobalRateLimiter_Tokens_AllModelsIsolated(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	models := []struct {
		name     string
		capacity uint32
	}{
		{"model-a", 100},
		{"model-b", 200},
		{"model-c", 50},
	}

	limiters := make([]*GlobalRateLimiter, len(models))
	for i, m := range models {
		limiters[i] = newTestGlobalRateLimiter(t, mr, m.name, "input", m.capacity, networkingv1alpha1.Second)
	}

	// Consume different amounts from each
	limiters[0].AllowN(time.Now(), 80)
	limiters[1].AllowN(time.Now(), 50)
	// Don't consume from model-c

	// Verify each model has independent state
	tokensA := limiters[0].Tokens()
	tokensB := limiters[1].Tokens()
	tokensC := limiters[2].Tokens()

	assert.InDelta(t, 20, tokensA, 5, "model-a should have ~20 tokens remaining")
	assert.InDelta(t, 150, tokensB, 5, "model-b should have ~150 tokens remaining")
	assert.Equal(t, float64(50), tokensC, "model-c should have full capacity")

	// Verify separate Redis keys exist
	for _, m := range models {
		key := fmt.Sprintf("kthena:ratelimit:%s:input", m.name)
		assert.True(t, mr.Exists(key), "Redis key should exist for %s", m.name)
	}
}
