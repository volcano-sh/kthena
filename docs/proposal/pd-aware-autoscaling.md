# PD-Aware Autoscaling for ModelServing

> **Superseded.** This draft is retained for review history only. The current
> proposal lives at [`pd-disaggregated-autoscaling.md`](./pd-disaggregated-autoscaling.md)
> and follows the API direction in maintainer draft
> [hzxuzhonghu/kthena@6edaeaf](https://github.com/hzxuzhonghu/kthena/commit/6edaeafe1ba6969f24e2b783d1cfad7a05757253):
> merging `AutoscalingPolicyBinding` into `AutoscalingPolicy` and introducing a
> first-class `DisaggregatedTarget` with a `ratioRange` constraint, in place of
> the driver/follower coordination band described below.

**Status:** Superseded · **Area:** `pkg/autoscaler`

This proposal focuses on the **API primitive** for prefill/decode (PD) disaggregated autoscaling. Implementation — pressure scoring, allocator math, defaults, controller wiring — is intentionally out of scope and will follow once the API direction is agreed.

## Motivation

Kthena's ModelServing supports disaggregated prefill/decode deployments, but the autoscaler treats each role as an isolated workload: each role has its own `AutoscalingPolicyBinding` `SubTarget`, its own metric history, and its own scaling decision. No signal crosses between roles.

PD serving is not symmetric. **Prefill produces KV state; decode consumes it.** Decode is typically the binding constraint, and prefill capacity has to *track* decode capacity to keep the pipeline fed. With independent per-role autoscaling, two failure modes appear:

- **Imbalance stalls throughput.** One role saturates while the other is idle; neither autoscaler sees the other as the end-to-end bottleneck.
- **Lockstep over-spend.** Correlated traffic signals make both roles scale together even when only one is constrained — replicas added to a role that doesn't need them.

We need an API primitive that lets users express the **replica relationship between roles**, not just per-role bounds.

## Why the current API is insufficient

The existing `HeterogeneousTarget` exposes:

- per-target `minReplicas` / `maxReplicas` — local bounds, no cross-role coupling.
- `Cost` — hardware heterogeneity (H100 vs A100), not role semantics.

Neither expresses "prefill replicas should track decode replicas." A naive fix — symmetric per-role percent-of-total bands — flattens PD's asymmetry into two equally weighted knobs and hides which role drives scaling. Recent work (HeteroScale, *Taming the Chaos*) handles PD with a **fixed P:D ratio and decode-driven joint scaling**; we want the same shape but expressed as a soft band so workload drift (chat vs. summarization vs. mixed) does not require reconfiguring the policy.

The API does not prescribe the driver role's scaling metric — it composes with whatever `AutoscalingPolicy` is bound to the driver role. HeteroScale-style decode-goodput scaling is one valid choice; QPS, queue depth, or any other metric work equally well. The primitive is the *relationship*, not the signal.

## Proposed API

One optional block, `coordination`, is added to `HeterogeneousTarget`. It introduces two primitives:

1. **`driverRole`** — the role whose load signals drive joint scale (decode, for PD).
2. **`replicaRatios`** — soft bands expressing each follower role's replica count relative to the driver, in integer percent.

```yaml
heterogeneousTarget:
  params: [ ... ]                   # existing per-role SubTargets, unchanged
  coordination:
    mode: Preferred                 # Off | Preferred. Default: Off.
    driverRole: decode              # must match a subTargets.name in params
    replicaRatios:
      - role: prefill               # follower role
        minPercent: 20              # replicas[prefill] ≥ 20% of replicas[decode]
        maxPercent: 33              # replicas[prefill] ≤ 33% of replicas[decode]
```

### Why "replica ratio", not "capacity ratio"

The band constrains `replicas[follower] / replicas[driver]`, not effective compute capacity. Under heterogeneous hardware (H100 vs A100), one replica is not equal to one unit of capacity. Naming the field `replicaRatios` keeps the contract honest: this is a topology-shape primitive. A capacity-aware variant (weighted by `Cost`) is a natural Phase 2 extension.

### Field reference

| Field | Type | Required | Description |
|---|---|---|---|
| `coordination` | object | no | Omitting it ≡ `mode: Off`. |
| `coordination.mode` | enum | no | `Off` (default) or `Preferred`. `Off` is byte-identical to today. |
| `coordination.driverRole` | string | yes if `Preferred` | Role whose signals set joint scale. Must match a `subTargets.name`. |
| `coordination.replicaRatios[]` | list | yes if `Preferred` | One entry per non-driver role. |
| `replicaRatios[].role` | string | yes | Follower role name. Must match a `subTargets.name` and differ from `driverRole`. |
| `replicaRatios[].minPercent` | int | yes | Lower bound on `replicas[role] / replicas[driver]`, in percent. |
| `replicaRatios[].maxPercent` | int | yes | Upper bound on the same ratio, in percent. |

Percent semantics: `50` ≡ ratio `0.50` (1 follower per 2 driver). Values may exceed 100 for follower-heavy topologies (e.g. `200` ≡ 2 followers per driver).

### Validation

- `0 < minPercent ≤ maxPercent`. `minPercent == maxPercent` is allowed (degenerate fixed ratio).
- `driverRole` and every `role` must reference an existing `subTargets.name` in the same binding.
- The `SubTarget` named by `driverRole` must have an `AutoscalingPolicyBinding`. Without one, joint scaling has no signal source — this is rejected at admission.
- **Every non-driver role in `params` must appear in `replicaRatios` exactly once.** Silently leaving a role unconstrained is almost always a misconfiguration; v1 requires explicit coverage. (Phase 2 may relax this if real use cases emerge.)

## Semantics

When `mode: Preferred`:

1. The **driver role** is sized from its own load signals via its bound `AutoscalingPolicy`, as today.
2. Each **follower role** is sized so that `replicas[follower] / replicas[driver]` lies within `[minPercent, maxPercent]`.
3. Per-target `minReplicas` / `maxReplicas` and panic mode **always win** over the band. When they force a drift, status reports it.
4. `mode: Off` (default) is **byte-identical to today** — no behavioral change for existing bindings.

### Hysteresis (scale-up vs scale-down)

The band is a **no-action zone** for the follower. Concretely:

- If the realized ratio falls **below** `minPercent`, the follower scales **up** until the ratio reaches `minPercent`.
- If the realized ratio rises **above** `maxPercent`, the follower scales **down** until the ratio reaches `maxPercent`.
- While the realized ratio is **inside** `[minPercent, maxPercent]`, the follower's replica count is unchanged.

This makes scale-up and scale-down symmetric and gives the band a clear control-loop meaning: the width of the band is the tolerance before the follower reacts to driver changes. A wider band means less follower churn when the driver oscillates; a narrower band means tighter tracking. `minPercent == maxPercent` collapses to a fixed ratio with no hysteresis.

### Driver pinned at `maxReplicas` (the common Clamped case)

If the driver hits its per-target `maxReplicas` while end-to-end pressure is still rising, the follower cannot grow further within the band — there is no larger driver count to scale against. This is the most common reason `bandSatisfied` reports `false` in practice. Status surfaces it via `reason: Clamped`. The fix is operator-side: raise the driver's `maxReplicas` or widen the band.

Three things this proposal **does not** specify, by design:

- *How* the optimizer reaches the band (blending, pressure scoring, normalization).
- *Which* signals the driver uses beyond what the autoscaler already collects.
- *What constants* (smoothing, hysteresis windows beyond the band itself) the controller applies.

These are implementation concerns for a follow-up. The API contract is the band, the driver-role designation, and the hysteresis rule above.

## Configuration examples

**Decode-heavy workload (chat / long-form generation).** ~3–5 decode replicas per prefill:

```yaml
coordination:
  mode: Preferred
  driverRole: decode
  replicaRatios:
    - role: prefill
      minPercent: 20
      maxPercent: 33
```

**Prefill-heavy workload (summarization / RAG).** Comparable or more prefill than decode:

```yaml
coordination:
  mode: Preferred
  driverRole: decode
  replicaRatios:
    - role: prefill
      minPercent: 100
      maxPercent: 200
```

**Fixed ratio (HeteroScale parity).** `min == max` collapses the band to a single point — exactly HeteroScale's fixed P:D ratio model:

```yaml
coordination:
  mode: Preferred
  driverRole: decode
  replicaRatios:
    - role: prefill
      minPercent: 50
      maxPercent: 50              # 1 prefill per 2 decode, fixed
```

**Coordination disabled (default).** Omit the `coordination` block; behavior is unchanged from today.

## Observability

When `mode: Preferred`, the binding status reports the *outcome* of coordination:

```yaml
status:
  coordination:
    driverRole: decode
    realizedRatios:
      - role: prefill
        percent: 27               # actual replicas[prefill] / replicas[decode] * 100
        bandSatisfied: true       # within [minPercent, maxPercent] this cycle
        reason: ""                # one of: "", Clamped, PanicMode, Infeasible
    lastTransitionTime: "..."
  conditions:
    - type: CoordinationBandSatisfied
      status: "True"              # logical AND of bandSatisfied across all replicaRatios
```

`bandSatisfied` is reported **per follower role** so partial satisfaction is visible. The aggregated `CoordinationBandSatisfied` condition is the logical AND across all ratios — `True` only when every follower is inside its band.

`realizedRatios[].percent` is the actual ratio after all clamping, not the target. It deviates from the band only when (a) per-target `minReplicas` / `maxReplicas` force it (commonly the driver-pinned case above), (b) panic mode is active, or (c) integer rounding nudges the ratio at low replica counts.

## Alternatives considered

| Approach | Models PD asymmetry | Workload-drift tolerant | Notes |
|---|---|---|---|
| Independent per-role scaling (today) | No | n/a | Chronic imbalance under skewed workloads. |
| Fixed P:D ratio (HeteroScale) | Yes | No | Brittle to workload shape changes. Captured here as `minPercent == maxPercent`. |
| Symmetric per-role percent-of-total bands | No | Yes | Flattens producer/consumer asymmetry; two knobs that encode one degree of freedom. Rejected. |
| **Driver role + replica-ratio band (proposed)** | **Yes** | **Yes** | Generalizes HeteroScale; degenerate case = fixed ratio; extends to ≥3 roles. |

Also rejected: a new top-level CRD for "role groups", and joint-scaling logic inside per-role `Scaler`. Both duplicate capabilities the existing `Optimizer` already provides.

## Open questions (API-level)

- **Driver role: explicit or implicit?** v1 makes `driverRole` explicit. Should we instead infer it (decode is always the driver in PD) and revisit when non-PD topologies appear?
- **Capacity-aware variant.** v1's ratio is over replicas. Should a Phase 2 mode reinterpret the band against `Cost`-weighted effective capacity, or stay replica-based for predictability?
- **`mode: Strict` as a first-class value?** Or keep "strict" expressed only as `minPercent == maxPercent` under `Preferred`?
- **Chained followers.** v1 requires every ratio to be against `driverRole`. Are there real topologies that need follower-of-follower chains, or is single-driver fan-out sufficient indefinitely?
- **Per-binding tunables.** Should any controller constants (e.g. how aggressively to chase the band edges) ever be exposed in the API, or stay controller-global?
