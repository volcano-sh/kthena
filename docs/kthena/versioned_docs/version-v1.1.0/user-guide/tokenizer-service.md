# Tokenizer Service

The Kthena Tokenizer Service is an optional, GPU-free component that tokenizes prompts for the [kvcache-aware plugin](./kvcache-aware.md) so that the router does not need to call the backend inference engines for tokenization. A Go control plane watches ModelServer objects and supervises one lightweight, weight-free `vllm launch render` subprocess per served model, so tokenization and chat-template rendering are guaranteed to match the inference engine.

The feature is **disabled by default**. When enabled, the router sends `/tokenize` requests to the tokenizer service first and, by default, **falls back to engine-side tokenization** if the service is unavailable or has not loaded the requested model's tokenizer.

## Why use it

With `kvcache-aware` routing enabled, every scheduling decision tokenizes the prompt. Without the tokenizer service, the router sends this request to a randomly selected backend engine pod (vLLM `/tokenize`), which:

- adds load to GPU pods that are already serving inference traffic, and
- puts a busy engine HTTP frontend on the routing critical path, increasing latency.

The tokenizer service moves this work to a cheap CPU-only component that can scale independently.

## How it works

1. The service watches ModelServer objects (all namespaces by default) and reads their `spec.model` field.
2. For each served model with a configured tokenizer source, it launches a `vllm launch render` subprocess — vLLM's tokenizer/renderer frontend that loads no model weights and needs no GPU — and proxies the vLLM-compatible `/tokenize` API to it. The renderer is a Python CLI with an HTTP interface, so a local subprocess per model is the inherent cost of reusing the engine's exact tokenization behavior; the supervising control plane and frontend are a single static Go binary.
3. Because `spec.model` is an engine-side alias (`served-model-name`) and not necessarily a loadable model identifier, the tokenizer source must be configured explicitly per served name via the `models` map (a Hugging Face repository id or a mounted local path). The alias is passed to the renderer via `--served-model-name`.
4. If no renderer is ready for the requested model (unmapped, still loading, or failed), the frontend returns `503` and the router falls back to engine tokenization (unless fallback is disabled).

## Mapping served models to tokenizers

Set the `models` Helm value (rendered into the `MODEL_TOKENIZERS` env var) to map each served model name to its tokenizer source:

```yaml
networking:
  kthenaRouter:
    tokenizerService:
      enabled: true
      models:
        # served model name -> Hugging Face repo id or mounted local path
        deepseek-v3: deepseek-ai/DeepSeek-V3
        qwen3: /models/qwen3   # directory containing tokenizer files
```

Served models without a mapping are skipped and keep using engine-side tokenization.

## Deployment modes

### Sidecar mode (default)

The tokenizer runs as an extra container in the router pod. This gives the lowest latency (localhost) and scales together with the router.

```bash
helm upgrade --install kthena charts/kthena -n kthena-system \
  --set networking.kthenaRouter.tokenizerService.enabled=true \
  --set networking.kthenaRouter.tokenizerService.mode=sidecar
```

### Standalone mode

The tokenizer runs as its own Deployment and ClusterIP Service. Use this when you serve many models or want to scale tokenizer capacity independently of router replicas.

```bash
helm upgrade --install kthena charts/kthena -n kthena-system \
  --set networking.kthenaRouter.tokenizerService.enabled=true \
  --set networking.kthenaRouter.tokenizerService.mode=standalone \
  --set networking.kthenaRouter.tokenizerService.standalone.replicas=2
```

## Router configuration

Enable the tokenizer service in the `kvcache-aware` plugin args of the router ConfigMap (`kthena-router-config`):

```yaml
scheduler:
  pluginConfig:
  - name: kvcache-aware
    args:
      blockSizeToHash: 16
      maxBlocksToMatch: 128
      tokenizerService:
        # Default: false. When false, the router tokenizes via the backend engines.
        enabled: true
        # Sidecar mode:
        endpoint: http://127.0.0.1:8100
        # Standalone mode (adjust the namespace):
        # endpoint: http://kthena-tokenizer.kthena-system.svc:8100
        # Default: true. Fall back to engine tokenization when the service fails.
        fallbackToEngine: true
  plugins:
    Score:
      enabled:
        - name: kvcache-aware
          weight: 1
        - name: least-request
          weight: 1
```

Restart the router after changing the ConfigMap (hot reload is not supported).

| Field                               | Default                   | Description                                                 |
| ----------------------------------- | ------------------------- | ----------------------------------------------------------- |
| `tokenizerService.enabled`          | `false`                   | Use the dedicated tokenizer service for prompt tokenization |
| `tokenizerService.endpoint`         | — (required when enabled) | Base URL of the tokenizer service                           |
| `tokenizerService.fallbackToEngine` | `true`                    | Fall back to engine `/tokenize` on any service failure      |

## Helm values reference

All values live under `networking.kthenaRouter.tokenizerService`:

| Value                 | Default                               | Description                                      |
| --------------------- | ------------------------------------- | ------------------------------------------------ |
| `enabled`             | `false`                               | Deploy the tokenizer service                     |
| `mode`                | `sidecar`                             | `sidecar` or `standalone`                        |
| `port`                | `8100`                                | Frontend listen port                             |
| `image.repository`    | `ghcr.io/volcano-sh/kthena-tokenizer` | Image repository                                 |
| `image.tag`           | `latest`                              | Image tag                                        |
| `maxTokenizers`       | `8`                                   | Max concurrently loaded model tokenizers         |
| `models`              | `{}`                                  | Served model name → tokenizer source (HF repo id or local path) |
| `extraEnv`            | `[]`                                  | Extra env vars, e.g. `HF_TOKEN` for gated models |
| `resources`           | 500m/1Gi – 2/4Gi                      | Container resources                              |
| `standalone.replicas` | `1`                                   | Replicas in standalone mode                      |

Service environment variables (advanced, set via `extraEnv`):

| Variable                           | Default    | Description                                                                    |
| ---------------------------------- | ---------- | ------------------------------------------------------------------------------ |
| `MODEL_TOKENIZERS`                 | `{}`       | JSON object mapping served model names to tokenizer sources (set via `models`) |
| `MAX_TOKENIZERS`                   | `8`        | Cap on concurrently running renderers                                          |
| `WATCH_NAMESPACE`                  | `""` (all) | Restrict the ModelServer watch to one namespace                                |
| `VLLM_RENDER_EXTRA_ARGS`           | —          | Extra arguments appended to every renderer command, e.g. `--trust-remote-code` |
| `RENDERER_STARTUP_TIMEOUT_SECONDS` | `600`      | Max time for a renderer to become healthy (includes tokenizer download)        |
| `RENDERER_MAX_RESTARTS`            | `3`        | Restart budget for a crashed renderer                                          |
| `HF_TOKEN`                         | —          | Hugging Face token for gated models                                            |
| `HF_ENDPOINT`                      | —          | Alternative Hugging Face endpoint, e.g. a private mirror                       |

## Verification

1. Check that the tokenizer discovered your models:

   ```bash
   # Standalone mode
   kubectl -n kthena-system port-forward svc/kthena-tokenizer 8100:8100 &
   curl -s localhost:8100/models | jq
   ```

   Expected output once the renderer is ready:

   ```json
   {"models": [{"model": "qwen3", "source": "Qwen/Qwen3-0.6B", "status": "ready", "port": 8200, "restarts": 0, "lastError": ""}]}
   ```

2. Tokenize directly against the service:

   ```bash
   curl -s localhost:8100/tokenize \
     -H 'Content-Type: application/json' \
     -d '{"model": "qwen3", "prompt": "Hello, world!"}' | jq
   ```

3. Send an inference request through the router and confirm in the router logs (`-v=4`) that tokenization no longer targets engine pod IPs, and that `KVCacheAware.Score` reports tokens.

4. Test the fallback: scale the tokenizer to zero (or request a model it has not loaded) and confirm requests still route successfully with a router log line like `tokenizer service failed for model ..., falling back to engine`.

## Troubleshooting

- **Model stays in `loading`**: the renderer is starting and downloading tokenizer files from the model hub. For gated models, set `HF_TOKEN` via `extraEnv`; for air-gapped clusters, mount the tokenizer files and use a local path in `models`.
- **Model shows `failed`**: check `lastError` in `/models` and the pod logs. Common causes: wrong tokenizer source in `models`, hub unreachable, renderer startup timeout, exhausted restart budget, or `maxTokenizers` reached (increase it or use standalone mode).
- **Model missing from `/models`**: the served name has no entry in the `models` map; add one, otherwise the router keeps using engine-side tokenization for it.
- **Router never uses the service**: verify `tokenizerService.enabled: true` and a non-empty `endpoint` in the kvcache-aware plugin args, then restart the router.
