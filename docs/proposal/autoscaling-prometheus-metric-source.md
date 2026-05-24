---
title: Autoscaling Prometheus Metric Source Support
authors:
- @hzxuzhonghu
- @LiZhenCheng9527
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-05-08

---

## Autoscaling Prometheus Metric Source Support

### Summary

The Kthena Autoscaler currently collects metrics by directly scraping each pod's HTTP endpoint (Prometheus text format). While this covers the typical LLM serving case, it cannot consume metrics that are only available in an external Prometheus server — such as
cluster-level aggregations, cross-service SLOs, or GPU utilisation data exported by DCGM/node-exporter.

This proposal extends the binding `Target` model so that each target declares metric sources only through `metricSources`, including **Prometheus** and **Pod** source variants per metric key. Prometheus metrics are evaluated against an external Prometheus HTTP API; pod metrics are scraped directly from pods but are also configured via `metricSources` (not via `MetricEndpoint`). `AutoscalingPolicy` remains a pure scaling contract (metric name + threshold + behaviour) with no knowledge of where metrics come from.

### Motivation

The current `AutoscalingPolicyMetric` struct carries only `metricName` and `targetValue`. `metricName` serves two roles simultaneously:

1. The **metricName** — the string the scaling algorithm uses to look up the corresponding `targetValue`.
2. The **pod scrape filter** — the exact Prometheus metric name matched against the text scraped from each pod endpoint.

This conflation works for the pod-scraping case, but breaks down when the metric is not exposed directly by the pods. Common real-world examples:

- **Cluster-level queue depth** aggregated by a Prometheus recording rule (e.g. `sum(kthena:num_requests_waiting{serving="my-serving"})`) — no single pod exposes this value.
- **GPU utilisation** from DCGM node-exporter — exported by a DaemonSet, not by the inference pods.
- **Custom SLO metrics** owned by an observability platform team and available only via `Thanos/Cortex` query endpoints.

Additionally, for **heterogeneous target** scaling, each deployment type (e.g. `gpu-serving` vs `cpu-serving`) needs its own scoped metric value. A single policy-level query aggregating all targets together loses the per-target granularity the optimizer requires.

#### Goals

- Support per-target PromQL queries in the binding's `Target` struct so that both `HomogeneousTarget` and `HeterogeneousTarget` can retrieve metrics from an external Prometheus server.
- Keep `AutoscalingPolicy` as a pure scaling contract; it defines metric names and thresholds but has no knowledge of where metrics come from.
- Keep pod-scraping support as a first-class source type (`Pod`) in `metricSources`.
- Support bearer-token and TLS authentication when connecting to Prometheus.
- Introduce a clean separation between algorithm key (`name` on the policy) and metric selector (`metricSources` on the binding target).

#### Non-Goals

- Support for other external metric backends (Datadog, CloudWatch, etc.) — these may be added in future proposals following the same pattern.
- Replacing the pod-scraping path or changing the aggregation semantics for existing pod-sourced metrics.
- Push-based or webhook-triggered metric delivery.
- Automatic discovery of Prometheus server addresses from `ServiceMonitor` CR.

### Proposal

Introduce a `MetricSource` discriminated union on **`Target.metricSources`** only — a `map[string]MetricSource` (keys match `AutoscalingPolicy.spec.metrics[].name`) that lets each binding target declare how to fetch each metric. This design enforces a clean separation of concerns:

- **`AutoscalingPolicy`** defines *what* to scale on (metric name, threshold, behaviour) — it is reusable across different targets and has no metric-source knowledge.
- **`AutoscalingPolicyBinding` / `Target`** defines *how* to fetch each metric for that specific deployment via `metricSources` (`Pod` or `Prometheus`).

The scaling algorithm (`RecommendedInstancesAlgorithm`) already has a split between `ReadyInstancesMetrics` (pod-source, per-instance) and `ExternalMetrics` (single aggregate value). Prometheus-sourced metrics are populated into `ExternalMetrics` since they return a single scalar value, not a per-pod value. The existing `getDesiredInstancesForSingleExternalMetric` function then handles them without changes.

#### User Stories

##### Story 1 — Scale on a Prometheus recording rule

A user has a Prometheus recording rule `kthena:num_requests_waiting:rate5m{serving="my-serving"}` that smooths queue depth over a 5-minute window. The pods do not expose this derived metric themselves. The user wants the autoscaler to react to this rule rather than to raw per-pod counters.

The policy remains unchanged — it only declares the metric name and threshold:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: scaling-policy
spec:
  tolerancePercent: 10
  metrics:
  - name: num_requests_waiting
    targetValue: "10"
  behavior:
    scaleUp:
      stablePolicy:
        stabilizationWindow: 1m
        period: 30s
    scaleDown:
      stabilizationWindow: 5m
      period: 1m
```

The Prometheus source is configured in the binding's target:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: scaling-binding
spec:
  policyRef:
    name: scaling-policy
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: my-serving
      metricSources:
        num_requests_waiting:
          type: Prometheus
          prometheus:
            serverURL: "http://prometheus.monitoring.svc.cluster.local:9090"
            query: 'kthena:num_requests_waiting:rate5m{serving="my-serving"}'
    minReplicas: 1
    maxReplicas: 10
```

##### Story 2 — Heterogeneous targets with per-deployment GPU utilisation

A user runs a `HeterogeneousTarget` binding covering an H100 deployment and an A100 deployment. DCGM exports `DCGM_FI_DEV_GPU_UTIL` per node; Prometheus aggregates these per serving instance. The user wants the optimizer to see each deployment's GPU utilisation separately.

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: optimizer-binding
spec:
  policyRef:
    name: gpu-util-policy
  heterogeneousTarget:
    costExpansionRatePercent: 20
    params:
    - target:
        targetRef:
          kind: ModelServing
          name: h100-serving
        metricSources:
          gpu_utilization:
            type: Prometheus
            prometheus:
              serverURL: "http://prometheus.monitoring.svc.cluster.local:9090"
              query: 'avg(DCGM_FI_DEV_GPU_UTIL{serving="h100-serving"})'
      minReplicas: 1
      maxReplicas: 5
      cost: 100
    - target:
        targetRef:
          kind: ModelServing
          name: a100-serving
        metricSources:
          gpu_utilization:
            type: Prometheus
            prometheus:
              serverURL: "http://prometheus.monitoring.svc.cluster.local:9090"
              query: 'avg(DCGM_FI_DEV_GPU_UTIL{serving="a100-serving"})'
      minReplicas: 2
      maxReplicas: 8
      cost: 60
```

#### Notes/Constraints/Caveats

- Prometheus queries must return a **scalar or single-element instant vector**. The controller rejects (logs an error and skips the metric) results with zero or more than one element.
- `Target.metricSources` is the only metric-source configuration surface. Each policy metric key must have a corresponding source entry.
- `MetricName` in the existing API is renamed to `Name` to reflect its role as a pure algorithm key with no scrape-filter semantics. A kubebuilder validation ensures it cannot be empty.
- For `PodMetricSource`, `name` defaults to the policy metric key when omitted.

#### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Prometheus server unavailable at scrape time | Treat as a missing metric: the controller logs a warning and leaves `ExternalMetrics[name]` unset, causing `skip=true` for that metric (no scaling action). |
| PromQL query returns unexpected type (string, matrix) | Validate result type in the collector; log error and skip the metric rather than crashing. |
| Bearer token exposed in CRD spec | Token is stored in a `SecretKeySelector` reference; the controller reads the Secret at runtime. Never inline credentials. |
| Per-target query misconfiguration in heterogeneous mode silently blocks optimal allocation | Surface fetch errors in `AutoscalingPolicyBindingStatus.conditions` using standard Kubernetes conditions so users can diagnose with `kubectl` and common tooling. |
| TLS `insecureSkipVerify` misuse in production | The field is available but kubebuilder marker documents it as dev-only; admission webhook can warn when set on non-dev namespaces in future work. |

### Design Details

#### API Changes

##### `autoscalingpolicy_types.go`

`AutoscalingPolicyMetric` is simplified: `MetricName` is renamed to `Name` to reflect its role as a pure algorithm key. No source field is added — the policy has no knowledge of where metrics come from.

```go
// AutoscalingPolicyMetric defines a metric and its target value for scaling decisions.
type AutoscalingPolicyMetric struct {
    // Name is the stable key used by the scaling algorithm.
    // Must match keys used in Target.metricSources in the binding.
    Name string `json:"name"`
    // TargetValue defines the scaling threshold for this metric.
    TargetValue resource.Quantity `json:"targetValue"`
}
```

##### `autoscalingpolicybinding_types.go`

All metric-source types are defined here. `Target` contains a `MetricSources` map that declares how to fetch each metric. The new types are:

```go
// MetricSourceType selects the backend from which a metric value is fetched.
// +kubebuilder:validation:Enum=Pod;Prometheus
type MetricSourceType string

const (
    PodMetricSource        MetricSourceType = "Pod"
    PrometheusMetricSource MetricSourceType = "Prometheus"
)

// MetricSource is a discriminated union selecting the metric backend.
// +kubebuilder:validation:XValidation:rule="self.type != 'Prometheus' || has(self.prometheus)",message="prometheus config is required when type is Prometheus"
// +kubebuilder:validation:XValidation:rule="self.type != 'Pod' || !has(self.prometheus)",message="prometheus config must not be set when type is Pod"
type MetricSource struct {
    // Type selects the metric source backend.
    // +kubebuilder:default="Pod"
    Type MetricSourceType `json:"type"`
    // Pod configures direct pod endpoint scraping. Optional; overrides the pod-side
    // metric name filter when the scraped name differs from the policy metric name.
    // +optional
    Pod *PodMetricSource `json:"pod,omitempty"`
    // Prometheus configures an external Prometheus server as the metric source.
    // Required when Type is "Prometheus".
    // +optional
    Prometheus *PrometheusMetricSource `json:"prometheus,omitempty"`
}

// PodMetricSource configures pod-endpoint scraping for a metric.
// Used inside Target.metricSources.
type PodMetricSource struct {
    // Name is the Prometheus metric name matched against labels in the pod's
    // scraped output. Defaults to the policy metric name when omitted.
    // +optional
    Name string `json:"name,omitempty"`
}

// PrometheusMetricSource configures an external Prometheus server as a metric backend.
type PrometheusMetricSource struct {
    // ServerURL is the base URL of the Prometheus HTTP API server.
    // e.g. "http://prometheus.monitoring.svc.cluster.local:9090"
    ServerURL string `json:"serverURL"`
    // Query is a PromQL instant-query expression. The result must be a scalar or a
    // single-element instant vector. The returned sample value is used directly as
    // the metric input to the scaling algorithm.
    // e.g. 'sum(kthena:num_requests_waiting{serving="my-serving"})'
    Query string `json:"query"`
    // QueryTimeout is intentionally not added in v1.
    // The query call must use the reconcile-scoped context timeout
    // (AutoscaleCtxTimeoutSeconds) to keep timeout behavior consistent with
    // pod-source fetches and Prometheus fetches and to avoid per-target timeout skew.
    // Auth holds optional authentication configuration for the Prometheus server.
    // +optional
    Auth *PrometheusAuth `json:"auth,omitempty"`
}

// PrometheusAuth configures authentication when connecting to an external Prometheus server.
type PrometheusAuth struct {
    // BearerTokenSecret references a Secret key whose value is used as the
    // HTTP Authorization bearer token.
    // +optional
    BearerTokenSecret *corev1.SecretKeySelector `json:"bearerTokenSecret,omitempty"`
    // TLSConfig controls TLS certificate validation.
    // +optional
    TLSConfig *PrometheusTLSConfig `json:"tlsConfig,omitempty"`
}

// PrometheusTLSConfig holds TLS settings for Prometheus HTTPS connections.
type PrometheusTLSConfig struct {
    // InsecureSkipVerify disables TLS certificate verification. For development only.
    // +optional
    InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
    // CASecret references a Secret key containing a custom PEM-encoded CA bundle.
    // +optional
    CASecret *corev1.SecretKeySelector `json:"caSecret,omitempty"`
}
```

`Target` uses `MetricSources` as the only per-target metric source configuration:

```go
// Target defines a ModelServing deployment that can be monitored and scaled.
type Target struct {
    TargetRef      corev1.ObjectReference  `json:"targetRef"`
    SubTarget      *SubTarget              `json:"subTargets,omitempty"`
    // MetricSources declares how to fetch specific metrics for this target.
    // Keys must match AutoscalingPolicy.spec.metrics[].name.
    // This is the only metric-source configuration surface for Target.
    // Missing keys are treated as invalid configuration.
    // +optional
    MetricSources  map[string]MetricSource `json:"metricSources,omitempty"`
}
```

#### Controller Changes

##### Metric collection (`metric_collector.go`)

`MetricCollector` splits collection into two paths, keyed by resolved source type:

1. **Pod path** (existing, unchanged): scrapes pods via `fetchMetricsFromPods` and populates `ReadyInstancesMetrics []algorithm.Metrics`.
2. **Prometheus path** (new): calls `fetchPrometheusMetric` per metric and populates `ExternalMetrics algorithm.Metrics`.

Source resolution for a given metric `m` within a target `t`:

1. `t.MetricSources[m.Name]` must be present and is used as the source of truth.
2. Missing entry is treated as invalid configuration for that metric key.

The new `fetchPrometheusMetric` function uses `github.com/prometheus/client_golang/api/prometheus/v1` (already an indirect dependency via `client_model` and `common`) to execute an instant query and extract the scalar value.

```go
func (collector *MetricCollector) fetchPrometheusMetric(
    ctx context.Context,
    src *v1alpha1.PrometheusMetricSource,
) (float64, error)
```

Timeout and failure-mode contract:

- `fetchPrometheusMetric` must always use the caller-provided `ctx` for `api.Query(...)`; do not use `context.Background()`.
- The reconcile path already derives `ctx` with `AutoscaleCtxTimeoutSeconds` (the same timeout used by pod scraping), so a slow/hanging Prometheus query is cancelled automatically when the deadline expires.
- On timeout/cancellation, the function returns an error and the metric is treated as fetch-failed (consistent with other fetch errors). No separate per-source timeout field is required in this phase.

Rationale for not adding `QueryTimeout` in this proposal:

- keeps behavior deterministic across metric sources (pod and Prometheus use the same reconcile budget);
- avoids introducing per-target timeout tuning complexity before baseline observability is in place;
- can be added later as an optional override if real workloads show a clear need.

`UpdateMetrics` signature gains the `targetMetricSources` argument (from `Target.MetricSources`) so the collector can resolve sources. The policy itself is not passed since it carries no source information:

```go
func (collector *MetricCollector) UpdateMetrics(
    ctx context.Context,
    podLister listerv1.PodLister,
    targetMetricSources map[string]v1alpha1.MetricSource, // from Target.MetricSources
) (unreadyCount int32, readyMetrics []algorithm.Metrics, externalMetrics algorithm.Metrics, err error)
```

##### Algorithm (`recommendation.go`)

No changes required. `ExternalMetrics` is already handled by `getDesiredInstancesForSingleExternalMetric`.

##### Status reporting

`AutoscalingPolicyBindingStatus` should surface fetch health using standard Kubernetes conditions (`[]metav1.Condition`) rather than a custom error struct. This gives us `Type`, `Status`, `Reason`, `Message`, and `LastTransitionTime` with built-in tooling compatibility.

```go
type AutoscalingPolicyBindingStatus struct {
  // Conditions represents the latest available observations of binding state.
  // +patchMergeKey=type
  // +patchStrategy=merge
  // +listType=map
  // +listMapKey=type
    // +optional
  Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}
```

Recommended condition conventions:

- `Type=MetricsFetchReady`, `Status=True|False|Unknown`
- `Reason` examples: `PrometheusQuerySucceeded`, `PrometheusQueryFailed`, `PodScrapeFailed`, `InvalidMetricSourceConfig`
- `Message`: concise failure context (metric key, target name, brief root cause)
- set `ObservedGeneration` when writing conditions so stale status is obvious to operators

If we later need per-metric detail, we can still encode the metric key in `Reason`/`Message` and/or add a separate detailed status field, while keeping Conditions as the primary health surface.

#### Backward Compatibility

All existing `AutoscalingPolicy` and `AutoscalingPolicyBinding` resources continue to work without modification:

- `MetricName` is renamed to `Name` in the Go struct, and the JSON tag `metricName` also rename to `name` for the first release.
- **Removing `Target.MetricEndpoint`** is a breaking API change for bindings that relied on endpoint defaults.

#### Test Plan

- **Unit tests** for `fetchPrometheusMetric`: mock the Prometheus HTTP API using `httptest.Server` returning various result types (scalar, vector, matrix, error).
- **Unit tests** for source resolution and validation (`MetricSources[metricKey]` required; missing key is invalid).
- **Unit tests** for `MetricCollector.UpdateMetrics` with mixed pod + Prometheus sources.
- **Integration tests**: deploy a real Prometheus instance (via `setup-envtest` or a sidecar) and verify end-to-end scrape → scale decision.
- **E2E tests**: extend existing autoscaler e2e suite with a Prometheus-source binding and verify replica count adjusts in response to changes in a Prometheus gauge.

### Alternatives

<!--
Note: This is a simplified version of kubernetes enhancement proposal template.
https://github.com/kubernetes/enhancements/tree/3317d4cb548c396a430d1c1ac6625226018adf6a/keps/NNNN-kep-template
-->
