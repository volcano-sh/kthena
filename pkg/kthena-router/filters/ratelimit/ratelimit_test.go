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

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestTokenRateLimiter_Basic(t *testing.T) {
	rl := NewTokenRateLimiter()
	limiterKey := testLimiterKey("default", "test-route")
	prompt := "hello world" // 3 tokens
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(limiterKey, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
	})

	// Should allow up to 10 tokens immediately
	for i := 0; i < 3; i++ {
		err := rl.RateLimit(limiterKey, prompt)
		if err != nil {
			t.Fatalf("unexpected error on allowed request: %v, %d", err, i)
		}
	}

	// 4th request should be rate limited
	err := rl.RateLimit(limiterKey, prompt)
	if err == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_NoLimiter(t *testing.T) {
	rl := NewTokenRateLimiter()
	// No limiter added, should always allow
	err := rl.RateLimit(testLimiterKey("unknown", "route"), "test")
	if err != nil {
		t.Fatalf("expected nil error for unknown model, got %v", err)
	}
}

func TestTokenRateLimiter_ResetAfterTime(t *testing.T) {
	rl := NewTokenRateLimiter()
	limiterKey := testLimiterKey("default", "test-route")
	prompt := "hello world"
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(limiterKey, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
	})

	// Use up tokens
	for i := 0; i < 3; i++ {
		err := rl.RateLimit(limiterKey, prompt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// Should be rate limited now
	err := rl.RateLimit(limiterKey, prompt)
	if err == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}

	// Wait for refill
	time.Sleep(1100 * time.Millisecond)
	err = rl.RateLimit(limiterKey, prompt)
	if err != nil {
		t.Fatalf("expected nil after refill, got %v", err)
	}
}

func TestTokenRateLimiter_OutputTokenRecording(t *testing.T) {
	rl := NewTokenRateLimiter()
	limiterKey := testLimiterKey("default", "test-route")
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(limiterKey, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &tokens,
		Unit:                unit,
	})

	// Record output tokens - this should not block/error
	rl.RecordOutputTokens(limiterKey, 5)
	rl.RecordOutputTokens(limiterKey, 3)
	rl.RecordOutputTokens(limiterKey, 2) // Total: 10 tokens consumed

	// Recording more tokens should still work (just consumes from the bucket)
	rl.RecordOutputTokens(limiterKey, 1)
}

func TestTokenRateLimiter_CombinedInputOutput(t *testing.T) {
	rl := NewTokenRateLimiter()
	limiterKey := testLimiterKey("default", "test-route")
	prompt := "hello world hello world" // Should be ~6 tokens
	inputTokens := uint32(8)            // Allow only one request (6 tokens < 8, but two requests = 12 > 8)
	outputTokens := uint32(10)          // Allow output recording
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(limiterKey, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit:  &inputTokens,
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First request should be allowed
	err := rl.RateLimit(limiterKey, prompt)
	if err != nil {
		t.Fatalf("unexpected error on first request: %v", err)
	}
	// Record output tokens used
	rl.RecordOutputTokens(limiterKey, 2)

	// Second request should be rate limited due to input token exhaustion
	err = rl.RateLimit(limiterKey, prompt)
	if err == nil {
		t.Fatalf("expected rate limit error after exhausting input tokens")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_OutputNoLimiter(t *testing.T) {
	rl := NewTokenRateLimiter()
	// No limiter added, should not error when recording output tokens
	rl.RecordOutputTokens(testLimiterKey("unknown", "route"), 100)
	// RecordOutputTokens doesn't return error, just silently does nothing
}

func TestTokenRateLimiter_DeleteLimiter(t *testing.T) {
	rl := NewTokenRateLimiter()
	limiterKey := testLimiterKey("default", "test-route")
	inputTokens := uint32(3) // Very restrictive
	outputTokens := uint32(5)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(limiterKey, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit:  &inputTokens,
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// Verify limiter exists and restricts
	err := rl.RateLimit(limiterKey, "hello world") // ~3 tokens
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	err = rl.RateLimit(limiterKey, "hello world") // Should be rate limited
	if err == nil {
		t.Fatalf("expected rate limit error")
	}

	// Delete limiters
	rl.DeleteLimiter(limiterKey)

	// Should now be unrestricted
	for i := 0; i < 10; i++ {
		err = rl.RateLimit(limiterKey, "hello world")
		if err != nil {
			t.Fatalf("expected nil after deletion, got %v", err)
		}
	}

	// Recording output tokens should work without error
	rl.RecordOutputTokens(limiterKey, 100)
}

func TestTokenRateLimiter_OutputRateLimit(t *testing.T) {
	rl := NewTokenRateLimiter()
	limiterKey := testLimiterKey("default", "test-route")
	prompt := "hello world"
	outputTokens := uint32(5) // Very low limit
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(limiterKey, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First request should be allowed (has 5 tokens available)
	err := rl.RateLimit(limiterKey, prompt)
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	// Consume most tokens
	rl.RecordOutputTokens(limiterKey, 5)

	// Next request should be blocked due to insufficient output tokens
	err = rl.RateLimit(limiterKey, prompt)
	if err == nil {
		t.Fatalf("expected output rate limit error")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_InputAndOutputErrors(t *testing.T) {
	rl := NewTokenRateLimiter()
	longPrompt := "hello world hello world hello world" // Should be ~9 tokens
	inputTokens := uint32(5)                            // Very low input limit
	outputTokens := uint32(10)                          // Higher output limit
	unit := networkingv1alpha1.Second

	// Test input rate limit error
	inputLimiterKey := testLimiterKey("default", "test-route-input")
	rl.AddOrUpdateLimiter(inputLimiterKey, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &inputTokens,
		Unit:               unit,
	})

	err := rl.RateLimit(inputLimiterKey, longPrompt)
	if err == nil {
		t.Fatalf("expected input rate limit error")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}

	// Test output rate limit error
	outputLimiterKey := testLimiterKey("default", "test-route-output")
	rl.AddOrUpdateLimiter(outputLimiterKey, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First make a successful request to establish the limiter
	err = rl.RateLimit(outputLimiterKey, "short")
	if err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}

	// Consume all available output tokens
	rl.RecordOutputTokens(outputLimiterKey, 10) // Consume all 10 tokens

	// Wait a bit for the tokens to be recorded
	time.Sleep(10 * time.Millisecond)

	// Next request should be blocked due to insufficient output tokens (< 1 token available)
	err = rl.RateLimit(outputLimiterKey, "short") // Short prompt to avoid input limit
	if err == nil {
		t.Fatalf("expected output rate limit error")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func testLimiterKey(namespace, modelRouteName string) string {
	return fmt.Sprintf("%s/%s", namespace, modelRouteName)
}
