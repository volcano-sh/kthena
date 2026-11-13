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
	"math"
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
	// ReturnN gives n previously consumed tokens back, without exceeding the bucket capacity
	ReturnN(now time.Time, n int)
	// Tokens returns the number of tokens currently available
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

type LocalLimiter struct {
	mu     sync.Mutex
	limit  rate.Limit
	burst  float64
	tokens float64
	last   time.Time
}

// NewLocalLimiter creates a new LocalLimiter with a full bucket
func NewLocalLimiter(limit rate.Limit, burst int) *LocalLimiter {
	return &LocalLimiter{
		limit:  limit,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

// advance refills the bucket for the time elapsed since the last update.
func (l *LocalLimiter) advance(now time.Time) {
	if !now.After(l.last) {
		return
	}
	l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*float64(l.limit))
	l.last = now
}

// AllowN reports whether n tokens may be consumed at now and consumes them if so
func (l *LocalLimiter) AllowN(now time.Time, n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.advance(now)
	if float64(n) > l.tokens {
		return false
	}
	l.tokens -= float64(n)
	return true
}

// ReturnN gives n tokens back, without exceeding the burst
func (l *LocalLimiter) ReturnN(now time.Time, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.advance(now)
	l.tokens = math.Min(l.burst, l.tokens+float64(n))
}

// Tokens returns the number of tokens currently available
func (l *LocalLimiter) Tokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.advance(time.Now())
	return l.tokens
}

type InputCharge struct {
	limiter Limiter
	tokens  int
}

// Refund returns the charged tokens to the limiter they were taken from.
func (c *InputCharge) Refund() {
	if c == nil || c.limiter == nil || c.tokens <= 0 {
		return
	}
	c.limiter.ReturnN(time.Now(), c.tokens)
}

// NewTokenRateLimiter creates a new TokenRateLimiter instance
func NewTokenRateLimiter() *TokenRateLimiter {
	return &TokenRateLimiter{
		inputLimiter:  make(map[string]Limiter),
		outputLimiter: make(map[string]Limiter),
		tokenizer:     tokenizer.NewSimpleEstimateTokenizer(),
	}
}

// RateLimit checks if the request is within rate limits for both input and output tokens
func (r *TokenRateLimiter) RateLimit(model, prompt string) error {
	_, err := r.rateLimit(model, prompt, true)
	return err
}

// ChargeRateLimit is RateLimit that also returns the input quota it took, so the caller can refund it.
func (r *TokenRateLimiter) ChargeRateLimit(model, prompt string) (*InputCharge, error) {
	return r.rateLimit(model, prompt, true)
}

// CheckRateLimit reports whether the request is within rate limits without consuming any quota.
func (r *TokenRateLimiter) CheckRateLimit(model, prompt string) error {
	_, err := r.rateLimit(model, prompt, false)
	return err
}

func (r *TokenRateLimiter) rateLimit(model, prompt string, consume bool) (*InputCharge, error) {
	r.mutex.RLock()
	inputLimiter, hasInputLimit := r.inputLimiter[model]
	outputLimiter, hasOutputLimit := r.outputLimiter[model]
	r.mutex.RUnlock()

	if !hasInputLimit && !hasOutputLimit {
		return nil, nil
	}

	// Check output token rate limit - we conservatively check if there's at least 1 token available
	// This prevents starting requests that likely won't be able to complete
	if hasOutputLimit && outputLimiter.Tokens() < 1.0 {
		return nil, &OutputRateLimitExceededError{}
	}

	if !hasInputLimit {
		return nil, nil
	}

	tokens, err := r.tokenizer.CalculateTokenNum(prompt)
	if err != nil {
		klog.Errorf("failed to calculate token number: %v", err)
		tokens = len(prompt) / 4 // fallback estimation
	}

	if !consume {
		if inputLimiter.Tokens() < float64(tokens) {
			return nil, &InputRateLimitExceededError{}
		}
		return nil, nil
	}
	if !inputLimiter.AllowN(time.Now(), tokens) {
		return nil, &InputRateLimitExceededError{}
	}
	return &InputCharge{limiter: inputLimiter, tokens: tokens}, nil
}

// RecordOutputTokens records the actual output tokens consumed after response generation
func (r *TokenRateLimiter) RecordOutputTokens(model string, tokenCount int) {
	r.mutex.RLock()
	outputLimiter, exists := r.outputLimiter[model]
	r.mutex.RUnlock()

	if exists {
		outputLimiter.AllowN(time.Now(), tokenCount)
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
