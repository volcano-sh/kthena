# KEDA Autoscaling for ModelServing

Autoscale ModelServing instances using Prometheus metrics and [KEDA](https://keda.sh/).

**Pipeline:** Prometheus -> KEDA -> HPA -> ModelServing -> Pod Scaling

## Prerequisites

- Kubernetes cluster with [Kthena (Volcano)](https://github.com/volcano-sh/volcano) installed
- [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator) (for ServiceMonitor/PodMonitor CRDs)
- [KEDA](https://keda.sh/docs/deploy/) v2.x installed in the cluster
- A deployed ModelServing instance with kthena-router

## Manifests

| File | Purpose |
|------|---------|
| `servicemonitor-router.yaml` | Scrapes metrics from kthena-router (port 8080) |
| `podmonitor-inference.yaml` | Scrapes vLLM metrics from inference pods (port 8000) |
| `keda-scaledobject.yaml` | KEDA ScaledObject that drives HPA based on Prometheus queries |

## Deployment

### 1. Deploy monitoring targets

```bash
kubectl apply -f examples/keda-autoscaling/servicemonitor-router.yaml
kubectl apply -f examples/keda-autoscaling/podmonitor-inference.yaml
```

Verify metrics are being scraped in your Prometheus UI.

### 2. Configure the ScaledObject

Edit `keda-scaledobject.yaml` before applying:

- **`spec.scaleTargetRef.name`** — set to your ModelServing resource name
- **`model` label in router query** — set to the model name your ModelServing instance serves (e.g. `"deepseek-ai/DeepSeek-R1"`)
- **`model_serving` label in vLLM query** — set to your ModelServing `metadata.name`

```bash
kubectl apply -f examples/keda-autoscaling/keda-scaledobject.yaml
```

### 3. Verify

```bash
# Check KEDA created the HPA
kubectl get hpa

# Check ScaledObject status
kubectl get scaledobject modelserving-scaler

# Watch pods scale
kubectl get pods -w
```

## Prometheus Queries Explained

### Router active requests (per model)

```promql
sum(kthena_router_active_downstream_requests{model="my-model-name"})
```

The `kthena_router_active_downstream_requests` gauge tracks currently active client requests to the router. The `model` label identifies which model the request targets, so filtering by `model` ensures each ModelServing instance scales based only on **its own traffic** — not aggregate load across all models.

### vLLM pending requests (per ModelServing)

```promql
avg(vllm:num_requests_waiting{model_serving="my-modelserving"})
```

The `vllm:num_requests_waiting` gauge is exposed by each vLLM inference pod. Filtering by `model_serving` scopes the average to pods belonging to a specific ModelServing instance.

## Per-Model Scaling

The kthena-router supports routing to multiple models simultaneously. Without per-model filtering, a single busy model would trigger scaling for **all** ModelServing instances. Each ScaledObject must filter metrics to the specific model/ModelServing it manages:

- Create **one ScaledObject per ModelServing** instance
- Set the `model` label filter to match the model name served by that instance
- Adjust `threshold`, `minReplicaCount`, and `maxReplicaCount` per model based on expected load

## Testing

Generate load against a specific model and watch scaling:

```bash
# Send requests to trigger scaling
for i in $(seq 1 50); do
  curl -s http://<router-endpoint>/v1/completions \
    -H "Content-Type: application/json" \
    -d '{"model": "my-model-name", "prompt": "Hello", "max_tokens": 100}' &
done

# Watch the HPA react
kubectl get hpa -w
```

The ScaledObject's `cooldownPeriod: 120` means pods scale down 2 minutes after load drops below the threshold.
