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
	"reflect"
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
		err := rl.RateLimit(model, prompt)
		if err != nil {
			t.Fatalf("unexpected error on allowed request: %v, %d", err, i)
		}
	}

	// 4th request should be rate limited
	err := rl.RateLimit(model, prompt)
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
	err := rl.RateLimit("unknown-model", "test")
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
		err := rl.RateLimit(model, prompt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// Should be rate limited now
	err := rl.RateLimit(model, prompt)
	if err == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if _, ok := err.(*InputRateLimitExceededError); !ok {
		t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
	}

	// Wait for refill
	time.Sleep(1100 * time.Millisecond)
	err = rl.RateLimit(model, prompt)
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
	rl.RecordOutputTokens(model, 5)
	rl.RecordOutputTokens(model, 3)
	rl.RecordOutputTokens(model, 2) // Total: 10 tokens consumed

	// Recording more tokens should still work (just consumes from the bucket)
	rl.RecordOutputTokens(model, 1)
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
	err := rl.RateLimit(model, prompt)
	if err != nil {
		t.Fatalf("unexpected error on first request: %v", err)
	}
	// Record output tokens used
	rl.RecordOutputTokens(model, 2)

	// Second request should be rate limited due to input token exhaustion
	err = rl.RateLimit(model, prompt)
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
	rl.RecordOutputTokens("unknown-model", 100)
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
	err := rl.RateLimit(model, "hello world") // ~3 tokens
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	err = rl.RateLimit(model, "hello world") // Should be rate limited
	if err == nil {
		t.Fatalf("expected rate limit error")
	}

	// Delete limiters
	rl.DeleteLimiter(model)

	// Should now be unrestricted
	for i := 0; i < 10; i++ {
		err = rl.RateLimit(model, "hello world")
		if err != nil {
			t.Fatalf("expected nil after deletion, got %v", err)
		}
	}

	// Recording output tokens should work without error
	rl.RecordOutputTokens(model, 100)
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
	err := rl.RateLimit(model, prompt)
	if err != nil {
		t.Fatalf("first request should be allowed: %v", err)
	}

	// Consume most tokens
	rl.RecordOutputTokens(model, 5)

	// Next request should be blocked due to insufficient output tokens
	err = rl.RateLimit(model, prompt)
	if err == nil {
		t.Fatalf("expected output rate limit error")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_OutputRejectionKeepsInputQuota(t *testing.T) {
	rl := NewTokenRateLimiter()
	model := "test-model"
	inputTokens := uint32(10)
	outputTokens := uint32(5)

	// A minute-based unit keeps refill negligible during the test.
	rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		InputTokensPerUnit:  &inputTokens,
		OutputTokensPerUnit: &outputTokens,
		Unit:                networkingv1alpha1.Minute,
	})
	rl.RecordOutputTokens(model, 5)

	before := rl.inputLimiter[model].Tokens()
	err := rl.RateLimit(model, "hello world")
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
	if after := rl.inputLimiter[model].Tokens(); after < before {
		t.Fatalf("input quota consumed by rejected request: before=%v after=%v", before, after)
	}
}

func TestTokenRateLimiter_CheckRateLimitDoesNotConsume(t *testing.T) {
	model := "test-model"
	prompt := "hello world"
	tokens, err := NewTokenRateLimiter().tokenizer.CalculateTokenNum(prompt)
	if err != nil {
		t.Fatalf("failed to calculate tokens: %v", err)
	}
	inputTokens := uint32(tokens)
	outputTokens := uint32(5)

	tests := []struct {
		name          string
		exhaustInput  bool
		exhaustOutput bool
		wantErr       error
	}{
		{name: "within quota"},
		{name: "input quota exhausted", exhaustInput: true, wantErr: &InputRateLimitExceededError{}},
		{name: "output quota exhausted", exhaustOutput: true, wantErr: &OutputRateLimitExceededError{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rl := NewTokenRateLimiter()
			// A minute-based unit keeps refill negligible during the test.
			rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
				InputTokensPerUnit:  &inputTokens,
				OutputTokensPerUnit: &outputTokens,
				Unit:                networkingv1alpha1.Minute,
			})
			if tt.exhaustInput {
				if err := rl.RateLimit(model, prompt); err != nil {
					t.Fatalf("failed to exhaust input quota: %v", err)
				}
			}
			if tt.exhaustOutput {
				rl.RecordOutputTokens(model, int(outputTokens))
			}

			before := rl.inputLimiter[model].Tokens()
			for i := 0; i < 3; i++ {
				err := rl.CheckRateLimit(model, prompt)
				if tt.wantErr == nil && err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				if tt.wantErr != nil && reflect.TypeOf(err) != reflect.TypeOf(tt.wantErr) {
					t.Fatalf("expected %T, got %T: %v", tt.wantErr, err, err)
				}
			}
			if after := rl.inputLimiter[model].Tokens(); after < before {
				t.Fatalf("CheckRateLimit consumed input quota: before=%v after=%v", before, after)
			}
		})
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

	err := rl.RateLimit(model+"-input", longPrompt)
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
	err = rl.RateLimit(model+"-output", "short")
	if err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}

	// Consume all available output tokens
	rl.RecordOutputTokens(model+"-output", 10) // Consume all 10 tokens

	// Wait a bit for the tokens to be recorded
	time.Sleep(10 * time.Millisecond)

	// Next request should be blocked due to insufficient output tokens (< 1 token available)
	err = rl.RateLimit(model+"-output", "short") // Short prompt to avoid input limit
	if err == nil {
		t.Fatalf("expected output rate limit error")
	}
	if _, ok := err.(*OutputRateLimitExceededError); !ok {
		t.Fatalf("expected OutputRateLimitExceededError, got %T: %v", err, err)
	}
}

func TestTokenRateLimiter_ChargeRateLimitRefund(t *testing.T) {
	const (
		model  = "test-model"
		prompt = "hello world" // 3 tokens
	)

	tests := []struct {
		name   string
		global bool
		// limit is the input token limit per minute.
		limit uint32
		// refundTwice refunds the same charge twice to show refunds never exceed the capacity.
		refundTwice bool
		// wantAllowed is the number of further requests admitted after the refund.
		wantAllowed int
	}{
		{name: "local refund restores the charged tokens", limit: 6, wantAllowed: 2},
		{name: "local refund does not exceed capacity", limit: 3, refundTwice: true, wantAllowed: 1},
		{name: "global refund restores the charged tokens", global: true, limit: 6, wantAllowed: 2},
		{name: "global refund does not exceed capacity", global: true, limit: 3, refundTwice: true, wantAllowed: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit := tt.limit
			// A minute-based unit keeps refill negligible during the test.
			config := &networkingv1alpha1.RateLimit{InputTokensPerUnit: &limit, Unit: networkingv1alpha1.Minute}
			if tt.global {
				mr, redisConfig := setupMiniRedis(t)
				t.Cleanup(mr.Close)
				config.Global = &networkingv1alpha1.GlobalRateLimit{Redis: redisConfig}
			}
			rl := NewTokenRateLimiter()
			if err := rl.AddOrUpdateLimiter(model, config); err != nil {
				t.Fatalf("AddOrUpdateLimiter() error = %v", err)
			}

			charge, err := rl.ChargeRateLimit(model, prompt)
			if err != nil {
				t.Fatalf("ChargeRateLimit() error = %v", err)
			}
			if charge == nil {
				t.Fatalf("expected a charge for a model with an input limit")
			}
			charge.Refund()
			if tt.refundTwice {
				charge.Refund()
			}

			allowed := 0
			for ; allowed < 5; allowed++ {
				if err := rl.RateLimit(model, prompt); err != nil {
					if _, ok := err.(*InputRateLimitExceededError); !ok {
						t.Fatalf("expected InputRateLimitExceededError, got %T: %v", err, err)
					}
					break
				}
			}
			if allowed != tt.wantAllowed {
				t.Fatalf("expected %d requests after refund, got %d", tt.wantAllowed, allowed)
			}
		})
	}
}

func TestTokenRateLimiter_ChargeRateLimitWithoutInputLimit(t *testing.T) {
	model := "test-model"
	outputTokens := uint32(10)
	rl := NewTokenRateLimiter()
	if err := rl.AddOrUpdateLimiter(model, &networkingv1alpha1.RateLimit{
		OutputTokensPerUnit: &outputTokens,
		Unit:                networkingv1alpha1.Minute,
	}); err != nil {
		t.Fatalf("AddOrUpdateLimiter() error = %v", err)
	}

	charge, err := rl.ChargeRateLimit(model, "hello world")
	if err != nil {
		t.Fatalf("ChargeRateLimit() error = %v", err)
	}
	if charge != nil {
		t.Fatalf("expected no charge without an input limit, got %+v", charge)
	}
	// Refunding a nil charge is a no-op.
	charge.Refund()
}

func TestLocalLimiter(t *testing.T) {
	start := time.Now()
	tests := []struct {
		name string
		// run drives a limiter with 10 tokens per second and a burst of 10.
		run        func(l *LocalLimiter) bool
		wantResult bool
		wantTokens float64
	}{
		{
			name:       "starts full and consumes",
			run:        func(l *LocalLimiter) bool { return l.AllowN(start, 4) },
			wantResult: true,
			wantTokens: 6,
		},
		{
			name: "rejects more than available without consuming",
			run: func(l *LocalLimiter) bool {
				l.AllowN(start, 8)
				return l.AllowN(start, 3)
			},
			wantResult: false,
			wantTokens: 2,
		},
		{
			name: "refills over time up to the burst",
			run: func(l *LocalLimiter) bool {
				l.AllowN(start, 10)
				return l.AllowN(start.Add(time.Hour), 0)
			},
			wantResult: true,
			wantTokens: 10,
		},
		{
			name: "returned tokens are capped at the burst",
			run: func(l *LocalLimiter) bool {
				l.AllowN(start, 3)
				l.ReturnN(start, 5)
				return true
			},
			wantResult: true,
			wantTokens: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewLocalLimiter(10, 10)
			l.last = start
			if got := tt.run(l); got != tt.wantResult {
				t.Fatalf("expected result %v, got %v", tt.wantResult, got)
			}
			if l.tokens != tt.wantTokens {
				t.Fatalf("expected %v tokens, got %v", tt.wantTokens, l.tokens)
			}
		})
	}
}
