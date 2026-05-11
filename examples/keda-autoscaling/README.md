# KEDA Autoscaling for ModelServing

This example autoscales a `ModelServing` using [KEDA](https://keda.sh/) driven
by a Prometheus query against kthena-router metrics.

## Prerequisites

- KEDA installed in the `keda` namespace.
- A Prometheus stack reachable at the `serverAddress` used in `scaledobject.yaml`
  (the example assumes kube-prometheus-stack in the `monitoring` namespace).
- kthena-router deployed and serving traffic for a model known to Prometheus
  under the `model` label (see "Model label" below).

## Files

- `modelserving.yaml`   — a minimal ModelServing named `test-model` (the scale target).
- `servicemonitor.yaml` — scrapes kthena-router `/metrics` into Prometheus.
- `rbac.yaml`           — grants the KEDA operator access to the ModelServing
  `scale` subresource (required for KEDA to change `spec.replicas`).
- `scaledobject.yaml`   — the KEDA `ScaledObject` that drives scaling.
- `slow-backend.yaml`   — a Python HTTP server that sleeps ~3s per request, used as
  the traffic backend so the `active_downstream_requests` gauge stays above
  threshold under load.
- `route.yaml`          — `ModelRoute` + `ModelServer` wiring `model=test` traffic
  through kthena-router to `slow-backend`.
- `loadgen.yaml`        — in-cluster load generator `Deployment`. Scale to 1 to
  start load, scale to 0 to stop.

## Usage

```bash
kubectl apply -f rbac.yaml
kubectl apply -f servicemonitor.yaml
kubectl apply -f modelserving.yaml
kubectl apply -f slow-backend.yaml
kubectl apply -f route.yaml
kubectl apply -f scaledobject.yaml
kubectl apply -f loadgen.yaml

# drive autoscaling with real traffic
kubectl scale deploy loadgen --replicas=1   # metric rises, ModelServing scales up
kubectl scale deploy loadgen --replicas=0   # metric drops, scales back down
```

## Model label

The Prometheus query filters by `model="test"`:

```promql
sum(kthena_router_active_downstream_requests{model="test"})
```

The `model` label is populated by kthena-router from the request (e.g. the
`model` field in an OpenAI-style payload, or the ModelRoute match). Change the
label value to match the model name your traffic uses — a global `sum(...)`
without the filter would aggregate every model on the router and produce wrong
scaling decisions.

## Scaling unit: ServingGroup, not pod

`spec.replicas` on a ModelServing is the number of **ServingGroups**, and each
group can contain several pods (one entry + workers per role). The scale
subresource's `status.labelSelector` is therefore scoped to the entry pod of
the first role, so the selector matches exactly one pod per group and the
HPA/KEDA pod count lines up with `spec.replicas`.
