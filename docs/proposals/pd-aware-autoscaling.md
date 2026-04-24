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

Introduce a **soft coordination layer** inside the `Optimizer` that biases replica redistribution toward a preferred prefill/decode balance — as a *preference*, not a hard rule.

Core properties:

- **Preference, not enforcement.** Users declare a soft band. The optimizer stays inside it when possible, drifts outside only when per-target min/max or panic mode require it.
- **Opt-in.** Disabled by default; existing bindings behave identically.
- **No new scaling path.** Coordination refines the existing optimizer's redistribution step; it does not replace it.

## API

One optional field, `coordination`, is added to `HeterogeneousTarget`. It is the entire user-facing surface.

### Scope (Phase 1)

Phase 1 supports **exactly two roles**: `prefill` and `decode`. The role of a `SubTarget` is taken from `subTargets.name`, which must be one of `prefill` or `decode`. Generalization to N roles is Phase 2.

### Field reference

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `coordination` | object | no | unset (≡ `mode: Off`) | Optional block. Omitting it is equivalent to `mode: Off`. |
| `coordination.mode` | enum | no | `Off` | `Off` or `Preferred`. `Off` disables coordination; `Preferred` enables the soft band below. |
| `coordination.preferredRatio.prefill.min` | int | yes if `Preferred` | — | Lower bound on prefill's share of total replicas, in percent. |
| `coordination.preferredRatio.prefill.max` | int | yes if `Preferred` | — | Upper bound on prefill's share of total replicas, in percent. |

Decode's share is implicit: `100 - prefillPercent`. The band is *soft* — it is a preference the optimizer tries to hold; per-target `minReplicas` / `maxReplicas` and panic mode always win.

**Validation.** `0 ≤ min ≤ max ≤ 100`. `min == max` is allowed (pins the preferred split to a single point, still soft — per-target min/max and panic mode can still override).

### Example 1 — Decode-heavy workload (chat / long-form generation)

Prefill should stay a small minority of replicas:

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
        prefill: { min: 15, max: 35 }   # prefill gets 15–35% of replicas
```

### Example 2 — Prefill-heavy workload (summarization / RAG)

Long inputs, short outputs: prefill should be the majority:

```yaml
heterogeneousTarget:
  params: [ ... ]
  coordination:
    mode: Preferred
    preferredRatio:
      prefill: { min: 50, max: 80 }    # prefill gets 50–80% of replicas
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
2. Compute joint scalar recommendation. *(unchanged)*
3. **New:** compute a per-role *pressure score* (see below) and a preferred split that respects `preferredRatio`.
4. Redistribute replicas using existing cost ordering, biased toward the preferred split; clamp to per-target `minReplicas` / `maxReplicas`.
5. Write replicas per target. *(unchanged)*

### Phase 1 pressure signals

A concrete, fixed formula for v1:

- **Prefill pressure:** normalized `queueDepth` + normalized `TTFT`.
- **Decode pressure:** normalized `queueDepth` + normalized `kvCacheUtilization`.
- **Fallback:** if a secondary signal (TTFT or KV-cache) is unavailable for the runtime, pressure falls back to `queueDepth` alone.

The normalization constants and blend weight (pressure vs cost) live in the controller config, not the API. Making the pressure function pluggable is deferred to Phase 2.

### Interaction with existing features

- **No breaking changes.** Existing bindings without `coordination` behave identically.
- **Per-target `minReplicas` / `maxReplicas`** always win over the preferred band. When they force a drift, status reflects it (see below).
- **Panic mode** suspends coordination for the duration of the panic (Phase 1 decision; revisit in Phase 2).
- **Stabilization windows** are unaffected — coordination runs after the scalar recommendation, not instead of it.
- **Metric collectors** are reused; no new scrape path.

### Observability

When `mode: Preferred`, the binding status reports:

```yaml
status:
  coordination:
    realizedPrefillPercent: 27          # actual split this cycle
    pressure:
      prefill: 0.42                     # normalized 0.0–1.0
      decode: 0.71
    bandSatisfied: true                 # false if clamp forced drift outside preferredRatio
    lastTransitionTime: "..."
  conditions:
    - type: CoordinationBandSatisfied
      status: "True"                    # "False" with reason=Clamped / PanicMode
```

The same values are emitted as Prometheus metrics in Phase 2.

## Example Scenario

A tenant runs a mixed workload on `llama-3`: chat traffic (decode-heavy) during the day, a summarization batch job (prefill-heavy) at night. The binding uses `preferredRatio.prefill: { min: 20, max: 60 }`.

**Daytime — decode-heavy (long outputs):**

- Decode queue grows; prefill is idle.
- Today: decode scales up alone, but prefill is over-provisioned from the morning ramp and stays put → wasted replicas.
- With coordination: pressure score favors decode; optimizer shifts the next redistribution toward decode (prefill near 20%, decode near 80%), freeing replica budget where it is actually needed.

**Nighttime — prefill-heavy (long prompts, short outputs):**

- Prefill TTFT spikes; decode is underused.
- Today: prefill scales up, but decode replicas linger from daytime → both roles peak simultaneously.
- With coordination: pressure shifts to prefill; allocator pushes the split toward the upper bound (prefill near 60%), decode drains naturally.

Across the day, the ratio stays inside the band when the workload allows, and `status.coordination` reports the realized split so the user can validate before tuning.

## Alternatives Considered

| Approach | Pros | Cons |
|---|---|---|
| **Independent scaling (today)** | Simple; already shipping | No cross-role awareness; chronic imbalance under skewed workloads |
| **Strict ratio** | Trivial, predictable | Wrong for variable workloads; fights per-target min/max and panic mode; double-spends on spikes |
| **Preference-based (proposed)** | Adapts to workload shape; reuses existing pipeline; opt-in; safe default | Blended objective is harder to reason about than a fixed rule; needs good defaults and visibility |

Also rejected: a new top-level CRD for "role groups", and joint-scaling logic inside per-role `Scaler`. Both duplicate capabilities the `Optimizer` already provides.

## Implementation Plan

**Phase 1 — Minimal, opt-in**

- Extend `HeterogeneousTarget` with the `coordination` block plus validation (`0 ≤ min ≤ max ≤ 100`, role name ∈ {`prefill`, `decode`}).
- Add the fixed Phase 1 pressure-score function after the scalar recommendation step.
- Replace the current redistribution with a blended allocator; blend weight is zero when `coordination` is unset or `Off` (byte-identical to today).
- Populate `status.coordination` and the `CoordinationBandSatisfied` condition.

**Phase 2 — Hardening and evolution**

- Benchmark on representative workloads (summarization-heavy, decode-heavy, mixed); tune defaults.
- Prometheus metrics mirroring `status.coordination`.
- Revisit panic-mode interaction (full suspend vs reduced bias).
- Optional runtime-specific signal plugins (e.g. vLLM KV-cache metrics) — makes the pressure function pluggable.
- Extend beyond two roles if multi-role bindings are needed.

## Open Questions

- Is `preferredRatio` best expressed as percent-of-total (generalizes to >2 roles) or as a `prefill:decode` range (reads more naturally)?
- Do we want an escape-hatch `mode: Strict` later, or keep strict permanently out of the API?
