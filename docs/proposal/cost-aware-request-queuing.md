---
title: Cost-Aware Request Queuing and Token-Budget Admission Control in Kthena Router
authors:
- "@JagjeevanAK"
- "@hzxuzhonghu"
reviewers:
- "@JagjeevanAK"
- TBD
approvers:
- TBD

creation-date: 2026-04-26
---

## Cost-Aware Request Queuing and Token-Budget Admission Control in Kthena Router

### Summary

This proposal introduces a token-cost-aware admission control layer in `kthena-router` so routing decisions and queueing behavior reflect actual request cost, not only request count. Today, pod selection is dominated by request-count signals (for example, `running + 100 * waiting` in `least-request`), which is not robust for mixed workloads where one request may be 64x or more expensive than another. This mismatch can admit expensive requests into already constrained pods, trigger KV-cache pressure/OOM, and inflate tail latency.

The proposal adds token-budget-based admission at router level:
1. Estimate request token cost before dispatch (with safe fallbacks).
2. Track pod in-flight token reservations.
3. Admit only when at least one candidate pod has enough token headroom.
4. Reconcile estimates with actual usage on response.
5. Keep compatibility with existing fairness scheduling, queue semantics, and configuration style.

This design is intentionally incremental: operators can deploy it in observe-only mode first, then soft enforcement, then strict admission once confidence is high.

### Motivation

Current scheduler and queue behavior can misrepresent real load:

1. **Pod score mismatch under mixed costs**  
   A pod handling 2 long-context requests may be effectively saturated while a pod handling 8 short requests is not. Request count alone cannot express this.

2. **Head-of-line and fairness distortions**  
   FIFO/count-driven queue release can admit large requests that block capacity for multiple smaller requests, harming interactive latency.

3. **KV cache and memory pressure**  
   Requests with large prompt and generation budgets can rapidly consume decode/prefill memory. Without admission guardrails, pressure is discovered too late.

4. **Limited operational predictability**  
   Operators currently tune QPS or concurrent-count limits, but these knobs are only indirect proxies for real resource usage.

#### Goals

1. Introduce **token-budget admission control** per model queue and pod.
2. Support **router-side queueing** when all candidate pods exceed budget.
3. Use **request token-cost estimates pre-dispatch**, then reconcile with actual usage.
4. Integrate with existing fairness mechanisms:
   - `ENABLE_FAIRNESS_SCHEDULING`
   - fairness queue priorities (`token + request-count` weighted)
   - queue timeout/cancel semantics
5. Provide strong observability for:
   - estimated vs actual token deltas
   - budget utilization
   - queue delay and rejection reasons
6. Preserve backward compatibility and safe rollout controls.

#### Non-Goals

1. Build a globally consistent cross-router coordinator in this phase.
2. Guarantee perfect request cost prediction for all models/vendors.
3. Replace all scheduler plugins or remove `least-request`.
4. Couple admission logic to vendor-specific engine internals.
5. Enforce hard tenant quotas or billing in this proposal.

### Proposal

Introduce a **Token-Budget Admission Controller** in router request flow:

1. Request enters router and passes authentication/rate-limit checks.
2. Router estimates request cost in token units.
3. Scheduler determines candidate pods (existing path).
4. Admission controller checks each candidate pod's available token headroom.
5. If fit exists, reserve tokens on selected pod and dispatch request.
6. If no fit exists, enqueue request in fairness queue (or cost-aware sub-queue).
7. On response usage event, reconcile reservation with actual usage and release budget.
8. On timeout/cancel/error, release reservation deterministically.

#### User Stories (Optional)

##### Story 1

As a platform operator running mixed workloads (chat + long-context generation), I want admission to consider token cost so long requests do not overwhelm pods and degrade p99 latency.

##### Story 2

As a tenant with bursty short prompts, I want smaller requests to continue flowing when a few heavy requests arrive, instead of being blocked behind them due to count-only scheduling.

##### Story 3

As an SRE, I want metrics that explain queueing decisions (`admitted`, `queued_budget_exceeded`, `rejected_timeout`) and estimate error (`estimated_vs_actual`) so I can tune limits safely.

##### Story 4

As a maintainer, I want this feature to be opt-in with compatibility fallback so existing clusters can upgrade without behavior breakage.

#### Notes/Constraints/Caveats (Optional)

1. **Estimator uncertainty is expected.**  
   The design explicitly supports conservative estimates and post-response reconciliation.

2. **Streaming usage events may arrive late.**  
   Release logic must be robust even when usage is only available at stream tail.

3. **In-memory accounting is per router instance.**  
   In multi-replica router deployments, state is local unless future shared-state mode is enabled.

4. **Routing scope remains model-local in v1.**  
   This proposal does not attempt cross-model pooled budgets.

#### Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Cost estimator underestimates large requests | Over-admission, memory pressure | Safety factor, minimum reservation floor, strict-mode guardrails |
| Estimator overestimates | Unnecessary queue delay | Reconciliation + dynamic calibration metrics |
| Budget leak on error path | Permanent reduced capacity | Single-owner reservation lifecycle with defer-based release |
| Queue starvation of large requests | Fairness regression | Aging factor + max-wait timeout + fairness priority blending |
| Increased routing overhead | Router latency | O(1) accounting structures, bounded queue ops, targeted metrics |
| Multi-router inconsistency | Uneven enforcement | Documented limitation + future Redis-backed shared budget mode |

### Design Details

#### 1. Current State Summary

Relevant existing behavior in codebase:

1. Scheduler `least-request` scoring uses:
   - `base = RequestRunningNum + 100 * RequestWaitingNum`
2. Fairness queue already supports:
   - queue timeout (`FAIRNESS_QUEUE_TIMEOUT`)
   - request cancel propagation
   - optional semaphore mode (`FAIRNESS_MAX_CONCURRENT`)
   - weighted priority (`FAIRNESS_PRIORITY_TOKEN_WEIGHT`, `FAIRNESS_PRIORITY_REQUEST_NUM_WEIGHT`)
3. Token tracker exists:
   - sliding window (`FAIRNESS_WINDOW_SIZE`, default 5m, valid 1m to 1h)
   - input/output weights (`FAIRNESS_INPUT_TOKEN_WEIGHT`, `FAIRNESS_OUTPUT_TOKEN_WEIGHT`)
4. Router already parses usage and updates user-model token stats from response usage fields.

This proposal extends these primitives to budget-admission with pod-level token accounting.

#### 2. High-Level Architecture

```mermaid
flowchart TD
  A[Incoming Request] --> B[Parse + Auth + RateLimit]
  B --> C[Estimate Token Cost]
  C --> D[Schedule Candidate Pods]
  D --> E{Any pod has token headroom?}
  E -->|Yes| F[Reserve tokens on chosen pod]
  F --> G[Dispatch to backend]
  G --> H[Receive usage]
  H --> I[Reconcile estimated vs actual]
  I --> J[Release reservation]
  E -->|No| K[Enqueue in fairness/cost queue]
  K --> L[Re-attempt admission on release or tick]
  L --> E
```

#### 3. Core Components

##### 3.1 TokenCostEstimator

Computes a conservative token-cost estimate for incoming request:

`estimated_total = estimated_prompt_tokens + estimated_output_tokens`

Where:
1. `estimated_prompt_tokens`:
   - Primary: tokenizer estimate from prompt/messages.
   - Fallback: `len(prompt)/4` approximation (existing style).
2. `estimated_output_tokens`:
   - Prefer explicit request fields (`max_tokens` or equivalent).
   - Else default per-model ceiling.
3. Apply safety controls:
   - `estimated_total = ceil(estimated_total * safety_factor)`
   - lower bound (`min_reservation_tokens`)
   - upper bound clamp (`max_reservation_tokens`)

Estimator modes:
1. **Conservative** (default): higher safety factor.
2. **Balanced**: moderate factor.
3. **Observe-only**: no admission enforcement, metrics only.

##### 3.2 PodTokenBudgetTracker

Maintains per-pod budget state:

```go
type PodBudgetState struct {
    PodKey           string
    Model            string
    BudgetTokens     int64
    ReservedInflight int64
    LastUpdatedUnix  int64
}
```

Accounting operations:
1. `TryReserve(pod, cost) -> bool`
2. `Release(pod, reservationID)`
3. `Reconcile(pod, reservationID, actualCost)` (adjust delta)
4. `Utilization(pod) = ReservedInflight / BudgetTokens`

##### 3.3 Admission Queue Manager

Extends fairness queue behavior with budget checks:

1. Queue remains per model.
2. Dequeue attempt includes **fit check** against candidate pods.
3. If no fit, request is reinserted with bounded retry/aging.
4. Timeout/cancel follows existing request-scoped context semantics.

Priority function (composite):

`effective_priority = fairness_priority + cost_weight * normalized_estimated_cost - age_boost`

Notes:
1. Lower value remains higher priority.
2. `age_boost` prevents starvation.
3. Cost term is bounded to avoid over-penalizing large but legitimate jobs.

##### 3.4 Reservation Lifecycle and Safety

Each admitted request receives `reservationID` and release handler:

1. Reserve before proxy dispatch.
2. On every terminal path (success/error/cancel/timeout): release.
3. If usage is available: reconcile before release.
4. Double-release is guarded by `sync.Once`.

#### 4. Request Lifecycle (Detailed)

```mermaid
sequenceDiagram
  participant C as Client
  participant R as Router
  participant Q as Fairness/Admission Queue
  participant B as Budget Tracker
  participant P as Pod

  C->>R: Request
  R->>R: Estimate token cost
  R->>Q: Enqueue/attempt admission
  Q->>B: TryReserve(candidate pod, est_cost)
  alt reserve success
    Q-->>R: Admit + reservation handle
    R->>P: Proxy request
    P-->>R: Response + usage
    R->>B: Reconcile(reservation, actual)
    R->>B: Release(reservation)
    R-->>C: Response
  else reserve fail
    Q-->>R: Wait (queued)
    Note over Q,B: Wake on release/timer/cancel
  end
```

#### 5. Token Estimation Strategy

This proposal defines three strategies and selects hybrid by default:

1. **Actual-only strategy**
   - Reserve fixed default, rely on actual usage later.
   - Pros: simple.
   - Cons: weak pre-dispatch protection.

2. **Pre-request estimate strategy**
   - Estimate from prompt + request limits.
   - Pros: proactive admission quality.
   - Cons: estimation error risk.

3. **Hybrid strategy (recommended)**
   - Pre-request estimate for admission.
   - Post-response reconciliation for correction.
   - Tracks estimator error metrics to support tuning.

#### 6. Scheduler Integration

Two compatible integration options:

1. **Extend `least-request` plugin (recommended)**
   - Add optional token-aware score term:
     - lower utilization -> higher score
   - Preserve existing behavior when disabled.

2. **New score plugin**
   - `token-budget-aware` plugin with clear separation.
   - Can be composed in scheduler profile.

Initial recommendation: extend existing plugin behind explicit config for lower migration cost.

#### 7. Configuration

Keep configuration style consistent with existing fairness settings under:
`networking.kthenaRouter.fairness.*`

Proposed additions:

| Helm value | Env var | Default | Description |
|---|---|---|---|
| `fairness.tokenBudget.enabled` | `FAIRNESS_TOKEN_BUDGET_ENABLED` | `false` | Enable token-budget admission |
| `fairness.tokenBudget.podBudgetTokens` | `FAIRNESS_POD_TOKEN_BUDGET` | `262144` | Per-pod token budget |
| `fairness.tokenBudget.estimationMode` | `FAIRNESS_COST_ESTIMATION_MODE` | `hybrid` | `observe`,`conservative`,`balanced`,`hybrid` |
| `fairness.tokenBudget.safetyFactor` | `FAIRNESS_COST_SAFETY_FACTOR` | `1.2` | Multiplicative safety factor |
| `fairness.tokenBudget.minReservationTokens` | `FAIRNESS_MIN_RESERVATION_TOKENS` | `64` | Minimum reservation |
| `fairness.tokenBudget.maxReservationTokens` | `FAIRNESS_MAX_RESERVATION_TOKENS` | `32768` | Max reservation clamp |
| `fairness.tokenBudget.queueRetryInterval` | `FAIRNESS_ADMISSION_RETRY_INTERVAL` | `10ms` | Retry tick for queued fit checks |
| `fairness.tokenBudget.maxQueueWait` | `FAIRNESS_QUEUE_TIMEOUT` | `60s` | Max wait before timeout |
| `fairness.tokenBudget.ageBoostPerSecond` | `FAIRNESS_QUEUE_AGE_BOOST` | `0.01` | Starvation mitigation term |

Validation rules:
1. Budget > 0
2. Safety factor >= 1.0
3. `maxReservation >= minReservation`
4. Retry interval in sensible bounds (e.g., 1ms–1s)

#### 8. API and Data-Contract Considerations

No external API breaking changes are required.

Internal additions:
1. Extend request metadata in queue entries:
   - estimated cost
   - reservation id
   - admission attempts
2. Extend metrics labels/reasons for admission outcomes.
3. Optional scheduler plugin args for token-aware scoring.

Backend usage fields:
1. Prefer existing OpenAI-compatible usage (`prompt_tokens`, `completion_tokens`, `total_tokens`).
2. If usage unavailable:
   - release by estimate
   - emit `usage_missing` metric

#### 9. Failure Handling and Edge Cases

1. **Client disconnect while queued**: remove/skip queued entry via request context cancellation.
2. **Client disconnect after reserve**: release reservation immediately.
3. **Proxy error with no usage**: release estimate, mark reconcile status `unknown`.
4. **Streaming usage only at end**: retain estimate until stream complete, then reconcile.
5. **Queue timeout**: release if reserved, return 504.
6. **Pod disappears after reserve**: force release and retry scheduling.
7. **Negative reconcile delta** (actual < estimate): immediate credit-back.
8. **Positive delta exceeding remaining headroom**: allow bounded temporary over-commit and emit alert metric.
9. **Estimator unavailable**: fallback to conservative default reservation.
10. **Admission flapping**: debounce retries and cap reinsert loops.

#### 10. Observability

Add metrics:

1. `kthena_router_token_budget_reserved{model,pod}`
2. `kthena_router_token_budget_utilization{model,pod}`
3. `kthena_router_admission_total{model,result}` where result in:
   - `admitted`
   - `queued_budget_exceeded`
   - `rejected_timeout`
   - `rejected_cancelled`
4. `kthena_router_cost_estimate_tokens{model,type}` with type in:
   - `estimated`
   - `actual`
5. `kthena_router_cost_estimation_error_ratio{model}`
6. `kthena_router_admission_queue_wait_seconds{model,user_id}`

Operational dashboards:
1. estimate error trend
2. budget utilization heatmap by pod
3. queue wait p50/p95/p99
4. admission outcome rates

#### 11. Rollout Plan

1. **Phase 0: Observe-only**
   - compute estimates and hypothetical decisions
   - do not block admissions
2. **Phase 1: Soft enforcement**
   - enforce budgets with conservative values
   - alert-only on estimator drift
3. **Phase 2: Full enforcement**
   - strict admission + tuned safety factor
4. **Phase 3: Optimization**
   - refine model-specific defaults
   - evaluate optional shared-state mode

Rollback:
1. Toggle `FAIRNESS_TOKEN_BUDGET_ENABLED=false`
2. Existing fairness and request-count behavior continues unchanged.

#### Test Plan

1. **Unit tests**
   - estimator correctness across prompt shapes
   - budget reserve/release/reconcile invariants
   - queue ordering with aging and fairness weights
   - cancellation/timeout release correctness

2. **Integration tests**
   - mixed workload (short + long requests) across multiple pods
   - verify no budget leaks across failures/retries
   - verify compatibility with fairness disabled and enabled modes

3. **Performance tests**
   - compare p95/p99 latency under mixed-cost load
   - throughput under observe-only vs enforcement modes
   - admission overhead on router CPU and lock contention

4. **Resilience/chaos tests**
   - router restart during in-flight requests
   - pod churn while queue backlog exists
   - streaming cancellation storms

Success criteria:
1. reduced OOM incidents under mixed traffic
2. improved p99 latency stability
3. bounded estimator error after tuning
4. zero reservation leaks in stress tests

### Alternatives

1. **Keep request-count model with different constants**
   - Pros: simplest.
   - Cons: still blind to cost variance; only shifts failure points.

2. **Backend-only queueing/admission**
   - Pros: backend has deeper runtime context.
   - Cons: router still sends bursts blindly; less central policy control.

3. **Prompt-only token estimation without reconciliation**
   - Pros: low complexity.
   - Cons: poor for generation-heavy requests; drift accumulates.

4. **Global Redis-backed token budget coordinator (immediate)**
   - Pros: consistent across router replicas.
   - Cons: higher complexity and failure modes; not required for initial delivery.

5. **Strict per-user quotas instead of budget admission**
   - Pros: policy simplicity.
   - Cons: does not solve pod saturation from heterogeneous request cost.

Recommended path: hybrid estimation + local pod budget admission first, then optional shared-state enhancement.

---

### Appendix A: Example Helm Values

```yaml
networking:
  kthenaRouter:
    fairness:
      enabled: true
      windowSize: "5m"
      inputTokenWeight: 1.0
      outputTokenWeight: 2.0
      tokenBudget:
        enabled: true
        podBudgetTokens: 262144
        estimationMode: hybrid
        safetyFactor: 1.2
        minReservationTokens: 64
        maxReservationTokens: 32768
        queueRetryInterval: "10ms"
        ageBoostPerSecond: 0.01
```

### Appendix B: Open Questions for Review

1. Should initial budget defaults be static or model-size-aware?
2. Should queue aging be linear or capped exponential?
3. Should we support per-model override values in first implementation?
4. Is local-only accounting acceptable for initial multi-router deployments, or should shared mode be included in v1?
