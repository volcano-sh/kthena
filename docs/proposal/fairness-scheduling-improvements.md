# Fairness Scheduling — Architecture Design Review & Improvement Proposal

**Author:** Architecture Review  
**Date:** 2026-04-01  
**Status:** Draft  
**Component:** `pkg/kthena-router` (router, datastore, scheduler)

---

## 1. Current Architecture

```
┌────────────────────────────────────────────────────────────────────────┐
│                         Request Flow (Fairness ON)                     │
│                                                                        │
│  HTTP Request                                                          │
│      │                                                                 │
│      ▼                                                                 │
│  ┌──────────┐    ┌──────────────┐    ┌────────────────────────┐       │
│  │  Router   │───▶│ GetTokenCount│───▶│ InMemorySlidingWindow  │       │
│  │ Handler   │    │  (priority)  │    │    TokenTracker         │       │
│  └──────────┘    └──────────────┘    │  [user][model] → float  │       │
│      │                               └────────────────────────┘       │
│      ▼                                                                 │
│  ┌──────────────────────────────┐                                      │
│  │    store.Enqueue(Request)     │                                      │
│  │  ┌────────────────────────┐  │                                      │
│  │  │ Per-Model Priority Queue│  │   One queue per model               │
│  │  │  (min-heap by priority) │  │   Run() goroutine @ 100 QPS        │
│  │  │                        │  │                                      │
│  │  │  Less():               │  │                                      │
│  │  │   same user → FIFO     │  │                                      │
│  │  │   diff user → lower    │  │                                      │
│  │  │     token usage first  │  │                                      │
│  │  └────────────────────────┘  │                                      │
│  └──────────────────────────────┘                                      │
│      │                                                                 │
│      │ <── blocks on NotifyChan (max 60s) ──┐                          │
│      │                                      │                          │
│      ▼                               ┌──────┴──────┐                   │
│  ┌───────────────┐                   │  Run() loop  │                   │
│  │doLoadbalance()│◀── close(NotifyChan) ──│  ticker @    │              │
│  └───────────────┘                   │  10ms/tick   │                   │
│                                      └─────────────┘                   │
│      │                                                                 │
│      ▼                                                                 │
│  ┌──────────┐    ┌──────────────┐                                      │
│  │ Scheduler│───▶│ Proxy to Pod │                                      │
│  └──────────┘    └──────────────┘                                      │
│      │                                                                 │
│      ▼                                                                 │
│  ┌──────────────┐                                                      │
│  │UpdateTokenCnt│  (post-response, records completion tokens)          │
│  └──────────────┘                                                      │
└────────────────────────────────────────────────────────────────────────┘
```

### Current Components

| Component | Location | Responsibility |
|-----------|----------|----------------|
| `handleFairnessScheduling` | `router.go:965` | Entry point; extracts userId, gets priority, enqueues, blocks |
| `RequestPriorityQueue` | `fairness_queue.go` | Min-heap ordered by token usage; per-model goroutine dequeues at fixed QPS |
| `InMemorySlidingWindowTokenTracker` | `token_tracker.go` | Sliding-window weighted token accumulator per (user, model) |
| `EnableFairnessScheduling` | `router.go:68` | Global env-var kill switch (`ENABLE_FAIRNESS_SCHEDULING`) |
| Metrics | `metrics.go` | `kthena_router_fairness_queue_size`, `kthena_router_fairness_queue_duration_seconds` |

---

## 2. Identified Design Gaps

### Gap 1: Fixed-Rate Dequeue (Critical)

**Problem:** `Run()` dequeues at a **fixed QPS** (default 100) regardless of actual backend throughput or load. This creates two failure modes:

- **Under-provisioned:** If the model backends can serve 500 req/s, the queue artificially caps throughput at 100 req/s, adding unnecessary latency.
- **Over-provisioned:** If backends can only handle 20 req/s, the queue releases 100 req/s, overwhelming backends and negating the fairness benefit.

**Impact:** The dequeue rate is entirely disconnected from actual serving capacity.

### Gap 2: Priority Is a Point-in-Time Snapshot (Medium)

**Problem:** Priority is computed **once** at enqueue time via `GetTokenCount(userId, modelName)`. While the request waits in the queue (potentially up to 60s), other users' priorities change as their requests complete and tokens accumulate. The queued request's priority is never re-evaluated.

**Impact:** A user who had low token usage at enqueue time but whose other concurrent requests finish (accumulating tokens) will still be treated as low-usage, violating the fairness invariant.

### Gap 3: No Unified Request Cancellation / Timeout Handling (Medium)

**Problem:** When a client disconnects (TCP RST, context cancelled), the `Request` stays in the priority queue. `NotifyChan` is still closed when dequeued, and `doLoadbalance()` may run after the caller is already gone. There is also no unified request-scoped timeout: the handler waits on a hard-coded `time.After(60 * time.Second)`, but that timeout does not cancel the queued item itself.

There is no mechanism to:
1. Remove cancelled or timed-out requests from the queue.
2. Detect `c.Request.Context().Done()` while waiting.
3. Ensure the queue entry and the handler share the same cancellation lifetime.

**Impact:** Wasted backend capacity on dead requests; queue contains phantom entries that distort fairness ordering; timed-out requests may still dequeue later.

### Gap 4: Queue Lifecycle Semantics Are Incomplete (Medium)

**Problem:** `Enqueue()` calls `go newQueue.Run(context.TODO(), defaultQueueQPS)` — the context is `context.TODO()` which never cancels. The current store already removes waiting queues when a `ModelRoute` is deleted, but queue lifetime is still incomplete in two ways:

1. Queue goroutines are not rooted in a parent context and therefore cannot participate in global store shutdown.
2. `Close()` stops the queue loop, but pending waiters are not actively drained or failed with a deterministic error path.

**Impact:** Queue shutdown behavior is brittle, and leaked goroutines remain possible for queues that outlive their intended lifecycle.

### Gap 5: No Multi-Model Fairness (Low-Medium)

**Problem:** Fairness operates independently per model queue. A user consuming heavy resources across 10 different models is treated as a low-priority user on each individual model, because token counts are tracked per `(user, model)`.

**Impact:** Users can circumvent fairness by spreading load across models (relevant in multi-model deployments).

### Gap 6: In-Memory Token Tracker Not HA-Safe (Low)

**Problem:** `InMemorySlidingWindowTokenTracker` is local to a single router pod. In multi-replica deployments, each replica has independent token accounting. User A can get full priority on replica-1 while being heavy on replica-2.

**Impact:** Fairness enforcement weakens linearly with replica count.

### Gap 7: Hard-Coded 60-Second Timeout (Low)

**Problem:** The fairness wait timeout is hard-coded to 60 seconds. No configuration surface exists. For latency-sensitive workloads this may be too high; for batch workloads it may be too low.

### Gap 8: Token-Only Priority Without Request-Count Awareness (Low)

**Problem:** Priority is purely token-based. A user sending many small requests (low tokens each) can monopolize the queue position over a user sending fewer but larger requests, even if the small-request user is consuming disproportionate scheduling/compute overhead.

---

## 3. Proposed Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    Improved Fairness Scheduling                         │
│                                                                         │
│  HTTP Request                                                           │
│      │                                                                  │
│      ▼                                                                  │
│  ┌──────────────────────────────────────────────────┐                   │
│  │              AdmissionController                  │                   │
│  │  1. Extract userId                                │                   │
│  │  2. Get priority (deferred to dequeue)            │                   │
│  │  3. Enqueue with request context + metadata       │                   │
│  └──────────────────────────────────────────────────┘                   │
│      │                                                                  │
│      ▼                                                                  │
│  ┌──────────────────────────────────────────────────┐                   │
│  │           FairnessQueue (per-model)               │                   │
│  │                                                   │                   │
│  │  ┌─────────────────────────────┐                  │                   │
│  │  │  Dequeue Strategy           │                  │                   │
│  │  │  ┌───────────────────────┐  │                  │                   │
│  │  │  │ Backpressure-Aware    │  │  Dequeue rate    │                   │
│  │  │  │ Semaphore:            │  │  = min(QPS cap,  │                   │
│  │  │  │  permits =            │  │    available     │                   │
│  │  │  │  activeUpstream       │  │    capacity)     │                   │
│  │  │  │  Capacity - inFlight  │  │                  │                   │
│  │  │  └───────────────────────┘  │                  │                   │
│  │  │                             │                  │                   │
│  │  │  Priority refresh:          │                  │                   │
│  │  │  bounded recompute +        │                  │                   │
│  │  │  reinsertion / rebuild      │                  │                   │
│  │  └─────────────────────────────┘                  │                   │
│  │                                                   │                   │
│  │  Cancellation:                                    │                   │
│  │  - Request carries request-scoped ctx             │                   │
│  │  - Cancelled entries skipped on dequeue           │                   │
│  │  - Close drains pending waiters with error        │                   │
│  └──────────────────────────────────────────────────┘                   │
│      │                                                                  │
│      │ close(NotifyChan)                                                │
│      ▼                                                                  │
│  ┌──────────────────────────────────────────────────┐                   │
│  │              doLoadbalance()                       │                   │
│  │              (unchanged)                           │                   │
│  └──────────────────────────────────────────────────┘                   │
│      │                                                                  │
│      ▼                                                                  │
│  ┌──────────────────────────────────────────────────┐                   │
│  │           TokenTracker (pluggable)                │                   │
│  │  ┌────────────┐  ┌──────────────┐                │                   │
│  │  │  InMemory   │  │ Redis-backed │  (future)     │                   │
│  │  │  (default)  │  │ (HA deploy)  │               │                   │
│  │  └────────────┘  └──────────────┘                │                   │
│  └──────────────────────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 4. Detailed Improvement Proposals

### 4.1 Backpressure-Aware Dequeue (Addresses Gap 1)

**Goal:** Replace the fixed-rate ticker with a capacity-based admission control.

**Design:**

```go
// FairnessQueueConfig holds configurable parameters for the fairness queue
type FairnessQueueConfig struct {
    // MaxConcurrent is the maximum number of in-flight requests 
    // allowed through the fairness gate for this model.
    // When 0, falls back to MaxQPS-based rate limiting.
    MaxConcurrent int

    // MaxQPS is the upper-bound dequeue rate (retained as a safety cap).
    MaxQPS int
}
```

Replace the fixed ticker in `Run()` with a **semaphore + signal** pattern:

```
On dequeue:
  1. Acquire semaphore permit (blocks if at capacity)
  2. Pop from heap
  3. Close NotifyChan → request proceeds to doLoadbalance()

On request completion (new callback):
    4. Release semaphore permit → unblocks next dequeue
```

**Permit lifecycle requirement:** The permit must be released on every exit path after dequeue, including proxy success, proxy error, request timeout, streaming disconnect, and retry exhaustion. The implementation should have a single owner for permit release, ideally via a single `defer req.Release()` in the handler after dequeue, so capacity cannot leak.

**Capacity source:** `MaxConcurrent` can be derived from:
- Static configuration per model (simplest, recommended first).
- Dynamic: `sum(pod_count) × per_pod_concurrency` from ModelServer spec.

**Fallback:** When `MaxConcurrent = 0`, retain the existing QPS-ticker behavior for backward compatibility.

**Key change in `Request` struct:**

```go
type Request struct {
    // ... existing fields ...
    Release func() // Assigned by the queue after dequeue; called by the handler once
}
```

### 4.2 Bounded Priority Refresh (Addresses Gap 2)

**Goal:** Reduce enqueue-time priority staleness without claiming stronger correctness than the data structure can provide.

**Design:**

Avoid the previous `top-K` heuristic as a correctness mechanism. A heap ordered by stale priorities does not guarantee that the new true minimum remains near the top after priorities drift. Instead, use a bounded refresh strategy:

```
On popWhenAvailable():
    1. Pop the current heap root
    2. Refresh its priority using GetTokenCount(user, model)
    3. If it is still admissible, dequeue it
    4. If not, reinsert it with the refreshed priority and retry
    5. After N refresh-and-reinsert attempts, rebuild the heap from refreshed priorities
```

This keeps the steady-state pop cost low while preserving a deterministic escape hatch when many queued priorities have drifted.

**Configuration:**

```go
// MaxPriorityRefreshRetries bounds refresh-and-reinsert loops before a heap rebuild.
// 0 disables dequeue-time refresh (current behavior).
MaxPriorityRefreshRetries int // default: 2

// RebuildThreshold controls when to refresh all queued priorities and rebuild the heap.
RebuildThreshold int // default: 64
```

**Tradeoff:** This is still an approximation of ideal fairness, but it is a bounded and explicit one. The proposal should describe it as a practical mitigation, not as a proof of exact dequeue optimality. To avoid `heap.Init` becoming a hot-path bottleneck, full heap rebuilds should remain gated by a queue-size threshold; above that threshold the system should continue with incremental refresh-and-reinsert only.

### 4.3 Request Cancellation Support (Addresses Gap 3)

**Goal:** Stop wasting backend capacity on requests whose clients have disconnected.

**Design:**

Keep the request-scoped `context.Context` in the handler, but only store its cancellation signal on the queue item:

```go
type Request struct {
    // ... existing fields ...
    CancelCh <-chan struct{} // Derived from reqCtx.Done(); carries client and timeout lifecycle
}
```

**Three-point cancellation:**

1. **At enqueue:** Create `reqCtx, cancel := context.WithTimeout(c.Request.Context(), r.fairnessTimeout)` and store `reqCtx.Done()` in the `Request`.
2. **At dequeue (in `popWhenAvailable`):** After popping, check whether `req.CancelCh` is closed. If cancelled, skip (decrement metrics, continue to next).
3. **At wait site (in `handleFairnessScheduling`):** Wait on the same request context used by the queue:

```go
defer cancel()

select {
case <-queueReq.NotifyChan:
    r.doLoadbalance(c, modelRequest)
    return nil
case <-reqCtx.Done():
    if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
        return fmt.Errorf("request processing timed out in fairness queue")
    }
    return fmt.Errorf("client disconnected while waiting in fairness queue")
}
```

This unifies client disconnect and server-side timeout handling so the queue entry, the handler, and the eventual dequeue path share one cancellation source.

### 4.4 Queue Lifecycle Management (Addresses Gap 4)

**Goal:** Make queue shutdown deterministic and compatible with store lifecycle.

**Design:**

1. Pass a **cancellable context** to `Run()` instead of `context.TODO()`:

```go
func (s *store) Enqueue(req *Request) error {
    // ...
    if !ok {
        ctx, cancel := context.WithCancel(s.rootCtx)
        newQueue := NewRequestPriorityQueue(nil)
        newQueue.cancel = cancel  // store cancel func
        go newQueue.Run(ctx, defaultQueueQPS)
    }
    // ...
}
```

2. Preserve the existing **model-delete cleanup** path, but make queue close semantics stronger:

```go
func (pq *RequestPriorityQueue) Close(err error) {
    // stop dequeue loop
    // fail all pending waiters deterministically
}
```

3. Optionally add an **idle timeout** (e.g., 10 minutes with no enqueue) so empty per-model queues can self-remove.

**Note:** The current code already deletes waiting queues when a `ModelRoute` is removed. The missing piece is deterministic cancellation and draining, not delete detection itself.

### 4.5 Configurable Timeout (Addresses Gap 7)

**Goal:** Allow operators to tune the fairness wait timeout.

**Design:**

```go
// Environment variable: FAIRNESS_QUEUE_TIMEOUT (default: "60s")
type FairnessConfig struct {
    QueueTimeout time.Duration
}
```

Read from env in `NewRouter()`, store in the `Router` struct, and use in `handleFairnessScheduling`:

```go
reqCtx, cancel := context.WithTimeout(c.Request.Context(), r.fairnessConfig.QueueTimeout)
```

### 4.6 Composite Priority Score (Addresses Gap 8)

**Goal:** Factor in request count alongside token usage for a more balanced priority, while keeping units comparable.

**Design:**

Extend the priority formula:

```
priority = α × normalized_weighted_tokens + β × normalized_request_count
```

Where:
- `normalized_weighted_tokens` = current `GetTokenCount()` value transformed onto a bounded scale
- `normalized_request_count` = request count in the same window, also normalized
- `α`, `β` = configurable weights (default `α=1.0`, `β=0.0` for backward compat)

This requires the `TokenTracker` to also track request counts. Extend the interface:

```go
type TokenTracker interface {
    GetTokenCount(user, model string) (float64, error)
    GetRequestCount(user, model string) (int, error)   // new
    UpdateTokenCount(user, model string, inputTokens, outputTokens float64) error
}
```

**Note:** `β=0.0` by default ensures no behavior change unless explicitly configured. Without normalization, token counts and request counts live on different scales and become difficult to tune safely.

### 4.7 Observability, Starvation, and Rollout Criteria

**Goal:** Make the fairness system measurable and safe to roll out.

**Additional metrics:**

- `fairness_queue_cancelled_total`
- `fairness_queue_timeout_total`
- `fairness_queue_inflight`
- `fairness_queue_dequeue_total`
- wait latency percentiles per model (`p50/p95/p99`)
- priority refresh / heap rebuild counters

**Starvation policy:**

The scheduler should define whether requests age while waiting. Without an aging rule, a perpetually low-priority user can starve under sustained high load. A simple option is to subtract a small waiting-time bonus from effective priority after a configurable delay.

**Load-test acceptance criteria:**

- No throughput regression when fairness is enabled under balanced load.
- No permit leaks under success, error, timeout, and disconnect paths.
- Queue wait percentiles stay bounded under overload.
- Fairness skew improves versus the current fixed-QPS implementation.

---

## 5. Implementation Phases

### Phase 1: Safety & Correctness (Recommended First)

| Item | Gap | Effort | Risk |
|------|-----|--------|------|
| Unified cancellation + timeout support (4.3) | Gap 3 | Small | Low |
| Queue lifecycle management (4.4) | Gap 4 | Small | Low |
| Configurable timeout (4.5) | Gap 7 | Trivial | None |

These are bug-fixes / resource-leak fixes with no behavioral change to existing users.

### Phase 2: Core Fairness Improvement

| Item | Gap | Effort | Risk |
|------|-----|--------|------|
| Backpressure-aware dequeue (4.1) | Gap 1 | Medium | Medium — needs load testing |
| Bounded priority refresh (4.2) | Gap 2 | Medium | Medium |
| Rollout metrics + starvation policy (4.7) | Cross-cutting | Small | Low |

These improve the quality of fairness scheduling. The backpressure change should be gated behind a configuration flag initially.

### Phase 3: Advanced (Future)

| Item | Gap | Effort | Risk |
|------|-----|--------|------|
| Composite priority (4.6) | Gap 8 | Small | Low |
| Cross-model fairness with normalized per-model cost | Gap 5 | Medium | Medium |
| Distributed token tracker (Gap 6) | Gap 6 | Large | Medium — needs Redis dependency |

---

## 6. Recommended Design

**Phase 1 should ship first**, followed by a config-gated Phase 2. The recommended target architecture:

```
┌─────────────────────────────────────────────────────────────┐
│                 Target State Summary                         │
├───────────────────────┬─────────────────────────────────────┤
│ Dequeue strategy      │ Semaphore-gated (capacity-aware)    │
│                       │ with QPS cap as optional safety     │
│                       │ bound                               │
├───────────────────────┼─────────────────────────────────────┤
│ Priority evaluation   │ Root refresh + bounded reinsertion  │
│                       │ with heap rebuild fallback          │
├───────────────────────┼─────────────────────────────────────┤
│ Cancellation          │ Single request-scoped context for   │
│                       │ client disconnect and timeout       │
├───────────────────────┼─────────────────────────────────────┤
│ Queue lifecycle       │ Rooted in store ctx; close drains   │
│                       │ pending waiters deterministically   │
├───────────────────────┼─────────────────────────────────────┤
│ Timeout               │ Configurable via env var             │
├───────────────────────┼─────────────────────────────────────┤
│ Token tracker         │ InMemory (unchanged); interface      │
│                       │ ready for Redis backend              │
├───────────────────────┼─────────────────────────────────────┤
│ Rollout safety        │ Metrics, load tests, and explicit    │
│                       │ anti-starvation policy               │
├───────────────────────┼─────────────────────────────────────┤
│ Backward compatibility│ All new behaviors gated by config;   │
│                       │ defaults preserve current behavior   │
└───────────────────────┴─────────────────────────────────────┘
```

This design addresses the critical throughput mismatch (Gap 1), correctness issues (Gaps 2-4), and operability (Gap 7) while keeping the implementation incremental and backward-compatible. It also avoids overstating the correctness of dequeue-time priority refresh and makes rollout safety a first-class part of the design.
