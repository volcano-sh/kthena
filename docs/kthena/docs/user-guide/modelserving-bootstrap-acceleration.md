# Bootstrap Acceleration with ModelExpress

When many `ModelServing` replicas start together, every role replica loads the full checkpoint from storage at the same time, and the fleet becomes ready only as fast as that contended load. [ModelExpress (MX)](https://github.com/ai-dynamo/modelexpress) copies post-processed weights from a Ready serving replica over NIXL/RDMA, but it only helps replicas that start **after** a compatible source is serving.

`spec.bootstrapAccelerateStrategy` makes the ModelServing controller create role replicas in two stages so that most of them load weights peer-to-peer:

1. While a role has no Ready replica of the current template, the controller creates at most `initialSeedReplicas` replicas. They find no source, load from storage, and become MX sources.
2. Once a seed replica is Ready, the controller creates every remaining replica. They pull weights from the Ready replicas.

All replicas run the same engine configuration; the controller only decides **when** to create them.

## Prerequisites

- An MX-enabled inference engine (for example vLLM with `--load-format modelexpress`) that can reach a ModelExpress server.
- The controller manager is started with `--modelexpress-server-address=<host>:<port>` (Helm: `controllerManager.modelExpressServerAddress`). The address is injected into engine containers as `MX_SERVER_ADDRESS`.
- The Pod readiness probe only reports Ready once weights are loaded, because a `Running` role replica is treated as a source.

## Configuration

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: bootstrap-accelerate
spec:
  replicas: 16
  bootstrapAccelerateStrategy:
    roles:
      - name: prefill
        initialSeedReplicas: 2
    modelExpress:
      engineContainers: ["vllm"]
      readyURL: http://127.0.0.1:8000/health
  template:
    roles:
      - name: prefill
        replicas: 1
        entryTemplate:
          spec:
            containers:
              - name: vllm
                image: vllm/vllm-openai:latest
                args: ["--model", "/models/deepseek-v4", "--load-format", "modelexpress"]
                readinessProbe:
                  httpGet: {path: /health, port: 8000}
```

| Field | Description |
|-------|-------------|
| `provider` | Weight transfer system. Only `ModelExpress` is supported; it is the default. |
| `roles[].name` | A role in `spec.template.roles`. Only listed roles are created in stages and receive MX configuration. |
| `roles[].initialSeedReplicas` | Replicas of this role that may be created while it has no Ready source. A positive integer or a percentage (`1%`–`100%`) of `spec.replicas × role.replicas`, rounded up. Defaults to `1`. |
| `modelExpress.engineContainers` | Names of the inference-engine containers in the entry and worker templates of in-scope roles. Only these containers receive MX environment variables. |
| `modelExpress.readyURL` | Engine endpoint that MX polls before a source publishes its weights. Injected as `MX_ARTIFACT_READY_URL`. Optional. |

If `modelExpress` is omitted, replicas are still created in stages but no environment variables are injected; configure the engine yourself.

The strategy is mutable and takes effect on the next reconcile. Changing it never triggers a rolling update: it is not part of the revision hash.

## Injected environment

For in-scope roles the controller appends a managed `modelexpress` plugin after any user plugins. It sets the following variables in the selected engine containers **only when they are not already set**:

| Variable | Value |
|----------|-------|
| `MX_MODEL_REVISION` | `<role template hash>-<WORKER_INDEX>`, so sources are shared only within one template revision and one position in the replica (entry `0`, workers `1..N`), never across weight shards |
| `MX_SERVER_ADDRESS` | The controller's `--modelexpress-server-address` |
| `MX_ARTIFACT_READY_URL` | `modelExpress.readyURL` |
| `MX_WORKER_HOST` | The Pod IP (downward API) |
| `MX_P2P_METADATA` / `MX_METADATA_PORT` / `MX_WORKER_GRPC_PORT` | `1` / `5555` / `6555` |
| `MX_NIXL_BACKEND` / `MX_INSTANT_TENSOR` / `MX_ARTIFACT_TRANSFER` | `UCX` / `0` / `1` |
| `MX_POOL_REG` / `MX_VMM_ARENA` | `1` / `1` (pooled NIXL registration and VMM arena receive buffers) |
| `MODEL_EXPRESS_LOG_LEVEL` / `NIXL_LOG_LEVEL` / `UCX_LOG_LEVEL` | `INFO` / `INFO` / `WARN` |

Override any of them in the container `env` of the template. The plugin cannot be listed in `spec.plugins`.

## Behavior

- **Gang scheduling.** With `schedulerName: volcano`, a ServingGroup's gang-required replicas of each role (`minRoleReplicas`, or all replicas by default) are created together with its PodGroup, and the seed budget is raised to at least that number. The remaining replicas are added once a source is Ready.
- **Scale-up and recovery.** New replicas join the pool of their role template. If a Ready replica of that template exists, they are created immediately.
- **Existing fleets.** Sources are identified by role template, not by how their Pods were rendered. Enabling the strategy on a fleet created without ModelExpress, or changing the controller's server address or `engineContainers`, does not throttle new replicas; they start together and ModelExpress falls back to storage until the Pods are rolled.
- **Rolling updates.** A changed role template is a new pool, so the first new-revision replicas are a seed batch; later rollout steps load from already-updated replicas. `maxUnavailable`, `maxSurge`, and `partition` keep their meaning. `updateMethod: InPlace` falls back to recreating outdated replicas while the strategy is set.
- **Observability.** `BootstrapBatchCreated` events report how many replicas of each role were created and how many Ready sources they can use. The `ModelServing` stays `Progressing` until every replica is created.
- **Disabling.** Removing `spec.bootstrapAccelerateStrategy` creates every missing replica on the next reconcile.
