# PD-Aware Autoscaling for ModelServing

**Status:** Draft · **Area:** `pkg/autoscaler`

## Motivation

Kthena's ModelServing supports disaggregated prefill/decode deployments, but the autoscaler treats each role as an isolated workload. In the homogeneous path, each role is driven by its own `AutoscalingPolicyBinding` via a `SubTarget`, and each binding instantiates an independent `Autoscaler` with its own metric collector and stabilization history. No signal crosses between roles.

The only existing multi-target decision point is the `Optimizer` (heterogeneous path), which was designed for hardware heterogeneity (H100 vs A100) rather than role coordination. It produces one scalar recommendation and redistributes replicas by `Cost` / `CostExpansionRatePercent`. Role semantics (prefill vs decode) are not part of that model.

## Problem

Prefill and decode have different cost drivers:

- **Prefill** scales with prompt length (input tokens, one-shot compute).
- **Decode** scales with generated length (output tokens, sustained KV-cache pressure).

The mix varies sharply by workload:

- **Summarization / RAG:** long input, short output → prefill-heavy.
- **Code generation / long-form chat:** short input, long output → decode-heavy.
- **Mixed tenants:** ratio drifts continuously.

Because each role scales on its own signals and its own history, two failure modes appear in practice:

1. **Imbalance stalls throughput.** One role saturates while the other is idle. Neither autoscaler sees the other as the end-to-end bottleneck.
2. **Inefficient replica spend.** Both roles scale up together on correlated load even when only one is constrained.

A strict ratio (e.g. `prefill:decode = 1:2`) was considered but rejected: workload variability makes any fixed ratio wrong most of the time, and it conflicts with per-target `MinReplicas` / `MaxReplicas` and panic mode.

## Proposal: Preference-based Coordination

Introduce a **soft coordination layer** inside the `Optimizer` that biases replica redistribution toward a preferred prefill/decode balance derived from live workload signals — as a *preference*, not a hard rule.

Core properties:

- **Preference, not enforcement.** Users declare a soft band (a range). The optimizer stays inside it when possible, drifts outside only when per-target min/max or panic mode require it.
- **Opt-in.** Disabled by default; existing bindings behave identically.
- **No new scaling path.** Coordination is a refinement of the existing optimizer's redistribution step, not a parallel system.

## Design Overview

### Signals

All signals come from the existing per-target `MetricCollector` pipeline — no new scrape path.

Candidate inputs:

- Pending / waiting request queue depth per role.
- Prefill token rate and decode token rate.
- TTFT for prefill pressure, TPOT / KV-cache utilization for decode pressure.

### Pressure, at a high level

Each role is assigned a relative **pressure score** from its metrics. The score expresses "how constrained is this role right now relative to the other." When pressure is balanced, the allocator falls back to today's cost-ordered behavior. When pressure is skewed, the allocator biases the next replicas toward the constrained role, within the user's soft band.

The exact formula is internal and tunable; the API does not expose it.

### Integration with the existing optimizer

The optimizer pipeline is unchanged up to the redistribution step:

1. Collect per-target metrics *(unchanged)*.
2. Compute joint scalar recommendation *(unchanged)*.
3. **New:** compute per-role pressure and preferred split.
4. Redistribute replicas using a blend of existing cost ordering and pressure preference, clamped to per-target min/max.
5. Write replicas per target *(unchanged)*.

**No breaking changes.** When the new coordination field is absent, the blend weight is zero and the output is identical to today's.

## UX / API Design

Design principles:

- **Opt-in, default off.** No change for existing users.
- **One concept per knob.** A mode switch plus a soft range. No exposed pressure weights or signal selection.
- **Ranges, not points.** Users express intent as a band; the controller picks inside it.
- **Observability over configuration.** Surface the chosen split and pressure scores in status/events so users can validate before tuning.

Minimal surface — one optional block on `HeterogeneousTarget`:

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
    coordination:                     # NEW, optional
      mode: Preferred                  # Off | Preferred
      preferredRatio:                  # soft band, not enforced
        prefill: { min: 20, max: 50 } # percent of total replicas
```

That is the entire user-facing surface for Phase 1.

## Alternatives Considered

| Approach | Pros | Cons |
|---|---|---|
| **Independent scaling (today)** | Simple; already shipping | No cross-role awareness; chronic imbalance under skewed workloads |
| **Strict ratio** | Trivial, predictable | Wrong for variable workloads; fights per-target min/max and panic mode; double-spends on spikes |
| **Preference-based (proposed)** | Adapts to workload shape; reuses existing pipeline; opt-in; safe default | Blended objective is harder to reason about than a fixed rule; needs good defaults and visibility |

Also rejected: a new top-level CRD for "role groups" and joint-scaling logic inside per-role `Scaler`. Both duplicate capabilities the `Optimizer` already provides.

## Implementation Plan

**Phase 1 — Minimal, opt-in**

- Extend `HeterogeneousTarget` with the optional `coordination` block plus validation.
- Add an internal pressure-score function wired after the scalar recommendation step.
- Replace the current redistribution with a blended allocator; blend weight is zero when `coordination` is unset (byte-identical to today).
- Surface the chosen per-role split and pressure scores in logs and binding status.

**Phase 2 — Hardening and evolution**

- Benchmark on representative workloads (summarization-heavy, decode-heavy, mixed); tune defaults.
- Prometheus metrics for pressure and band violations.
- Review interaction with panic mode and stabilization windows.
- Optional runtime-specific signal plugins (e.g. vLLM KV-cache metrics).
- Extend beyond two roles if multi-role bindings are needed.

## Open Questions

- Should the pressure score be a fixed formula or pluggable per runtime?
- Is `preferredRatio` best expressed as percent-of-total (generalizes to >2 roles) or as a `prefill:decode` range (reads more naturally)?
- How should coordination interact with panic mode — suspend the preference, or keep the bias?
- Do we want an escape-hatch `mode: Strict` later, or keep strict permanently out of the API?
