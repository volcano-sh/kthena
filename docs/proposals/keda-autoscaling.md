# Proposal: KEDA + Prometheus Autoscaling for Kthena ModelServing

**Status:** Draft
**Authors:** @david_laid
**Date:** 2026-04-02

### Related PRs

| PR | Description |
|----|-------------|
| [#831](https://github.com/volcano-sh/kthena/pull/831) | Initial prototype -- example manifests (ServiceMonitor, PodMonitor, ScaledObject) |
| [#836](https://github.com/volcano-sh/kthena/pull/836) | Helm integration -- templates + `values.yaml` for monitoring and autoscaling |
| [#839](https://github.com/volcano-sh/kthena/pull/839) | Controller fix -- populates `status.labelSelector` so HPA can actually find pods |

All three reference [#799](https://github.com/volcano-sh/kthena/issues/799). We've validated the full flow end-to-end before writing this up.

---

## 1. Problem

LLM inference traffic is bursty. Without autoscaling, you either overprovision GPUs or eat latency spikes when load surges.

Kthena already has an autoscaler (`AutoscalingPolicy` + `AutoscalingPolicyBinding`). It scrapes metrics from pod endpoints, has panic mode, supports heterogeneous cost-optimized scaling. Works fine for pod-level signals like `kthena:num_requests_waiting`.

Where it falls short:

1. **Can't talk to Prometheus.** It scrapes pods directly. Most teams already have Prometheus running -- the autoscaler can't use it.

2. **No per-model demand signal.** The router exposes `kthena_router_active_downstream_requests{model="..."}` which tells you how much traffic a model is getting *before* it hits backends. The built-in autoscaler doesn't use this.

3. **Extra moving parts.** Teams already running KEDA end up maintaining two autoscaling systems side by side.

The goal here is to add KEDA as an optional autoscaling path. We're not touching AutoscalingPolicy.

### Non-goals

- Modifying or replacing `AutoscalingPolicy` / `AutoscalingPolicyBinding`
- Building a custom metrics adapter
- Multi-model-per-ModelServing (we assume 1:1)
- Role-level scaling via KEDA (built-in autoscaler handles that with `subTargets.kind: Role`)
- Auto-generating ScaledObjects (Phase 3 at earliest)

---

## 2. Proposed Approach

### Architecture

```
                    ┌─────────────┐
                    │ Prometheus   │
                    │ (scrapes     │
                    │  router)     │
                    └──────┬──────┘
                           │ PromQL query
                    ┌──────▼──────┐
                    │    KEDA      │
                    │ ScaledObject │
                    └──────┬──────┘
                           │ creates/manages
                    ┌──────▼──────┐
                    │     HPA      │
                    │ (targets     │
                    │  scale       │
                    │  subresource)│
                    └──────┬──────┘
                           │ PATCH spec.replicas
                    ┌──────▼──────┐
                    │ ModelServing │
                    │  Controller  │
                    └──────┬──────┘
                           │ creates/deletes
                    ┌──────▼──────┐
                    │ ServingGroups│
                    │  + Pods      │
                    └─────────────┘
```

### Scale-up flow at runtime

```
  User traffic              Router               Prometheus           KEDA              HPA             ModelServing
       │                      │                      │                 │                 │                    │
       │── requests ─────────►│                      │                 │                 │                    │
       │                      │── active_downstream  │                 │                 │                    │
       │                      │   _requests = 12 ───►│                 │                 │                    │
       │                      │                      │                 │                 │                    │
       │                      │                      │◄── PromQL ──────│                 │                    │
       │                      │                      │── returns 12 ──►│                 │                    │
       │                      │                      │                 │                 │                    │
       │                      │                      │                 │── update ──────►│                    │
       │                      │                      │                 │   external      │                    │
       │                      │                      │                 │   metric        │                    │
       │                      │                      │                 │                 │                    │
       │                      │                      │                 │                 │── PATCH ──────────►│
       │                      │                      │                 │                 │   spec.replicas=3  │
       │                      │                      │                 │                 │                    │
       │                      │                      │                 │                 │                    │── create
       │                      │                      │                 │                 │                    │   ServingGroups
       │                      │                      │                 │                 │                    │   + pods
```

### How it works

1. Prometheus scrapes the router, collects `kthena_router_active_downstream_requests{model="..."}`.
2. KEDA ScaledObject runs a PromQL query scoped to a specific model, targeting a ModelServing CR.
3. KEDA spins up an HPA pointing at the ModelServing scale subresource.
4. HPA reads `status.replicas` + `status.labelSelector`, patches `spec.replicas` when it needs to scale.
5. ModelServing controller sees the change, creates or deletes ServingGroups via `manageServingGroupReplicas()`.

### Why this works with ModelServing

ModelServing isn't a Deployment, but it already has a scale subresource:

```go
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.labelSelector
```

So HPA/KEDA talk to it the same way they'd talk to a Deployment:

| Field | Path | Purpose |
|-------|------|---------|
| Desired replicas | `spec.replicas` | HPA writes this |
| Current replicas | `status.replicas` | HPA reads this |
| Pod selector | `status.labelSelector` | HPA uses this to count pods |

One catch: the controller wasn't populating `status.labelSelector`. HPA couldn't find pods, scaling broke silently with a `selector is required` error. Fixed in [#839](https://github.com/volcano-sh/kthena/pull/839).

### Metrics

Primary metric -- per-model active requests from the router:

```promql
sum(kthena_router_active_downstream_requests{model="my-llama-model"})
```

This is a Gauge on active client-to-router requests, labeled by `model`. It tells you how much demand a model is seeing before requests even reach backends -- better signal than backend saturation for scaling decisions.

Other metrics we can use later:

| Metric | Source | Use case |
|--------|--------|----------|
| `kthena_router_active_upstream_requests` | Router | In-flight requests to backends |
| `kthena_router_fairness_queue_size` | Router | Queued requests per model/user |
| vLLM `num_requests_waiting` | Backend pods | Backend queue depth |

Combining multiple metrics (router + backend) is future work.

### What we learned building the prototype

We ran the full flow on a KIND cluster before writing this. Some stuff that came up:

1. **labelSelector was the blocker.** The CRD had the scale subresource defined correctly, but the controller never set `status.labelSelector`. KEDA created the HPA just fine, HPA couldn't find any pods. Took a while to figure out because the error (`selector is required`) doesn't point you to the right place. [#839](https://github.com/volcano-sh/kthena/pull/839) fixes this.

2. **KEDA needed zero patches.** Point a ScaledObject at ModelServing, KEDA creates the HPA, HPA uses the scale subresource. All config, no code changes on KEDA's side.

3. **Global metrics were our first attempt and they didn't work.** We started with `sum(kthena_router_active_downstream_requests)` (no model filter). Worked, but a spike on one model scaled everything. Per-model queries fixed it -- one-line change, big difference.

4. **End-to-end latency is ~60s worst case.** Prometheus scrape (15s) + KEDA poll (30s) + HPA sync (15s). For LLM workloads where loading a model takes minutes anyway, this is fine.

---

## 3. Key Design Decisions

### 3.1 Why KEDA instead of extending AutoscalingPolicy

| | AutoscalingPolicy | KEDA + Prometheus |
|--|-------------------|-------------------|
| Metric source | Direct pod scraping | Prometheus (any query) |
| Metric scope | Per-pod | Per-model (via PromQL labels) |
| External metrics | No | Yes |
| Ecosystem | Kthena-specific | CNCF graduated |
| Panic mode | Yes | No |
| Heterogeneous scaling | Yes | No |

We're adding KEDA as another option, not replacing anything. Teams pick what fits their setup.

### 3.2 Why per-model metric scoping

One ModelServing = one model (typically). Without filtering by model in the PromQL query, a spike on `model-A` would scale `model-B` too. The `model` label on router metrics makes scoping trivial -- each ScaledObject just queries for its model.

### 3.3 How ModelServing is targeted

ScaledObject points at ModelServing directly:

```yaml
scaleTargetRef:
  apiVersion: workload.serving.volcano.sh/v1alpha1
  kind: ModelServing
  name: my-model-serving
```

KEDA creates an HPA that hits the `/scale` subresource -- same as Deployments, StatefulSets. Nothing custom.

### 3.4 Why labelSelector matters

HPA needs `status.labelSelector` to count pods and compute scaling ratios. If it's empty, HPA sees 0 pods and scaling goes sideways. The selector has to match all pods in the CR's ServingGroups.

---

## 4. Alternatives Considered

### 4.1 Using only AutoscalingPolicy CRD

Has panic mode and heterogeneous scaling, which are nice. But it can't query Prometheus, can't scope by model from the router, and bolting Prometheus support onto it would just be reinventing KEDA. Keep both -- they cover different use cases.

### 4.2 CPU/memory-based scaling

Doesn't work for LLM inference. It's GPU-bound, and GPU util is a misleading signal -- a model at 30% GPU util can be completely saturated if KV-cache is full. Request-based metrics are what you actually want.

### 4.3 Global (non per-model) metrics

`sum(kthena_router_active_downstream_requests)` without a model filter -- simpler, but one model's spike scales everything. Wastes GPUs on models that don't need capacity.

Per-model scoping is a must for multi-model setups.

---

## 5. Failure Modes

What breaks and what happens:

| Failure | What happens | What to do |
|---------|-------------|------------|
| **Prometheus goes down** | KEDA can't get metrics, scaling freezes at current count | Use KEDA's `fallback` config (see below) to hold at a safe replica count |
| **Router pod restarts** | Metrics gap for a scrape interval or two | Run multiple router replicas. `sum()` still works with partial data |
| **KEDA operator dies** | HPA stops getting updates, but the existing HPA keeps running -- kube-controller-manager owns it | Run KEDA with 2+ replicas. Active scaling continues even without KEDA for a while |
| **Both autoscalers target same CR** | They fight over `spec.replicas`, replicas oscillate | Don't do this. Phase 2 adds a webhook to prevent it |
| **Bad PromQL query** (wrong model name) | Returns 0, may scale down to `minReplicaCount` | Check your query returns data before applying the ScaledObject |

Most failures just freeze scaling where it is -- you don't get runaway scaling. The `fallback` config handles the Prometheus-down case:

```yaml
fallback:
  failureThreshold: 3    # after 3 failed scrapes
  replicas: 2            # hold at 2
```

---

## 6. Open Questions / Future Work

### Model name to ModelServing CR mapping

Right now operators have to manually match the `model` label in the PromQL query to the correct ModelServing CR. Easy to mess up.

We should add a `kthena.io/model-name` annotation on ModelServing. Doesn't need a new controller -- just a standard place to record the mapping. We can build tooling on top of it later if auto-generating ScaledObjects makes sense.

### Multi-metric scaling

KEDA can take multiple triggers per ScaledObject (picks the highest recommendation). We could combine router demand with backend capacity:

```
trigger 1: kthena_router_active_downstream_requests{model="X"}  (demand)
trigger 2: vllm_num_requests_waiting                              (saturation)
```

Not blocking Phase 1 on this. We need real-world threshold data before we can write good defaults for multi-trigger configs.

### Scale-to-zero

KEDA supports it, but LLM model loading takes 30s to 5min. For dev/staging, `minReplicaCount: 0` is fine. For production, keep it at 1 until we have model preloading or some kind of warm cache.

### Conflict with existing autoscaler

If AutoscalingPolicy and KEDA both target the same ModelServing, they'll fight over `spec.replicas`. For now we just document that you shouldn't do this. In Phase 2 we add a validation webhook to block it.

---

## 7. Rollout Plan

### Phase 1: Foundation (PRs already open)

- [#839](https://github.com/volcano-sh/kthena/pull/839): Controller fix for `status.labelSelector`. This is the blocker -- nothing works without it.
- [#831](https://github.com/volcano-sh/kthena/pull/831): Example manifests (ServiceMonitor, PodMonitor, ScaledObject) in `examples/keda-autoscaling/`.
- [#836](https://github.com/volcano-sh/kthena/pull/836): Helm chart templates behind feature flags (`metrics.enabled`, `autoscaling.enabled`), off by default.

Merge #839 first, then #831 and #836 in any order.

After this, users can set up KEDA autoscaling with per-model Prometheus metrics. Everything is opt-in, existing setups are unaffected.

### Phase 2: Hardening

- `kthena.io/model-name` annotation on ModelServing
- Validation webhook to block AutoscalingPolicy + KEDA on the same CR
- Recommended thresholds based on production data
- Multi-trigger ScaledObject examples

### Phase 3: If there's demand

- Controller that auto-generates ScaledObjects from annotated ModelServing CRs
- Scale-to-zero for dev/staging
- Grafana dashboard for scaling decisions

---

## 8. Example Configuration

### Minimal ScaledObject

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: my-model-autoscaler
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: workload.serving.volcano.sh/v1alpha1
    kind: ModelServing
    name: my-model-serving
  minReplicaCount: 1
  maxReplicaCount: 10
  cooldownPeriod: 60
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring.svc:9090
        query: |
          sum(kthena_router_active_downstream_requests{model="my-llama-model"})
        threshold: "5"
        activationThreshold: "1"
```

Scales `ModelServing/my-model-serving` via its scale subresource. Scales up when active requests per replica go above 5, stays between 1-10 replicas, cools down for 60s before scaling back.

### Per-model query examples

```promql
# Active requests (how much demand right now)
sum(kthena_router_active_downstream_requests{model="my-llama-model"})

# Request rate (req/s over 2 minutes)
sum(rate(kthena_router_requests_total{model="my-llama-model"}[2m]))

# Queue depth
sum(kthena_router_fairness_queue_size{model="my-llama-model"})
```

### Prerequisites

1. Prometheus scraping the Kthena router (ServiceMonitor or scrape config)
2. KEDA installed in the cluster
3. ModelServing controller populating `status.labelSelector` ([#839](https://github.com/volcano-sh/kthena/pull/839))
4. RBAC for KEDA to hit the ModelServing scale subresource:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: keda-modelserving-scaler
rules:
  - apiGroups: ["workload.serving.volcano.sh"]
    resources: ["modelservings/scale"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["workload.serving.volcano.sh"]
    resources: ["modelservings"]
    verbs: ["get", "list", "watch"]
```
