# Bootstrap Acceleration

Bootstrap acceleration creates ModelServing role replicas in source-aware batches so that most replicas load model weights peer-to-peer from ready replicas instead of from storage.

## Overview

When many replicas start together, every one of them loads the full checkpoint from storage at once, and the fleet becomes ready only as fast as that contended load. [ModelExpress (MX)](https://github.com/ai-dynamo/modelexpress) copies weights from a ready serving replica over NIXL/RDMA, but it only helps replicas that start **after** a compatible source is serving.

With `spec.bootstrapAccelerateStrategy`, the ModelServing controller decides **when** to create each role replica:

- While no replica of a role is ready, it creates a first batch of up to `seedReplicas` role replicas. They find no source, load from storage, and then become MX sources.
- Once replicas are ready, it creates the rest in batches of up to `sourceFanOut` starting replicas per ready replica. They pull their weights from the ready replicas.

All replicas are identical: each one tries P2P first and falls back to storage. The strategy applies to initial deployment, scale-up, recovery, and rolling updates. Without the field, nothing changes.

## Prerequisites

- A Kubernetes cluster with Kthena installed.
- A ModelExpress server reachable from the inference Pods. Kthena does not deploy it.
- An MX-enabled inference engine, for example vLLM 0.23 or later with `--load-format modelexpress`. Otherwise the strategy only limits how many replicas bootstrap at the same time.
- A readiness probe that succeeds only after the weights are loaded, such as vLLM's `/health`. A role replica counts as a source once all its Pods are Ready.

## Configuration

```yaml
spec:
  replicas: 8
  bootstrapAccelerateStrategy:
    roles: ["prefill"]        # optional, empty means all roles
    seedReplicas: 1           # optional, default 1
    sourceFanOut: 2           # optional, default 1
    modelExpress:             # optional
      engineContainers: ["engine"]
      serverAddress: modelexpress-server.modelexpress:8001
      readyURL: http://127.0.0.1:8000/health
```

| Field | Description |
|---|---|
| `provider` | Weight transfer system. Only `ModelExpress` is supported, and it is the default. |
| `roles` | Roles whose replicas are created in batches. Empty means all roles. Other roles are created as usual. |
| `seedReplicas` | Role replicas created per role while no ready replica exists. A positive integer, or a percentage from `1%` to `100%` of `spec.replicas * role.replicas`, rounded up. |
| `sourceFanOut` | Starting role replicas allowed per ready replica of the same role. At least 1. |
| `modelExpress.engineContainers` | Required. Regular containers that run the inference engine in the entry and worker Pods of in-scope roles. Only these containers receive MX environment variables. |
| `modelExpress.serverAddress` | `host:port` of the MX server, injected as `MX_SERVER_ADDRESS`. |
| `modelExpress.readyURL` | Absolute `http` or `https` URL that MX polls before a source publishes itself, injected as `MX_ARTIFACT_READY_URL`. |

The field is mutable. Changes take effect on the next reconcile and never trigger a rolling update, because the strategy is not part of the ModelServing revision.

### Environment injection

When `modelExpress` is set, Kthena adds the following variables to the selected engine containers of in-scope Pods:

| Variable | Value |
|---|---|
| `MX_SERVER_ADDRESS` | `modelExpress.serverAddress`, if set. |
| `MX_ARTIFACT_READY_URL` | `modelExpress.readyURL`, if set. |
| `MX_WORKER_HOST` | The Pod IP, through the downward API. |

A variable already defined in the container's `env` takes precedence. Variables supplied through `envFrom` are not detected, so move them to `env` or leave the corresponding field empty. Remove `MODEL_EXPRESS_URL` from templates and images, because MX prefers it over `MX_SERVER_ADDRESS`. Init containers and other containers are never modified.

Other MX settings, such as `MX_NIXL_BACKEND`, ports, or timeouts, can be set directly in the engine container's `env`. If `modelExpress` is omitted, no variables are injected and you configure the engine yourself.

Do not add a `modelexpress` entry to `spec.plugins`. Kthena runs this plugin automatically after your plugins, and the webhook rejects explicit registration.

## How batches are created

Replicas are grouped into pools. A pool is a role with a given role template and a given bootstrap configuration (MX endpoint, engine containers, and plugin configuration). Only ready replicas of the same pool count as sources. A rolling update that changes a role's template therefore starts a new pool with a seed batch, and roles that did not change keep their sources.

For each pool, with `R` ready replicas, the controller lets at most this many replicas bootstrap at the same time:

- `R = 0`: `seedReplicas`
- `R > 0`: `sourceFanOut * R`

Missing replicas that do not fit are created on a later reconcile, after more replicas become ready. Lower ordinals are created first.

For example, with `spec.replicas: 8` and one replica per role:

| Policy | Created replicas after each batch |
|---|---|
| `seedReplicas: 1`, `sourceFanOut: 1` | 1 → 2 → 4 → 8 |
| `seedReplicas: 1`, `sourceFanOut: 2` | 1 → 3 → 8 |
| Scale from 8 to 16, `sourceFanOut: 1` | 8 → 16 |

### Gang scheduling

With `schedulerName: volcano`, each ServingGroup needs `minRoleReplicas` replicas of every role to schedule together (see [Gang Scheduling](./gang-scheduling.md)). Kthena creates a new ServingGroup with its PodGroup and only this gang core of the in-scope roles, plus all replicas of the other roles. The remaining replicas of the group are added later, one at a time.

The batch size of a pool is never smaller than one gang core, so the default `seedReplicas: 1` is valid even when a core needs more replicas. A smaller `minRoleReplicas` makes cores smaller and keeps batches closer to `sourceFanOut`.

## Deploy the example

The example deploys 8 single-GPU vLLM replicas with `seedReplicas: 1` and `sourceFanOut: 2`. Update the image and the MX server address for your environment first.

```bash
kubectl apply -f examples/model-serving/bootstrap-acceleration.yaml
```

Kthena creates one replica first. After it is ready, Kthena creates 2 more, then the remaining 5:

```bash
kubectl get pods -l modelserving.volcano.sh/name=bootstrap-acceleration -w
```

Each batch emits a `BootstrapBatchCreated` event on the ModelServing:

```bash
kubectl get events --field-selector involvedObject.name=bootstrap-acceleration,reason=BootstrapBatchCreated
```

Pods created by the strategy carry the `workload.serving.volcano.sh/bootstrap-config-hash` annotation, which identifies their pool. Deferred replicas are logged by the controller at verbosity level 4.

## Notes

- Removing `bootstrapAccelerateStrategy` creates all missing replicas on the next reconcile.
- If a first-batch replica never becomes ready, its pool waits. Failed replicas are re-created by the recovery policy.
- Replicas created before the strategy was enabled have no configuration annotation and are not counted as sources.
- Later batches pull their image only after they are created. Pre-pull the engine image on GPU nodes to keep batches short.
- An init container that downloads the full model still blocks P2P loading. Use a shared cache or no downloader.
