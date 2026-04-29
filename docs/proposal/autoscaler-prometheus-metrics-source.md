---
title: Prometheus Metrics Source for Autoscaler
authors:
- "@LiZhenCheng9527"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-04-27

---

## Prometheus Metrics Source for Autoscaler

### Summary

Today the kthena autoscaler computes scaling decisions exclusively from metrics scraped directly off endpoint pods (each pod's `/metrics` endpoint is fetched on every reconcile). This proposal extends the autoscaler with a second metric source: a Prometheus server reached over HTTP(S) using PromQL instant queries. The source is selected per `Target` via a new optional `MetricEndpoint.Prometheus` field, so the existing pod-scrape behavior remains the default and untouched.

When a target opts in, the autoscaler:

1. still uses the cluster's Pod lister to determine which target pods exist and how many are not yet ready (so the existing `ReadyInstancesMetrics` / `UnreadyInstancesCount` semantics survive verbatim);
2. resolves the configured metric values by issuing one PromQL instant query per metric against the configured Prometheus address;
3. feeds the resulting `algorithm.Metrics` into the unchanged recommendation/correction algorithms used by both `Autoscaler` (homogeneous) and `Optimizer` (heterogeneous) flows.

This unblocks scaling on metrics that are already centralized in Prometheus (business KPIs, gateway-level latency/QPS, multi-instance aggregates) and removes the requirement that every target pod expose a Prometheus-compatible text endpoint reachable from the controller.

### Motivation

The current pod-scrape collector has a few practical limitations:

- It requires every target pod to expose Prometheus text on a port reachable from the kthena-controller-manager. That doesn't hold when metrics are exported to a centralized Prometheus by a sidecar/agent, or when network policies prevent the controller from reaching pods directly.
- Histogram quantiles are computed by maintaining an in-process sliding window over scraped raw histograms. Prometheus already does this much more capably (`histogram_quantile`, `rate`, recording rules).
- Many SRE-/product-defined SLO metrics only exist as PromQL aggregates (e.g. cross-pod TTFT P95, gateway QPS, queue depth from a service the serving pods don't own).

Adding Prometheus as a metric source is the smallest change that lets users keep their existing observability pipeline as the source of truth for scaling decisions while keeping the autoscaler's algorithm and CRDs largely unchanged.

#### Goals

- Allow each `AutoscalingPolicyBinding` target to opt into reading metrics from a Prometheus server instead of from pod endpoints.
- Allow each `AutoscalingPolicyMetric` to define an optional PromQL expression (`Query`) that produces the aggregated value across all ready instances. When omitted, the metric name is used as the PromQL expression.
- Preserve the existing reconcile loop, scaling algorithm, panic mode, and homogeneous/heterogeneous logic.
- Keep the existing pod-scrape mode as the default (no behavior change for existing CRs).

#### Non-Goals

- Authentication beyond optional TLS `InsecureSkipVerify`. Bearer tokens, basic auth, mTLS, and custom headers are out of scope for this proposal.
- Range queries (`query_range`), recording-rule management, or query templating.
- Multi-tenant / multi-Prometheus federation, Thanos/VictoriaMetrics-specific features, or controller-level global Prometheus defaults.
- Maintaining the in-process histogram sliding window when the Prometheus source is selected. Histogram quantiles are expressed in PromQL by the user.
- Per-instance series mapping (matching individual Prometheus samples back to individual pods via `instance` / `pod` labels).

### Proposal

A new optional `MetricEndpoint.Prometheus` field is introduced on every `Target` in `AutoscalingPolicyBinding`. When set, the autoscaler instantiates a `PrometheusMetricSource` for that target instead of the pod-scrape `MetricCollector`. Both implementations satisfy a new `MetricSource` interface; the rest of the autoscaler reconciles against this interface.

The `AutoscalingPolicyMetric` struct gains an optional `Query` field containing a PromQL expression. The expression is the user's contract: it must return a scalar or a single-sample instant vector whose value already represents *the aggregated metric across all ready target instances* (the same semantics the algorithm assumes today when summing per-pod samples). When `Query` is empty, the metric name itself is used as the PromQL expression — useful for already-aggregated gauges/counters.

Pod readiness counting still flows through the existing pod lister and label selector logic, which means:

- `Target.MetricEndpoint.LabelSelector` still controls which pods are considered "the target's instances" for ready/unready accounting.
- `unreadyInstancesCount` is still incremented when any matching pod is not ready, exactly as in pod-scrape mode.
- The pod-scrape-only fields (`Uri`, `Port`) are simply ignored when `Prometheus` is set.

#### User Stories

##### Story 1: Aggregated QPS from a gateway

A platform team already exports request-rate metrics from their inference gateway (not from the model pods themselves). They configure an autoscaling policy with a single metric `qps` whose query is `sum(rate(gateway_requests_total{model="my-model"}[1m]))` and a target value of `50`. They bind the policy to a `ModelServing` with a Prometheus endpoint pointing at their in-cluster Prometheus. The autoscaler queries Prometheus every reconcile and scales replicas so that average QPS per replica converges on 50.

##### Story 2: Cross-pod latency P95

A team wants to scale on a TTFT P95 across all replicas of a `ModelServing` role. They use a query like `histogram_quantile(0.95, sum by (le) (rate(vllm_ttft_seconds_bucket{role="prefill"}[2m])))` and let Prometheus do the histogram math. The kthena autoscaler treats the query result as a single scalar and feeds it into the existing recommendation algorithm — no in-process sliding window needed.

#### Notes/Constraints/Caveats

- **Aggregation contract**: The PromQL must return a *single* aggregated value. When the result is an instant vector with multiple samples, the source sums them. This matches today's per-pod summation semantics but surfaces oddly if users hand-write a query returning per-instance samples — the current behavior is "summed", which is rarely what those users want. We log this case at V(4) but otherwise proceed.
- **Empty result handling**: An empty `model.Vector` is treated as `0`, consistent with pod-scrape mode where missing metrics fall through to `0`. This deliberately favors progress over halting on cold-start vectors.
- **Query failure handling**: An HTTP/PromQL error on any single metric aborts the current reconcile for that target (the source returns the error, the scaler/optimizer logs and skips). This mirrors the pod-scrape collector's `IsFailed` path.
- **Histogram fallback**: There is no in-process histogram sliding window in Prometheus mode. Users who need quantiles must express them in PromQL (`histogram_quantile(...)`). This is the explicit trade-off in exchange for not duplicating Prometheus inside the controller.
- **Auth scope**: Address is treated as opaque (scheme-prefixed). For HTTPS endpoints, `InsecureSkipVerify` is the only knob. We expect most in-cluster Prometheus to be accessible over HTTP, and explicitly defer richer auth.

#### Risks and Mitigations

- *Prometheus unavailable*: Each metric query runs with the existing 3s reconcile timeout (`AutoscaleCtxTimeoutSeconds`). On error the reconcile is skipped and replicas are left untouched, so a Prometheus outage degrades to "scaling frozen" rather than "wrong scaling".
- *Misconfigured PromQL returning per-pod samples*: Mitigated by clear field documentation on `AutoscalingPolicyMetric.Query` and an explicit V(4) log when a multi-sample vector is summed.
- *Security*: TLS skip-verify is opt-in and defaults to `false`. No credentials are stored in the CR. If credentialed access is needed, the user can front Prometheus with a Service-account-aware proxy until a follow-up extends `PrometheusEndpoint` with auth fields.

### Design Details

##### CRD additions

`pkg/apis/workload/v1alpha1/autoscalingpolicybinding_types.go`:

```go
// MetricEndpoint defines the endpoint configuration for scraping metrics.
type MetricEndpoint struct {
    // Existing fields (Uri, Port, LabelSelector) ...

    // Prometheus, if set, switches the metric source from per-pod scraping
    // to a Prometheus server. When set, metrics are fetched via PromQL
    // against the configured Prometheus address; per-pod readiness is still
    // derived from the matching pods in the cluster.
    // +optional
    Prometheus *PrometheusEndpoint `json:"prometheus,omitempty"`
}

// PrometheusEndpoint configures a Prometheus metric source.
type PrometheusEndpoint struct {
    // Address is the base URL of the Prometheus server, including scheme.
    // Must start with "http://" or "https://", e.g.
    // "http://prometheus.monitoring.svc:9090".
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:Pattern=`^https?://[^\s/$.?#].[^\s]*$`
    Address string `json:"address"`

    // InsecureSkipVerify disables TLS certificate verification when the
    // address uses https.
    // +optional
    // +kubebuilder:default=false
    InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}
```

The `Pattern` ensures the configured value is rejected by the API server at admission time when it does not begin with `http://` / `https://` or is otherwise obviously malformed. `MinLength=1` alone would accept arbitrary strings such as `"prometheus"` or `"://"`, which would only fail much later inside the controller when the Prometheus client tried to dial. In addition, the controller still parses the address with `net/url` on construction and logs a clear error if validation somehow allows an unusable value through (defense in depth):

```go
if _, parseErr := url.Parse(endpoint.Address); parseErr != nil {
    klog.Errorf("invalid prometheus address %q: %v", endpoint.Address, parseErr)
    return source // api stays nil; UpdateMetrics will return an error
}
```

`pkg/apis/workload/v1alpha1/autoscalingpolicy_types.go`:

```go
type AutoscalingPolicyMetric struct {
    MetricName  string            `json:"metricName"`
    TargetValue resource.Quantity `json:"targetValue"`

    // Query is an optional PromQL expression evaluated against the
    // Prometheus server configured on the target's MetricEndpoint. Must
    // return a scalar or single-sample instant vector representing the
    // aggregated value across all ready instances.
    // Only used when MetricEndpoint.Prometheus is set; ignored in
    // pod-scrape mode. If empty in Prometheus mode, MetricName is used
    // as the PromQL expression.
    // +optional
    Query string `json:"query,omitempty"`
}
```

##### Source abstraction

A new interface unifies pod scrape and Prometheus collectors:

```go
// MetricSource abstracts how scaling metrics are collected for a Target.
type MetricSource interface {
    UpdateMetrics(ctx context.Context, podLister listerv1.PodLister) (
        unreadyInstancesCount int32,
        readyInstancesMetric algorithm.Metrics,
        err error,
    )
    GetMetricTargets() map[string]float64
}

// NewMetricSource returns the appropriate MetricSource for the target.
func NewMetricSource(
    target *v1alpha1.Target,
    binding *v1alpha1.AutoscalingPolicyBinding,
    autoscalePolicy *v1alpha1.AutoscalingPolicy,
    metricTargets map[string]float64,
) MetricSource
```

`Autoscaler.Collector` and `Optimizer.Collectors` are typed against the interface; the existing `*MetricCollector` (pod scrape) keeps its implementation, simply adding `GetMetricTargets()`.

A small helper extracted from the pod-scrape path is shared between the two implementations:

```go
// evaluatePodReadiness reports all Ready / anyFailed for the given pods,
// matching the existing per-pod collector semantics.
func evaluatePodReadiness(pods []*corev1.Pod) (ready, anyFailed bool)
```

##### PrometheusMetricSource

```go
type PrometheusMetricSource struct {
    Target        *v1alpha1.Target
    Scope         Scope
    MetricTargets map[string]float64
    Queries       map[string]string  // metricName -> PromQL (empty = use name)
    api           promv1.API
}

func (p *PrometheusMetricSource) UpdateMetrics(
    ctx context.Context, podLister listerv1.PodLister,
) (unready int32, ready algorithm.Metrics, err error) {
    // 1. List target pods via util.GetMetricPods(...)
    // 2. evaluatePodReadiness(pods):
    //    - anyFailed → return zero values (caller skips reconcile)
    //    - !ready → unready++; return
    // 3. For each metricName in MetricTargets:
    //      expr := Queries[name]; if "" → expr = name
    //      v, err := api.Query(ctx, expr, time.Now())
    //      reduce v to a single float64 (Scalar / sum(Vector); empty → 0)
    //      ready[name] = v
    // 4. Return ready metrics.
}
```

The Prometheus client is built with `github.com/prometheus/client_golang/api` and `api/prometheus/v1`. TLS is configured by overriding `RoundTripper` on the `promapi.Config` when `InsecureSkipVerify` is true.

The per-query timeout reuses `util.AutoscaleCtxTimeoutSeconds` (3s) via a derived context, matching the existing per-pod HTTP timeout.

##### Scaler / Optimizer wiring

- `NewAutoscaler` constructs its `Collector` with `NewMetricSource(...)` passing the policy so the Prometheus source can read each metric's optional `Query`.
- `NewOptimizer` does the same per-target inside its `Collectors` map.
- All other call sites use `Collector.GetMetricTargets()` instead of the field access `Collector.MetricTargets`.

##### Backward compatibility

- The new fields are optional with zero values. Existing CRs continue to work and continue to use the pod-scrape `MetricCollector`.
- Generated code (`zz_generated.deepcopy.go`, applyconfiguration, CRD YAML) is regenerated via the existing `make generate` flow.

#### Test Plan

### Alternatives
