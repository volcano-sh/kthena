# SchedulerProfile for per-ModelServer scheduling

This example shows how to give a `ModelServer` its own scheduler plugin
combination instead of the router's global scheduler configuration.

A `SchedulerProfile` holds a complete plugin combination (`pluginConfig` +
`plugins`). A `ModelServer` references one profile by name through
`spec.trafficPolicy.loadBalancer.schedulerProfile`; the profile must be in
the same namespace. Requests for that ModelServer are scheduled with the
profile's plugin chain, while every other ModelServer keeps using the
router's global scheduler configuration.

Which plugins to enable, and with which weights and arguments, depends on
the workload — the values in this example demonstrate the structure only,
they are not tuned recommendations.

## Files

- `profile.yaml`     — a SchedulerProfile demonstrating the full structure.
- `modelserver.yaml` — a ModelServer referencing the profile.

## Usage

```bash
kubectl apply -f profile.yaml
kubectl apply -f modelserver.yaml
```

## Verification

```bash
kubectl get schedulerprofile
NAME             AGE
sample-profile   5m

kubectl get modelserver deepseek-r1-1-5b -o jsonpath='{.status.conditions}'
# SchedulerProfileResolved=True/False reports whether the referenced
# profile was resolved by the router.
```

If the condition reports `ProfileNotFound`, no chain exists for the
referenced profile and requests to this ModelServer fail until the
reference is fixed — a broken profile reference fails requests rather than
falling back to the global configuration. `ProfileResolutionFailed` means
the current profile spec failed to build: if a previous version of the
profile was serving, it keeps serving while the condition reports the
failure.
