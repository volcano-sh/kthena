# Bootstrap Acceleration with ModelExpress

When many `ModelServing` replicas (`ServingGroup`s) start together, every role replica loads the full checkpoint from storage at the same time, and the fleet becomes ready only as fast as that contended load. [ModelExpress (MX)](https://github.com/ai-dynamo/modelexpress) copies post-processed weights from a Ready serving replica over NIXL/RDMA, but it only helps replicas that start **after** a compatible source is serving.

`spec.bootstrapAccelerateStrategy` makes the ModelServing controller create whole `ServingGroup`s in two stages so that most of them load weights peer-to-peer without conflicting with `ServingGroup`-scoped gang scheduling, network topology, or role coordination:

1. While any in-scope role template pool of a target `ServingGroup` has no Ready replica, the controller creates at most `initialSeedReplicas` whole `ServingGroup`s. Cold roles in those seed groups find no source, load from storage, and publish MX metadata.
2. Once every in-scope role pool has at least one Ready replica, the controller creates all remaining `ServingGroup`s in full. Their engines pull weights from the Ready replicas.

All Pods run the same engine configuration; the controller only decides **when** to create whole `ServingGroup`s. Role creation inside an already-created `ServingGroup` is never gated.

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
| `initialSeedReplicas` | Number of seed `ServingGroup`s that may be created while any in-scope role pool has no Ready source. A positive integer or a percentage (`1%`–`100%`) of `spec.replicas`, rounded up. Defaults to `1`. |
| `roles` | Optional list of role names in `spec.template.roles` to include in source tracking and MX environment injection (for example, to exclude a non-engine router or tokenizer role). When omitted or empty, all roles in `spec.template.roles` are in scope. Seed admission is always whole-`ServingGroup`. |
| `modelExpress.engineContainers` | Names of the inference-engine containers in the entry and worker templates of in-scope roles. Only these containers receive MX environment variables. |
| `modelExpress.readyURL` | Engine endpoint that MX polls before a source publishes its weights. Injected as `MX_ARTIFACT_READY_URL`. Optional. |

If `modelExpress` is omitted, `ServingGroup`s are still created in stages but no environment variables are injected; configure the engine yourself.

The strategy is mutable and takes effect on the next reconcile. Changing it never triggers a rolling update: it is not part of the revision hash.

## Injected environment

For in-scope roles the controller appends a managed `modelexpress` plugin after any user plugins. It sets the following variables in the selected engine containers **only when they are not already set**:

| Variable | Value |
|----------|-------|
| `MX_MODEL_REVISION` | `<namespace>/<ModelServing name>/<role template hash>-<WORKER_INDEX>`, so sources are shared only within one ModelServing, one template revision and one position in the replica (entry `0`, workers `1..N`), never across workloads or weight shards |
| `MX_SERVER_ADDRESS` | The controller's `--modelexpress-server-address` |
| `MX_ARTIFACT_READY_URL` | `modelExpress.readyURL` |
| `MX_WORKER_HOST` | The Pod IP (downward API) |
| `POD_NAME` / `POD_UID` / `POD_NAMESPACE` | The Pod's name, UID and namespace (downward API). ModelExpress reports them with each source so that its Kubernetes metadata backend removes the source record together with the Pod |
| `MX_RDMA_NIC_PIN` | `auto`, which gives each GPU rank its own RDMA NIC. Not set when the container defines `UCX_NET_DEVICES`, because the probe would overwrite it |
| `MX_P2P_METADATA` / `MX_METADATA_PORT` / `MX_WORKER_GRPC_PORT` | `1` / `5555` / `6555` |
| `MX_NIXL_BACKEND` / `MX_INSTANT_TENSOR` / `MX_ARTIFACT_TRANSFER` | `UCX` / `0` / `1` |
| `MX_POOL_REG` / `MX_VMM_ARENA` | `1` / `1` (pooled NIXL registration and VMM arena receive buffers) |
| `MODEL_EXPRESS_LOG_LEVEL` / `NIXL_LOG_LEVEL` / `UCX_LOG_LEVEL` | `INFO` / `INFO` / `WARN` |

Override any of them in the container `env` of the template. The plugin cannot be listed in `spec.plugins`.

## Behavior

- **Gang scheduling and network topology.** With `schedulerName: volcano`, every admitted `ServingGroup` is created in full together with its `PodGroup` (`minMember`, `SubGroupPolicy`, and `NetworkTopology`). Deferred `ServingGroup`s create neither `PodGroup` nor Pods.
- **RoleRollingUpdate and RoleCoordination.** Bootstrap admission gates only whole-`ServingGroup` creation, never individual role replicas inside an existing `ServingGroup`, so `RoleRecreate`, `RoleRollingUpdate`, and `roleCoordination` (`maxSkew`, `dependencies`, and staged `PodGroup` expansion) run without conflict while still receiving MX environment injection.
- **Scale-up and recovery.** New `ServingGroup`s check their in-scope role template pools. If every pool already has a Ready replica, all new `ServingGroup`s are created immediately.
- **Existing fleets.** Sources are identified by role template, not by how their Pods were rendered. Enabling the strategy on a fleet created without ModelExpress, or changing the controller's server address or `engineContainers`, does not throttle new `ServingGroup`s; they start together and ModelExpress falls back to storage until the Pods are rolled.
- **Rolling updates.** Under `ServingGroupRollingUpdate`, a changed in-scope role template starts a cold pool, so the first new-revision `ServingGroup` batch is limited to `initialSeedReplicas`; later rollout steps load from already-updated groups at normal `maxUnavailable` / `maxSurge` pace. `updateMethod: InPlace` falls back to recreating outdated units while the strategy is set.
- **Observability.** `BootstrapBatchCreated` events report how many `ServingGroup`s were created for each pool and how many Ready sources they can use. The `ModelServing` stays `Progressing` until every `ServingGroup` is created and Ready.
- **Disabling.** Removing `spec.bootstrapAccelerateStrategy` creates every missing `ServingGroup` on the next reconcile.
