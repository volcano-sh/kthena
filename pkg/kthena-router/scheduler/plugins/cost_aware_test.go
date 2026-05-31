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

package plugins

import (
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

func TestCostAwareScoreUsesTokenBudget(t *testing.T) {
	plugin := NewCostAware(runtime.RawExtension{Raw: []byte(`{
		"mode": "score",
		"podBudgetTokens": 100,
		"safetyFactor": 1,
		"minReservationTokens": 1,
		"maxReservationTokens": 100
	}`)})
	pods := []*datastore.PodInfo{
		testCostAwarePod("default", "pod-1"),
		testCostAwarePod("default", "pod-2"),
	}
	ctx := testCostAwareContext()

	scores := plugin.Score(ctx, pods)
	if scores[pods[0]] != 80 || scores[pods[1]] != 80 {
		t.Fatalf("expected both pods to score 80 before reservation, got pod-1=%d pod-2=%d", scores[pods[0]], scores[pods[1]])
	}

	reservation := plugin.Reserve(ctx, pods[0])
	if reservation == nil {
		t.Fatal("expected reservation")
	}

	scores = plugin.Score(ctx, pods)
	if scores[pods[0]] != 60 {
		t.Fatalf("expected reserved pod score 60, got %d", scores[pods[0]])
	}
	if scores[pods[1]] != 80 {
		t.Fatalf("expected unreserved pod score 80, got %d", scores[pods[1]])
	}

	plugin.Finish(ctx, reservation, nil)
	scores = plugin.Score(ctx, pods)
	if scores[pods[0]] != 80 {
		t.Fatalf("expected released pod score 80, got %d", scores[pods[0]])
	}
}

func TestCostAwareObserveModeReturnsNeutralScores(t *testing.T) {
	plugin := NewCostAware(runtime.RawExtension{Raw: []byte(`{
		"mode": "observe",
		"podBudgetTokens": 100,
		"safetyFactor": 1,
		"minReservationTokens": 1,
		"maxReservationTokens": 100
	}`)})
	pod := testCostAwarePod("default", "pod-1")
	ctx := testCostAwareContext()

	scores := plugin.Score(ctx, []*datastore.PodInfo{pod})
	if scores[pod] != 100 {
		t.Fatalf("expected observe mode neutral score 100, got %d", scores[pod])
	}
	if reservation := plugin.Reserve(ctx, pod); reservation != nil {
		t.Fatalf("expected no reservation in observe mode, got %#v", reservation)
	}
}

func TestCostAwareOutputCapPriorityAndChoices(t *testing.T) {
	plugin := NewCostAware(runtime.RawExtension{Raw: []byte(`{
		"mode": "score",
		"defaultOutputTokens": 7,
		"safetyFactor": 1,
		"minReservationTokens": 1,
		"maxReservationTokens": 1000
	}`)})
	ctx := &framework.Context{
		Model:  "test-model",
		Prompt: common.ChatMessage{Text: "12345678"},
		RequestBody: map[string]interface{}{
			"max_tokens":            float64(20),
			"max_completion_tokens": float64(11),
			"n":                     float64(3),
		},
	}

	plugin.Score(ctx, []*datastore.PodInfo{testCostAwarePod("default", "pod-1")})
	if ctx.RequestCost == nil {
		t.Fatal("expected request cost estimate")
	}
	if ctx.RequestCost.OutputTokens != 33 {
		t.Fatalf("expected max_completion_tokens to win and multiply by n, got %d", ctx.RequestCost.OutputTokens)
	}
}

func TestCostAwareBytesPerTokenEMA(t *testing.T) {
	plugin := NewCostAware(runtime.RawExtension{Raw: []byte(`{
		"mode": "score",
		"defaultBytesPerToken": 4,
		"safetyFactor": 1,
		"minReservationTokens": 1,
		"maxReservationTokens": 1000
	}`)})
	pod := testCostAwarePod("default", "pod-1")
	ctx := &framework.Context{
		Model:       "test-model",
		Prompt:      common.ChatMessage{Text: "12345678"},
		RequestBody: map[string]interface{}{"max_tokens": float64(1)},
		RequestID:   "req-ema-1",
	}

	plugin.Score(ctx, []*datastore.PodInfo{pod})
	if ctx.RequestCost.PromptTokens != 2 {
		t.Fatalf("expected initial prompt tokens from default bytes/token to be 2, got %d", ctx.RequestCost.PromptTokens)
	}
	reservation := plugin.Reserve(ctx, pod)
	plugin.Finish(ctx, reservation, &framework.TokenUsage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5})

	ctx = &framework.Context{
		Model:       "test-model",
		Prompt:      common.ChatMessage{Text: "12345678"},
		RequestBody: map[string]interface{}{"max_tokens": float64(1)},
		RequestID:   "req-ema-2",
	}
	plugin.Score(ctx, []*datastore.PodInfo{pod})
	if ctx.RequestCost.PromptTokens != 4 {
		t.Fatalf("expected prompt tokens to use EMA-updated bytes/token, got %d", ctx.RequestCost.PromptTokens)
	}
}

func TestCostAwareEMAUpdatesWithoutReservation(t *testing.T) {
	plugin := NewCostAware(runtime.RawExtension{Raw: []byte(`{
		"mode": "observe",
		"defaultBytesPerToken": 4,
		"safetyFactor": 1,
		"minReservationTokens": 1,
		"maxReservationTokens": 1000
	}`)})
	pod := testCostAwarePod("default", "pod-1")
	ctx := &framework.Context{
		Model:       "test-model",
		Prompt:      common.ChatMessage{Text: "12345678"},
		RequestBody: map[string]interface{}{"max_tokens": float64(1)},
	}

	plugin.Score(ctx, []*datastore.PodInfo{pod})
	plugin.Finish(ctx, nil, &framework.TokenUsage{PromptTokens: 4})

	ctx = &framework.Context{
		Model:       "test-model",
		Prompt:      common.ChatMessage{Text: "12345678"},
		RequestBody: map[string]interface{}{"max_tokens": float64(1)},
	}
	plugin.Score(ctx, []*datastore.PodInfo{pod})
	if ctx.RequestCost.PromptTokens != 4 {
		t.Fatalf("expected observe mode to update EMA without reservation, got %d", ctx.RequestCost.PromptTokens)
	}
}

func TestCostAwareConcurrentReleaseIsIdempotent(t *testing.T) {
	plugin := NewCostAware(runtime.RawExtension{Raw: []byte(`{
		"mode": "score",
		"podBudgetTokens": 100,
		"safetyFactor": 1,
		"minReservationTokens": 1,
		"maxReservationTokens": 100
	}`)})
	pod := testCostAwarePod("default", "pod-1")
	ctx := testCostAwareContext()
	reservation := plugin.Reserve(ctx, pod)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plugin.Finish(ctx, reservation, nil)
		}()
	}
	wg.Wait()

	scores := plugin.Score(ctx, []*datastore.PodInfo{pod})
	if scores[pod] != 80 {
		t.Fatalf("expected idempotent release to restore score 80, got %d", scores[pod])
	}
}

func testCostAwareContext() *framework.Context {
	return &framework.Context{
		Model:  "test-model",
		Prompt: common.ChatMessage{Text: "12345678"},
		RequestBody: map[string]interface{}{
			"max_tokens": float64(18),
		},
		RequestID: "req-1",
	}
}

func testCostAwarePod(namespace, name string) *datastore.PodInfo {
	return &datastore.PodInfo{
		Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}},
	}
}
