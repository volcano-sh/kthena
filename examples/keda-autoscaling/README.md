# KEDA Autoscaling for ModelServing

This example demonstrates how to autoscale a ModelServing resource using [KEDA](https://keda.sh/) with Prometheus metrics.

## Prerequisites

- KEDA installed in the cluster (`keda` namespace)
- Prometheus stack deployed (`monitoring` namespace)
- Kthena router exposing metrics via ServiceMonitor
- KEDA RBAC for ModelServing (see `keda-rbac.yaml` in the repo root)

## Usage

```bash
kubectl apply -f modelserving.yaml
kubectl apply -f scaledobject.yaml
```

## How it works

- `modelserving.yaml` creates a ModelServing resource named `test-model` with entry and worker roles.
- `scaledobject.yaml` creates a KEDA ScaledObject that targets the ModelServing resource and scales based on Prometheus metrics from the kthena-router.
- KEDA queries Prometheus for `kthena_router_active_downstream_requests` and adjusts `spec.replicas` (the number of serving groups) accordingly.

## Scaling: groups vs pods

ModelServing scales at the **group** level (`spec.replicas`), not at the individual pod level. Each group may contain multiple pods (entry + workers). The Prometheus query uses `sum()` to aggregate metrics across all pods, and the threshold is set relative to group capacity. This ensures the scaling decision correctly maps to the number of groups, even though the actual pod count is a multiple of the group count.
