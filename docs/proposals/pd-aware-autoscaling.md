# PD-Aware Autoscaling for ModelServing

**Status:** Draft · **Area:** `pkg/autoscaler`

## Motivation

Kthena's ModelServing supports disaggregated prefill/decode deployments, but the autoscaler treats each role as an isolated workload. Each role is driven by its own `AutoscalingPolicyBinding` via a `SubTarget`, with an independent `Autoscaler`, metric collector, and stabilization history. No signal crosses between roles.

The only existing multi-target decision point is the `Optimizer` (heterogeneous path), designed for hardware heterogeneity (H100 vs A100) rather than role coordination. It produces one scalar recommendation and redistributes replicas by `Cost` / `CostExpansionRatePercent`. Role semantics (prefill vs decode) are not part of that model.

## Problem

Prefill and decode have different cost drivers:

- **Prefill** scales with prompt length (input tokens, one-shot compute).
- **Decode** scales with generated length (output tokens, sustained KV-cache pressure).

The mix varies sharply by workload:

- **Summarization / RAG:** long input, short output → prefill-heavy.
- **Code generation / long-form chat:** short input, long output → decode-heavy.
- **Mixed tenants:** ratio drifts continuously.

Because each role scales on its own signals, two failure modes appear:

1. **Imbalance stalls throughput.** One role saturates while the other is idle. Neither autoscaler sees the other as the end-to-end bottleneck.
2. **Inefficient replica spend.** Both roles scale up together on correlated load even when only one is constrained.

A strict ratio (e.g. `prefill:decode = 1:2`) was considered and rejected: workload variability makes any fixed ratio wrong most of the time, and it conflicts with per-target `MinReplicas` / `MaxReplicas` and panic mode.

## Proposal: Preference-based Coordination

Introduce a **soft coordination layer** inside the `Optimizer` that biases replica redistribution toward a preferred per-role balance — as a *preference*, not a hard rule.

Core properties:

- **Preference, not enforcement.** Users declare a soft band per role. The optimizer stays inside it when possible, drifts outside only when per-target min/max or panic mode require it.
- **Opt-in.** Disabled by default; existing bindings behave identically.
- **No new scaling path.** Coordination refines the existing optimizer's redistribution step; it does not replace it.

## API

One optional field, `coordination`, is added to `HeterogeneousTarget`. It is the entire user-facing surface.

### Scope (Phase 1)

Phase 1 is validated on the two-role `prefill` / `decode` case, but the API is **role-agnostic**: roles are expressed as a map keyed by `subTargets.name`. Adding a third role in Phase 2 requires no schema change.

### Field reference

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `coordination` | object | no | unset (≡ `mode: Off`) | Optional block. Omitting it is equivalent to `mode: Off`. |
| `coordination.mode` | enum | no | `Off` | `Off` or `Preferred`. `Off` disables coordination; `Preferred` enables the soft bands below. |
| `coordination.preferredRatio.roles` | map[string]Band | yes if `Preferred` | — | Map from role name (matching `subTargets.name`) to its `{min, max}` percent-of-total band. |
| `coordination.preferredRatio.roles[*].min` | int | yes | — | Lower bound on this role's share of total replicas, in percent. |
| `coordination.preferredRatio.roles[*].max` | int | yes | — | Upper bound on this role's share of total replicas, in percent. |

**Validation.**
- For each role: `0 ≤ min ≤ max ≤ 100`. `min == max` is allowed (single-point preference, still soft).
- Sum of all `min` values ≤ 100 and sum of all `max` values ≥ 100 (otherwise no feasible split exists).
- Every role key must match an existing `subTargets.name` in the same binding. Roles in `params` but absent from the map are unconstrained (treated as `{min: 0, max: 100}`).

The bands are *soft* — per-target `minReplicas` / `maxReplicas` and panic mode always win.

### Example 1 — Decode-heavy workload (chat / long-form generation)

```yaml
apiVersion: workload.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
spec:
  policyRef:
    name: llm-policy
  heterogeneousTarget:
    params:
      - target:
          targetRef: { kind: ModelServing, name: llama-3 }
          subTargets: { kind: Role, name: prefill }
        minReplicas: 1
        maxReplicas: 20
      - target:
          targetRef: { kind: ModelServing, name: llama-3 }
          subTargets: { kind: Role, name: decode }
        minReplicas: 1
        maxReplicas: 40
    coordination:
      mode: Preferred
      preferredRatio:
        roles:
          prefill: { min: 15, max: 35 }
          decode:  { min: 65, max: 85 }
```

### Example 2 — Prefill-heavy workload (summarization / RAG)

```yaml
heterogeneousTarget:
  params: [ ... ]
  coordination:
    mode: Preferred
    preferredRatio:
      roles:
        prefill: { min: 50, max: 80 }
        decode:  { min: 20, max: 50 }
```

### Example 3 — Coordination disabled (default, identical to today)

```yaml
heterogeneousTarget:
  params: [ ... ]
  # coordination omitted → mode: Off
```

## Behavior

### When `coordination` is off or unset

The optimizer runs exactly as today: compute a joint scalar recommendation, redistribute by `Cost` / `CostExpansionRatePercent`, clamp to per-target min/max. **Byte-identical output to the current implementation.**

### When `coordination.mode: Preferred`

The optimizer pipeline gains one step (3); the others are unchanged:

1. Collect per-target metrics. *(unchanged)*
2. Compute joint scalar recommendation `N` (total replicas across roles). *(unchanged)*
3. **New:** compute a per-role *pressure score* and a target share for each role.
4. Redistribute replicas using existing cost ordering, biased toward the target shares; clamp to per-target `minReplicas` / `maxReplicas`.
5. Write replicas per target. *(unchanged)*

### Allocation logic

The blended allocator is a **convex combination** of two signals followed by **clamping into the preferred band**, then to per-target min/max. Inputs are computed once per cycle.

**Reading the formula in plain language.** For each role we compute two views of how many replicas it should get. `costShare` is the split today's optimizer would pick from cost alone — the "business as usual" answer. `pressureShare` is the split implied by live load signals (queue depth, TTFT, KV-cache) — the "where the pain is right now" answer. We blend them with `alpha`: `alpha = 0` ignores pressure (today's behavior), `alpha = 1` follows pressure entirely, `0.5` weighs them equally. The blended share can land outside the user's preferred band, so step 4 clamps it back in and renormalizes; step 5 then enforces the hard per-target `minReplicas` / `maxReplicas`. Clamping is what keeps the result a *preference* rather than a free-running optimization.

For each role `r`:

```
# 1. Cost-based share — what today's optimizer would pick (no coordination).
costShare[r]     = costAllocator(N, costs)[r] / N        # in [0, 1]

# 2. Pressure-based share — drives replicas toward the role under load.
pressure[r]      = normalize(role-specific signals)      # in [0, 1], see "Pressure signals"
pressureShare[r] = pressure[r] / sum(pressure[*])        # in [0, 1]
                                                         # if sum == 0, fall back to costShare

# 3. Blend cost and pressure (alpha = pressure influence).
rawShare[r] = (1 - alpha) * costShare[r] + alpha * pressureShare[r]

# 4. Clamp into the preferred band, then renormalize so shares sum to 1.
clamped[r]  = clip(rawShare[r], band[r].min/100, band[r].max/100)
share[r]    = clamped[r] / sum(clamped[*])

# 5. Convert to integer replicas, then apply per-target min/max.
replicas[r] = clamp(round(share[r] * N), target[r].minReplicas, target[r].maxReplicas)
```

If renormalization in step 4 pushes any share back outside its band (possible when the post-clip sum ≠ 1 and the bands are tight), the band cannot be jointly satisfied this cycle: keep the renormalized values and report `bandSatisfied=false` with `reason=Infeasible`. If step 5's per-target clamping forces the realized split outside the band, `bandSatisfied` is reported as `false` with `reason=Clamped`. When `coordination` is off, `alpha = 0` and bands are absent — the formula collapses to `replicas[r] = costAllocator(...)[r]`, byte-identical to today.

### Phase 1 pressure signals

A concrete, fixed formula for v1:

- **Prefill pressure:** `0.5 * norm(queueDepth) + 0.5 * norm(TTFT)`
- **Decode pressure:** `0.5 * norm(queueDepth) + 0.5 * norm(kvCacheUtilization)`
- **Fallback:** if a secondary signal (TTFT or KV-cache) is unavailable for the runtime, pressure falls back to `norm(queueDepth)` alone (weight 1.0). If any role's pressure is unavailable (collector error, distinct from a zero reading), `pressureShare` is skipped this cycle and `rawShare = costShare` for all roles; `reason=PressureUnavailable` is reported.

**Why KV-cache for decode.** Decode replicas hold one KV-cache entry per in-flight request for as long as the request is generating tokens. The cache is finite GPU memory, so as more concurrent decode requests pile up, utilization climbs; once it nears full, the engine must evict, queue, or reject new requests. High KV-cache utilization is therefore a direct, early indicator that *this* decode replica is running out of room and another one would help — unlike GPU utilization, which can remain high even when the cache is the binding constraint.

**Why `min(x, 1)` (saturation).** Raw signals like queue depth or TTFT have no natural upper bound — a queue of 200 and a queue of 2000 are both "very bad," but a linear ratio would make the second look 10× worse and dominate the blend. Clamping each normalized signal at 1.0 says "past this threshold, pressure is maxed out; further increases don't change the allocation decision." This keeps one runaway metric from drowning out the others and keeps `pressureShare` well-behaved.

Making the pressure function pluggable is deferred to Phase 2.

### Phase 1 defaults

Defaults live in controller config (not the API) so operators can tune without a CRD change. Phase 1 ships with:

| Constant | Default | Intent |
|---|---|---|
| `alpha` (pressure influence in the blend) | **0.5** | Equal weight to cost-based allocation and observed pressure. High enough to react to imbalance, low enough to avoid thrash. |
| `beta` (intra-role weight between primary and secondary signals) | **0.5** | Queue depth and the role-specific signal (TTFT or KV-cache) contribute equally. |
| `queueDepth` normalization | `min(queueDepth / 32, 1.0)` | `queueDepth` is the per-replica average (target backlog ÷ ready replicas) at the optimizer tick. Saturates at a backlog of 32 requests per replica — empirically the point where added latency dominates. |
| `TTFT` normalization | `min(ttft_ms / 2000, 1.0)` | Saturates at 2 s TTFT, the typical user-perceived ceiling. |
| `kvCacheUtilization` normalization | identity (already 0–1) | Provider metric is already a ratio. |
| Coordination cycle | one optimizer tick (existing cadence) | No new control loop. |

These are starting points, not policy. Phase 2 tunes them against representative benchmarks.

### Interaction with existing features

- **No breaking changes.** Existing bindings without `coordination` behave identically.
- **Per-target `minReplicas` / `maxReplicas`** always win over the preferred band. When they force a drift, status reflects it (see below).
- **Panic mode.** If any sub-target enters panic mode, coordination is suspended for the whole binding for that cycle (`alpha = 0`, no band clamp); `reason=PanicMode` is reported. Per-role partial suspend is a Phase 2 question.
- **Stabilization windows** are unaffected — coordination runs after the scalar recommendation, not instead of it.
- **Metric collectors** are reused; no new scrape path.

### Observability

When `mode: Preferred`, the binding status reports:

```yaml
status:
  coordination:
    realizedShares:                     # actual percent of replicas per role this cycle,
      prefill: 27                       # after all clamping. Compare to preferredRatio:
      decode: 73                        # equal → band held; different → min/max or panic forced drift.
    pressure:
      prefill: 0.42                     # normalized 0.0–1.0
      decode: 0.71
    bandSatisfied: true                 # false if clamp forced drift outside preferredRatio
    lastTransitionTime: "..."
  conditions:
    - type: CoordinationBandSatisfied
      status: "True"                    # "False" with reason=Clamped / PanicMode
```

`realizedShares` is the *outcome*, not the *intent*: it is the percent split that was actually written to the targets after the blended allocator, the band clamp, the per-target `minReplicas` / `maxReplicas` clamp, and any panic-mode override. When the workload sits comfortably inside the preferred band, `realizedShares` will track inside `preferredRatio`. It can deviate when (a) a per-target min/max forces more or fewer replicas than the band wants, (b) panic mode is active and coordination is suspended, or (c) integer rounding at small replica counts nudges the split by a percentage point. `bandSatisfied=false` with a `reason` is how the status surfaces these deviations.

The same values are emitted as Prometheus metrics in Phase 2.

## Example Scenario

A tenant runs a mixed workload on `llama-3`: chat traffic (decode-heavy) during the day, a summarization batch job (prefill-heavy) at night. The binding uses `prefill: {min: 20, max: 60}`, `decode: {min: 40, max: 80}`.

**Daytime — decode-heavy (long outputs):**

- Decode queue grows; prefill is idle.
- Today: decode scales up alone, but prefill is over-provisioned from the morning ramp and stays put → wasted replicas.
- With coordination: pressure favors decode; allocator shifts the split toward decode (prefill near 20%, decode near 80%), freeing replica budget where it is actually needed.

**Nighttime — prefill-heavy (long prompts, short outputs):**

- Prefill TTFT spikes; decode is underused.
- Today: prefill scales up, but decode replicas linger from daytime → both roles peak simultaneously.
- With coordination: pressure shifts to prefill; allocator pushes the split toward the upper bound (prefill near 60%), decode drains naturally.

Across the day, the realized shares stay inside the bands when the workload allows, and `status.coordination` reports them so the user can validate before tuning.

## Alternatives Considered

| Approach | Pros | Cons |
|---|---|---|
| **Independent scaling (today)** | Simple; already shipping | No cross-role awareness; chronic imbalance under skewed workloads |
| **Strict ratio** | Trivial, predictable | Wrong for variable workloads; fights per-target min/max and panic mode; double-spends on spikes |
| **Preference-based (proposed)** | Adapts to workload shape; reuses existing pipeline; opt-in; safe default | Blended objective is harder to reason about than a fixed rule; needs good defaults and visibility |

Also rejected: a new top-level CRD for "role groups", and joint-scaling logic inside per-role `Scaler`. Both duplicate capabilities the `Optimizer` already provides.

## Implementation Plan

**Phase 1 — Minimal, opt-in**

- Extend `HeterogeneousTarget` with the `coordination` block (role-keyed map) plus validation.
- Add the fixed Phase 1 pressure-score function after the scalar recommendation step.
- Replace the current redistribution with the blended allocator above; `alpha` defaults to 0 when `coordination` is unset or `Off` (byte-identical to today).
- Populate `status.coordination` and the `CoordinationBandSatisfied` condition.

**Phase 2 — Hardening and evolution**

- Benchmark on representative workloads (summarization-heavy, decode-heavy, mixed); tune `alpha`, `beta`, and normalization constants.
- Prometheus metrics mirroring `status.coordination`.
- Revisit panic-mode interaction (full suspend vs reduced bias).
- Optional runtime-specific signal plugins (e.g. vLLM KV-cache metrics) — makes the pressure function pluggable.
- Validate the role-keyed API on bindings with more than two roles.

## Open Questions

- Should `alpha` be exposed per-binding (as an advanced field) or stay controller-global?
- Do we want an escape-hatch `mode: Strict` later, or keep strict permanently out of the API?
