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
	ReconcileN(now time.Time, delta int) (bool, error)

	Tokens() float64
}

// TokenRateLimiter provides rate limiting functionality for both input and output tokens
type TokenRateLimiter struct {
	mutex sync.RWMutex

	// Unified rate limiters using Limiter interface
	inputLimiter  map[string]Limiter
	outputLimiter map[string]Limiter

	// Redis client for global rate limiting
	redisClient *redis.Client

	tokenizer tokenizer.Tokenizer
}

// LocalLimiter wraps golang.org/x/time/rate.Limiter to implement our Limiter interface for local rate limiting also its same struct
type LocalLimiter struct {
	mu sync.Mutex

	limit rate.Limit
	burst int

	tokens float64
	last   time.Time
}

// NewLocalLimiter creates a new LocalLimiter
func NewLocalLimiter(limit rate.Limit, burst int) *LocalLimiter {
	now := time.Now()
	return &LocalLimiter{
		limit:  limit,
		burst:  burst,
		tokens: float64(burst),
		last:   now,
	}
}

// Tokens returns the number of tokens currently available
func (l *LocalLimiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refill(time.Now())
	return l.tokens
}

func (l *LocalLimiter) refill(now time.Time) float64 {
	elapsed := now.Sub(l.last)

	l.tokens += elapsed.Seconds() * float64(l.limit)
	if l.tokens > float64(l.burst) {
		l.tokens = float64(l.burst)
	}

	l.last = now
	return l.tokens
}

// NewTokenRateLimiter creates a new TokenRateLimiter instance
func NewTokenRateLimiter() *TokenRateLimiter {
	return &TokenRateLimiter{
		inputLimiter:  make(map[string]Limiter),
		outputLimiter: make(map[string]Limiter),
		tokenizer:     tokenizer.NewSimpleEstimateTokenizer(),
	}
}

// RateLimit checks limits and returns the number of tokens reserved for output.
func (r *TokenRateLimiter) RateLimit(model, prompt string, estimatedOutputTokens int) (int, error) {
	tokens, err := r.tokenizer.CalculateTokenNum(prompt)
	if err != nil {
		klog.Errorf("failed to calculate token number: %v", err)
		tokens = len(prompt) / 4
	}

	r.mutex.RLock()
	inputLimiter, hasInputLimit := r.inputLimiter[model]
	outputLimiter, hasOutputLimit := r.outputLimiter[model]
	r.mutex.RUnlock()

	if hasInputLimit && !inputLimiter.AllowN(time.Now(), tokens) {
		return 0, &InputRateLimitExceededError{}
	}

	reserved := 0
	if hasOutputLimit {
		if !outputLimiter.AllowN(time.Now(), estimatedOutputTokens) {
			// Input tokens were already consumed above — refund them (see fix #2).
			if hasInputLimit {
				if _, refundErr := inputLimiter.ReconcileN(time.Now(), -tokens); refundErr != nil {
					klog.Errorf("failed to refund input tokens for model %s: %v", model, refundErr)
				}
			}
			return 0, &OutputRateLimitExceededError{}
		}
		reserved = estimatedOutputTokens
	}
	return reserved, nil
}

// RecordOutputTokens true-ups the bucket for the difference between what was
// reserved (estimated) and what was actually used. Pass the value returned by
// RateLimit as `reserved`.
func (r *TokenRateLimiter) RecordOutputTokens(model string, reserved, actual int) {
	r.mutex.RLock()
	outputLimiter, exists := r.outputLimiter[model]
	r.mutex.RUnlock()

	if exists {
		delta := actual - reserved // positive = debit more, negative = refund
		outputLimiter.ReconcileN(time.Now(), delta)
	}
}

// AddOrUpdateLimiter adds or updates rate limiter for a model
func (r *TokenRateLimiter) AddOrUpdateLimiter(model string, ratelimit *networkingv1alpha1.RateLimit) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Determine if we should use global or local rate limiting
	useGlobal := ratelimit.Global != nil && ratelimit.Global.Redis != nil

	if useGlobal {
		// Initialize Redis client if not already done
		if r.redisClient == nil {
			r.redisClient = redis.NewClient(&redis.Options{
				Addr: ratelimit.Global.Redis.Address,
			})

			// Test connection
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.redisClient.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("failed to connect to redis: %w", err)
			}
		}

		// Create global rate limiters
		if ratelimit.InputTokensPerUnit != nil {
			r.inputLimiter[model] = NewGlobalRateLimiter(
				r.redisClient,
				"kthena:ratelimit",
				model,
				"input",
				*ratelimit.InputTokensPerUnit,
				ratelimit.Unit,
			)
		}

		if ratelimit.OutputTokensPerUnit != nil {
			r.outputLimiter[model] = NewGlobalRateLimiter(
				r.redisClient,
				"kthena:ratelimit",
				model,
				"output",
				*ratelimit.OutputTokensPerUnit,
				ratelimit.Unit,
			)
		}
	} else {
		// Create local rate limiters
		duration := getTimeUnitDuration(ratelimit.Unit)

		if ratelimit.InputTokensPerUnit != nil {
			r.inputLimiter[model] = NewLocalLimiter(
				rate.Limit(float64(*ratelimit.InputTokensPerUnit)/duration.Seconds()),
				int(*ratelimit.InputTokensPerUnit),
			)
		}

		if ratelimit.OutputTokensPerUnit != nil {
			r.outputLimiter[model] = NewLocalLimiter(
				rate.Limit(float64(*ratelimit.OutputTokensPerUnit)/duration.Seconds()),
				int(*ratelimit.OutputTokensPerUnit),
			)
		}
	}

	return nil
}

// DeleteLimiter deletes rate limiter for a model
func (r *TokenRateLimiter) DeleteLimiter(model string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.inputLimiter, model)
	delete(r.outputLimiter, model)
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

// consumeN updates the token bucket by consuming n tokens.
// If enforceLimit is true, the request is rejected when insufficient
// tokens are available.
// If enforceLimit is false, the tokens are always deducted, allowing
// the bucket to become negative for post-request accounting.
// Based on the token bucket algorithm from golang.org/x/time/rate,
// adapted to support unconditional post-request accounting.
func (l *LocalLimiter) consumeN(now time.Time, n int, enforceLimit bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refill(now)

	remaining := l.tokens - float64(n)

	if enforceLimit && remaining < 0 {
		// Admission failed. Preserve the current bucket state.
		return false
	}

	// Commit the updated bucket state.
	l.tokens = remaining

	return true
}

func (l *LocalLimiter) AllowN(now time.Time, n int) bool {
	return l.consumeN(now, n, true)
}

func (l *LocalLimiter) ReconcileN(now time.Time, delta int) (bool, error) {
	return l.consumeN(now, delta, false), nil
}
