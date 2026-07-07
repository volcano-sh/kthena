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
	"testing"
	"time"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestTokenRateLimiter_Basic(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	prompt := "hello world" // 3 tokens
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
	})

	// Should allow up to 10 tokens immediately
	for i := 0; i < 3; i++ {
		_, err := rl.RateLimit(model, prompt, 0)
		if err != nil {
			t.Fatalf("unexpected error on allowed request: %v, %d", err, i)
		}
	}

	// 4th request should be rate limited
	_, err := rl.RateLimit(model, prompt, 0)
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
	_, err := rl.RateLimit("unknown-model", "test", 0)
	if err != nil {
		t.Fatalf("expected nil error for unknown model, got %v", err)
	}
}

func TestTokenRateLimiter_ResetAfterTime(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	prompt := "hello world"
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &tokens,
		Unit:               unit,
	})

	// Use up tokens
	for i := 0; i < 3; i++ {
		_, err := rl.RateLimit(model, prompt, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// Should be rate limited now
	_, err := rl.RateLimit(model, prompt, 0)
	if err == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}

	// Wait for refill
	time.Sleep(1100 * time.Millisecond)
	_, err = rl.RateLimit(model, prompt, 0)
	if err != nil {
		t.Fatalf("expected nil after refill, got %v", err)
	}
}

func TestTokenRateLimiter_OutputTokenRecording(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	tokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &tokens,
		Unit:                unit,
	})

	// Record output tokens - this should not block/error
	rl.RecordOutputTokens(model, 0, 5)
	rl.RecordOutputTokens(model, 0, 3)
	rl.RecordOutputTokens(model, 0, 2) // Total: 10 tokens consumed

	// Recording more tokens should still work (just consumes from the bucket)
	rl.RecordOutputTokens(model, 0, 1)
}

func TestTokenRateLimiter_CombinedInputOutput(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	prompt := "hello world hello world" // Should be ~6 tokens
	inputTokens := uint32(8)            // Allow only one request (6 tokens < 8, but two requests = 12 > 8)
	outputTokens := uint32(10)          // Allow output recording
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit:  &inputTokens,
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First request should be allowed
	reserved, err := rl.RateLimit(model, prompt, 2)
	if err != nil {
		t.Fatalf("unexpected error on first request: %v", err)
	}
	// Record output tokens used
	rl.RecordOutputTokens(model, reserved, 2)

	// Second request should be rate limited due to input token exhaustion
	_, err = rl.RateLimit(model, prompt, 2)
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
	rl.RecordOutputTokens("unknown-model", 0, 100)
	// RecordOutputTokens doesn't return error, just silently does nothing
}

func TestTokenRateLimiter_DeleteLimiter(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	inputTokens := uint32(3) // Very restrictive
	outputTokens := uint32(5)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit:  &inputTokens,
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// Verify limiter exists and restricts
	_, err := rl.RateLimit(model, "hello world", 0) // ~3 tokens
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	_, err = rl.RateLimit(model, "hello world", 0) // Should be rate limited
	if err == nil {
		t.Fatalf("expected rate limit error")
	}

	// Delete limiters
	rl.DeleteLimiter(model)

	// Should now be unrestricted
	for i := 0; i < 10; i++ {
		_, err = rl.RateLimit(model, "hello world", 0)
		if err != nil {
			t.Fatalf("expected nil after deletion, got %v", err)
		}
	}

	// Recording output tokens should work without error
	rl.RecordOutputTokens(model, 0, 100)
}

func TestTokenRateLimiter_OutputRateLimit(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	prompt := "hello world"
	outputTokens := uint32(5) // Very low limit
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First request should be allowed (has 5 tokens available)
	reserved, err := rl.RateLimit(model, prompt, 1)
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	// Consume most tokens
	rl.RecordOutputTokens(model, reserved, 5)

	// Next request should be blocked due to insufficient output tokens
	_, err = rl.RateLimit(model, prompt, 1)
	if err == nil {
		t.Fatalf("expected output rate limit error")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_InputAndOutputErrors(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	longPrompt := "hello world hello world hello world" // Should be ~9 tokens
	inputTokens := uint32(5)                            // Very low input limit
	outputTokens := uint32(10)                          // Higher output limit
	unit := networkingv1alpha1.Second

	// Test input rate limit error
	rl.AddOrUpdateLimiter(model+"-input", &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &inputTokens,
		Unit:               unit,
	})

	_, err := rl.RateLimit(model+"-input", longPrompt, 0)
	if err == nil {
		t.Fatalf("expected input rate limit error")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}

	// Test output rate limit error
	rl.AddOrUpdateLimiter(model+"-output", &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First make a successful request to establish the limiter
	reserved, err := rl.RateLimit(model+"-output", "short", 1)
	if err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}

	// Consume all available output tokens
	rl.RecordOutputTokens(model+"-output", reserved, 10) // Consume all 10 tokens

	// Wait a bit for the tokens to be recorded
	time.Sleep(10 * time.Millisecond)

	// Next request should be blocked due to insufficient output tokens (< 1 token available)
	_, err = rl.RateLimit(model+"-output", "short", 1) // Short prompt to avoid input limit
	if err == nil {
		t.Fatalf("expected output rate limit error")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_OutputRateLimit_Overdraw(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	prompt := "hello world"
	outputTokens := uint32(5) // Very low limit
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                unit,
	})

	// First request reserves 2 tokens. Allowed.
	reserved, err := rl.RateLimit(model, prompt, 2)
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	// Refund the 2 tokens (as if 0 actual tokens were consumed)
	rl.RecordOutputTokens(model, reserved, 0)

	// Now record 10 tokens (exceeding capacity of 5).
	// This should unconditionally deduct them, making balance negative.
	rl.RecordOutputTokens(model, 0, 10)

	// Next request trying to reserve 1 token should be blocked.
	_, err = rl.RateLimit(model, prompt, 1)
	if err == nil {
		t.Fatalf("expected output rate limit error due to overdraw debt")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_ClockSkew(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	inputTokens := uint32(10)
	unit := networkingv1alpha1.Second

	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit: &inputTokens,
		Unit:               unit,
	})

	// First request. Allowed.
	_, err := rl.RateLimit(model, "test", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	l := rl.inputLimiter[model].(*LocalLimiter)

	// Consume some tokens at now
	now := time.Now()
	allowed := l.AllowN(now, 5)
	if !allowed {
		t.Fatalf("expected allowed")
	}

	// Call AllowN with past time (clock skew)
	past := now.Add(-10 * time.Second)
	allowed = l.AllowN(past, 1)
	if !allowed {
		t.Fatalf("expected allowed, clock skew should not drain tokens")
	}

	// Remaining tokens should not have dropped due to clock skew
	tokens := l.Tokens()
	if tokens < 0 {
		t.Fatalf("expected non-negative tokens, got %v", tokens)
	}
}
