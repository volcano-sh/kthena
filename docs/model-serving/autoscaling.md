# Autoscaling ModelServing with KEDA

Scale a `ModelServing` based on live traffic. KEDA reads a Prometheus metric
from the kthena-router and updates `ModelServing.spec.replicas` through its
scale subresource.

```
Prometheus  →  KEDA  →  HPA  →  ModelServing  →  Pods
```

## Prerequisites

- A Kubernetes cluster with Kthena installed.
- [KEDA](https://keda.sh/docs/latest/deploy/) installed in the cluster.
- Prometheus scraping the kthena-router `/metrics` endpoint (port `8080`).
- A reachable Prometheus URL, e.g. `http://prometheus.monitoring.svc:9090`.

Check KEDA is ready:

```bash
kubectl get pods -n keda
kubectl get crd scaledobjects.keda.sh
```

## Step 1 — Deploy a ModelServing

Edit [`examples/model-serving/autoscaling-with-keda.yaml`](../../examples/model-serving/autoscaling-with-keda.yaml)
and replace every `<...>` placeholder. Then apply it:

```bash
kubectl apply -f examples/model-serving/autoscaling-with-keda.yaml
kubectl get modelserving <modelserving-name>
```

## Step 2 — Apply the ScaledObject

The same file already contains a `ScaledObject`. After it is applied, KEDA
creates an HPA pointing at the `ModelServing` scale subresource:

```bash
kubectl get scaledobject <modelserving-name>-scaler
kubectl get hpa
```

## Step 3 — Verify scaling

Send traffic through the kthena-router and watch the replicas change:

```bash
kubectl get modelserving <modelserving-name> -w
kubectl describe scaledobject <modelserving-name>-scaler
```

Replicas should rise once the metric crosses the threshold and fall again
after the cooldown.

## How it works

- The kthena-router exports `kthena_router_active_downstream_requests`,
  labeled by the served `model` name.
- KEDA polls Prometheus every `pollingInterval` seconds.
- If the query value exceeds `threshold`, KEDA scales the `ModelServing`
  through its scale subresource (the same path HPA uses).
- After traffic drops, KEDA waits `cooldownPeriod` before scaling down to
  `minReplicaCount`.

## Notes

- Pod labels in `entryTemplate` / `workerTemplate` are user-defined. The
  example uses `model-name` only as a hint — the trigger is router-level
  and does not depend on any pod label.
- Any Prometheus query works. Swap the example for queue depth, latency,
  GPU utilization, or anything else you scrape.
- KEDA scales the number of `ServingGroups` (`spec.replicas`). Per-role
  replicas inside a group are set in the template and stay fixed.
- Use `minReplicaCount: 1` to keep the model warm. Set it to `0` only if
  cold starts are acceptable.
