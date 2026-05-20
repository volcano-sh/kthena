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
2. **Add first-class `DisaggregatedTarget`** — replace the generic `SubTarget` mechanism with a purpose-built structure for coordinated Prefill/Decode scaling, including independent per-role metrics, replica bounds, and a P/D ratio range constraint.

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
- Support per-role metrics and metric endpoints (prefill and decode may scale on different signals and expose metrics on different ports/paths).
- Remove the `AutoscalingPolicyBinding` CRD and the generic `SubTarget` type.

#### Non-Goals

- Controller implementation and reconciliation loop design (covered separately).
- Multi-ModelServing (heterogeneous hardware) P/D scaling — that remains in `HeterogeneousTarget`.

### Proposal

#### User Stories

##### Story 1: Single-resource autoscaling

As an ML platform operator, I want to define the complete autoscaling configuration — metrics, behavior, and target — in a single `AutoscalingPolicy` resource instead of maintaining a policy and a separate binding that cross-reference each other.

##### Story 2: Independent P/D scaling with ratio guardrails

As an ML platform operator, I deploy a vLLM disaggregated model with prefill and decode roles. I want the autoscaler to scale prefill replicas between 1–8 and decode replicas between 2–16, while always maintaining a P:D ratio between 1:1 and 1:4. This means if I have 2 prefill replicas, the decode replicas must be between 2 and 8.

##### Story 3: Per-role metrics and endpoints

As a platform engineer, my prefill pods should scale based on `num_requests_waiting` (targeting 5) scraped on port 8100, while decode pods should scale based on `gpu_kv_cache_usage_percent` (targeting 80%) on port 9100. I need to configure both the scaling metrics and scraping endpoints for each role independently, all in one place.

##### Story 4: Migration from Policy + Binding

As an existing user with an `AutoscalingPolicy` and one or more `AutoscalingPolicyBinding` objects, I want to consolidate into a single `AutoscalingPolicy` resource.

#### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Breaking change: removes `AutoscalingPolicyBinding` CRD | Both CRDs are alpha-level. Provide a migration guide and conversion tooling. The merged API is strictly simpler. |
| Breaking change for users currently using `SubTarget` | `SubTarget` was alpha-level and only used for P/D roles — the replacement `DisaggregatedTarget` is strictly more capable. |
| Loss of policy reuse across bindings | In practice policies are rarely shared. If reuse is needed, users can use templating tools (Helm, Kustomize). The UX win of a single resource outweighs the theoretical reuse loss. |
| Ratio enforcement may conflict with per-role min/max bounds | Webhook validates that `ratioRange` is achievable given the min/max replica bounds at admission time. |
| Increased controller complexity | Ratio enforcement is a bounded constraint-satisfaction problem; design details are deferred to the controller proposal. |

### Design Details

#### API Changes Overview

| Change | Description |
|--------|-------------|
| Delete `AutoscalingPolicyBinding` CRD | All target/binding fields move into `AutoscalingPolicy`. |
| Delete `SubTarget` type | Replaced by `DisaggregatedTarget`. |
| Expand `AutoscalingPolicySpec` | Add target fields (`homogeneousTarget`, `heterogeneousTarget`, `disaggregatedTarget`) directly. Metrics become the default; per-role metrics can override them. |
| Add `DisaggregatedTarget` | New first-class P/D scaling type with `Prefill`, `Decode`, and `RatioRange`. |
| Simplify `Target` | Remove `SubTarget` field. |

##### 1. Merged `AutoscalingPolicy`

```go
// AutoscalingPolicySpec defines the desired state of AutoscalingPolicy.
// +kubebuilder:validation:XValidation:rule="[has(self.heterogeneousTarget), has(self.homogeneousTarget), has(self.disaggregatedTarget)].filter(x, x).size() == 1",message="Exactly one of heterogeneousTarget, homogeneousTarget, or disaggregatedTarget must be set."
type AutoscalingPolicySpec struct {
	// Metrics defines the default list of metrics used to evaluate scaling decisions.
	// For HomogeneousTarget and HeterogeneousTarget these are the metrics used directly.
	// For DisaggregatedTarget these serve as the fallback when a role does not specify its own metrics.
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

Delete the `SubTarget` struct. `Target` is simplified to:

```go
// Target defines a ModelServing deployment that can be monitored and scaled.
type Target struct {
	// TargetRef references the target object to be monitored and scaled.
	TargetRef corev1.ObjectReference `json:"targetRef"`
	// MetricEndpoint defines the configuration for scraping metrics from the target pods.
	// +optional
	MetricEndpoint MetricEndpoint `json:"metricEndpoint,omitempty"`
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

	// Metrics overrides the policy-level metrics for this specific role.
	// This allows prefill and decode roles to scale on different signals
	// (e.g., prefill on queue depth, decode on KV cache utilization).
	// If not set, the top-level spec.metrics are used.
	// +optional
	// +kubebuilder:validation:MinItems=1
	Metrics []AutoscalingPolicyMetric `json:"metrics,omitempty"`

	// MetricEndpoint defines the configuration for scraping metrics from
	// pods of this role. If not specified, the controller uses defaults.
	// +optional
	MetricEndpoint *MetricEndpoint `json:"metricEndpoint,omitempty"`
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
  # Default metrics — used as fallback when a role doesn't specify its own
  metrics:
    - metricName: pending_requests
      targetValue: "5"
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
      metrics:                           # override default metrics for prefill
        - metricName: num_requests_waiting
          targetValue: "5"
      metricEndpoint:
        uri: /metrics
        port: 8100
    decode:
      roleName: decode
      minReplicas: 2
      maxReplicas: 16
      metrics:                           # override default metrics for decode
        - metricName: gpu_kv_cache_usage_percent
          targetValue: "80"
      metricEndpoint:
        uri: /metrics
        port: 9100
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
    - metricName: pending_requests
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
    - metricName: pending_requests
      targetValue: "5"
  behavior: { ... }
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: my-model
    minReplicas: 1
    maxReplicas: 10
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

#### Scaling Semantics (Controller Contract)

> **Note**: Controller implementation is out of scope for this proposal. These semantics define the contract the controller must honor.

1. **Independent metric evaluation**: The controller evaluates metrics independently for prefill and decode, producing a desired replica count for each role. If a role defines its own `metrics`, those are used; otherwise the controller falls back to the top-level `spec.metrics`.
2. **Per-role clamping**: Each desired count is clamped to `[minReplicas, maxReplicas]` of the corresponding role.
3. **Ratio enforcement**: If `ratioRange` is configured, after clamping, the controller adjusts replica counts to satisfy `minRatio <= P/D <= maxRatio`. The adjustment strategy (e.g., scale up the lagging side vs. scale down the leading side) is a controller implementation detail.
4. **Atomic patch**: The controller patches both `spec.template.roles[prefillIndex].replicas` and `spec.template.roles[decodeIndex].replicas` in a single ModelServing update to avoid intermediate states that violate the ratio.

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
| Binding A: `homogeneousTarget.target.subTargets: {kind: Role, name: prefill}` | `spec.disaggregatedTarget.prefill.roleName: prefill` |
| Binding B: `homogeneousTarget.target.subTargets: {kind: Role, name: decode}` | `spec.disaggregatedTarget.decode.roleName: decode` |
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

- **Status surface.** Should `AutoscalingPolicyStatus` expose `currentRatio`
  and a `RatioWithinRange` condition for `DisaggregatedTarget`, or is this a
  follow-up concern?
- **Per-role `tolerancePercent`.** Currently tolerance is policy-wide. Some
  workloads may want different deadbands for prefill vs decode.
- **Ratio enforcement strategy selection.** Step 3 of the controller contract
  is intentionally unspecified. Do we want the API to ever expose a hint
  (e.g., "prefer scaling up" vs "prefer scaling down" when the ratio drifts),
  or keep that strictly controller-side?
- **Defaulting `ratioRange`.** Should omitting `ratioRange` be the literal
  "no coordination" semantics, or should we ship an opinionated default
  (e.g., `0.1`–`1.0`) once we have production data?
