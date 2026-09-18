---
title: Dedicated Tokenizer Service for KV-Cache-Aware Routing
authors:
- TBD
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-09-01

---

## Dedicated Tokenizer Service for KV-Cache-Aware Routing

### Summary

Introduce an optional, dedicated tokenizer service that the Kthena router uses to
tokenize prompts for the `kvcache-aware` scheduling plugin, instead of sending
`/tokenize` requests to the backend inference engines. The service is a Go control
plane that watches ModelServer objects and supervises one `vllm launch render`
subprocess — vLLM's lightweight, weight-free tokenizer/renderer frontend — per
served model, resolved through an explicit model-to-tokenizer mapping. It can be
deployed either as a router sidecar or as a standalone, autoscalable service. The
feature is disabled by default, and the router falls back to engine-side
tokenization when the service cannot serve a model.

### Motivation

The `kvcache-aware` score plugin must convert the request prompt into token IDs to
compute token-block hashes and query the KV-block index. Today the router picks a
random backend pod and calls the engine's `/tokenize` endpoint (vLLM or SGLang).
This has two problems:

1. **Backend pressure**: every scheduling decision issues an extra HTTP request to a
   GPU-serving engine pod. The tokenize API shares the engine's HTTP frontend with
   inference traffic, so heavy routing traffic steals CPU cycles from the serving
   path and adds load to pods that are already busy.
2. **Latency**: the tokenize round trip is on the critical path of request routing.
   When engine pods are loaded, their HTTP frontends respond slowly, inflating
   time-to-first-token for all requests.

A dedicated CPU-only tokenizer service removes this work from the GPU fleet and
gives it an independent scaling axis.

#### Goals

1. Provide a tokenizer service that exposes the vLLM-compatible `/tokenize` API
   without loading model weights or requiring GPUs.
2. Tokenize with the engine's own frontend: each served model is backed by a
   `vllm launch render` subprocess, so token IDs and chat-template rendering are
   identical to what the engine produces at inference time. The renderer is a
   Python CLI with an HTTP interface, so a supervised local subprocess per model
   is the inherent cost of this reuse; the control plane and HTTP frontend are a
   single static Go binary.
3. Keep loaded tokenizers in sync with the cluster: watch ModelServer objects and
   dynamically load/unload the tokenizer for each served model that has a
   configured tokenizer source. The served name (`spec.model`) is an engine-side
   alias, so the source (Hugging Face repository id or local path) is configured
   explicitly per served name instead of being derived from it.
4. Support two deployment modes, selectable by the user:
   - **sidecar** of the router pod (lowest latency, scales with the router), and
   - **standalone** Deployment + Service with a configurable replica count.
5. Fall back transparently to engine-side tokenization when the service is
   unavailable or has not (yet) loaded the requested model's tokenizer. Fallback is
   configurable and enabled by default.
6. The whole feature is opt-in and disabled by default; existing behavior is
   unchanged when it is off.

#### Non-goals

1. Replacing engine-side tokenization entirely; the engine path remains the default
   and the fallback.
2. Serving chat-template rendering for request preprocessing outside the scheduler
   (may be a follow-up; the service already serves `/detokenize` and can be
   extended).
3. Exact token parity guarantees between the service and every engine version.
   The renderers reuse vLLM's own tokenization path, but the service and engine
   versions may still differ; block hashing tolerates the rare divergence the
   same way it tolerates today's cross-pod differences.
4. A Kubernetes CRD for the tokenizer service; configuration stays in Helm values
   and router plugin args.
5. Autoscaling of the standalone Deployment. Users can attach their own HPA to the
   `kthena-tokenizer` Deployment if needed; a built-in HPA may be a follow-up.

### Proposal

#### User stories

1. **Platform operator running kvcache-aware routing at scale**: tokenize traffic
   noticeably loads vLLM frontends. The operator enables the tokenizer service as a
   router sidecar; scheduling no longer touches engine pods for tokenization.
2. **Operator with many models and high QPS**: a single sidecar cannot hold
   tokenizers for dozens of models. The operator deploys the standalone mode with
   multiple replicas so tokenizer capacity scales independently of router
   replicas.
3. **Operator rolling out a new model**: until the tokenizer for the new model is
   downloaded and ready, the router silently falls back to engine tokenization —
   no requests fail and no scheduling quality is lost.

#### Architecture

```
                 ┌────────────────────────────────────────────┐
                 │                kube-apiserver               │
                 └───────▲────────────────────────▲───────────┘
                         │ watch ModelServer      │ watch ModelServer
        ┌────────────────┴───────────┐     ┌──────┴──────────────────────┐
        │        kthena-router       │     │   kthena-tokenizer (svc)    │
        │  ┌──────────────────────┐  │     │  ┌───────────────────────┐  │
        │  │ kvcache-aware plugin │──┼──┬──┼─▶│ Go frontend           │  │
        │  └──────────┬───────────┘  │  │  │  │  POST /tokenize       │  │
        │             │ fallback     │  │  │  └──────────┬────────────┘  │
        └─────────────┼──────────────┘  │  │             │ proxy to      │
                      │                 │  │             │ 127.0.0.1:82xx│
                      ▼                 │  │  ┌──────────▼────────────┐  │
        ┌──────────────────────────┐    │  │  │ vllm render (model A) │  │
        │  vLLM / SGLang pods      │    │  │  │ vllm render (model B) │  │
        │  POST /tokenize (GPU)    │    │  │  │ ... one per model     │  │
        └──────────────────────────┘    │  └─────────────────────────────┘
                                        └── sidecar mode: same pod,
                                            endpoint http://127.0.0.1:8100
```

The tokenizer service is a single Go binary with three parts, plus one renderer
subprocess per model:

1. **Frontend** (Go, `net/http`): serves `POST /tokenize` / `POST /detokenize`
   with the same request/response shapes as vLLM, plus `/models`, `/healthz`,
   `/readyz`. It inspects the `model` field of each request and proxies it over
   localhost to the renderer serving that model. If no renderer is ready for the
   model, it returns `503`, which triggers the router-side fallback.
2. **Renderer manager** (Go): supervises one `vllm launch render` subprocess per
   served model. The renderer loads only the tokenizer and chat template — no
   model weights, no GPU — so text prompts are encoded and chat prompts are
   rendered exactly as the engine does at inference time. The manager launches
   each renderer on its own local port, polls its health until ready, restarts
   crashed renderers within a bounded budget, and caps the number of concurrent
   renderers at `MAX_TOKENIZERS`. Because the served model name is an engine-side
   alias (`served-model-name`) rather than a loadable model identifier, each name
   is resolved through the explicit `MODEL_TOKENIZERS` mapping (served name →
   Hugging Face repository id or local path) and the alias itself is passed to
   the renderer via `--served-model-name`; unmapped models are skipped and fall
   back to the engine.
3. **ModelServer watcher** (Go, client-go informer): lists and watches
   ModelServer objects (optionally namespace-scoped) and reconciles the desired
   model set from each object's `spec.model`. Creating a ModelServer pre-warms
   its renderer; deleting the last ModelServer that references a model unloads
   it.

#### Router integration

The `kvcache-aware` plugin gains an optional `tokenizerService` argument:

```yaml
- name: kvcache-aware
  args:
    blockSizeToHash: 16
    maxBlocksToMatch: 128
    tokenizerService:
      enabled: true                        # default: false
      endpoint: http://127.0.0.1:8100      # sidecar; use the Service DNS for standalone
      fallbackToEngine: true               # default: true
```

`TokenizerManager.TokenizePrompt` tries the service first when enabled. On any
failure (connection error, non-2xx, unknown model) it falls back to the existing
engine-pod path unless `fallbackToEngine: false`. Because the service speaks the
vLLM `/tokenize` protocol, the router reuses the existing vLLM adapter and the
shared retryable HTTP client.

#### Deployment modes

Helm values (`networking.kthenaRouter.tokenizerService`):

| Key                   | Default   | Meaning                                              |
| --------------------- | --------- | ---------------------------------------------------- |
| `enabled`             | `false`   | Deploy the tokenizer service                         |
| `mode`                | `sidecar` | `sidecar` or `standalone`                            |
| `port`                | `8100`    | Frontend listen port                                 |
| `models`              | `{}`      | Served model name → tokenizer source (repo id/path)  |
| `maxTokenizers`       | `8`       | Max concurrently running renderers                   |
| `standalone.replicas` | `1`       | Replicas in standalone mode                          |

- **Sidecar**: the container is added to the router pod; the plugin endpoint is
  `http://127.0.0.1:8100`. It reuses the router ServiceAccount, which already has
  `get/list/watch` on ModelServers.
- **Standalone**: a Deployment, ClusterIP Service, and a dedicated ServiceAccount
  with a read-only ClusterRole on ModelServers. The
  plugin endpoint is `http://kthena-tokenizer.<namespace>.svc:8100`.

#### Notes / constraints

- `vllm launch render` is a Python CLI with an HTTP interface, so the image is
  the CPU-only vLLM base image plus the static Go binary: no torch CUDA, no GPU.
  Gated models need `HF_TOKEN` via `extraEnv`; `HF_ENDPOINT` selects an
  alternative hub endpoint.
- Renderer startup downloads tokenizer/config files from the model hub (or reads
  a mounted local path); `readyz` only reflects watcher sync, so a not-yet-ready
  model simply falls back rather than failing readiness.
- The service keys renderers by ModelServer `spec.model`, which is the same
  model name the router passes to `/tokenize` today; the tokenizer source for
  each name comes from the explicit `models` mapping.

#### Risks and mitigations

| Risk                                       | Mitigation                                                                                            |
| ------------------------------------------ | ----------------------------------------------------------------------------------------------------- |
| Tokenizer service down or model not loaded | Router falls back to engine tokenization (default on); scheduling quality unchanged                   |
| Tokenizer source missing or fails to load  | The model is reported failed/unmapped and requests for it fall back to the engine                     |
| Renderer subprocess crashes                | Bounded restart budget; a failed model falls back to the engine and is visible in `/models`           |
| Memory growth with many models             | `maxTokenizers` cap; standalone mode with more replicas for horizontal scale                          |
| Token mismatch vs engine                   | Renderers reuse vLLM's own tokenization path; block hashing already tolerates cold misses             |
| Extra hop in standalone mode               | Sidecar mode offers a localhost path; both are still far cheaper than a GPU pod round trip            |
