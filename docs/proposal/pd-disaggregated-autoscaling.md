---
title: P/D Disaggregated Autoscaling API
authors:
- TBD
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-05-20

---

## P/D Disaggregated Autoscaling API

### Summary

This proposal redesigns the autoscaling API with two goals:

1. **Merge `AutoscalingPolicyBinding` into `AutoscalingPolicy`** — today users must create two resources (policy + binding) and cross-reference them. Merging eliminates the indirection, avoids duplicating metric definitions across objects, and gives users a single resource that fully describes "what to scale, on what signal, and how."
2. **Add first-class `DisaggregatedTarget`** — replace the generic `SubTarget` mechanism with a purpose-built structure for coordinated Prefill/Decode scaling, including independent per-role metric sources, replica bounds, and a P/D ratio range constraint.

The `AutoscalingPolicyBinding` CRD and the `SubTarget` type are removed.

This proposal supersedes the earlier driver/follower coordination-band draft
([`pd-aware-autoscaling.md`](./pd-aware-autoscaling.md)) following maintainer
direction in commit
[hzxuzhonghu/kthena@6edaeaf](https://github.com/hzxuzhonghu/kthena/commit/6edaeafe1ba6969f24e2b783d1cfad7a05757253).

### Motivation

In disaggregated prefill/decode inference architectures, the prefill and decode stages have fundamentally different resource profiles:

- **Prefill** is compute-bound and bursty — it processes the full prompt in one forward pass.
- **Decode** is memory-bandwidth-bound and long-running — it generates tokens auto-regressively.

Scaling these two stages independently is essential for cost-efficient serving. However, independent scaling alone is insufficient — the P/D ratio must be coordinated. Too many prefill replicas starve decode capacity (growing queues); too many decode replicas waste GPU memory on idle KV caches. A healthy system keeps the ratio within an operator-defined range.

**Problems with the current two-resource model (AutoscalingPolicy + AutoscalingPolicyBinding):**

1. **Unnecessary indirection** — the user always creates a 1:1 pair (policy + binding). The binding adds a `policyRef` that points to a policy in the same namespace. This indirection provides no reuse benefit in practice (policies are rarely shared across multiple bindings) and doubles the number of objects to manage.
2. **Metric duplication** — with per-role metrics in the binding and policy-level metrics in the policy object, `AutoscalingPolicyMetric` appears in two CRDs. This is confusing and error-prone when users need to update metric targets.
3. **Fragmented view** — operators must read two resources to understand the complete autoscaling configuration for a single ModelServing.

**Problems with `SubTarget` for P/D disaggregation:**

1. **No coordination** — each binding scales its target independently; there is no concept of a ratio constraint between prefill and decode.
2. **Fragile coupling** — two bindings must manually agree on `targetRef`, and there is no validation that they reference the same ModelServing.
3. **Generic abstraction** — `SubTarget` is a generic kind/name pair. It provides no schema-level guidance, validation, or defaulting for P/D use cases.

#### Goals

- Merge `AutoscalingPolicyBinding` into `AutoscalingPolicy` to provide a single-resource UX.
- Provide a single `AutoscalingPolicy` resource that drives coordinated P/D scaling for one ModelServing.
- Allow independent `minReplicas` / `maxReplicas` per role to set per-stage capacity boundaries.
- Introduce a `ratioRange` constraint so the controller can enforce a healthy P/D ratio.
- Support per-role metric sources aligned with the [Prometheus metric source proposal](./autoscaling-prometheus-metric-source.md): each role declares its own `metricSources` map (Pod or Prometheus per key), letting prefill and decode scale on different signals fetched from different backends without duplicating threshold definitions.
- Remove the `AutoscalingPolicyBinding` CRD and the generic `SubTarget` type.

#### Non-Goals

- Full controller implementation (work allocation, queue management, metric scraping internals) is covered separately. This proposal defines the user-visible **contract** the controller must honor: phase ordering, conflict resolution, and status surface.
- Multi-ModelServing (heterogeneous hardware) P/D scaling — that remains in `HeterogeneousTarget`.

### Proposal

#### User Stories

##### Story 1: Single-resource autoscaling

As an ML platform operator, I want to define the complete autoscaling configuration — metrics, behavior, and target — in a single `AutoscalingPolicy` resource instead of maintaining a policy and a separate binding that cross-reference each other.

##### Story 2: Independent P/D scaling with ratio guardrails

As an ML platform operator, I deploy a vLLM disaggregated model with prefill and decode roles. I want the autoscaler to scale prefill replicas between 1–8 and decode replicas between 2–16, while always maintaining a P:D ratio between 1:1 and 1:4. This means if I have 2 prefill replicas, the decode replicas must be between 2 and 8.

##### Story 3: Per-role metric sources

As a platform engineer, my prefill pods should scale on `num_requests_waiting` (targeting 5), scraped directly from prefill pods, while decode pods should scale on `gpu_kv_cache_usage_percent` (targeting 80%), fetched from an external Prometheus server that aggregates DCGM data. The policy declares both algorithm keys and their thresholds; each role's `metricSources` declares how its values are fetched.

##### Story 4: Migration from Policy + Binding

As an existing user with an `AutoscalingPolicy` and one or more `AutoscalingPolicyBinding` objects, I want to consolidate into a single `AutoscalingPolicy` resource.

#### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Divergence from the Prometheus metric source proposal | This proposal adopts the `MetricSources` model from [`autoscaling-prometheus-metric-source.md`](./autoscaling-prometheus-metric-source.md) verbatim: `MetricEndpoint` is removed, and each role uses `metricSources map[string]MetricSource`. No conflicting source concepts are introduced. |
| Breaking change: removes `AutoscalingPolicyBinding` CRD | Both CRDs are alpha-level. Provide a migration guide and conversion tooling. The merged API is strictly simpler. |
| Breaking change for users currently using `SubTarget` | `SubTarget` was alpha-level and only used for P/D roles — the replacement `DisaggregatedTarget` is strictly more capable. |
| Loss of policy reuse across bindings | In practice policies are rarely shared. If reuse is needed, users can use templating tools (Helm, Kustomize). The UX win of a single resource outweighs the theoretical reuse loss. |
| Ratio enforcement may conflict with per-role min/max bounds (static infeasibility) | Webhook validates that `ratioRange` is achievable given the min/max replica bounds at admission time. |
| Per-role scaling decisions diverge (prefill wants up, decode wants down) and cannot be satisfied independently under `ratioRange` (dynamic divergence) | The controller resolves divergence deterministically with an **up-only** rule: ratio enforcement may only scale a role up relative to its standalone-desired count, never force a scale-down. See "Conflict Resolution and Coordination Semantics." |
| Up-only ratio enforcement may temporarily over-provision the lagging role | Eventual scale-down still occurs once both roles' standalone-desired counts drop; ratio enforcement only suppresses *premature* scale-down driven by divergent metrics, not eventual scale-down on sustained load reduction. |
| Coordination semantics are surprising to operators | The controller contract specifies a single deterministic rule (up-only ratio enforcement) with worked examples. Status conditions and events make every coordination decision auditable. |

### Design Details

#### API Changes Overview

| Change | Description |
|--------|-------------|
| Delete `AutoscalingPolicyBinding` CRD | All target/binding fields move into `AutoscalingPolicy`. |
| Delete `SubTarget` type | Replaced by `DisaggregatedTarget`. |
| Expand `AutoscalingPolicySpec` | Add target fields (`homogeneousTarget`, `heterogeneousTarget`, `disaggregatedTarget`) directly. `spec.metrics[]` remains the single algorithm-key list for the policy. |
| Add `DisaggregatedTarget` | New first-class P/D scaling type with `Prefill`, `Decode`, and `RatioRange`. |
| Simplify `Target` | Remove `SubTarget` field. Adopt `MetricSources` from the Prometheus metric source proposal in place of `MetricEndpoint`. |
| `RoleScalingParam` adopts `MetricSources` | Per-role metric sources use the same `map[string]MetricSource` shape as `Target.MetricSources`. No per-role metric or threshold fields. |

##### 1. Merged `AutoscalingPolicy`

```go
// AutoscalingPolicySpec defines the desired state of AutoscalingPolicy.
// +kubebuilder:validation:XValidation:rule="[has(self.heterogeneousTarget), has(self.homogeneousTarget), has(self.disaggregatedTarget)].filter(x, x).size() == 1",message="Exactly one of heterogeneousTarget, homogeneousTarget, or disaggregatedTarget must be set."
type AutoscalingPolicySpec struct {
	// Metrics is the single list of algorithm keys and thresholds for this policy.
	// For HomogeneousTarget and HeterogeneousTarget, the target's MetricSources
	// declares how each key is fetched. For DisaggregatedTarget, each role's
	// MetricSources picks the subset of keys that role scales on.
	// +kubebuilder:validation:MinItems=1
	Metrics []AutoscalingPolicyMetric `json:"metrics"`

	// TolerancePercent defines the percentage of deviation tolerated before scaling actions are triggered.
	// Scaling operations are performed only when |current - desired| >= current * tolerancePercent / 100.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	TolerancePercent int32 `json:"tolerancePercent"`

	// Behavior defines the scaling behavior configuration for both scale up and scale down operations.
	// +optional
	Behavior AutoscalingPolicyBehavior `json:"behavior"`

	// --- Target (exactly one must be set) ---

	// HomogeneousTarget enables traditional metric-based scaling for a
	// single ModelServing deployment (whole-deployment granularity).
	// +optional
	HomogeneousTarget *HomogeneousTarget `json:"homogeneousTarget,omitempty"`

	// HeterogeneousTarget enables optimization-based scaling across multiple
	// ModelServing deployments with different hardware capabilities.
	// +optional
	HeterogeneousTarget *HeterogeneousTarget `json:"heterogeneousTarget,omitempty"`

	// DisaggregatedTarget enables coordinated autoscaling of prefill and decode
	// roles within a single ModelServing that uses disaggregated serving.
	// +optional
	DisaggregatedTarget *DisaggregatedTarget `json:"disaggregatedTarget,omitempty"`
}
```

##### 2. Remove `SubTarget` and simplify `Target`

Delete the `SubTarget` struct. `Target` is reshaped per the [Prometheus metric source proposal](./autoscaling-prometheus-metric-source.md): `MetricEndpoint` is removed and replaced by a `MetricSources` map.

```go
// Target defines a ModelServing deployment that can be monitored and scaled.
type Target struct {
	// TargetRef references the target object to be monitored and scaled.
	TargetRef corev1.ObjectReference `json:"targetRef"`
	// MetricSources declares how to fetch each metric for this target.
	// Keys must match AutoscalingPolicy.spec.metrics[].name.
	// See the Prometheus metric source proposal for the MetricSource union
	// (Pod | Prometheus).
	// +optional
	MetricSources map[string]MetricSource `json:"metricSources,omitempty"`
}
```

`Target` remains in use by `HomogeneousTarget` (whole-ModelServing scaling) and `HeterogeneousTarget` (multi-ModelServing optimization). Both operate at the ModelServing level and never used `SubTarget` meaningfully.

##### 3. `DisaggregatedTarget` and supporting types

```go
// DisaggregatedTarget defines coordinated autoscaling for prefill/decode
// disaggregated serving within a single ModelServing deployment.
type DisaggregatedTarget struct {
	// TargetRef references the ModelServing deployment that contains
	// prefill and decode roles.
	TargetRef corev1.ObjectReference `json:"targetRef"`

	// Prefill defines scaling parameters for the prefill role.
	Prefill RoleScalingParam `json:"prefill"`

	// Decode defines scaling parameters for the decode role.
	Decode RoleScalingParam `json:"decode"`

	// RatioRange defines the acceptable range for the Prefill-to-Decode
	// replica ratio (P:D). The controller will respect this range when
	// making scaling decisions. Both values express ratios as
	// prefillReplicas / decodeReplicas.
	//
	// Example: minRatio=0.25, maxRatio=1.0 means for every decode replica,
	// there should be between 0.25 and 1.0 prefill replicas (i.e., P:D
	// ranges from 1:4 to 1:1).
	//
	// See "Conflict Resolution and Coordination Semantics" in the proposal
	// for the exact rule used when standalone-desired prefill/decode counts
	// violate this range.
	//
	// +optional
	RatioRange *PDRatioRange `json:"ratioRange,omitempty"`
}

// RoleScalingParam defines the scaling configuration for a single role
// (prefill or decode) within a disaggregated serving deployment.
type RoleScalingParam struct {
	// RoleName is the name of the role as defined in the ModelServing
	// spec.template.roles[].name. Defaults to "prefill" for the prefill
	// field and "decode" for the decode field if not specified.
	// +optional
	RoleName string `json:"roleName,omitempty"`

	// MinReplicas defines the minimum number of replicas for this role.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`

	// MaxReplicas defines the maximum number of replicas for this role.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`

	// MetricSources declares how to fetch each metric for this role.
	// Keys must match entries in AutoscalingPolicy.spec.metrics[].name.
	// Only metrics whose key appears here are evaluated for this role.
	//
	// Prefill and decode roles typically declare disjoint or partially
	// overlapping source maps — e.g., prefill scales on a queue-depth
	// signal scraped from prefill pods, decode scales on a KV-cache
	// signal fetched from Prometheus. A role with no entries does not
	// scale on metrics and stays at MinReplicas.
	//
	// See the Prometheus metric source proposal for the MetricSource
	// union (Pod | Prometheus).
	//
	// +optional
	MetricSources map[string]MetricSource `json:"metricSources,omitempty"`
}

// PDRatioRange defines the acceptable range for the prefill-to-decode ratio.
// +kubebuilder:validation:XValidation:rule="self.minRatio <= self.maxRatio",message="minRatio must be <= maxRatio"
type PDRatioRange struct {
	// MinRatio is the minimum allowed value of prefillReplicas / decodeReplicas.
	// +kubebuilder:validation:Minimum=0
	MinRatio resource.Quantity `json:"minRatio"`

	// MaxRatio is the maximum allowed value of prefillReplicas / decodeReplicas.
	MaxRatio resource.Quantity `json:"maxRatio"`
}
```

> **Why `resource.Quantity` for ratios?** Kubernetes does not support native `float` fields in CRDs. `resource.Quantity` is the idiomatic way to express decimal values in the Kubernetes API (e.g., `"0.25"`, `"1"`, `"2.5"`). It avoids floating-point imprecision and is already used throughout the Kubernetes and Kthena APIs for similar purposes.

##### 4. `HomogeneousTarget` (unchanged, except `SubTarget` removed from `Target`)

```go
type HomogeneousTarget struct {
	// Target defines the object to be monitored and scaled.
	Target Target `json:"target,omitempty"`
	// MinReplicas defines the minimum number of replicas to maintain.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas defines the maximum number of replicas allowed.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxReplicas int32 `json:"maxReplicas"`
}
```

##### 5. Delete `AutoscalingPolicyBinding` CRD

The entire `AutoscalingPolicyBinding`, `AutoscalingPolicyBindingSpec`, `AutoscalingPolicyBindingStatus`, and `AutoscalingPolicyBindingList` types are removed. The `policyRef` indirection is eliminated.

**Coordination with the Prometheus metric source proposal.** PR 931 places the `MetricSource`, `PodMetricSource`, `PrometheusMetricSource`, `PrometheusAuth`, and `PrometheusTLSConfig` types in `autoscalingpolicybinding_types.go`, and surfaces the `MetricsFetchReady` condition on `AutoscalingPolicyBindingStatus`. When this proposal removes `AutoscalingPolicyBinding`, those types move to `autoscalingpolicy_types.go` and the condition moves to `AutoscalingPolicyStatus`. This is a pure file relocation driven by the binding/policy merge; no semantic change to PR 931's design.

#### Full YAML Examples

##### Disaggregated P/D scaling (single resource)

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: llm-pd-scaling
  namespace: default
spec:
  tolerancePercent: 10
  # Policy declares every algorithm key and its threshold.
  # Each role's metricSources picks the subset of keys it scales on.
  metrics:
    - name: num_requests_waiting
      targetValue: "5"
    - name: gpu_kv_cache_usage_percent
      targetValue: "80"
  behavior:
    scaleUp:
      stablePolicy:
        instances: 2
        period: 30s
        stabilizationWindow: 60s
      panicPolicy:
        period: 10s
        panicThresholdPercent: 200
        panicModeHold: 120s
    scaleDown:
      instances: 1
      period: 60s
      stabilizationWindow: 300s
  disaggregatedTarget:
    targetRef:
      kind: ModelServing
      name: llm-vllm-disagg
      apiVersion: workload.serving.volcano.sh/v1alpha1
    prefill:
      roleName: prefill
      minReplicas: 1
      maxReplicas: 8
      metricSources:
        num_requests_waiting:
          type: Pod
          pod:
            name: vllm:num_requests_waiting
    decode:
      roleName: decode
      minReplicas: 2
      maxReplicas: 16
      metricSources:
        gpu_kv_cache_usage_percent:
          type: Prometheus
          prometheus:
            serverURL: "http://prometheus.monitoring.svc.cluster.local:9090"
            query: 'avg(DCGM_FI_DEV_FB_USED{serving="llm-vllm-disagg",role="decode"}) / avg(DCGM_FI_DEV_FB_TOTAL{serving="llm-vllm-disagg",role="decode"}) * 100'
    ratioRange:
      minRatio: "0.25"                   # P:D >= 1:4
      maxRatio: "1"                       # P:D <= 1:1
```

##### Homogeneous scaling (single resource, before vs. after)

Before (two resources):

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: my-policy
spec:
  tolerancePercent: 10
  metrics:
    - name: pending_requests
      targetValue: "5"
  behavior: { ... }
---
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: my-binding
spec:
  policyRef:
    name: my-policy
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: my-model
    minReplicas: 1
    maxReplicas: 10
```

After (single resource):

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: my-policy
spec:
  tolerancePercent: 10
  metrics:
    - name: pending_requests
      targetValue: "5"
  behavior: { ... }
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: my-model
      metricSources:
        pending_requests:
          type: Pod
          pod:
            name: pending_requests   # optional; defaults to the policy key
    minReplicas: 1
    maxReplicas: 10
```

##### Observed status (illustrative)

```yaml
status:
  disaggregatedScaling:
    prefillCurrentReplicas: 4
    prefillDesiredReplicas: 4
    decodeCurrentReplicas: 4
    decodeDesiredReplicas: 4
    currentRatio: "1"
    lastScaleTime: "2026-05-28T12:00:00Z"
    lastRatioAdjustmentTime: "2026-05-28T12:00:00Z"
  conditions:
    - type: ScalingActive
      status: "True"
    - type: RatioWithinRange
      status: "True"
```

#### Validation Rules (Webhook)

| Rule | Scope |
|------|-------|
| Exactly one of `homogeneousTarget`, `heterogeneousTarget`, `disaggregatedTarget` must be set. | `AutoscalingPolicySpec` (CEL) |
| `spec.metrics` must have at least one entry. | `AutoscalingPolicySpec` |
| `targetRef.kind` must be `ModelServing`. | `DisaggregatedTarget` |
| `prefill.roleName` and `decode.roleName` must reference existing roles in the referenced ModelServing. | `DisaggregatedTarget` |
| `prefill.roleName != decode.roleName` | `DisaggregatedTarget` |
| `minReplicas <= maxReplicas` for both prefill and decode. | `RoleScalingParam` |
| If `ratioRange` is set, `minRatio <= maxRatio`. | `PDRatioRange` (CEL) |
| If `ratioRange` is set, the ratio range must be achievable: `prefill.minReplicas / decode.maxReplicas >= minRatio` **and** `prefill.maxReplicas / decode.minReplicas <= maxRatio` (when `decode.minReplicas > 0`). | `DisaggregatedTarget` |
| For every key `k` in `prefill.metricSources` or `decode.metricSources`, an entry with `name: k` must exist in `spec.metrics`. | `DisaggregatedTarget` |
| At least one of `prefill.metricSources` or `decode.metricSources` must be non-empty (otherwise neither role scales on metrics). | `DisaggregatedTarget` |
| The union of `prefill.metricSources` keys and `decode.metricSources` keys must equal the set of `spec.metrics[].name`. Every declared policy key is sourced by at least one role; no orphan keys. (PD-shape of the Prometheus metric source proposal's "each policy metric key must have a corresponding source entry" rule.) | `DisaggregatedTarget` |

#### Status

`AutoscalingPolicyStatus` exposes the following fields when `disaggregatedTarget` is set:

```go
type DisaggregatedScalingStatus struct {
    PrefillCurrentReplicas  int32             `json:"prefillCurrentReplicas"`
    PrefillDesiredReplicas  int32             `json:"prefillDesiredReplicas"`
    DecodeCurrentReplicas   int32             `json:"decodeCurrentReplicas"`
    DecodeDesiredReplicas   int32             `json:"decodeDesiredReplicas"`
    CurrentRatio            resource.Quantity `json:"currentRatio,omitempty"`
    LastScaleTime           *metav1.Time      `json:"lastScaleTime,omitempty"`
    LastRatioAdjustmentTime *metav1.Time      `json:"lastRatioAdjustmentTime,omitempty"`
}
```

The split between `LastScaleTime` and `LastRatioAdjustmentTime` lets operators distinguish metric-driven scaling from ratio-driven adjustments.

The following standard conditions are reported on `AutoscalingPolicyStatus`:

| Condition | Meaning |
|---|---|
| `MetricsFetchReady` | Per the [Prometheus metric source proposal](./autoscaling-prometheus-metric-source.md). `False` when any role's metric fetch failed; `Reason`/`Message` identify the failing role and metric key (e.g. `Reason=PrometheusQueryFailed`, `Message="decode/gpu_kv_cache_usage_percent"`). |
| `ScalingActive` | `True` when both roles produced a valid recommendation this cycle. Implies `MetricsFetchReady=True`. |
| `AbleToScale` | `True` when the controller can patch the referenced ModelServing. |
| `RatioWithinRange` | `True` when `currentRatio ∈ ratioRange`. Present only when `ratioRange` is set. |
| `RatioInfeasible` | `True` when ratio enforcement has no solution under current `min/maxReplicas` (see "Conflict Resolution and Coordination Semantics"). |

#### Scaling Semantics (Controller Contract)

> **Note**: Controller implementation is out of scope for this proposal. These semantics define the contract the controller must honor.

The controller honors the following phase ordering on each reconcile of an `AutoscalingPolicy` with a `DisaggregatedTarget`. Phases 1–5 are applied independently per role; phase 6 is the only coordination phase.

1. **Metric collection (per role).** If a role's metrics are unavailable, the controller freezes both roles at their current replica counts and sets `MetricsFetchReady=False` (and therefore `ScalingActive=False`).
2. **Independent desired computation.** Each role's desired replicas are computed using the metrics named in `spec.metrics[]` for which the role declares a source in its `metricSources` map. Values are fetched per the source type (`Pod` or `Prometheus`) defined in the Prometheus metric source proposal. A role with no `metricSources` entries does not scale on metrics and stays at `minReplicas`.
3. **Tolerance, stabilization, and behavior (per role).** `tolerancePercent`, stabilization windows, and `behavior` rate-limits are applied independently per role, in this order.
4. **Per-role clamping.** Each desired count is clamped to `[minReplicas, maxReplicas]` of the corresponding role.
5. **Ratio coordination.** If `ratioRange` is configured, the controller adjusts the pair to satisfy `minRatio ≤ P/D ≤ maxRatio` using the rule in "Conflict Resolution and Coordination Semantics" below.
6. **Atomic patch.** Prefill and decode `replicas` are updated in a single ModelServing patch to avoid intermediate states that violate the ratio.

#### Conflict Resolution and Coordination Semantics

Because prefill and decode evaluate metrics independently, their per-role desired counts may diverge — e.g., prefill signals scale-up while decode signals scale-down. When the resulting pair `(P*, D*)` falls outside `ratioRange`, the controller resolves the conflict with a single deterministic rule.

**v1 rule — ratio enforcement is up-only.**

Let `(P*, D*)` be the per-role clamped desired counts produced by phases 1–4. The controller chooses `(P', D')` as the point that:

- lies in the feasible region (`P' ∈ [Pmin, Pmax]`, `D' ∈ [Dmin, Dmax]`, `minRatio ≤ P'/D' ≤ maxRatio`),
- satisfies `P' ≥ P*` **and** `D' ≥ D*` (ratio enforcement may only scale up),
- minimizes `(P' - P*) + (D' - D*)`.

Ratio enforcement therefore never forces a role to scale down. When the standalone-desired pair violates `ratioRange`, the controller grows the lagging role rather than shrinking the leading one. This preserves the property that the chosen replica count for each role is always greater than or equal to its standalone decision — consistent with the broader Kubernetes autoscaling principle of preferring temporary over-provisioning to under-provisioning.

**Infeasibility.** If the lagging role cannot grow without exceeding its `maxReplicas`, the controller does not patch. It holds at current replicas, sets `RatioInfeasible=True` on the status, and emits a `Warning` event. The controller does not breach `maxReplicas` to satisfy `ratioRange`; `maxReplicas` is treated as a hard capacity ceiling.

**Metric-failure policy.** If either role's metrics cannot be collected, the controller freezes both roles and surfaces `ScalingActive=False` with reason `<Role>MetricsUnavailable`. Skipping ratio enforcement on partial data is intentionally not supported in v1.

**Determinism guarantees.** Given a fixed `(spec, ModelServing state, metric samples, recommendation history)` the output `(P', D')` is uniquely defined. The chosen pair is monotone in metrics (higher load never produces a smaller chosen pair), and ratio enforcement never decreases either role's replica count relative to its standalone-desired.

**Worked examples.**

| Scenario | Current (P,D) | Standalone (P*, D*) | `ratioRange` | Result (P', D') |
|---|---|---|---|---|
| Both within range | (2, 8) | (4, 4) | [0.25, 1.0] | (4, 4) |
| Decode wanted to shrink; ratio absorbs it | (2, 8) | (4, 2) | [0.25, 1.0] | (4, 4) |
| Symmetric scale-up under-shoots ratio | (4, 4) | (6, 5) | [0.25, 1.0] | (6, 6) |
| Infeasible — decode at `maxReplicas` | (2, 3), Dmax=3 | (4, 2) | [0.25, 1.0] | held at (2, 3), `RatioInfeasible=True` |

A future revision may introduce a `ratioEnforcementPolicy` enum (e.g., `PreferUp` / `PreferDown` / `Bidirectional`) to opt into alternate strategies; v1 behavior corresponds to `PreferUp` and remains the default.

#### Migration

##### From `AutoscalingPolicy` + `AutoscalingPolicyBinding`

| Before | After |
|--------|-------|
| `AutoscalingPolicy` with metrics + behavior | Same fields stay in `AutoscalingPolicy.spec` |
| `AutoscalingPolicyBinding` with `policyRef` + target | Target fields move into `AutoscalingPolicy.spec`; `policyRef` is deleted |
| Two resources per scaling config | One resource |

##### From `SubTarget` P/D bindings

| Before (policy + two bindings with SubTarget) | After (single policy) |
|---|---|
| Policy: metrics + behavior | `spec.metrics` + `spec.behavior` (same policy) |
| Binding A: `homogeneousTarget.target.subTargets: {kind: Role, name: prefill}` + `metricEndpoint` | `spec.disaggregatedTarget.prefill.roleName: prefill` + `prefill.metricSources` (Pod or Prometheus, per the Prometheus metric source proposal) |
| Binding B: `homogeneousTarget.target.subTargets: {kind: Role, name: decode}` + `metricEndpoint` | `spec.disaggregatedTarget.decode.roleName: decode` + `decode.metricSources` |
| 3 resources, no ratio coordination | 1 resource, `ratioRange` provides coordination |

### Alternatives

#### Alternative 1: Keep `AutoscalingPolicyBinding` as a separate CRD

Keep the current two-resource model and only add `DisaggregatedTarget` to the binding.

**Rejected because**: The policy/binding split provides no practical benefit — policies are not shared across bindings. It forces metric definitions to live in two places (policy-level and per-role overrides in the binding), increases the number of objects to manage, and makes the complete autoscaling configuration harder to read. Merging into one resource is simpler for both users and the controller.

#### Alternative 2: Keep `SubTarget` and add ratio annotation

Add a `volcano.sh/pd-ratio-range` annotation to coordinate two separate bindings.

**Rejected because**: Annotations are untyped, unvalidated, and invisible to schema tooling. Coordination between two separate resources via annotations is fragile and hard to reason about.

#### Alternative 3: Generic `roles[]` list instead of explicit `prefill` / `decode` fields

```go
type DisaggregatedTarget struct {
    TargetRef  corev1.ObjectReference `json:"targetRef"`
    Roles      []RoleScalingParam     `json:"roles"`
    RatioRange *PDRatioRange          `json:"ratioRange,omitempty"`
}
```

**Rejected because**: P/D disaggregation is inherently a two-role pattern. A generic list makes ratio semantics ambiguous (which role is the numerator?), loses schema-level defaulting for role names, and opens the door to unsupported configurations (3+ roles with ratio constraints). If future architectures require more than two roles, a new target type can be introduced.

#### Alternative 4: Extend `HomogeneousTarget` with optional P/D fields

Add `prefill` and `decode` fields inside `HomogeneousTarget`.

**Rejected because**: `HomogeneousTarget` is inherently single-target. Embedding P/D semantics overloads its purpose and creates confusing validation rules (e.g., `minReplicas`/`maxReplicas` at top level vs. per-role). A separate target type is cleaner.

#### Alternative 5: Driver-role + asymmetric coordination band (original draft of this proposal)

The earlier revision of this work
([`pd-aware-autoscaling.md`](./pd-aware-autoscaling.md)) added a `coordination`
block to `HeterogeneousTarget` with a `driverRole` (decode) and follower
`replicaRatios[]` expressed as integer percent bands. Followers tracked the
driver with a hysteresis no-action zone.

**Rejected because**:

- It was layered onto the wrong type — `HeterogeneousTarget` is for
  multi-ModelServing optimization across heterogeneous hardware, not for
  intra-ModelServing role coordination.
- The driver/follower asymmetry hard-codes the assumption that one role's
  metric "leads." In practice both prefill and decode have meaningful
  independent load signals, and an API that forces one to be derived from the
  other loses information.
- It left the two-CRD `AutoscalingPolicy` + `AutoscalingPolicyBinding` model
  untouched, missing the larger UX simplification.
- The hysteresis control-loop semantics belong in the controller, not the
  API surface. `ratioRange` exposes the constraint without prescribing the
  loop.

### Open Questions

- **Status surface.** *Resolved: see "Status" subsection.*
- **Ratio enforcement strategy selection.** *Resolved: v1 uses the up-only rule
  defined in "Conflict Resolution and Coordination Semantics." A
  `ratioEnforcementPolicy` enum may be added in a future revision.*
- ~~Per-role `tolerancePercent`~~ — *resolved: per-role tuning is expressed via
  distinct policy-level metric keys with distinct thresholds, consistent with
  the "policy = what, target = how" model.*
- **Defaulting `ratioRange`.** Current behavior is "omitted = no coordination."
  A small opinionated default (e.g., `0.1`–`1.0`) is a candidate once
  production data exists, but is deferred — defaults are easier to add than to
  remove.
- **Empty `metricSources` on a role.** Current text says the role stays at
  `minReplicas` if no sources are declared. Should this be an admission error
  instead, or is a no-op scaling role a useful state (e.g., during incremental
  rollouts where decode is temporarily pinned)?
