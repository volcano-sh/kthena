---
title: Accurate Token Counting via Sidecar Tokenizer Service
authors:
- "@nXtCyberNet"
reviewers:
- "@hzxuzhonghu"
approvers:
- "@hzxuzhonghu"

creation-date: 2026-06-16
---

## Accurate Token Counting via Sidecar Tokenizer Service

### Summary
The current token rate limiter uses the `len(prompt) / 4` heuristic to estimate input tokens, which creates significant inaccuracy for billing, compliance, and strict pre-request rate limiting.

This proposal introduces an optional sidecar tokenizer service (containerized Python application using HuggingFace's tokenizers library) that operators can deploy alongside the router for accurate token counting. The router coordinates tokenizer lifecycle via **direct HTTP calls**, eliminating external dependencies.

### Motivation
The SimpleEstimateTokenizer works acceptably for latency-sensitive development environments but breaks down for production billing and compliance paths:
- **Billing Accuracy Problem** — The /4 heuristic estimates can diverge ±30-40% from actual token counts depending on model architecture and content type.
- **Compliance Requirements** — Regulated environments require auditable, deterministic token accounting that matches model-reported usage. Heuristic estimates cannot satisfy compliance or audit requirements.

#### Goals

- Provide accurate token counting for billing, compliance, and predictable quota enforcement without forcing all operators to pay the latency cost
- Support model-agnostic tokenization across HuggingFace Hub, local PVC-mounted models, and ConfigMap-based tokenizer configs
- Maintain backward compatibility: existing ModelRoute behavior unchanged.
- Keep latency predictable: sidecar operates in-pod (HTTP over localhost).

#### Non-Goals
- Support for third-party MaaS tokenization (OpenAI, Anthropic) — these already return token usage in responses.
- Modify existing `ModelRoute` behavior — models without the tokenizer annotation continue to use `len(prompt)/4` heuristic unchanged.

---

### Proposal

#### Core Design Principles

- **Global Sidecar Toggle** – A Helm value (`kthenaRouter.tokenizerSidecar.enabled`) controls whether the tokenizer sidecar is deployed in the router pod.  
  - **If disabled**: the sidecar is omitted; the router uses the `len(prompt)/4` heuristic for **all** models.  
  - **If enabled**: the sidecar runs and is activated only for models with the annotation.

- **Per‑Model Activation** – A **single annotation** on the `ModelServer` (`kthena.volcano.sh/tokenizer-enabled: "true"`) tells the router to use accurate tokenization for that specific model. No new CRD fields.
  - **If annotation present**: router **requires** the sidecar to tokenize successfully. If sidecar fails, the request is **blocked**.
  - **If annotation absent**: router uses the `len(prompt)/4` heuristic (backward compatible).

- **Direct HTTP Coordination** – The router calls `/v1/load` and `/v1/unload` endpoints directly when `ModelServer` annotations change.

- **HTTP‑Only Tokenizer API** – The sidecar exposes `/v1/encode`, `/v1/load`, and `/v1/unload` endpoints.

---

#### Lifecycle Flow

1. **Helm Deployment**  
   - If `tokenizerSidecar.enabled: false` in `values.yaml`, the sidecar container is **not** included in the router pod. The router starts, detects it's missing, and logs that it will use the heuristic.

2. **Operator Creates/Updates a `ModelServer` with Annotation**  
  ```yaml
  apiVersion: networking.serving.volcano.sh/v1alpha1
  kind: ModelServer
  metadata:
    name: deepseek-r1-7b
    namespace: default
    annotations:
      kthena.volcano.sh/tokenizer-enabled: "true"
  spec:
    workloadSelector:
      matchLabels:
        app: deepseek-r1-7b
    workloadPort:
      port: 8000
    model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B"
    inferenceEngine: "vLLM"
    trafficPolicy:
      timeout: 10s
   ```

   - The router's `store.RegisterCallback` for `ModelServer` detects the event.
   - If the annotation is `"true"`, the callback makes a direct **HTTP POST to `/v1/load`**:

     ```json
     POST http://localhost:50051/v1/load
     {
       "model_server_id": "qwen-3.5-server",
       "model_id": "hf:Qwen/Qwen3.5-397B-A17B"
     }
     ```

   - If load succeeds: sidecar caches the tokenizer; router marks model as "tokenizer-ready".
   - If load fails: router logs error; future requests to this model will **block with HTTP 503** (unavailable).
   - If the annotation is missing or set to any value other than `"true"`, the callback makes a **POST to `/v1/unload`**:

     ```json
     POST http://localhost:50051/v1/unload
     {
       "model_server_id": "qwen-3.5-server"
     }
     ```

3. **Sidecar Processes Load Request**
   - For `hf:` prefixes – downloads `tokenizer.json` from HuggingFace Hub.
   - For `ms://` prefixes – downloads from ModelScope registry using `ModelScopeDownloader`.
   - For `local:` prefixes – reads from a mounted PVC.
   - Caches tokenizer in‑memory (`model_server_id → Tokenizer`).
   - Maintains an in-memory list of loaded tokenizer models (accessible via `/v1/list`).
   - Returns HTTP 200 with `{"status": "loaded"}` or HTTP 400 with error details.

4. **Client Request Arrives**  
   - The **RateLimiter** checks if the target `model_server_id` has the tokenizer annotation.
   - **If annotation is present** (tokenizer required):
     - Router makes a `POST /v1/encode` request:
       ```json
       POST http://localhost:50051/v1/encode
       {
         "model_server_id": "qwen-3.5-server",
         "text": "Hello, world!",
         "return_tokens": false
       }
       ```
     - If encode succeeds: sidecar returns token count; rate limiter deducts from quota.
     - If encode fails: request is **blocked** with HTTP 503 (Tokenizer Unavailable).
   - **If annotation is absent** (optional tokenizer):
     - Router uses `len(prompt)/4` heuristic for quota deduction.

5. **ModelServer Deletion or Annotation Removal**  
   - Callback makes HTTP POST to `/v1/unload`; sidecar removes tokenizer from cache.

---

#### Global Flag

- The global flag is a **Helm value** – it does **not** require a CRD change.
  ```yaml
  kthenaRouter:
    tokenizerSidecar:
      enabled: true
  ```
- When `enabled: false` – sidecar is not deployed; all models use `len(prompt)/4` heuristic.
- When `enabled: true` – sidecar is deployed; models with annotation require tokenizer success or requests are blocked.

---

#### Per‑Model Annotation

The annotation is the **only** piece of configuration an operator touches per model:

```yaml
metadata:
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"   # require accurate tokenization
```

- **If absent** → router uses `len(prompt)/4` heuristic (backward compatible).
- **If `"true"`** → router **requires** accurate tokenization via sidecar:
  - Sidecar must successfully encode the prompt.
  - If sidecar fails or is unavailable, request is **blocked** with HTTP 503.
  - Operator must ensure sidecar is running and tokenizer is loaded.

---

#### Rate Limiter Integration 

- The rate limiter checks the model's annotation (via `tokenizerManager.IsAnnotated(modelServerID)`).
- **If annotated** → call `/v1/encode` to get accurate token count.
  - Success: deduct accurate count from quota.
  - Failure: **block request** with HTTP 503 (Tokenizer Unavailable).
- **If not annotated** → use `len(prompt)/4` heuristic for quota deduction.

---

#### Helm Configuration

```yaml
# values.yaml
kthenaRouter:
  tokenizerSidecar:
    enabled: false   # global toggle; if false, sidecar omitted
    image: tokenizer-sidecar:latest
    resources:
      requests: { memory: "128Mi", cpu: "50m" }
      limits:   { memory: "1Gi", cpu: "500m" }
```

- When `enabled: false` – sidecar is not deployed; all models use heuristic regardless of annotation.
- When `enabled: true` – sidecar is deployed; models with annotation **require** tokenizer success.

---

### User Stories

#### Story 1: Enable accurate token counting for a production model

A PaaS operator runs a multi-tenant LLM service. They need accurate token counts for their flagship qwen-3.5-397b model to ensure billing compliance and pass financial audits.

**Flow:**
1. Operator sets `kthenaRouter.tokenizerSidecar.enabled: true` in Helm.
2. Operator adds annotation `kthena.volcano.sh/tokenizer-enabled: "true"` to the qwen ModelServer.
3. Router detects the annotation and calls `POST /v1/load` on the sidecar.
4. Sidecar loads the tokenizer from HuggingFace Hub; responds with `{"status": "loaded"}`.
5. Subsequent requests use accurate token counts via `/v1/encode`.
6. Billing system deducts correct token amounts from user quotas.

#### Story 2: Disable the sidecar entirely for resource-constrained development

A development team runs Kthena on a small, resource-constrained cluster. They do not require accurate token counting and want to minimize overhead.

**Flow:**
1. Operator sets `kthenaRouter.tokenizerSidecar.enabled: false` in Helm values.
2. Router does not deploy sidecar container.
3. All models use `len(prompt)/4` estimator for quota deduction.
4. Zero memory/CPU overhead from tokenization.
5. Models annotated with tokenizer flag are safe (annotation is simply ignored when sidecar is disabled).

---

### Design Details

#### Component Changes

**Router (kthena-router):**
- New package: `pkg/tokenizer/` with:
  - `Manager` – coordinates load/unload, caches enabled model list
  - `LoadRequest(modelServerID, modelID)` – HTTP POST to `/v1/load`
  - `UnloadRequest(modelServerID)` – HTTP POST to `/v1/unload`
  - `IsEnabled(modelServerID)` – returns cached enabled status
- Modified `RateLimiter.CheckQuota()` to call `Manager.IsEnabled()` and `/v1/encode` endpoint
- Enhanced `store.RegisterCallback` for `ModelServer` to detect annotation changes and call load/unload

**Sidecar (Python + FastAPI):**
- New container in router pod (conditional via Helm)
- Endpoints:
  - `POST /v1/load` – load tokenizer for a model
  - `POST /v1/unload` – unload tokenizer for a model
  - `POST /v1/encode` – tokenize text and return token count
  - `GET /v1/list` – return list of currently loaded tokenizers

**Helm Chart:**
- New value: `kthenaRouter.tokenizerSidecar.enabled` (default `false`)
- Conditional container inclusion in router pod

---

#### API Details

**Router → Sidecar Load Request:**
```json
POST /v1/load
{
  "model_server_id": "qwen-3.5-server",
  "model_id": "hf:Qwen/Qwen3.5-397B-A17B"
}

Response (200 OK):
{
  "status": "loaded"
}

Response (400 Bad Request):
{
  "error": "Failed to download tokenizer: model not found on HuggingFace Hub"
}
```

**Router → Sidecar Unload Request:**
```json
POST /v1/unload
{
  "model_server_id": "qwen-3.5-server"
}

Response (200 OK):
{
  "status": "unloaded"
}
```

**Sidecar Encode Request:**
```json
POST /v1/encode
{
  "model_server_id": "qwen-3.5-server",
  "text": "Hello, world!",
  "return_tokens": false
}

Response (200 OK):
{
  "token_count": 4,
  "token_ids": null
}

Response (404 Not Found):
{
  "error": "Tokenizer not loaded for server qwen-3.5-server"
}
```

**List Loaded Tokenizers:**
```json
GET /v1/list

Response (200 OK):
{
  "loaded_models": [
    {
      "model_server_id": "qwen-3.5-server",
      "model_id": "hf:Qwen/Qwen3.5-397B-A17B",
      "loaded_at": "2026-06-23T14:32:10Z",
      "size_mb": 142
    },
    {
      "model_server_id": "deepseek-r1",
      "model_id": "ms://deepseek-ai/DeepSeek-R1",
      "loaded_at": "2026-06-23T14:28:45Z",
      "size_mb": 89
    }
  ]
}
```

---

#### Router Implementation Details

**EventData Carrier Enhancement:**

To support tokenizer lifecycle events, a new `EventData` field is required in `pkg/kthena-router/router/store.go`:


**Functions Modified:**
- `AddOrUpdateModelServer()` – extracts tokenizer annotation and model_id from ModelServer spec; publishes load/unload via HTTP
- `DeleteModelServer()` – publishes unload request when model is deleted
- `store.RegisterCallback()` – existing callback infrastructure; enhanced to check annotation and invoke sidecar endpoints

This is a **minimal extension** to existing event plumbing — no new callback types, just richer EventData payload and HTTP sidecar coordination.



---

#### Test Plan

**Unit Tests:**
- `Manager.IsAnnotated()` returns correct value for models
- Load/unload HTTP calls made with correct payloads
- Sidecar loads from HuggingFace Hub (mocked)
- Sidecar loads from local PVC (mocked filesystem)
- `/v1/encode` returns correct token count
- `/v1/encode` returns 404 if model not loaded
- `/v1/load` and `/v1/unload` handle errors gracefully
- Rate limiter **blocks** request (HTTP 503) if `/v1/encode` fails for annotated model
- Rate limiter uses heuristic if model is not annotated

**End-to-End Scenario:**
1. Deploy full Kthena stack with sidecar enabled
2. Create ModelServer with tokenizer annotation
3. Send prompts of varying content (code, natural language, mixed)
4. Verify requests succeed and quota deductions match sidecar counts
5. Simulate sidecar crash; verify subsequent requests are **blocked** with HTTP 503
6. Remove annotation; verify fallback to heuristic (requests unblocked)
7. Disable sidecar via Helm; verify all models use heuristic (no requests blocked)

---

### Alternative Approaches

**Alternative 1: Adding spec field to ModelServer instead of annotation**
- **Rejected**: Requires CRD schema change, forcing cluster-wide upgrade
- Annotations are simpler, optional, and don't affect CRD API version



---

### Notes/Constraints/Caveats

- **Memory Limits**: Each loaded tokenizer consumes ~50–200MB. Sidecar uses LRU cache (default: 5 tokenizers). Set `resources.limits.memory` appropriately (default 1Gi).
- **Helm Rollout Behavior**: Changing `tokenizerSidecar.enabled` triggers rolling restart. During rollout, requests to annotated models are **blocked** with HTTP 503 until sidecar is back online.
- **Annotation Strictness**: Annotation value must be exactly `"true"` (case-sensitive). `"True"`, `"yes"`, `"1"` are treated as false (uses heuristic).
- **Load Latency**: First request to a tokenizer incurs load latency (network download + parsing). Subsequent requests hit cache.
- **Blocking Semantics**: Once a model is annotated with `tokenizer-enabled: "true"`, all requests **require** successful tokenization. If sidecar is down, requests are blocked. This is by design for billing compliance.

---

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Sidecar crashes; requests for annotated models blocked | **By design** – operator is responsible for sidecar availability. Use K8s liveness probes and restart policy. |
| Tokenizer download fails (network issue) | Load endpoint returns 400; operator must fix config/network and retry load or restart model. |
| Memory leak in sidecar (tokenizers not freed) | LRU cache + explicit unload calls on deletion; memory monitoring via Prometheus. |
| Load endpoint call times out | Set reasonable timeout (e.g., 5s); load failure blocks model; operator must investigate. |
| Annotation case mismatch (`"True"` vs `"true"`) | Documentation emphasizes exact case; validation in router rejects non-exact values. |
| Operator enables sidecar but doesn't deploy it | Requests to annotated models are blocked (HTTP 503). Operator must either disable sidecar in Helm or deploy it. |