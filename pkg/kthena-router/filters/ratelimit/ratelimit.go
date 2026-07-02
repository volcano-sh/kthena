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
	"golang.org/x/time/rate"
	"k8s.io/klog/v2"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/tokenizer"
)

type RateLimitExceededError struct{}

func (e *RateLimitExceededError) Error() string {
	return "rate limit exceeded"
}

type InputRateLimitExceededError struct{}

func (e *InputRateLimitExceededError) Error() string {
	return "input token rate limit exceeded"
}

type OutputRateLimitExceededError struct{}

func (e *OutputRateLimitExceededError) Error() string {
	return "output token rate limit exceeded"
}

// Limiter interface that both local and global rate limiters implement
// Only includes methods that are actually used
type Limiter interface {
	// AllowN reports whether n tokens may be consumed and consumes them if so
	AllowN(now time.Time, n int) bool
	// Tokens returns the number of tokens currently available
	Tokens() float64
}

// LimiterConfig stores the configuration for a rate limiter to detect changes
type LimiterConfig struct {
	Limiter            Limiter
	TokensPerUnit      uint32 // copied value (not pointer)
	Unit               networkingv1alpha1.RateLimitUnit
	IsGlobal           bool
	GlobalRedisAddress string // empty if local
}

// TokenRateLimiter provides rate limiting functionality for both input and output tokens
type TokenRateLimiter struct {
	mutex sync.RWMutex

	// Store input and output limiters separately for independent change detection
	inputLimiters  map[string]*LimiterConfig
	outputLimiters map[string]*LimiterConfig

	// Redis client for global rate limiting
	redisClient *redis.Client

	tokenizer tokenizer.Tokenizer
}

// LocalLimiter wraps golang.org/x/time/rate.Limiter to implement our Limiter interface
type LocalLimiter struct {
	*rate.Limiter
}

// NewLocalLimiter creates a new LocalLimiter
func NewLocalLimiter(limit rate.Limit, burst int) *LocalLimiter {
	return &LocalLimiter{
		Limiter: rate.NewLimiter(limit, burst),
	}
}

// Tokens returns the number of tokens currently available
func (l *LocalLimiter) Tokens() float64 {
	return l.Limiter.Tokens()
}

// NewTokenRateLimiter creates a new TokenRateLimiter instance
func NewTokenRateLimiter() *TokenRateLimiter {
	return &TokenRateLimiter{
		inputLimiters:  make(map[string]*LimiterConfig),
		outputLimiters: make(map[string]*LimiterConfig),
		tokenizer:      tokenizer.NewSimpleEstimateTokenizer(),
	}
}

// RateLimit checks if the request is within rate limits for both input and output tokens
func (r *TokenRateLimiter) RateLimit(model, prompt string) error {
	// Estimate input tokens
	tokens, err := r.tokenizer.CalculateTokenNum(prompt)
	if err != nil {
		klog.Errorf("failed to calculate token number: %v", err)
		tokens = len(prompt) / 4 // fallback estimation
	}

	r.mutex.RLock()
	inputConfig, hasInputLimit := r.inputLimiters[model]
	outputConfig, hasOutputLimit := r.outputLimiters[model]
	r.mutex.RUnlock()

	// Check input token rate limit
	if hasInputLimit && inputConfig.Limiter != nil &&
		!inputConfig.Limiter.AllowN(time.Now(), tokens) {
		return &InputRateLimitExceededError{}
	}

	// Check output token rate limit - verify tokens are available
	// Actual consumption happens in RecordOutputTokens() with proper reservation handling
	if hasOutputLimit && outputConfig.Limiter != nil {
		// Non-consuming check: ensure at least 1 token available
		if outputConfig.Limiter.Tokens() < 1 {
			return &OutputRateLimitExceededError{}
		}
	}

	return nil
}

// RecordOutputTokens records the actual output tokens consumed after response generation
func (r *TokenRateLimiter) RecordOutputTokens(model string, tokenCount int) {
	r.mutex.RLock()
	outputConfig, exists := r.outputLimiters[model]
	r.mutex.RUnlock()

	if !exists || outputConfig.Limiter == nil {
		return
	}
	if localLimiter, ok := outputConfig.Limiter.(*LocalLimiter); ok {
		// Reserve the actual number of tokens consumed
		res := localLimiter.Limiter.ReserveN(time.Now(), tokenCount)
		if !res.OK() {
			res.Cancel()
			return
		}
		// Reservation confirmed - tokens are consumed and will be properly tracked
		return
	}

	if tokenCount == 0 {
		outputConfig.Limiter.AllowN(time.Now(), -1)
	} else if tokenCount > 1 {
		// Consume remaining tokens beyond the 1 already reserved
		outputConfig.Limiter.AllowN(time.Now(), tokenCount-1)
	}
	// If tokenCount == 1, pre-reserved token covers it exactly
}

// AddOrUpdateLimiter adds or updates rate limiter for a model
// Only recreates limiters if the configuration has actually changed,
// preserving limiter state across reconciliation events
func (r *TokenRateLimiter) AddOrUpdateLimiter(model string, ratelimit *networkingv1alpha1.RateLimit) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Determine if we should use global or local rate limiting
	useGlobal := ratelimit.Global != nil && ratelimit.Global.Redis != nil

	// Initialize Redis client if needed (only happens once per pod lifetime)
	if useGlobal && r.redisClient == nil {
		r.redisClient = redis.NewClient(&redis.Options{
			Addr: ratelimit.Global.Redis.Address,
		})

		// Test connection
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.redisClient.Ping(ctx).Err(); err != nil {
			r.redisClient = nil
			return fmt.Errorf("failed to connect to redis: %w", err)
		}
	}

	// Derive redisAddr for comparison in config change detection
	redisAddr := ""
	if useGlobal {
		redisAddr = ratelimit.Global.Redis.Address
	}

	// Helper: check if input config has changed
	inputConfigChanged := func(oldConfig *LimiterConfig) bool {
		if oldConfig == nil {
			return ratelimit.InputTokensPerUnit != nil // changed if new limit added
		}
		if ratelimit.InputTokensPerUnit == nil {
			return true // changed if limit removed
		}
		// Compare copied values (not pointers)
		if oldConfig.TokensPerUnit != *ratelimit.InputTokensPerUnit ||
			oldConfig.Unit != ratelimit.Unit ||
			oldConfig.IsGlobal != useGlobal ||
			oldConfig.GlobalRedisAddress != redisAddr {
			return true
		}
		return false
	}

	// Helper: check if output config has changed
	outputConfigChanged := func(oldConfig *LimiterConfig) bool {
		if oldConfig == nil {
			return ratelimit.OutputTokensPerUnit != nil // changed if new limit added
		}
		if ratelimit.OutputTokensPerUnit == nil {
			return true // changed if limit removed
		}
		// Compare copied values (not pointers)
		if oldConfig.TokensPerUnit != *ratelimit.OutputTokensPerUnit ||
			oldConfig.Unit != ratelimit.Unit ||
			oldConfig.IsGlobal != useGlobal ||
			oldConfig.GlobalRedisAddress != redisAddr {
			return true
		}
		return false
	}

	// Process input rate limiter
	if ratelimit.InputTokensPerUnit != nil {
		oldInputConfig, exists := r.inputLimiters[model]
		if !exists || inputConfigChanged(oldInputConfig) {
			// Config changed or doesn't exist - create new limiter
			var newLimiter Limiter

			if useGlobal {
				newLimiter = NewGlobalRateLimiter(
					r.redisClient,
					"kthena:ratelimit",
					model,
					"input",
					*ratelimit.InputTokensPerUnit,
					ratelimit.Unit,
				)
			} else {
				// Create local rate limiter
				duration := getTimeUnitDuration(ratelimit.Unit)
				newLimiter = NewLocalLimiter(
					rate.Limit(float64(*ratelimit.InputTokensPerUnit)/duration.Seconds()),
					int(*ratelimit.InputTokensPerUnit),
				)
			}

			r.inputLimiters[model] = &LimiterConfig{
				Limiter:            newLimiter,
				TokensPerUnit:      *ratelimit.InputTokensPerUnit, // copied value
				Unit:               ratelimit.Unit,
				IsGlobal:           useGlobal,
				GlobalRedisAddress: redisAddr,
			}
		}
		// If config hasn't changed, keep existing limiter with its state
	} else {
		// Input rate limit removed
		delete(r.inputLimiters, model)
	}

	// Process output rate limiter
	if ratelimit.OutputTokensPerUnit != nil {
		oldOutputConfig, exists := r.outputLimiters[model]
		if !exists || outputConfigChanged(oldOutputConfig) {
			// Config changed or doesn't exist - create new limiter
			var newLimiter Limiter

			if useGlobal {
				newLimiter = NewGlobalRateLimiter(
					r.redisClient,
					"kthena:ratelimit",
					model,
					"output",
					*ratelimit.OutputTokensPerUnit,
					ratelimit.Unit,
				)
			} else {
				// Create local rate limiter
				duration := getTimeUnitDuration(ratelimit.Unit)
				newLimiter = NewLocalLimiter(
					rate.Limit(float64(*ratelimit.OutputTokensPerUnit)/duration.Seconds()),
					int(*ratelimit.OutputTokensPerUnit),
				)
			}

			r.outputLimiters[model] = &LimiterConfig{
				Limiter:            newLimiter,
				TokensPerUnit:      *ratelimit.OutputTokensPerUnit, // copied value
				Unit:               ratelimit.Unit,
				IsGlobal:           useGlobal,
				GlobalRedisAddress: redisAddr,
			}
		}
		// If config hasn't changed, keep existing limiter with its state
	} else {
		// Output rate limit removed
		delete(r.outputLimiters, model)
	}

	return nil
}

// DeleteLimiter deletes rate limiter for a model
func (r *TokenRateLimiter) DeleteLimiter(model string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.inputLimiters, model)
	delete(r.outputLimiters, model)
}

func getTimeUnitDuration(unit networkingv1alpha1.RateLimitUnit) time.Duration {
	switch unit {
	case networkingv1alpha1.Second:
		return time.Second
	case networkingv1alpha1.Minute:
		return time.Minute
	case networkingv1alpha1.Hour:
		return time.Hour
	case networkingv1alpha1.Day:
		return 24 * time.Hour
	case networkingv1alpha1.Month:
		return 30 * 24 * time.Hour // Approximate
	default:
		return time.Second
	}
}
