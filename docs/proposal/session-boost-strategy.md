---
title: Session Boost Strategy for Multi-Turn Conversation Prefix Cache Optimization
authors:
- "@YaoZengzeng"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-06-03

---

## Session Boost Strategy for Multi-Turn Conversation Prefix Cache Optimization

### Summary

This proposal introduces a **session-boost priority strategy** for the shared request priority queue (`RequestPriorityQueue`). It is not a separate queue: it is a pluggable priority strategy on the existing priority queue that provides session-aware priority boosting for multi-turn conversations. It allows follow-up requests in the same conversation session to be prioritized for processing, maximizing **prefix cache hit rate** on LLM inference backends (e.g., vLLM) and significantly reducing Time-to-First-Token (TTFT) for multi-turn conversations.

### Motivation

In multi-turn conversation scenarios (e.g., ChatGPT-like interactions), each follow-up message in a conversation shares a common prefix with previous messages. Modern LLM inference engines like vLLM maintain a **prefix cache** (also called KV cache) that stores previously computed key-value attention states. When a follow-up request arrives at the same backend pod shortly after the previous request completes, the prefix cache is still warm, enabling the engine to skip recomputing attention for the shared prefix — dramatically reducing TTFT (often by 50-80%).

However, without session-aware scheduling, a follow-up request from the same conversation may be queued behind unrelated requests from other users. By the time it reaches the backend, the prefix cache may have been evicted, forcing a full recomputation.

The Session Boost Queue addresses this problem by promoting follow-up requests from recently completed sessions to the head of the request queue.

Rather than introducing a separate queue type, session boost is implemented as one of the **pluggable priority strategies** of the shared request priority queue (`RequestPriorityQueue`) that also powers [fairness scheduling](../kthena/docs/user-guide/fairness-scheduling.md). The strategies reuse the same heap, enqueue/dequeue, cancellation, backpressure, and shutdown machinery; they differ only in how request priority is computed:

- **User-fairness strategy** (default): orders requests by per-user recent token usage.
- **Session-boost strategy** (`ENABLE_SESSION_BOOST=true`): disables per-user fairness ordering and instead promotes requests whose session completed recently.

Enabling session boost therefore reconfigures the same per-model priority queue into a session-aware queue; the two strategies are mutually exclusive.

#### Goals

1. **Simple activation**: Session boost can be enabled via `ENABLE_SESSION_BOOST=true`. It is a scheduling strategy mutually exclusive with user fairness; enabling both is a configuration error.
2. **Configurable session identification**: Users can configure which HTTP header identifies conversation sessions via `SESSION_BOOST_HEADER` (e.g., `X-Session-ID`).
3. **KV cache optimization**: Prioritize follow-up requests from recently completed sessions to maximize warm cache hits.
4. **Grace period (advanced, off by default)**: An optional, tricky tuning knob that, after a request completes, briefly holds the dequeue slot for a potential follow-up from the same session before dispatching unrelated requests. It is **disabled by default** and should only be enabled by operators who fully understand that it deliberately delays unrelated requests in exchange for a higher same-session prefix-cache hit rate.
5. **Backpressure-aware**: Respect backend pod capacity to avoid flooding, using two-level admission control (inflight limit + backend metrics).
6. **Fail-fast queue wait timeout (optional, off by default)**: Optionally reject requests that wait in the queue longer than a configurable threshold with HTTP 504, so latency-sensitive clients shed load or retry instead of waiting indefinitely for backend capacity. `FAIRNESS_QUEUE_TIMEOUT` governs only the user-fairness queue and does not apply in session-boost mode.

#### Non-Goals

1. **Cross-router session state coordination**: Each router instance maintains independent session state.
2. **Guaranteed pod affinity**: Session boost only prioritizes queue ordering; it does not guarantee the request routes to the same pod (that's the domain of session sticky).
3. **Persistent session state**: Session tracking state does not survive router restarts.
4. **Replacing session sticky**: Session boost complements but does not replace pod-level session affinity for stateful inference.

### Proposal

#### Architecture

Session boost is a priority strategy of the shared per-model request priority queue that sits in the request processing pipeline between the router's HTTP handler and the backend load balancer. When `ENABLE_SESSION_BOOST=true`, the same `RequestPriorityQueue` that implements the default user-fairness strategy is constructed with the session-boost strategy instead.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          Request Flow                                   │
│                                                                         │
│  HTTP Request                                                           │
│       │                                                                 │
│       ▼                                                                 │
│  ┌───────────┐    ┌──────────────────┐    ┌──────────────────────────┐  │
│  │  Router   │───>│ RequestPriority  │───>│  Backend Load Balancer   │  │
│  │  Handler  │    │ Queue            │    │  (scheduler + plugins)   │  │
│  │           │    │ (session-boost   │    │                          │  │
│  │           │    │  mode)           │    │                          │  │
│  └───────────┘    └──────────────────┘    └──────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘
```

#### Core Mechanism: Session Tracking and Priority Boosting

```
┌──────────────────────────────────────────────────────────┐
│          Session Boost Queue Internals                   │
│                                                          │
│  ┌──────────────────┐     ┌─────────────────────────┐    │
│  │ SessionTracker   │<────│ MarkSessionRequest      │    │
│  │ (bounded LRU)    │     │ Completed()             │    │
│  │ keys: sessionID  │     │ (after response sent)   │    │
│  │ cap: 4096 default│     └─────────────────────────┘    │
│  └────────┬─────────┘                                    │
│            │                                             │
│            │ HasRecentCompletion(sessionID)?             │
│            ▼                                             │
│  ┌──────────────────┐                                    │
│  │  PushRequest()   │                                    │
│  │                  │                                    │
│  │  if recent ───> SessionBoost = true                   │
│  │  else      ───> SessionBoost = false                  │
│  └────────┬─────────┘                                    │
│            │                                             │
│            ▼                                             │
│  ┌──────────────────────────────────────┐                │
│  │       Priority Heap                  │                │
│  │                                      │                │
│  │  Ordering:                           │                │
│  │  1. SessionBoost=true > false        │                │
│  │  2. Within same boost: FIFO          │                │
│  │                                      │                │
│  │  [Boosted-1] [Boosted-2] [Normal-1]  │                │
│  └──────────────────────────────────────┘                │
│            │                                             │
│            ▼                                             │
│  ┌──────────────────────────────────────┐                │
│  │  Backpressure Dequeue Gate           │                │
│  │                                      │                │
│  │  Gate 1: inflight < MaxConcurrent    │                │
│  │  Gate 2: backendChecker() == true    │                │
│  │  Gate 3: grace period (optional)     │                │
│  └──────────────────────────────────────┘                │
└──────────────────────────────────────────────────────────┘
```

#### Session Boost Lifecycle (Multi-Turn Conversation)

```
Time ──────────────────────────────────────────────────────────────────▶

User A, Session "conv-123":

  Turn 1: "Hello, tell me about Kubernetes"
  ┌──────────┐    ┌──────────┐    ┌──────────────┐    ┌────────────────┐
  │ Enqueue  │──> │  Dequeue │───>│ Process on   │──> │ MarkRequest    │
  │ (no      │    │  (normal │    │ Pod-X        │    │ Completed      │
  │  boost)  │    │   order) │    │              │    │ ("conv-123")   │
  └──────────┘    └──────────┘    └──────────────┘    └───────┬────────┘
                                                              │
                  SessionTracker.MarkRequestCompleted("conv-123")     │
                                                              │
  Turn 2: "Can you give more details on pods?"                │
  ┌──────────┐                                                │
  │  Enqueue │ <── HasRecentCompletion("conv-123") = true  ───┘
  │  (BOOST  │     (still in LRU cache)
  │   =true) │
  └────┬─────┘
       │
       ▼ (Promoted to heap head, ahead of all non-boosted requests)
  ┌──────────┐    ┌──────────────┐
  │  Dequeue │ ─> │  Process on  │  ← Prefix cache HIT! TTFT reduced 50-80%
  │  (first!)│    │  Pod-X*      │    (*if session sticky also routes here)
  └──────────┘    └──────────────┘
```

#### Grace Period Mechanism

> **⚠️ Advanced, tricky feature — disabled by default.** The grace period is an advanced, scenario-specific tuning knob that is **off by default** (`SessionBoostGracePeriod = 0`). It is the single most easily-misused part of this design: enabling it deliberately **delays unrelated (non-boosted) requests** in the hope of catching a same-session follow-up. It only pays off for fast automated pipelines (RAG chains, agents) whose follow-up arrives within milliseconds; for human-driven or single-turn traffic it adds latency for no benefit. **Only enable it if you are certain you understand the trade-off described below**, and keep the window very small (tens of milliseconds at most). When in doubt, leave it disabled.

The grace period is a brief wait after a request completes (Release), designed to give the same session's follow-up request a chance to arrive and be prioritized:

```
Timeline:
         ┌─ req-1 completes, Release() called
         │
         │  ┌─ Grace Period (default 0 = OFF) ─┐
         │  │                                  │
         ▼  ▼                                  ▼
    ─────┼──┼──────────────────────────────────┼─────
         │  │                                  │
         │  │  Case A: Head already boosted →  │
         │  │  skip grace, dequeue immediately │
         │  │                                  │
         │  │  Case B: Otherwise hold the slot │
         │  │  for the full grace period, then │
         │  │  dequeue the head (boosted ranks │
         │  │  first, so a follow-up that      │
         │  │  arrived wins automatically)     │
         │  └────────────────────────────────┐
```

##### How the grace wait cooperates with the inflight and backend gates

The grace period is **not a third admission gate that decides whether a request may run** — that decision is still owned entirely by the two capacity gates (`inflight < MaxConcurrent` and `backendChecker() == true`). Instead, the grace wait is a *timing layer in front of those gates* that decides **which request gets to attempt admission first**, and **when** that attempt happens. The two capacity gates always run afterward, unchanged, inside `tryBackpressureDequeue`.

It is important that grace is **triggered only by release events**, because a release is the one moment that frees inflight capacity. The sequence is:

1. A request finishes → `Release()` runs → `inflightCount` is decremented → a signal is sent on `releaseCh`.
2. The freed slot would normally be claimed immediately by the current heap head (which may be an unrelated, non-boosted request). The grace wait instead **holds that just-freed slot for up to `SessionBoostGracePeriod`**, betting that a same-session follow-up is about to arrive and reuse the warm prefix cache.
3. When the wait resolves, `tryBackpressureDequeue` runs and re-checks **both** capacity gates before admitting anyone. A boosted follow-up that arrived during grace is admitted first because boosted requests outrank others in the heap, and only when `inflight < MaxConcurrent` **and** at least one backend pod reports capacity.

The grace wait can resolve in three ways, and in every case the two capacity gates are the final arbiter:

| Outcome                    | Trigger                                                                                       | What happens next                                                                                                                                                                                                                                       |
| -------------------------- | --------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Skip grace (fast path)** | The heap head is *already* session-boosted when the release fires (`isHeadSessionBoosted()`). | No wait at all — go straight to the capacity gates and admit if they pass. There is nothing to wait for.                                                                                                                                                |
| **Grace expires**          | The timer fires (`timer.C`).                                                                  | Stop holding the slot and fall through to the capacity gates, which admit the heap head. If a same-session follow-up arrived during the wait it is now boosted and sits at the head, so it wins automatically (subject to inflight + backend capacity). |

This ordering matters because the three checks answer different questions and run in sequence:

```
Release frees a slot
        │
        ▼
┌───────────────────────────┐   "Should I hold this freed slot briefly
│ Grace timing layer        │    for a same-session follow-up?"
│ (only on releaseCh)       │   → waits 0–GracePeriod, then proceeds
└───────────┬───────────────┘
            ▼
┌───────────────────────────┐   "Is there global inflight headroom?"
│ Gate 1: inflight <        │   → if not, hold (and drain cancelled reqs)
│         MaxConcurrent     │
└───────────┬───────────────┘
            ▼
┌───────────────────────────┐   "Does any backend pod actually have room?"
│ Gate 2: backendChecker()  │   → if not, hold (and drain cancelled reqs)
└───────────┬───────────────┘
            ▼
     Dequeue heap head
```

Two interactions are worth calling out explicitly:

- **Grace never overrides backpressure.** If the grace window ends but the inflight limit is already reached or every backend pod is busy, the request is **not** admitted — `tryBackpressureDequeue` simply holds (and drains any cancelled requests from the heap) until the next release or new arrival reopens the gates. Grace only chooses *who* tries next; the capacity gates decide *whether* anyone runs.
- **Fresh arrivals on an idle queue bypass grace.** Grace is tied to `releaseCh` (a freed slot), not to `notifyCh` (a new arrival). When a request lands on an otherwise idle queue with no pending release, it goes straight to the capacity gates with no grace delay, so enabling grace adds no admission latency to first turns. The only place a new arrival waits is *inside* an already-running grace window, where it is precisely the boosted follow-up the window exists to catch. When both a release and a new arrival are pending at once, the release is preferred so the freed slot is the one held for the grace window.

The net effect is a strict precedence: **grace timing → inflight gate → backend gate**. The grace layer is purely additive and optional (`SessionBoostGracePeriod = 0` removes it entirely, taking the no-grace fast path), and it can only ever *delay* an admission to favor a session-boosted follow-up — it can never admit a request that the inflight or backend gates would otherwise reject.

#### Configuration

| Environment Variable             | Default        | Description                                                                                                                                                                                                                                                              |
| -------------------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `ENABLE_SESSION_BOOST`           | `false`        | Enable the session-boost scheduling strategy (mutually exclusive with `ENABLE_FAIRNESS_SCHEDULING`)                                                                                                                                                                      |
| `SESSION_BOOST_HEADER`           | `X-Session-ID` | HTTP header used to identify conversation sessions                                                                                                                                                                                                                       |
| `SESSION_BOOST_MAX_SESSIONS`     | `4096`         | Maximum number of recently-completed sessions kept warm for boosting. Bounds an LRU cache; the least-recently-used session is evicted when exceeded. Sized by session count, not time                                                                                    |
| `SESSION_BOOST_GRACE_PERIOD`     | `0`            | Wait time after release for same-session follow-up. Disabled by default; enable only when you understand the latency trade-off                                                                                                                                           |
| `SESSION_BOOST_INFLIGHT_PER_POD` | `16`           | Inflight requests admitted per backend pod; total inflight = perPod x backend pod count. Size it from the estimated per-pod concurrency (e.g. vLLM's --max-num-seqs)                                                                                                     |
| `SESSION_BOOST_TIMEOUT`          | `30s`          | Maximum time a request may wait in the queue before it is rejected with HTTP 504. Enabled by default; set a non-positive duration (e.g. `0s`) to disable it. It is the only server-side queue-wait bound in session-boost mode (`FAIRNESS_QUEUE_TIMEOUT` does not apply) |

### Design Details

#### Data Structures

Session boost reuses the shared `RequestPriorityQueue` and its `FairnessQueueConfig`. The session-boost options are additional fields on that config, and session-boost state is added to the queue itself. When `SessionBoostEnabled` is set, the queue's `Less` comparison switches from per-user fairness ordering to session-boost ordering, and `Run` dispatches to the backpressure/grace-period dequeue loop instead of the QPS/semaphore loop.

```go
// FairnessQueueConfig (shared) — session-boost fields
type FairnessQueueConfig struct {
    // ... fairness fields (TokenWeight, RequestNumWeight, MaxQPS, ...) ...

    // MaxConcurrent is the queue-level limit; in the session-boost strategy it is the global (total) inflight
    // limit (default 16 when unset). Operators size it from per-pod concurrency
    // multiplied by pod count.
    MaxConcurrent int

    SessionBoostEnabled      bool          // Switch from user-fairness to session-boost strategy
    SessionIDHeader          string        // HTTP header for session identification
    SessionBoostMaxSessions  int           // LRU capacity: max recently-completed sessions kept warm
    SessionBoostGracePeriod  time.Duration // Wait for same-session follow-up (default: 0, disabled)
}

// RequestPriorityQueue (shared) — session-boost state
type RequestPriorityQueue struct {
    heap         []*Request      // Shared heap; ordering depends on mode (see Less)
    config       FairnessQueueConfig
    tokenTracker TokenTracker    // Used for fairness-mode priority

    // Session-boost mode (active when config.SessionBoostEnabled)
    sessionBoost   bool
    sessionTracker *SessionTracker       // Tracks recently completed sessions
    backendChecker BackendWaitingChecker // Backend capacity gate
    inflightCount  atomic.Int64          // Current inflight requests
    releaseCh      chan struct{}         // Release-driven dequeue signal
}

// Session-boost ordering (RequestPriorityQueue.Less when sessionBoost == true):
// 1. SessionBoost=true comes before SessionBoost=false
// 2. Within same boost status: earlier RequestTime comes first (FIFO)
```

#### Session Identification

Sessions are identified by a configurable HTTP header (default: `X-Session-ID`, controlled by `SESSION_BOOST_HEADER` environment variable). This is a client-provided identifier that groups related requests in a multi-turn conversation:

```
POST /v1/chat/completions
X-Session-ID: conv-abc-123
X-Request-ID: req-001

{"model": "llama-3", "messages": [...]}
```

Operators can customize the header name to match their client conventions:

```bash
# Use a custom header for session identification
export SESSION_BOOST_HEADER="X-Session-ID"
```

When a request with the configured session header (e.g., `X-Session-ID: conv-abc-123`) completes successfully, the session tracker records the session in a bounded LRU cache, promoting it to the most-recently-used position. Any subsequent request that carries the same session identifier, while the session is still in the cache (i.e. not yet evicted by newer sessions), will be marked as session-boosted and promoted to the head of the queue. Bounding by session count rather than a time-based TTL mirrors how inference engines evict their prefix cache and removes the need to tune a duration.

#### Backpressure Control

The queue uses two-level admission control:

1. **Inflight limit**: At most `InflightPerPod` requests can be in-flight per backend pod; the total limit scales with pod count (`SESSION_BOOST_INFLIGHT_PER_POD`). This prevents flooding backends between metric scrapes.
2. **Backend metrics check**: The `BackendWaitingChecker` reads the backend pod metrics already scraped by the store (e.g., vLLM's `RequestWaitingNum`) to confirm at least one pod has capacity. It does not scrape backends itself.

When a request completes (Release), the queue immediately attempts to dequeue the next request (release-driven dequeue), ensuring minimal latency between sequential requests. The loop is fully event-driven — there is no independent polling timer. In single-router operation every moment a backend frees capacity coincides with one of our own requests completing (a release), so release and arrival events alone cover every dequeue opportunity; the capacity check simply reads the pod metrics already scraped by the store (`METRICS_SCRAPE_INTERVAL`).

#### Queue Wait Timeout (504 Rejection)

Under sustained overload the two-level admission control legitimately holds requests in the queue until backend capacity frees up. `FAIRNESS_QUEUE_TIMEOUT` governs **only** the user-fairness queue and does not apply in session-boost mode. Instead, session boost bounds the wait with its own timeout so latency-sensitive front ends are told *quickly* that the system is saturated and can retry elsewhere or shed load rather than waiting open-endedly (until the client disconnects, which returns `503`).

The queue wait timeout provides this fail-fast behavior:

- It is controlled by `SESSION_BOOST_TIMEOUT` and enabled by default at `30s`. Setting it to a non-positive duration (e.g. `0s`) disables the timeout, leaving a session-boost request bounded only by client disconnect.
- When a request has been waiting in the queue longer than `SESSION_BOOST_TIMEOUT`, the router stops waiting, removes the request from the queue, and responds with `504 Gateway Timeout`.

It is implemented in the router's request-handling path rather than in the queue's dequeue loop, mirroring how the fairness queue's own `FAIRNESS_QUEUE_TIMEOUT` (504) is handled. Both queue-wait timeouts therefore return the same `504` status; they differ only in what arms them (`FAIRNESS_QUEUE_TIMEOUT` vs `SESSION_BOOST_TIMEOUT`). In session-boost mode the request context carries the `SESSION_BOOST_TIMEOUT` deadline (via `context.WithTimeout`) instead of the fairness deadline, so `FAIRNESS_QUEUE_TIMEOUT` has no effect. The handler waits on the admission (`NotifyChan`) and cancellation/timeout (`reqCtx.Done()`) signals:

- **Admitted first**: the request proceeds to the backend as usual (no rejection).
- **Timeout fires first**: the request context's deadline is exceeded, which makes the queue drop the request from the heap via its existing cancellation check (`isCancelled` / `drainCancelledLocked`), reusing the same cleanup path as client disconnects; the handler abandons the request, releases any permit that may have been granted concurrently, and returns `504 Gateway Timeout`.
- **Client disconnect fires first**: the existing behavior is unchanged (`503`).

Because it reuses the queue's cancellation cleanup, the wait timeout adds no new state to the queue and no extra polling. It is orthogonal to the inflight and backend capacity gates: those gates decide *whether* a request may run, while the wait timeout only bounds *how long* a request is willing to wait before giving up. `SESSION_BOOST_TIMEOUT` is the only server-side queue-wait bound in session-boost mode; the feature only applies in session-boost mode.

### Multi-Turn Conversation Advantages

#### 1. Prefix Cache Hit Rate Improvement

In a typical multi-turn conversation, each message includes the full conversation history as context:

```
Turn 1: System prompt + User message 1           (1000 tokens)
Turn 2: System prompt + User message 1 + Response 1 + User message 2  (3000 tokens)
Turn 3: System prompt + ... + User message 3     (5000 tokens)
```

Without session boost, Turn 2 may be queued behind 10 other requests. By the time it reaches the backend, the KV cache entries for the first 1000 tokens may be evicted. With session boost, Turn 2 is prioritized immediately after Turn 1 completes, hitting the warm prefix cache and only computing attention for the new ~2000 tokens.

**Expected TTFT improvement**: For a 5000-token prompt where 3000 tokens are cached prefix, TTFT is reduced by approximately 60% (only 2000 tokens need computation vs 5000).

#### 2. Grace Period for Natural Conversation Flow

Human users typically take 1-50ms between receiving a response and submitting the next message (for automated pipelines) or the follow-up may arrive within seconds (for human users). The grace period is a **tricky, advanced feature that is disabled by default (`0`)** to avoid adding latency to non-boosted requests. When explicitly enabled (e.g., `50ms`), it holds the dequeue slot briefly for automated multi-turn pipelines (like RAG chains or agentic workflows) that issue follow-up requests programmatically. Because it deliberately delays unrelated requests, **only enable it if you are sure you understand the trade-off**: it helps only when follow-ups arrive within milliseconds, and for human-driven or single-turn traffic it adds latency for no gain. Keep the window very small (tens of milliseconds), and when in doubt leave it off.

#### 3. No User ID Requirement for Priority

Session boost derives priority from the configured session header (default: `X-Session-ID`) rather than the user identity. Even though it runs as a strategy of the priority queue, the session-boost ordering does not depend on a user ID, so prefix cache optimization works for unauthenticated requests (which are simply enqueued without a boost until their session completes once).

#### 4. Complementary with Session Sticky

When combined with session sticky routing (which routes the same session to the same pod), session boost ensures the follow-up request is both:
- **Prioritized** (processed sooner via session boost queue)
- **Routed to the same pod** (via session sticky)

This combination maximizes prefix cache benefits: the request arrives quickly AND hits the pod where the cache is stored.
