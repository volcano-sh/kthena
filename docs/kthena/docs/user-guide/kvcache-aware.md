# KV Cache Aware Plugin

The `kvcache-aware` plugin is a score plugin for the Kthena Router scheduler that routes inference requests to pods most likely to have matching KV cache entries. It uses **token-block based matching** with **Redis-based distributed coordination** to maximize cache hits and reduce redundant prefill computation.

## Overview

When multiple vLLM pods serve the same model, each pod maintains its own KV cache. Without cache-aware routing, repeated or similar prompts may be sent to pods that lack cached token blocks, causing unnecessary recomputation.

The `kvcache-aware` plugin solves this by:
1. Tokenizing the incoming prompt using the model's tokenizer.
2. Dividing the token sequence into fixed-size blocks and hashing each block.
3. Querying Redis to find which pods have cached each token block.
4. Scoring pods based on consecutive block matches from the beginning of the prompt.

Pods with more consecutive matching blocks score higher and are preferred for routing.

## Prerequisites

- **Redis**: A Redis instance accessible by both the router and the runtime sidecars. Deploy Redis using the provided [redis-standalone.yaml](../assets/examples/redis/redis-standalone.yaml) example.
- **Kthena Runtime sidecar**: Must be deployed alongside each vLLM pod. The sidecar listens to vLLM's ZMQ `kv-events` stream and writes token block hashes into Redis.
- **vLLM v1 with KV event support**: The vLLM engine must be running with `VLLM_USE_V1=1` and expose the ZMQ kv-events topic.
- **Multi-pod inference deployment**: The plugin is meaningful only when multiple pods serve the same model.

## Architecture

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

## Setup

### Step 1: Deploy Redis

Deploy Redis in the `kthena-system` namespace (where the Kthena Router runs):

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/redis/redis-standalone.yaml -n kthena-system
```

This creates a `redis-config` ConfigMap, `redis-secret` Secret, and a `redis-server` Service in the `kthena-system` namespace. The router (in the same namespace) will reference these directly, while the runtime sidecars in other namespaces can reach Redis via its cross-namespace DNS name: `redis-server.kthena-system.svc.cluster.local`.

### Step 2: Deploy vLLM pods with the Kthena Runtime sidecar

**Option A: Using ModelBooster (recommended)**

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
        replicase: 1
        pods: 1
        resources:
          limits:
            nvidia.com/gpu: "1"
```

**Option B: Using ModelServing**

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
                image: ghcr.io/yaozengzeng/runtime:latest
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
          blockSizeToHash: 128
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

**Plugin arguments:**

| Parameter          | Default | Description                                                                      |
| ------------------ | ------- | -------------------------------------------------------------------------------- |
| `blockSizeToHash`  | 128     | Number of tokens per block. Must match the vLLM block size for optimal matching. |
| `maxBlocksToMatch` | 128     | Maximum number of blocks to process per request. Limits Redis queries.           |

**Helm values:**

The `kvcache-aware` plugin is **not** enabled in the Helm chart by default. To enable it, override the router ConfigMap in your `values.yaml` to include `kvcache-aware` in both `pluginConfig` and `Score.enabled` sections as shown above.

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
- `vLLM ZMQ subscriber initialized successfully`
- `Event handlers registered successfully`

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
kubectl port-forward svc/kthena-router 8080:8080 -n <namespace>
curl -s http://localhost:8080/metrics | grep -i kvcache
```

### 5. Send Test Requests

Send the same prompt to the router multiple times. On the first request, the `kvcache-aware` score will be 0 for all pods (no cached blocks yet). On subsequent requests with the same or similar prompts, the plugin should score pods with cached blocks higher, routing to those pods preferentially.

## How It Differs from Other Plugins

| Feature                | `prefix-cache`            | `kvcache-aware`                   |
| ---------------------- | ------------------------- | --------------------------------- |
| Matching unit          | Byte-based prefix         | Token-block based                 |
| Cache data source      | Router in-memory tracking | Redis (distributed)               |
| Cross-pod coordination | No (local to router)      | Yes (via Redis)                   |
| Cache truth source     | Router heuristic          | Actual engine KV events from vLLM |
| Dependencies           | None                      | Redis + Runtime sidecar           |

- Use **`prefix-cache`** when you want lightweight, dependency-free prefix matching for simple workloads.
- Use **`kvcache-aware`** when you need accurate, distributed KV cache coordination backed by real engine cache events — particularly effective with long shared system prompts.

## Troubleshooting

| Symptom                                                  | Possible Cause                          | Resolution                                                                                        |
| -------------------------------------------------------- | --------------------------------------- | ------------------------------------------------------------------------------------------------- |
| Plugin scores are always 0                               | Redis not reachable from router         | Verify Redis connectivity and env vars (`REDIS_HOST`, `REDIS_PORT`)                               |
| No Redis keys (`matrix:kv:block:*`)                      | Runtime sidecar not receiving KV events | Check that `VLLM_USE_V1=1` is set, runtime `--engine vllm` / `--pod` / `--model` args are correct |
| Runtime log: `Failed to initialize Redis client`         | Redis not deployed or unreachable       | Deploy Redis and verify the `redis-config` ConfigMap exists in the same namespace                 |
| Runtime log: `Pod identifier or model name not provided` | Missing `--pod` or `--model` args       | Ensure the runtime sidecar has `--pod $(POD_NAME).$(NAMESPACE)` and `--model <name>`              |
| Router log: `redis client not initialized`               | Router cannot connect to Redis          | Check that Redis env vars are available to the router pod                                         |
