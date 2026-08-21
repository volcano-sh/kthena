# Scale-to-Zero Support for Kthena Model Serving

## Overview

This proposal describes the design for supporting scale-to-zero and scale-from-zero in Kthena's Model Serving platform. When a ModelServing has zero replicas (e.g., due to autoscaling idle detection), the router can automatically trigger a scale-up and hold incoming requests until pods become available. This enables serverless-style cost optimization where idle inference workloads consume zero compute resources.

## Background

LLM inference workloads are expensive to run. Many models require GPU resources, and keeping pods warm 24/7 when traffic is sporadic wastes significant compute budget. Traditional autoscalers like HPA cannot scale to zero because they rely on live metrics from running pods. Kthena's autoscaler, combined with a router-level scale-from-zero mechanism, bridges this gap:

1. The **autoscaler** detects idle traffic and scales the ModelServing down to 0 replicas after a configurable cooldown period.
2. The **router** detects that no pods are available for a request, triggers a scale-up by patching the ModelServing to 1 replica, and holds the request until a pod is ready.

This two-component approach cleanly separates the scaling domain (autoscaler) from the routing domain (router), avoiding tight CRD coupling.

## Goals

1. Support scaling ModelServing workloads to zero replicas when traffic is idle.
2. Automatically scale from zero when new requests arrive at the router, with minimal user-perceived latency.
3. Prevent immediate scale-to-zero by introducing a configurable cooldown period on the autoscaler.
4. Avoid direct CRD coupling between the router and autoscaler domains by deriving the ModelServer-to-ModelServing mapping from pod labels at runtime.
5. Support concurrent requests to the same zero-replica ModelServer without triggering duplicate scale-ups.

## Non-Goals

1. Predictive pre-scaling (warming pods before traffic arrives based on historical patterns).
2. Cross-namespace scale-from-zero (ModelServer and ModelServing must be in the same namespace).
3. Custom scale-from-zero target replicas (always scales to 1 replica).

## Architecture

The scale-to-zero feature spans two subsystems: the **autoscaler** (cooldown logic) and the **router** (scale-from-zero manager).

### Component Overview

```
                    ┌─────────────────────────────────────────────┐
                    │              Autoscaler                      │
                    │                                             │
                    │  ┌─────────────┐    ┌──────────────────┐   │
                    │  │   Scaler /   │    │ CooldownPeriod   │   │
                    │  │  Optimizer   │───▶│ (IdleStartTime)  │   │
                    │  └─────────────┘    └──────────────────┘   │
                    │         │                                   │
                    │         ▼                                   │
                    │  ModelServing.spec.replicas = 0             │
                    └─────────────────────────────────────────────┘
                                      │
                                      ▼
                    ┌─────────────────────────────────────────────┐
                    │              Router                          │
                    │                                             │
                    │  ┌──────────────────┐                       │
                    │  │  doLoadbalance() │                       │
                    │  └────────┬─────────┘                       │
                    │           │ no pods found                   │
                    │           ▼                                 │
                    │  ┌──────────────────┐   ┌───────────────┐  │
                    │  │ scalefromzero    │──▶│ Patch         │  │
                    │  │ .Manager         │   │ ModelServing  │  │
                    │  └────────┬─────────┘   │ replicas=1    │  │
                    │           │             └───────────────┘  │
                    │           ▼                                 │
                    │  Hold request, wait for pod                 │
                    │           │                                 │
                    │           ▼                                 │
                    │  OnPodsAvailable() → forward request        │
                    └─────────────────────────────────────────────┘
```

### 1. Cooldown Period (Autoscaler)

#### Motivation

Without a cooldown, the autoscaler would immediately scale a ModelServing to zero the moment traffic drops below the threshold. This causes thrashing: a brief traffic lull triggers scale-to-zero, the next request triggers scale-from-zero, and the cycle repeats. A configurable cooldown period ensures the workload stays warm for a configurable duration after the last active metric.

#### Design

A `CooldownPeriod` field is added to `AutoscalingPolicyBehavior`:

```go
type AutoscalingPolicyBehavior struct {
    ScaleUp        AutoscalingPolicyScaleUpPolicy `json:"scaleUp"`
    ScaleDown      AutoscalingPolicyStablePolicy  `json:"scaleDown"`
    CooldownPeriod *metav1.Duration               `json:"cooldownPeriod,omitempty"`
}
```

- Only active when `minReplicas` is 0.
- When metrics become idle and the recommended replica count is 0, the autoscaler records `IdleStartTime` in the scaler/optimizer status.
- On each subsequent reconciliation, if the elapsed time since `IdleStartTime` is less than the cooldown period, the current replica count is preserved.
- When the cooldown expires, the scale-to-zero proceeds and `IdleStartTime` is reset.
- If traffic resumes during the cooldown, `IdleStartTime` is reset to 0.

The cooldown logic is applied in both code paths:
- **Scaler** (homogeneous targets): `pkg/autoscaler/autoscaler/scaler.go`
- **Optimizer** (heterogeneous targets): `pkg/autoscaler/autoscaler/optimizer.go`

#### Example Configuration

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
spec:
  tolerancePercent: 10
  metrics:
    - metricName: avg_requests_per_second
      targetValue: "10"
  behavior:
    cooldownPeriod: "5m"  # Wait 5 minutes of idle before scaling to zero
    scaleDown:
      stabilizationWindowSeconds: 300
```

### 2. Scale-from-Zero Manager (Router)

#### Design

A new `scalefromzero.Manager` component is introduced in `pkg/kthena-router/scalefromzero/`. It handles the router-side logic for detecting zero-pod ModelServers and triggering scale-ups.

**Key data structures:**

```go
type Manager struct {
    enabled       bool
    kubeClient    clientset.Interface
    bindingLister listerv1alpha1.AutoscalingPolicyBindingLister

    // ModelServer key ("ns/name") -> ModelServing key ("ns/name")
    servingForMS map[string]string

    // Binding existence cache: ModelServing key -> has binding
    bindingCache    map[string]bool
    bindingCacheTTL time.Duration

    // Pending scale-from-zero waits
    pending map[string]*pendingWait
    timeout time.Duration
}
```

**Flow:**

1. Router receives a request targeting a ModelServer.
2. `doLoadbalance()` discovers no ready pods for the ModelServer.
3. If `sfzManager.Enabled()`, delegates to `sfzManager.Handle()`.
4. `Handle()` looks up the ModelServing name from the runtime mapping (derived from pod labels).
5. Checks that the ModelServing has an associated `AutoscalingPolicyBinding` (via cached lister).
6. If not configured, returns `ErrNotConfigured` → 404 response.
7. If configured, patches the ModelServing to `replicas=1` (idempotent, only first request triggers the patch).
8. Holds the request on a channel, waiting for `OnPodsAvailable()`.
9. When a pod becomes ready, the ModelServer controller calls `OnPodsAvailable()`, which wakes the waiting request.
10. Router forwards the request to the now-available pod.

**ModelServer-to-ModelServing mapping:**

Rather than introducing a direct CRD reference between `ModelServer` and `ModelServing`, the mapping is derived at runtime from pod labels. The label `modelserving.volcano.sh/name` on pods identifies the parent ModelServing. This is extracted in the ModelServer controller's `syncModelServerHandler`:

```go
for _, pod := range podList {
    if name, ok := pod.Labels["modelserving.volcano.sh/name"]; ok {
        modelServingName = name
        break
    }
}
```

For the zero-pod case, the mapping falls back to the workload selector's match labels.

**Configuration (environment variables):**

| Variable | Default | Description |
|---|---|---|
| `ENABLE_SCALE_FROM_ZERO` | `false` | Enable/disable the scale-from-zero feature |
| `SCALE_FROM_ZERO_TIMEOUT` | `5m` | Maximum time to wait for pods before returning 504 |

**RBAC requirements:**

The router's ClusterRole needs additional permissions:
- `workload.serving.volcano.sh/modelservings`: get, list, patch
- `workload.serving.volcano.sh/autoscalingpolicybindings`: get, list, watch

#### Concurrency Handling

Multiple concurrent requests to the same zero-replica ModelServer share a single `pendingWait`. Only the first request triggers the `ModelServing` patch; subsequent requests wait on the same channel. When `OnPodsAvailable()` fires, all waiting requests are unblocked.

### 3. Pod Lifecycle Integration

The ModelServer controller's `syncPodHandler` is extended to notify the scale-from-zero manager when a pod becomes ready:

```go
if c.sfzManager != nil {
    // For each ModelServer matching this pod's labels
    for _, ms := range modelServers {
        if selector.Matches(labels.Set(pod.Labels)) {
            c.sfzManager.OnPodsAvailable(ms.Namespace, ms.Name, []*datastore.PodInfo{podInfo}, ms)
        }
    }
}
```

Additionally, when a ModelServer is deleted, its mapping is cleaned up:

```go
if c.sfzManager != nil {
    c.sfzManager.DeleteModelServerMapping(namespace, name)
}
```

## Configuration Design

### AutoscalingPolicy

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
spec:
  behavior:
    # Delays scaling to zero after metrics become idle.
    # Only active when the binding's minReplicas is 0.
    cooldownPeriod: "5m"
```

### Router Environment Variables

| Variable | Default | Description |
|---|---|---|
| `ENABLE_SCALE_FROM_ZERO` | `false` | Master switch for scale-from-zero |
| `SCALE_FROM_ZERO_TIMEOUT` | `5m` | Request hold timeout before 504 |

### Helm Values

```yaml
kthena-router:
  env:
    - name: ENABLE_SCALE_FROM_ZERO
      value: "true"
    - name: SCALE_FROM_ZERO_TIMEOUT
      value: "5m"
```

## Trade-offs and Limitations

### Advantages

- **Cost savings**: Idle GPU workloads consume zero compute resources.
- **Clean domain separation**: Router and autoscaler communicate through Kubernetes primitives (ModelServing spec/replicas) rather than direct API calls.
- **No CRD changes for routing**: The ModelServer-to-ModelServing mapping is derived from pod labels, avoiding tight coupling.
- **Concurrency-safe**: Multiple requests to the same zero-pod ModelServer coalesce into a single scale-up trigger.

### Limitations

- **Cold start latency**: The first request after scale-to-zero incurs pod startup latency (model loading, GPU initialization). This can be tens of seconds to minutes depending on model size.
- **Single replica target**: Scale-from-zero always targets 1 replica. If the workload needs multiple replicas, the autoscaler will scale up further on subsequent reconciliation cycles.
- **Same-namespace only**: The ModelServer and ModelServing must reside in the same namespace.
- **Binding cache staleness**: The binding cache has a 2-minute TTL. Newly created bindings may not be recognized immediately.

## Test Plan

### Unit Tests

- **Cooldown**: idle prevention, expiration, traffic resume reset, minReplicas>0 bypass, scale-from-zero resets cooldown.
- **Scale-from-zero Manager**: disabled by default, enabled via env, custom timeout, no mapping, no binding, triggers scale-up, waits for pods, timeout, concurrent requests share trigger, mapping CRUD, binding cache refresh (homogeneous + heterogeneous targets), list errors.

### Integration Tests

- End-to-end flow: autoscaler scales to zero → request arrives → router triggers scale-up → pod becomes ready → request forwarded.
- Multiple concurrent requests to zero-replica ModelServer.
- Cooldown period prevents premature scale-to-zero during traffic gaps.

## Future Enhancements

1. **Configurable scale-from-zero target replicas** (currently hardcoded to 1).
2. **Predictive warming** based on traffic patterns (e.g., time-of-day scheduling).
3. **Priority-based request holding** during cold start (VIP requests get forwarded first).
4. **Metrics for scale-from-zero events** (cold start count, average cold start latency, timeout rate).
