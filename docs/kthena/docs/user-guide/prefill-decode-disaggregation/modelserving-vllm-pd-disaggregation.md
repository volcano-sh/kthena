# Prefill-Decode Disaggregation with ModelServing (vLLM, NIXL & LMCache)

This page describes how to deploy prefill-decode disaggregated inference on a GPU
cluster by manually creating the three underlying Kthena resources
(`ModelServing`, `ModelServer`, `ModelRoute`), using either KV connector supported
by [vLLM](https://github.com/vllm-project/vllm):

- **`nixl`** — in-band, point-to-point KV transfer between prefill and decode.
- **`lmcache`** — KV reuse through a shared store (e.g. Redis).

This is the lower-level counterpart to the single-resource
[ModelBooster approach](./modelbooster-vllm-pd-disaggregation.md): it gives you
full control over the Pod templates, roles, and networking, at the cost of writing
more YAML.

## Deployment overview

The ModelServing approach requires you to create the following resources, in order:

1. **ModelServing** — manages the Pods for the `prefill` and `decode` roles.
2. **ModelServer** — configures the networking layer and PD-group routing,
   including the KV connector type.
3. **ModelRoute** — routes incoming requests by model name to the ModelServer.

## How the KV connector type is selected

Unlike the ModelBooster approach (which derives the connector type automatically),
with ModelServing you set `spec.kvConnector.type` **explicitly** on the
`ModelServer`. This value must match the `kv_connector` declared in each worker's
`--kv-transfer-config`:

| `kv_connector` in `kv-transfer-config` | `ModelServer` `kvConnector.type` |
| -------------------------------------- | -------------------------------- |
| `NixlConnector`                        | `nixl`                           |
| `LMCacheConnectorV1`                   | `lmcache`                        |

:::warning
If you omit `kvConnector.type`, the router falls back to a generic HTTP connector
that simply forwards the prefill request and then the decode request **without**
injecting the NIXL `kv_transfer_params` handshake. In that case a `NixlConnector`
deployment still returns responses, but decode recomputes the prefill KV instead of
receiving it — so no real KV transfer happens. Always set `kvConnector.type` to
match your connector.
:::

The two connectors reuse KV cache in fundamentally different ways:

| Connector | KV transfer mechanism                        | Shared backend required | Router connector                                                                  |
| --------- | -------------------------------------------- | ----------------------- | --------------------------------------------------------------------------------- |
| `nixl`    | In-band `kv_transfer_params` handshake (P2P) | No                      | NIXL connector (parses prefill response, forwards `kv_transfer_params` to decode) |
| `lmcache` | Shared store (Redis / lmcache-server)        | **Yes**                 | Generic HTTP connector (does not forward `kv_transfer_params`)                    |

## Prerequisites

- Kubernetes cluster with Kthena installed
- The [Volcano scheduler](https://github.com/volcano-sh/volcano) installed —
  Kthena schedules ModelServing Pods with `schedulerName: volcano` by default, so
  without Volcano the Pods stay `Pending`.
- NVIDIA GPU-enabled nodes with the appropriate device plugin configured (each
  option below uses two GPUs: one for prefill, one for decode)
- The target model (e.g. `Qwen/Qwen3-0.6B`) accessible from the cluster (the
  deployments use a downloader init container to pull the model)
- A vLLM image whose connector Python dependencies are fully installed:
  - **NIXL:** `ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2`
    already bundles a compatible NIXL (v0.4.1).
  - **LMCache:** use an image where LMCache is fully bundled, e.g. the official
    `lmcache/vllm-openai:latest`. The `ghcr.io/volcano-sh/vllm-openai:...-lmcache-0.3.2`
    image ships the LMCache package but not all of its runtime dependencies
    (`nvtx`, `sortedcontainers`, `redis`, ...), so loading `LMCacheConnectorV1`
    from it crash-loops with `ModuleNotFoundError`.
- **For the `lmcache` option only:** a standalone Redis instance reachable from the
  cluster to act as the shared LMCache backend

## Option A: NIXL connector (in-band P2P)

The `nixl` connector transfers KV cache directly between the prefill and decode
Pods via a `kv_transfer_params` handshake, so **no shared backend is required**.

### 1. ModelServing (NIXL)

The `ModelServing` resource defines two roles: `prefill` and `decode`. Each role
runs a vLLM server with `NixlConnector` as the KV-transfer backend (prefill as
`kv_producer`, decode as `kv_consumer`). A downloader init container fetches the
model weights before the server starts.

```sh
kubectl apply -f - <<'EOF'
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
      - name: prefill
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
              - name: prefill
                image: ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2
                command: ["sh", "-c"]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_producer"}'
                env:
                  - name: PYTHONHASHSEED
                    value: "1047"
                  - name: VLLM_NIXL_SIDE_CHANNEL_HOST
                    value: "0.0.0.0"
                  - name: VLLM_NIXL_SIDE_CHANNEL_PORT
                    value: "5558"
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
                  - name: UCX_TLS
                    value: ^gga
                volumeMounts:
                  - name: models
                    mountPath: /models
                    readOnly: true
                  - name: shared-mem
                    mountPath: /dev/shm
                resources:
                  limits:
                    nvidia.com/gpu: 1
                securityContext:
                  capabilities:
                    add:
                      - IPC_LOCK
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
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
      - name: decode
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
              - name: decode
                image: ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2
                command: ["sh", "-c"]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_consumer"}'
                env:
                  - name: PYTHONHASHSEED
                    value: "1047"
                  - name: VLLM_NIXL_SIDE_CHANNEL_HOST
                    value: "0.0.0.0"
                  - name: VLLM_NIXL_SIDE_CHANNEL_PORT
                    value: "5558"
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
                  - name: UCX_TLS
                    value: ^gga
                volumeMounts:
                  - name: models
                    mountPath: /models
                    readOnly: true
                  - name: shared-mem
                    mountPath: /dev/shm
                resources:
                  limits:
                    nvidia.com/gpu: 1
                securityContext:
                  capabilities:
                    add:
                      - IPC_LOCK
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
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
EOF
```

### 2. ModelServer (NIXL)

The `ModelServer` configures the networking layer. `pdGroup` tells Kthena how to
identify prefill and decode Pods, and `kvConnector.type: nixl` selects the NIXL
router connector that performs the `kv_transfer_params` handshake.

```sh
kubectl apply -f - <<'EOF'
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: vllm-qwen-06b
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      modelserving.volcano.sh/name: vllm-qwen-06b
    pdGroup:
      groupKey: "modelserving.volcano.sh/group-name"
      prefillLabels:
        modelserving.volcano.sh/role: prefill
      decodeLabels:
        modelserving.volcano.sh/role: decode
  workloadPort:
    port: 8000
  model: "Qwen/Qwen3-0.6B"
  inferenceEngine: "vLLM"
  kvConnector:
    type: nixl
  trafficPolicy:
    timeout: 10s
EOF
```

### 3. ModelRoute (NIXL)

The `ModelRoute` routes incoming requests by model name to the ModelServer.

```sh
kubectl apply -f - <<'EOF'
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: vllm-qwen-06b
  namespace: default
spec:
  modelName: "Qwen/Qwen3-0.6B"
  rules:
    - name: "default"
      targetModels:
        - modelServerName: "vllm-qwen-06b"
EOF
```

### 4. Verify the NIXL deployment

Confirm the connector type and that both Pods are running:

```sh
kubectl get modelserver vllm-qwen-06b -o jsonpath='{.spec.kvConnector.type}{"\n"}'
# nixl

kubectl get pod -owide -l modelserving.volcano.sh/name=vllm-qwen-06b
```

Expected output:

```
NAME                          READY   STATUS    RESTARTS   AGE   IP         NODE
vllm-qwen-06b-0-decode-0-0    1/1     Running   0          5m    <pod-ip>   <node>
vllm-qwen-06b-0-prefill-0-0   1/1     Running   0          5m    <pod-ip>   <node>
```

Send a chat completion request through the Kthena router:

```sh
curl -X POST http://<ROUTER_IP>:80/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"Hello"}],"max_tokens":20}'
```

You can confirm the NIXL handshake actually fired by checking the prefill Pod log
for the injected `kv_transfer_params` and the forced single-token prefill:

```sh
kubectl logs vllm-qwen-06b-0-prefill-0-0 | grep -i kv_transfer_params
```

```
... extra_args={'kv_transfer_params': {'do_remote_decode': True, 'do_remote_prefill': False}} ... max_tokens=1 ...
```

The `do_remote_decode: True` argument (injected by the router's NIXL connector)
and `max_tokens=1` on the prefill side confirm that KV is produced on the prefill
Pod for transfer to decode, rather than being recomputed.

:::warning
NIXL performs GPU-to-GPU KV transfer over its side channel (UCX). This requires a
cluster network/transport that NIXL can use between the prefill and decode Pods
(e.g. RDMA/InfiniBand, or a correctly configured `UCX_TLS`). On environments
without a suitable transport, the Pods start and requests are routed, but the
decode engine can fail during the KV read (`nixl_connector.py` `_read_blocks`).
Tune `UCX_TLS`, `NCCL_IB_DISABLE`, and `NCCL_IB_GID_INDEX` to match your network,
or use the `lmcache` shared-store option, which does not need a P2P transport.
:::

## Option B: LMCache connector (shared store)

The `lmcache` connector reuses KV cache **below** the request layer: the prefill
worker writes KV to a shared store and the decode worker reads it back. This
requires a shared backend (here Redis) and a **consistent hash seed** across Pods.

### 1. Deploy a shared Redis backend

```sh
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/redis/redis-standalone.yaml
```

This creates a `redis-server` Service. The `ModelServing` below points
`LMCACHE_REMOTE_URL` at `redis://redis-server.default.svc.cluster.local:6379` —
update the host/namespace if you deploy Redis elsewhere.

### 2. ModelServing (LMCache)

Both roles run `LMCacheConnectorV1` (prefill `kv_producer`, decode `kv_consumer`)
and share the same Redis backend.

:::warning
`PYTHONHASHSEED` **must be set to the same value on both the prefill and decode
workers**. LMCache hashes token blocks to compute the shared-store keys; if the
two Pods use different hash seeds, the keys don't match and the decode worker
cannot find the KV the prefill worker stored (`LMCache hit tokens: 0`). The decode
log warns about this: `Centralized cache sharing detected but PYTHONHASHSEED not
set`.
:::

```sh
kubectl apply -f - <<'EOF'
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: vllm-qwen-lmcache
  namespace: default
spec:
  schedulerName: volcano
  replicas: 1
  recoveryPolicy: ServingGroupRecreate
  template:
    restartGracePeriodSeconds: 60
    roles:
      - name: prefill
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
              - name: prefill
                image: lmcache/vllm-openai:latest
                command: ["sh", "-c"]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-transfer-config '{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_producer","kv_connector_extra_config":{"discard_partial_chunks":true,"lmcache_rpc_port":"10086"}}'
                env:
                  - name: PYTHONHASHSEED
                    value: "0"
                  - name: LMCACHE_CHUNK_SIZE
                    value: "256"
                  - name: LMCACHE_LOCAL_CPU
                    value: "True"
                  - name: LMCACHE_MAX_LOCAL_CPU_SIZE
                    value: "1"
                  - name: LMCACHE_REMOTE_URL
                    value: "redis://redis-server.default.svc.cluster.local:6379"
                  - name: LMCACHE_REMOTE_SERDE
                    value: "naive"
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
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
      - name: decode
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
              - name: decode
                image: lmcache/vllm-openai:latest
                command: ["sh", "-c"]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-transfer-config '{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_consumer","kv_connector_extra_config":{"discard_partial_chunks":true,"lmcache_rpc_port":"10086"}}'
                env:
                  - name: PYTHONHASHSEED
                    value: "0"
                  - name: LMCACHE_CHUNK_SIZE
                    value: "256"
                  - name: LMCACHE_LOCAL_CPU
                    value: "True"
                  - name: LMCACHE_MAX_LOCAL_CPU_SIZE
                    value: "1"
                  - name: LMCACHE_REMOTE_URL
                    value: "redis://redis-server.default.svc.cluster.local:6379"
                  - name: LMCACHE_REMOTE_SERDE
                    value: "naive"
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
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
EOF
```

The LMCache-specific env vars (applied to both workers):

- `LMCACHE_REMOTE_URL` — the shared Redis backend used to exchange KV cache.
- `LMCACHE_REMOTE_SERDE` — serialization format for the remote backend (`naive`).
- `LMCACHE_CHUNK_SIZE` — LMCache chunk size. Prompts shorter than this never reach
  the shared backend, so test with prompts **longer** than this value.
- `LMCACHE_LOCAL_CPU` / `LMCACHE_MAX_LOCAL_CPU_SIZE` — LMCache hard-requires a
  `LocalCPUBackend`. Keep `LOCAL_CPU=True` with a small size even when relying on
  the remote backend; `LOCAL_CPU=False` with `MAX_LOCAL_CPU_SIZE=0` triggers
  `KeyError: 'LocalCPUBackend'`.

### 3. ModelServer and ModelRoute (LMCache)

The `ModelServer` is identical to the NIXL one except `kvConnector.type: lmcache`.

```sh
kubectl apply -f - <<'EOF'
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: vllm-qwen-lmcache
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      modelserving.volcano.sh/name: vllm-qwen-lmcache
    pdGroup:
      groupKey: "modelserving.volcano.sh/group-name"
      prefillLabels:
        modelserving.volcano.sh/role: prefill
      decodeLabels:
        modelserving.volcano.sh/role: decode
  workloadPort:
    port: 8000
  model: "Qwen/Qwen3-0.6B"
  inferenceEngine: "vLLM"
  kvConnector:
    type: lmcache
  trafficPolicy:
    timeout: 30s
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: vllm-qwen-lmcache
  namespace: default
spec:
  modelName: "Qwen/Qwen3-0.6B"
  rules:
    - name: "default"
      targetModels:
        - modelServerName: "vllm-qwen-lmcache"
EOF
```

### 4. Verify the LMCache deployment

Confirm the connector type and Pod status:

```sh
kubectl get modelserver vllm-qwen-lmcache -o jsonpath='{.spec.kvConnector.type}{"\n"}'
# lmcache

kubectl get pod -owide -l modelserving.volcano.sh/name=vllm-qwen-lmcache
```

#### Confirm real cross-pod KV reuse

Because the `lmcache` connector reuses KV through the shared backend rather than
in-band `kv_transfer_params`, verify reuse by inspecting Redis and the LMCache logs
rather than the router logs. Use a prompt **longer** than `LMCACHE_CHUNK_SIZE`
tokens so KV actually reaches Redis.

1. Flush Redis so you start from a clean state:

   ```sh
   kubectl exec deploy/redis-server -- redis-cli FLUSHALL
   ```

2. Send a request with a long prompt through the router:

   ```sh
   curl -X POST http://<ROUTER_IP>:80/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"<long prompt of 300+ tokens>"}],"max_tokens":20}'
   ```

3. Confirm Redis is now populated (the prefill worker stored KV):

   ```sh
   kubectl exec deploy/redis-server -- redis-cli DBSIZE
   # (integer) 4
   ```

4. Check that the prefill worker **stored** and the decode worker **retrieved** KV
   for the same request id:

   ```sh
   kubectl logs vllm-qwen-lmcache-0-prefill-0-0 | grep -iE "Stored"
   kubectl logs vllm-qwen-lmcache-0-decode-0-0  | grep -iE "hit tokens|Retrieved"
   ```

   Expected (note the matching request id and non-zero hit/retrieve on decode):

   ```
   # prefill
   [req_id=chatcmpl-8be05125-...] Stored 512 out of total 512 tokens. ...
   # decode
   Reqid: chatcmpl-8be05125-..., ... LMCache hit tokens: 256, need to load: 256
   [req_id=chatcmpl-8be05125-...] Retrieved 256 out of 256 required tokens ...
   ```

A non-zero **LMCache hit tokens** / **Retrieved** count on the decode side, for the
same request id the prefill worker just stored, confirms that KV produced by
prefill was reused by decode via the shared Redis backend.

## Troubleshooting

- **Pods stuck in `Pending` with no scheduling events**: The Volcano scheduler is
  not installed. Kthena schedules ModelServing Pods with `schedulerName: volcano`;
  install Volcano or the Pods never schedule.
- **NIXL deployment returns responses but performs no KV transfer**: The
  `ModelServer` is missing `kvConnector.type: nixl`, so the router uses the generic
  HTTP connector and never injects `do_remote_decode`. Set the connector type
  explicitly.
- **`LMCache hit tokens: 0` on the decode worker even with a shared backend**: The
  prefill and decode Pods are using different `PYTHONHASHSEED` values (or it is
  unset), so their block hashes don't match. Set the same `PYTHONHASHSEED` on both
  workers.
- **`ModuleNotFoundError` (`nvtx`, `sortedcontainers`, `redis`, ...) with
  `lmcache`**: The image ships LMCache but not all of its runtime dependencies. Use
  an image where LMCache is fully installed, e.g. `lmcache/vllm-openai:latest`.
- **`KeyError: 'LocalCPUBackend'`**: Set `LMCACHE_LOCAL_CPU=True` with a small
  `LMCACHE_MAX_LOCAL_CPU_SIZE` (e.g. `1`) even when relying on the remote backend.
- **`LMCache hit tokens: 0` / KV never reaches Redis**: The prompt is shorter than
  `LMCACHE_CHUNK_SIZE` (with `discard_partial_chunks: true`, partial chunks are
  never stored), or `LMCACHE_REMOTE_URL` is unreachable. Use a longer prompt and
  verify the Redis Service address.
- **NIXL decode errors during the KV read**: Ensure `VLLM_NIXL_SIDE_CHANNEL_HOST`
  and `VLLM_NIXL_SIDE_CHANNEL_PORT` are set and the `NCCL_IB_*` / `UCX_TLS` values
  match your cluster network and transport.

## See also

- [Prefill-Decode Disaggregation with ModelBooster (vLLM, LMCache & NIXL)](./modelbooster-vllm-pd-disaggregation.md)
  — the single-resource approach that generates the `ModelServer` and `ModelRoute`
  automatically.
