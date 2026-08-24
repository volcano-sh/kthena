# Prefill-Decode Disaggregation with ModelBooster (vLLM, LMCache & NIXL)

This page describes how to deploy prefill-decode disaggregated inference with a
single `ModelBooster` resource on a GPU cluster, using either KV connector
supported by [vLLM](https://github.com/vllm-project/vllm):

- **`lmcache`** — KV reuse through a shared store (e.g. Redis).
- **`nixl`** — in-band, point-to-point KV transfer between prefill and decode.

## How the KV connector type is selected

Kthena chooses the `ModelServer`'s `spec.kvConnector.type` purely from the
`kv_connector` name inside each worker's `kv-transfer-config` — the image name has
**no** effect. An image built with `...-nixl-...-lmcache-...` can run either
connector; what matters is the JSON:

| `kv_connector` in `kv-transfer-config`      | Generated `kvConnector.type` |
| ------------------------------------------- | ---------------------------- |
| `LMCacheConnectorV1`                        | `lmcache`                    |
| `NixlConnector`                             | `nixl`                       |
| `MooncakeConnector` / `MooncakeConnectorV1` | `mooncake`                   |

The two connectors reuse KV cache in fundamentally different ways:

| Connector | KV transfer mechanism                           | Shared backend required | Router connector                                                                  |
| --------- | ----------------------------------------------- | ----------------------- | --------------------------------------------------------------------------------- |
| `nixl`    | In-band `kv_transfer_params` handshake (P2P)    | No                      | NIXL connector (parses prefill response, forwards `kv_transfer_params` to decode) |
| `lmcache` | Shared store (Redis / lmcache-server / RWX PVC) | **Yes**                 | Generic HTTP connector (does not forward `kv_transfer_params`)                    |

:::warning
For the `lmcache` type, KV reuse happens **below** the request layer through the
shared backend. Without a shared backend (`LMCACHE_REMOTE_URL` or a shared local
disk), each pod's LMCache writes only to its own local CPU memory, so **no cross-pod
KV reuse is possible** — the deployment behaves like request-level prefill/decode
routing only. This is the root cause reported in
[issue #1069](https://github.com/volcano-sh/kthena/issues/1069). The
`lmcache` type is intended for the shared-store mode; the P2P-over-LMCache path is
not currently supported. Use the `nixl` connector when you want in-band P2P transfer.
:::

## Prerequisites

- Kubernetes cluster with Kthena installed
- NVIDIA GPU-enabled nodes with the appropriate device plugin configured
- A vLLM image whose connector Python dependencies are fully installed:
  - **LMCache:** use an image where LMCache is fully bundled, e.g. the official
    `lmcache/vllm-openai:latest`. The `ghcr.io/volcano-sh/vllm-openai:...-lmcache-0.3.2`
    image ships the LMCache package but not all of its runtime dependencies
    (`nvtx`, `sortedcontainers`, `redis`, ...), so loading `LMCacheConnectorV1`
    from it crash-loops with `ModuleNotFoundError`.
  - **NIXL:** the `ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2`
    image already bundles a compatible NIXL (v0.4.1).
- The target model accessible from the cluster
- **For the `lmcache` connector only:** a standalone Redis instance reachable from
  the cluster to act as the shared LMCache backend

:::note
When using the ModelBooster approach, the `ModelServer` and `ModelRoute` resources
are created and managed automatically — you do not need to deploy them manually.
ModelBooster creates a `ModelServing` named `{modelbooster-name}-{backend-name}`,
and a `ModelRoute` whose `modelName` equals the ModelBooster name. Send inference
requests with `"model": "<modelbooster-name>"`, not the vLLM `served-model-name`.
:::

## Option A: LMCache connector (shared store)

### 1. Deploy a shared Redis backend

The `lmcache` connector needs a shared store for cross-pod KV reuse. Deploy the
provided Redis example (adjust the namespace as needed):

```sh
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/redis/redis-standalone.yaml
```

This creates a `redis-server` Service. The ModelBooster below points
`LMCACHE_REMOTE_URL` at `redis://redis-server.default.svc.cluster.local:6379` —
update the host/namespace if you deploy Redis elsewhere.

### 2. Deploy the ModelBooster

Deploy the [LMCache ModelBooster configuration](../../assets/examples/model-booster/lmcache-pd-disaggregation.yaml)
for LMCache-backed prefill-decode disaggregation:

```sh
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/model-booster/lmcache-pd-disaggregation.yaml
```

Key parts of the configuration:

- **`kv-transfer-config`**: Both workers declare `LMCacheConnectorV1`; the prefill
  worker uses `kv_role: kv_producer` and the decode worker uses
  `kv_role: kv_consumer`. This is what makes Kthena emit `kvConnector.type: lmcache`.
- **Shared-store env vars** (applied to both workers via `spec.backend.env`):
  - `LMCACHE_REMOTE_URL` — the shared Redis backend used to exchange KV cache.
  - `LMCACHE_REMOTE_SERDE` — serialization format for the remote backend (`naive`).
  - `LMCACHE_CHUNK_SIZE` — LMCache chunk size. Prompts shorter than this never reach
    the shared backend, so test with prompts **longer** than this value.
  - `LMCACHE_LOCAL_CPU` / `LMCACHE_MAX_LOCAL_CPU_SIZE` — LMCache hard-requires a
    `LocalCPUBackend`. Keep `LOCAL_CPU=True` with a small size even when relying on
    the remote backend; `LOCAL_CPU=False` with `MAX_LOCAL_CPU_SIZE=0` triggers
    `KeyError: 'LocalCPUBackend'`.

### 3. Verify the LMCache deployment

Confirm the `ModelServer` was generated with the `lmcache` connector type:

```sh
kubectl get modelserver -n default -o yaml | grep -A2 kvConnector
```

Expected output:

```yaml
    kvConnector:
      type: lmcache
```

Verify that both prefill and decode pods are running:

```sh
kubectl get pod -owide -l modelserving.volcano.sh/name=qwen-lmcache-pd-qwen-lmcache -n default
```

Expected output:

```
NAME                                          READY   STATUS    RESTARTS   AGE
qwen-lmcache-pd-qwen-lmcache-0-decode-0-0     2/2     Running   0          3m
qwen-lmcache-pd-qwen-lmcache-0-prefill-0-0    2/2     Running   0          3m
```

Send a chat completion request through the Kthena router. The `model` field must be
the ModelBooster name (`qwen-lmcache-pd`), which is the `ModelRoute` `modelName`.
Use a prompt **longer** than `LMCACHE_CHUNK_SIZE` tokens so the KV cache actually
reaches Redis:

```sh
curl -X POST http://<ROUTER_IP>:80/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-lmcache-pd","messages":[{"role":"user","content":"<long prompt of 300+ tokens>"}],"max_tokens":20}'
```

#### Confirm real KV reuse

Because the `lmcache` connector reuses KV through the shared backend rather than
in-band `kv_transfer_params`, verify reuse by inspecting Redis and the vLLM/LMCache
logs rather than the router logs.

1. Send the same long prompt twice (or to prefill first, then decode).
2. Check that Redis is populated:

   ```sh
   kubectl exec -it deploy/redis-server -n default -- redis-cli DBSIZE
   ```

3. Inspect the decode pod logs for an external prefix cache hit:

   ```
   LMCache INFO: Reqid: ..., LMCache hit tokens: 512, need to load: 496
   Prefix cache hit rate: 2.9%, External prefix cache hit rate: 70.8%
   ```

A non-zero **External prefix cache hit rate** on the decode side confirms that KV
cache produced by prefill was reused via the shared Redis backend.

## Option B: NIXL connector (in-band P2P)

The `nixl` connector transfers KV cache directly between prefill and decode via a
`kv_transfer_params` handshake, so **no shared Redis/LMCache backend is required**.
The same LMCache-enabled image can run NIXL — only the `kv_connector` name changes.

Deploy the [NIXL ModelBooster configuration](../../assets/examples/model-booster/nixl-pd-disaggregation.yaml):

```sh
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/model-booster/nixl-pd-disaggregation.yaml
```

Key parts of the configuration:

- **`kv-transfer-config`**: Both workers declare `NixlConnector` (prefill
  `kv_producer`, decode `kv_consumer`). This makes Kthena emit
  `kvConnector.type: nixl`, and the router's NIXL connector parses the prefill
  response and forwards `kv_transfer_params` to decode.
- **`KTHENA_SKIP_ENGINE_DEPENDENCY_INSTALL: "1"`**: For the NIXL connector Kthena
  otherwise runs `pip install -U nixl` at container startup, which upgrades the
  image's bundled NIXL to a version that is incompatible with the image's vLLM and
  crash-loops the engine. Since the recommended image already bundles a compatible
  NIXL, skip that install.
- **NIXL env vars** (applied to both workers via `spec.backend.env`):
  `VLLM_NIXL_SIDE_CHANNEL_HOST` / `VLLM_NIXL_SIDE_CHANNEL_PORT` for the side channel,
  plus `NCCL_IB_*` / `UCX_TLS` settings that match your cluster network. Do **not**
  set `GLOO_SOCKET_IFNAME` / `TP_SOCKET_IFNAME` / `HCCL_SOCKET_IFNAME` here — Kthena
  already injects those into the decode worker, and Kubernetes rejects duplicate
  env var names (the ModelServing would fail validation).

### Verify the NIXL deployment

Confirm the connector type and pod status:

```sh
kubectl get modelserver -n default -o yaml | grep -A2 kvConnector
```

Expected output:

```yaml
    kvConnector:
      type: nixl
```

```sh
kubectl get pod -owide -l modelserving.volcano.sh/name=qwen-nixl-pd-qwen-nixl -n default
```

Send a test request through the router:

```sh
curl -X POST http://<ROUTER_IP>:80/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-nixl-pd","messages":[{"role":"user","content":"Hello"}],"max_tokens":20}'
```

With NIXL, KV transfer is confirmed by the router log showing prefill and decode
routed within the same PD group, and by a successful response. Unlike `lmcache`, no
shared backend or minimum prompt length is required for KV transfer to occur.

:::warning
NIXL performs GPU-to-GPU KV transfer over its side channel (UCX). This requires a
cluster network/transport that NIXL can use between the prefill and decode pods
(e.g. RDMA/InfiniBand, or a correctly configured `UCX_TLS`). On environments
without a suitable transport, the pods start and requests are routed, but the
decode engine can fail during the KV read (`nixl_connector.py` `_read_blocks`).
Tune `UCX_TLS`, `NCCL_IB_DISABLE`, and `NCCL_IB_GID_INDEX` to match your network,
or use the `lmcache` shared-store option, which does not need a P2P transport.
:::

## Troubleshooting

- **`ModuleNotFoundError` (`nvtx`, `sortedcontainers`, `redis`, ...) on the vLLM
  container with `lmcache`**: The image ships LMCache but not all of its runtime
  dependencies. Use an image where LMCache is fully installed, e.g.
  `lmcache/vllm-openai:latest`.
- **NIXL vLLM container crash-loops right after a `pip install ... nixl` log line**:
  The startup dependency install upgraded NIXL to a version incompatible with the
  image's vLLM. Set `KTHENA_SKIP_ENGINE_DEPENDENCY_INSTALL: "1"` so the bundled
  NIXL is used.
- **ModelServing rejected with `Duplicate value: {"name":"GLOO_SOCKET_IFNAME"}`**:
  Kthena auto-injects `GLOO_SOCKET_IFNAME` / `TP_SOCKET_IFNAME` / `HCCL_SOCKET_IFNAME`
  into the decode worker. Remove those env vars from `spec.backend.env`.
- **Router returns `no decode pod found` / request fails**: A prefill or decode pod
  is not Ready (often crash-looping). Check pod status and the vLLM container logs
  for the underlying error.
- **`LMCache hit tokens: 0` / `External prefix cache hit rate: 0.0%`**: Usually means
  no shared backend is configured, or the prompt is shorter than
  `LMCACHE_CHUNK_SIZE` (with `discard_partial_chunks: true`, partial chunks are never
  stored). Verify `LMCACHE_REMOTE_URL` is reachable and use a longer prompt.
- **`KeyError: 'LocalCPUBackend'`**: Set `LMCACHE_LOCAL_CPU=True` with a small
  `LMCACHE_MAX_LOCAL_CPU_SIZE` (e.g. `1`) even when relying on the remote backend.
- **`kvConnector.type` is not what you expected**: The type comes from the
  `kv_connector` name in `kv-transfer-config`, not the image. Use
  `LMCacheConnectorV1` for `lmcache` and `NixlConnector` for `nixl`.
- **NIXL decode errors / no transfer**: Ensure `VLLM_NIXL_SIDE_CHANNEL_HOST` and
  `VLLM_NIXL_SIDE_CHANNEL_PORT` are set and the `NCCL_IB_*` / `UCX_TLS` values match
  your cluster network interfaces and transport.

For the ModelServing-based approach (manually creating `ModelServing`,
`ModelServer`, and `ModelRoute`), see
[Prefill-Decode Disaggregation with ModelServing (vLLM, NIXL & LMCache)](./modelserving-vllm-pd-disaggregation.md).
