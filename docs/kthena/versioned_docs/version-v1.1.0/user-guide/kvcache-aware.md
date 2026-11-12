# KV Cache Aware Plugin

The `kvcache-aware` plugin is a score plugin for the Kthena Router scheduler that routes inference requests to pods most likely to have matching KV cache entries. It uses **token-block based matching** with a choice of two coordination backends: **Redis-based distributed coordination** (default) or a **direct in-memory mode** where runtime sidecars stream KV events straight into the router's memory, removing the per-request Redis round-trip.

## Overview

When multiple vLLM pods serve the same model, each pod maintains its own KV cache. Without cache-aware routing, repeated or similar prompts may be sent to pods that lack cached token blocks, causing unnecessary recomputation.

The `kvcache-aware` plugin solves this by:
1. Tokenizing the incoming prompt using the model's tokenizer.
2. Dividing the token sequence into fixed-size blocks and hashing each block.
3. Looking up which pods have cached each token block — either in Redis, or in the router's local in-memory index.
4. Scoring pods based on consecutive block matches from the beginning of the prompt.

Pods with more consecutive matching blocks score higher and are preferred for routing.

## Coordination backends

| Backend           | How block ownership reaches the router                                                 | Per-request lookup                      | Extra infrastructure |
| ----------------- | -------------------------------------------------------------------------------------- | --------------------------------------- | -------------------- |
| `redis` (default) | Runtime sidecars write standardized block hashes into Redis                            | Batched Redis pipeline query            | Redis instance       |
| `memory`          | Every router instance streams KV events from each runtime sidecar over HTTP            | Local in-memory map lookup (no network) | None                 |

### How memory mode works

Because the router may run with multiple replicas, each router instance **pulls KV events itself** from every runtime sidecar:

1. Each router replica keeps one long-lived connection to the runtime sidecar of every model-serving pod it knows (pods selected by a `ModelServer` that are Running and Ready), by calling `POST /kvcache/routers/register` on the pod IP. The response is never finished: it is the sidecar's stream of KV events (newline-delimited JSON).
2. The stream starts with the sidecar's full KV block index, so a freshly started or restarted router immediately has complete state.
3. From then on, every KV cache event (`stored` / `removed` / `cleared`) is converted to standardized block hashes and sent on the stream of **every registered router instance**. An idle stream carries a heartbeat every 10s; the router reopens a stream that stays silent for 30s.
4. At request time the plugin answers block-ownership lookups from its local in-memory index — no Redis query, no network latency on the scoring path.

The router attributes the events of a stream to the pod it connected to, so the router does not expose any endpoint for KV events. The blocks of a pod are only kept while its stream is open: when the stream ends (the pod is deleted or stops being Ready, its sidecar restarts, the connection breaks), the router drops them, and the next stream sends them again. In particular, a pod that is Ready again after a readiness probe failure gets its warm cache back as soon as the router reconnects.

Stale entries are garbage-collected in the router after 24 hours, and ownership written before the owning pod's containers last restarted is ignored, same as in Redis mode.

**Trade-offs of memory mode:**

- Scoring lookups are local memory reads, removing the Redis round-trip latency from the request path.
- No Redis deployment is required for KV cache coordination.
- Each router replica keeps its own copy of the index (memory usage scales with cached blocks × replicas).
- If a runtime sidecar restarts, its in-memory engine-hash mapping is lost; its streams end and the router drops the blocks of the pod, which are rebuilt from the new KV events of the engine.

**Trust model of memory mode:**

- The router only receives KV events on connections it opens itself, to the pod IP of a pod selected by a `ModelServer`; nothing in a stream can claim another pod's identity. Pods sharing an IP (e.g. `hostNetwork` pods on the same node) cannot be told apart, so the router does not stream from them.
- A runtime sidecar only sends KV events back on the connection of the caller of `POST /kvcache/routers/register`, so a caller cannot direct them anywhere else. A registration with the `router_id` of an open stream replaces that stream, and a sidecar serves at most 128 streams at once; a replaced or ended stream still counts until its response has exited, and a response that is not read is aborted within a second.
- Store times are capped at the time the router receives them, and each pod can hold at most `maxBlocksPerPod` blocks in the index.
- The register endpoint does not authenticate its callers, so any workload that can reach the runtime port can read the block hashes of the pod. Restrict access to it with a NetworkPolicy if untrusted workloads share the cluster.

## Prerequisites

- **Redis** (Redis backend only): A Redis instance accessible by both the router and the runtime sidecars. Deploy Redis using the provided [redis-standalone.yaml](../assets/examples/redis/redis-standalone.yaml) example. Memory mode does not need Redis.
- **Kthena Runtime sidecar**: Must be deployed alongside each vLLM pod. The sidecar listens to vLLM's ZMQ `kv-events` stream and either writes token block hashes into Redis or streams them to registered routers.
- **vLLM v1 with KV event support**: The vLLM engine must be running with `VLLM_USE_V1=1` and expose the ZMQ kv-events topic.
- **Multi-pod inference deployment**: The plugin is meaningful only when multiple pods serve the same model.

## Architecture

**Redis backend (default):**

```
                        ┌────────────────┐
                        │ Client Request │
                        └───────┬────────┘
                                │
                                ▼
                  ┌──────────────────────────┐
                  │      Kthena Router       │
                  │  (kvcache-aware plugin)  │
                  └─────┬──────────────┬─────┘
                        │              │
              route to  │              │ query block hashes
             best pod   │              │
                        │    ┌─────────▼─────────┐
                        │    │      Redis        │
                        │    └─────────▲─────────┘
                        │              │
                        │              │ write block hashes
           ┌────────────┴──────────────┴────────────┐
           │                                        │
           ▼                                        ▼
┌─────────────────────┐              ┌─────────────────────┐
│     vLLM Pod A      │              │     vLLM Pod B      │
│                     │              │                     │
│  ┌───────────────┐  │              │  ┌───────────────┐  │
│  │Runtime sidecar│──┘              │  │Runtime sidecar│──┘
│  │(ZMQ listener) │                 │  │(ZMQ listener) │
│  └───────────────┘  │              │  └───────────────┘  │
│  ┌───────────────┐  │              │  ┌───────────────┐  │
│  │  vLLM Engine  │  │              │  │  vLLM Engine  │  │
│  │  (KV Cache)   │  │              │  │  (KV Cache)   │  │
│  └───────────────┘  │              │  └───────────────┘  │
└─────────────────────┘              └─────────────────────┘
```

- The **Runtime sidecar** subscribes to vLLM ZMQ kv-events (`VLLM_BLOCK_STORED`, `VLLM_BLOCK_REMOVED`, `VLLM_ALL_BLOCKS_CLEARED`) and writes standardized token block hashes into Redis.
- The **Router's `kvcache-aware` plugin** queries Redis at request time to find pods with matching blocks and scores them.

**Memory backend:**

```
                     ┌────────────────┐
                     │ Client Request │
                     └───────┬────────┘
                             │
                             ▼
     ┌───────────────────────┐   ┌───────────────────────┐
     │   Kthena Router #1    │   │   Kthena Router #2    │
     │  ┌─────────────────┐  │   │  ┌─────────────────┐  │
     │  │ in-memory index │  │   │  │ in-memory index │  │
     │  └─────────────────┘  │   │  └─────────────────┘  │
     └─────┬───────────▲─────┘   └─────┬───────────▲─────┘
           │           │               │           │
  register │           │ KV event      │ register  │ KV event
           │           │ stream        │           │ stream
           ▼           │               ▼           │
     ┌─────────────────┴───────────────────────────┴─────┐
     │                    vLLM Pods                      │
     │  ┌─────────────────┐                              │
     │  │ Runtime sidecar │ ◀── ZMQ kv-events            │
     │  └─────────────────┘                              │
     │  ┌─────────────────┐                              │
     │  │   vLLM Engine   │                              │
     │  └─────────────────┘                              │
     └───────────────────────────────────────────────────┘
```

- Each **router replica** registers with every runtime sidecar; the response of the registration is the sidecar's KV event stream.
- The **Runtime sidecar** (started with `KV_EVENT_SYNC_MODE=memory`) streams standardized block hashes to every registered router; each stream starts with the full block index.

## Setup

### Step 1: Deploy Redis (Redis backend only)

> Skip this step when using the in-memory backend (`indexMode: memory`).

Deploy Redis in the `kthena-system` namespace (where the Kthena Router runs):

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/redis/redis-standalone.yaml -n kthena-system
```

This creates a `redis-config` ConfigMap, `redis-secret` Secret, and a `redis-server` Service in the `kthena-system` namespace. The router (in the same namespace) will reference these directly, while the runtime sidecars in other namespaces can reach Redis via its cross-namespace DNS name: `redis-server.kthena-system.svc.cluster.local`.

### Step 2: Deploy vLLM pods with the Kthena Runtime sidecar

For new deployments, use the ModelServing example in Option B below and configure `ModelServer` and `ModelRoute` for routing.

**Option A: Using ModelBooster (deprecated)**

:::warning Deprecated example
ModelBooster is deprecated in v1.1; use ModelServing, ModelServer, and ModelRoute. Removal is no earlier than v1.5. This example is retained for existing users; see the [deprecation details](./model-deployment.md#modelbooster-deprecation).
:::

When using ModelBooster, the runtime sidecar is automatically injected with the correct Redis environment variables. No extra configuration is needed:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelBooster
metadata:
  name: deepseek-r1-7b
spec:
  name: deepseek-r1-distill-qwen-7b
  owner: example
  backend:
    name: "deepseek-r1-7b-server"
    type: "vLLM"
    modelURI: s3://models/deepseek-ai/DeepSeek-R1-Distill-Qwen-7B
    cacheURI: hostpath:///cache/
    envFrom:
      - secretRef:
          name: your-secrets
    env:
      - name: "VLLM_USE_V1"
        value: "1"
    minReplicas: 3
    maxReplicas: 3
    workers:
      - type: server
        image: vllm/vllm-openai:latest
        replicas: 1
        pods: 1
        resources:
          limits:
            nvidia.com/gpu: "1"
```

**Option B: Using ModelServing (recommended)**

A complete ModelServing example with KV cache awareness is provided at [gpu-kvcache-aware.yaml](../assets/examples/model-serving/gpu-kvcache-aware.yaml). This example deploys a vLLM server with the Kthena Runtime sidecar pre-configured for Redis-based KV cache coordination.

> **Note:** This example assumes Redis is deployed in the `kthena-system` namespace (Step 1). The runtime sidecar connects to Redis via `redis-server.kthena-system.svc.cluster.local`.

Apply it directly:

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/model-serving/gpu-kvcache-aware.yaml
```

The key parts of this example:

- The **vLLM server** container enables KV cache events with `--kv-events-config '{"enable_kv_cache_events":true,"topic":"kv-events"}'` and sets `VLLM_USE_V1=1`.
- The **Runtime sidecar** container connects to Redis at `redis-server.kthena-system.svc.cluster.local:6379` and listens to vLLM's ZMQ kv-events stream.

Below is the full manifest for reference:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: vllm-qwen-06b
  namespace: default
spec:
  schedulerName: volcano
  replicas: 1
  recoveryPolicy: ServingGroupRecreate
  template:
    restartGracePeriodSeconds: 60
    roles:
      - name: server
        replicas: 1
        entryTemplate:
          spec:
            initContainers:
              - name: downloader
                imagePullPolicy: IfNotPresent
                image: ghcr.io/volcano-sh/downloader:latest
                args:
                  - --source
                  - Qwen/Qwen3-0.6B
                  - --output-dir
                  - /models/Qwen3-0.6B/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: server
                image: vllm/vllm-openai:latest
                command: [ "sh", "-c" ]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-events-config '{"enable_kv_cache_events":true,"topic":"kv-events"}'
                env:
                  - name: VLLM_USE_V1
                    value: "1"
                  - name: VLLM_WORKER_MULTIPROC_METHOD
                    value: spawn
                  - name: VLLM_ENABLE_V1_MULTIPROCESSING
                    value: "0"
                  - name: GLOO_SOCKET_IFNAME
                    value: eth0
                  - name: NCCL_SOCKET_IFNAME
                    value: eth0
                  - name: NCCL_IB_DISABLE
                    value: "0"
                  - name: NCCL_IB_GID_INDEX
                    value: "7"
                  - name: NCCL_DEBUG
                    value: "INFO"
                volumeMounts:
                  - name: models
                    mountPath: /models
                    readOnly: true
                  - name: shared-mem
                    mountPath: /dev/shm
                resources:
                  limits:
                    nvidia.com/gpu: 1
                readinessProbe:
                  initialDelaySeconds: 5
                  periodSeconds: 5
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
                livenessProbe:
                  initialDelaySeconds: 900
                  periodSeconds: 5
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
              - name: runtime
                image: ghcr.io/volcano-sh/runtime:latest
                imagePullPolicy: Always
                args:
                  - --port
                  - "8900"
                  - --engine
                  - vllm
                  - --engine-base-url
                  - http://localhost:8000
                  - --engine-metrics-path
                  - /metrics
                  - --pod
                  - $(POD_NAME).$(NAMESPACE)
                  - --model
                  - Qwen/Qwen3-0.6B
                env:
                  - name: POD_NAME
                    valueFrom:
                      fieldRef:
                        fieldPath: metadata.name
                  - name: NAMESPACE
                    valueFrom:
                      fieldRef:
                        fieldPath: metadata.namespace
                  - name: VLLM_USE_V1
                    value: "1"
                  - name: REDIS_HOST
                    value: "redis-server.kthena-system.svc.cluster.local"
                  - name: REDIS_PORT
                    value: "6379"
                ports:
                  - containerPort: 8900
                readinessProbe:
                  httpGet:
                    path: /health
                    port: 8900
                  initialDelaySeconds: 5
                  periodSeconds: 10
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
```

### Step 3: Configure the Router

Create or update the router ConfigMap to enable the `kvcache-aware` score plugin:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kthena-router-config
  namespace: <namespace>
data:
  routerConfiguration: |-
    scheduler:
      pluginConfig:
      - name: least-request
        args:
          maxWaitingRequests: 10
      - name: kvcache-aware
        args:
          blockSizeToHash: 16
          maxBlocksToMatch: 128
      plugins:
        Filter:
          enabled:
            - least-request
        Score:
          enabled:
            - name: least-request
              weight: 1
            - name: kvcache-aware
              weight: 1
```

:::note
Always enable `kvcache-aware` together with at least one other score plugin (e.g. `least-request`). The plugin returns no scores when there are no cached blocks yet (cold start) or when tokenization fails. Pods it does not score stay candidates with a score of 0, so if it were the only score plugin, those requests would go to an arbitrary pod instead of the least loaded one.
:::

**Plugin arguments:**

| Parameter                     | Default  | Description                                                                                                                            |
| ----------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `blockSizeToHash`             | 16       | Number of tokens per block. Must match the vLLM block size for optimal matching.                                                       |
| `maxBlocksToMatch`            | 128      | Maximum number of blocks to process per request. Limits lookups.                                                                       |
| `vllmTokenizerPort`           | 8000     | Port used to fetch the tokenizer from vLLM pods.                                                                                       |
| `sglangTokenizerPort`         | 30000    | Port used to fetch the tokenizer from SGLang pods.                                                                                     |
| `tokenizerService`            | disabled | Use the dedicated [Tokenizer Service](./tokenizer-service.md) instead of engine pods for prompt tokenization, with automatic fallback. |
| `indexMode`                   | `redis`  | Coordination backend: `redis` or `memory`.                                                                                             |
| `runtimePort`                 | 9000     | Memory mode only: runtime sidecar port the router streams KV events from (match the sidecar `--port`).                                 |
| `maxBlocksPerPod`             | 1048576  | Memory mode only: maximum number of blocks one pod can hold in the router's index across all models; the oldest are evicted first.     |

> **Tip:** By default the plugin tokenizes prompts by calling the `/tokenize` endpoint of a backend engine pod, which adds load and latency to GPU pods. Consider enabling the [Tokenizer Service](./tokenizer-service.md) to offload tokenization to a dedicated, GPU-free component.

**Helm values:**

The `kvcache-aware` plugin is **not** enabled in the Helm chart by default. To enable it, override the router ConfigMap in your `values.yaml` to include `kvcache-aware` in both `pluginConfig` and `Score.enabled` sections as shown above.

### Using the in-memory backend (optional)

To remove the per-request Redis round-trip, switch both sides to memory mode:

**1. Runtime sidecar**: set `KV_EVENT_SYNC_MODE=memory` in the runtime sidecar container's environment (instead of the `REDIS_HOST`/`REDIS_PORT` variables):

```yaml
- name: runtime
  image: ghcr.io/volcano-sh/runtime:latest
  args: [ ... ]
  env:
    - name: KV_EVENT_SYNC_MODE
      value: "memory"
    # POD_NAME / NAMESPACE etc. unchanged
```

**2. Router ConfigMap**: set `indexMode: memory` on the plugin:

```yaml
scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      indexMode: memory
      blockSizeToHash: 16
      maxBlocksToMatch: 128
      runtimePort: 8900   # must match the runtime sidecar --port
  plugins:
    Score:
      enabled:
        - name: least-request
          weight: 1
        - name: kvcache-aware
          weight: 1
```

**3. Network**: router pods must be able to reach model pods on `runtimePort` by pod IP. The router identifies itself to sidecars by `POD_NAMESPACE`/`POD_NAME` (injected by the Helm chart), falling back to its hostname.

With multiple router replicas, every replica registers itself independently and each receives its own KV event stream, so scheduling decisions stay consistent across instances.

To verify memory mode is active:

```bash
# Router side
kubectl logs deployment/kthena-router -n <namespace> | grep -i "memory mode active"
# Sidecar side
kubectl logs <vllm-pod> -c runtime -n <namespace> | grep -iE "memory|router registered"
```

Expected messages:
- Router: `KVCacheAware: memory mode active, runtimePort=..., ...`
- Sidecar: `KV event sync mode: memory (stream to registered routers)` and `Router registered: id=<router-namespace>/<router-pod>`

### Step 4: Restart the Router

The router does not support hot reload of ConfigMap changes, so restart the router pod:

```bash
kubectl rollout restart deployment/kthena-router -n <namespace>
```

## Verifying the Plugin is Active

After deployment, use the following steps to confirm the `kvcache-aware` plugin is working.

### 1. Check Router Startup Logs

When the router starts and loads the plugin, the logs will show the `kvcache-aware` plugin being registered. Look for log entries that reference the plugin initialization:

```bash
kubectl logs deployment/kthena-router -n <namespace> | grep -i "kvcache"
```

### 2. Check Runtime Sidecar Logs

The runtime sidecar should show successful Redis connection and ZMQ subscriber initialization:

```bash
kubectl logs <vllm-pod> -c runtime -n <namespace> | grep -iE "redis|zmq|kv"
```

Expected messages:
- `Redis client initialized successfully`
- `vLLM ZMQ subscriber initialized successfully` (or `SGLang ZMQ subscriber initialized successfully`)
- `vLLM KV-cache event handler registered` (or `SGLang KV-cache event handler registered`)

### 3. Inspect Redis Keys

After some inference requests have been processed, the runtime sidecar writes token block hashes into Redis. Verify that keys exist:

```bash
# Port-forward to Redis
kubectl port-forward svc/redis-server 6379:6379 -n kthena-system

# In another terminal, scan for block keys
redis-cli KEYS "matrix:kv:block:*"
```

Each key follows the format `matrix:kv:block:{model}@{hash}` and its hash fields are the pod identifiers that have cached that block:

```bash
# Inspect a specific key
redis-cli HGETALL "matrix:kv:block:<model-name>@<hash>"
```

The output shows pod identifiers (e.g., `pod-name.namespace`) as field names and timestamps as values.

### 4. Check Router Metrics

The router exposes scheduler plugin metrics at the `/metrics` endpoint. You can check for score plugin activity:

```bash
kubectl port-forward svc/kthena-router-metrics 9090:9090 -n <namespace>
curl -s http://localhost:9090/metrics | grep -i kvcache
```

### 5. Send Test Requests

Send the same prompt to the router multiple times. On the first request, the `kvcache-aware` score will be 0 for all pods (no cached blocks yet). On subsequent requests with the same or similar prompts, the plugin should score pods with cached blocks higher, routing to those pods preferentially.

## How It Differs from Other Plugins

| Feature                | `prefix-cache`            | `kvcache-aware` (redis)           | `kvcache-aware` (memory)            |
| ---------------------- | ------------------------- | --------------------------------- | ----------------------------------- |
| Matching unit          | Byte-based prefix         | Token-block based                 | Token-block based                   |
| Cache data source      | Router in-memory tracking | Redis (distributed)               | Router in-memory index (streamed)   |
| Cross-pod coordination | No (local to router)      | Yes (via Redis)                   | Yes (each router streams from pods) |
| Cache truth source     | Router heuristic          | Actual engine KV events from vLLM | Actual engine KV events from vLLM   |
| Dependencies           | None                      | Redis + Runtime sidecar           | Runtime sidecar (no Redis)          |

- Use **`prefix-cache`** when you want lightweight, dependency-free prefix matching for simple workloads.
- Use **`kvcache-aware`** when you need accurate, distributed KV cache coordination backed by real engine cache events — particularly effective with long shared system prompts.

## Troubleshooting

Common to both backends:

| Symptom                                                  | Possible Cause                          | Resolution                                                                                        |
| -------------------------------------------------------- | --------------------------------------- | ------------------------------------------------------------------------------------------------- |
| No KV events in the runtime sidecar                      | Runtime sidecar not receiving KV events | Check that `VLLM_USE_V1=1` is set, runtime `--engine vllm` / `--pod` / `--model` args are correct |
| Runtime log: `Pod identifier or model name not provided` | Missing `--pod` or `--model` args       | Ensure the runtime sidecar has `--pod $(POD_NAME).$(NAMESPACE)` and `--model <name>`              |

Redis backend (`indexMode: redis`):

| Symptom                                          | Possible Cause                    | Resolution                                                                        |
| ------------------------------------------------ | --------------------------------- | --------------------------------------------------------------------------------- |
| Plugin scores are always 0                       | Redis not reachable from router   | Verify Redis connectivity and env vars (`REDIS_HOST`, `REDIS_PORT`)               |
| No Redis keys (`matrix:kv:block:*`)              | Sidecar cannot write to Redis     | Verify sidecar Redis env vars and network access to the Redis service             |
| Runtime log: `Failed to initialize Redis client` | Redis not deployed or unreachable | Deploy Redis and verify the `redis-config` ConfigMap exists in the same namespace |
| Router log: `redis client not initialized`       | Router cannot connect to Redis    | Check that Redis env vars are available to the router pod                         |

Memory backend (`indexMode: memory`):

| Symptom                                                                    | Possible Cause                                                                                                                      | Resolution                                                                                                                         |
| -------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| Plugin scores are always 0                                                 | The router cannot reach the runtime sidecars                                                                                        | Verify `runtimePort` matches the sidecar `--port` and model pods are reachable on it by pod IP; check router logs at `-v=4`        |
| Runtime log: `Runtime is not in memory KV event sync mode`                 | Sidecar not started in memory mode                                                                                                  | Set `KV_EVENT_SYNC_MODE=memory` on the runtime sidecar                                                                             |
| Runtime log: `KV event queue of router ... is full; ending its stream`     | The router does not read its stream fast enough                                                                                     | The router reconnects and receives the full block index again; check router CPU load                                              |
| Router log: `reached the limit of ... indexed blocks` (`-v=2`)             | The pod holds more blocks than `maxBlocksPerPod`, so its oldest blocks are evicted                                                  | Raise `maxBlocksPerPod` if the engine KV cache legitimately holds more blocks                                                      |
| Scores stale after pod deletion                                            | Missed removal events                                                                                                               | Blocks of deleted or not Ready pods are dropped as soon as the router notices; other stale entries are garbage-collected after 24 hours |
