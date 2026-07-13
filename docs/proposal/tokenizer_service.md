---
title: Local Tokenizer Sidecar Service for Performance and Architectural Separation
authors:
- "@nXtCyberNet"
reviewers:
- "@hzxuzhonghu"
approvers:
- "@hzxuzhonghu"

creation-date: 2026-06-16
last-updated: 2026-07-13
---

## Local Tokenizer Sidecar Service for Performance and Architectural Separation

### Summary

This proposal introduces an containerized tokenizer sidecar (Python + FastAPI using HuggingFace's tokenizers library) deployed alongside the router to decouple tokenization from model inference. The sidecar exposes HTTP endpoints for tokenization, enabling:

- **Latency Reduction & KV Cache Integration** — Local tokenization (HTTP over localhost in 1-2ms) enables the KV cache-aware scheduling plugin to perform rapid token-level prefix matching and pod scoring before routing, without incurring external network round-trip overhead.
- **Inference Engine Isolation** — By offloading tokenization to the local sidecar, tokenization requests do not consume backend inference engine resources (e.g. vLLM execution queues/slots), preventing any negative influence on the backend's inference performance.
- **De-duplication of Tokenizing (Separated Prefill/Decode)** — Decoupling tokenization from the backend prefill/decode stages allows the router to tokenize once. The generated token IDs can be passed directly to the inference engine (e.g. via `prompt_token_ids` in vLLM), avoiding duplicate tokenization passes at the backend engine.
- single time token manuplation - currently we are doing multiple checks for token so the kv cache based plugin will provide the token count to rate limit removing the overhead of the 4x haulutinating part 
- **Architectural Separation** — Tokenizer lifecycle is decoupled from model serving, enabling independent scaling, upgrades, and caching configurations.

The router coordinates tokenizer lifecycle via **direct HTTP calls**, eliminating external dependencies. This design supports local PVC-mounted tokenizers, HuggingFace Hub, and ModelScope registries.

---

### Motivation

Current rate limiting relies on the len(prompt) / 4 heuristic to estimate the number of input tokens. While sufficient for approximate accounting, it cannot provide the accuracy required for production workloads and introduces redundant work across the request pipeline.

This proposal introduces a local tokenizer service that performs single-pass tokenization during request scheduling. The KV cache-aware scheduling plugin already requires the complete token sequence for cache-aware pod scoring, making it the natural place to perform tokenization. Rather than discarding the result, the plugin propagates the generated token count to the rate limiter, eliminating additional token counting and replacing heuristic-based estimation with accurate token accounting.

The tokenizer lifecycle is managed automatically through ModelServer events. When a ModelServer is created, updated, or deleted, the router loads, reloads, or unloads the corresponding tokenizer in the sidecar, ensuring that the correct tokenizer is always available without manual configuration.

#### Performance Bottlenecks & Interference

- **Network Latency & Scheduler Blocking** — Querying remote tokenization services (OpenAI, Anthropic APIs) or even in-cluster backend inference engine tokenization endpoints adds 50-200ms of latency. When used by scheduling plugins like the KV Cache-Aware plugin, this latency directly blocks routing decisions.
- **Backend Inference Resource Contention** — Relying on the backend inference engine (e.g. vLLM) for tokenization means tokenization requests compete with prefill/decode tasks for GPU slots and execution queues, negatively influencing backend inference performance.
- **Duplicate Tokenization** —A prompt may be tokenized multiple times: first by the router, then by the KV cache-aware scheduling plugin, and again by the backend inference engine before the prefill stage. For inference engines supporting pre-tokenized inputs (for example, prompt_token_ids in vLLM), this final tokenization step can be eliminated.

#### Architectural Constraints

- **Tight Coupling** — Current design couples quota enforcement and scheduling logic (router) with model inference (backend), making it impossible to optimize or scale tokenization separately.

- **Cache Awareness Requirements** — The KV cache-aware scheduler plugin requires token-level sequence block matching (e.g., using Redis block hashes) *before* routing. Without a local tokenizer, the router needs to use the vllm tokenization endpoint increasing latency for long prompts.

#### Goals

- Provide **local, low-latency tokenization** (~1-2ms localhost latency) accessible from the router, scheduler plugins, and autoscaling components.
- **Integrate with KV cache-aware plugins** to enable fast, token-level prefix matching and pod scoring before routing decisions.
- **Decouple tokenizer from prefill/decode** to allow token IDs to be reused (e.g., passing `prompt_token_ids` directly to the backend) and reduce duplicate tokenizing.
- **Isolate tokenization workloads** from backend inference engines to prevent resource contention and latency spikes.
- **Decouple tokenizer lifecycle** from model inference, enabling independent scaling, upgrades, and resource allocation.
- **Support diverse tokenizer sources** — local PVC-mounted models, HuggingFace Hub, ModelScope registry.

#### Non-Goals

- Support for external MaaS tokenization endpoints (these remain available as fallback)
- Compliance/billing-specific accuracy requirements (this is a performance/architecture initiative, not a regulatory one)
- Modify existing `ModelRoute` behavior for models without tokenizer annotation

---

### Proposal

#### Core Design Principles

- **Local-First Deployment** – Sidecar runs in-pod alongside router (HTTP over localhost); no external network calls for tokenization
- **Automatic Lifecycle Management** – When a `ModelServer` is created/updated with tokenizer annotation, router automatically loads the tokenizer via `POST /v1/load`
- **Separation of Concerns & Isolation** – Tokenizer runs independently from model inference; does not consume GPU/CPU resources from the backend inference engine, preventing latency interference.
- **Cache-Aware & De-duplicated Design** – Local tokenization feeds the KV cache-aware scheduler plugin with token IDs. If supported, these same token IDs can be forwarded to the backend to bypass redundant tokenization in the prefill stage.
- **Direct HTTP Coordination** – Router calls `/v1/load` and `/v1/unload` endpoints directly when `ModelServer` annotations change

---

#### Architecture Overview

```
┌────────────────────────────────────────────────────────────┐
│                    Kthena Router Pod                        │
├────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌──────────────┐         ┌──────────────────────┐         │
│  │ Rate Limiter │         │   Autoscaler Plugin  │         │
│  │  (RateLim)   │         │   (Scheduling Logic) │         │
│  └──────┬───────┘         └──────────┬───────────┘         │
│         │                            │                      │
│         └─────────────┬──────────────┘                      │
│                       │                                      │
│              ┌────────▼────────┐                            │
│              │ Tokenizer HTTP  │                            │
│              │   Client (Go)   │                            │
│              └────────┬────────┘                            │
│                       │ localhost:50051                     │
│  ┌────────────────────▼──────────────────────────┐         │
│  │        Tokenizer Sidecar (Python/FastAPI)     │         │
│  ├────────────────────────────────────────────────┤         │
│  │                                                 │         │
│  │  ┌──────────────┐  ┌──────────────────────┐  │         │
│  │  │  /v1/load    │  │ HF Hub Downloader    │  │         │
│  │  │  /v1/unload  │  │ ModelScope Client    │  │         │
│  │  │  /v1/encode  │  │ Local PVC Reader     │  │         │
│  │  │  /v1/list    │  │                      │  │         │
│  │  └──────────────┘  └──────────────────────┘  │         │
│  │                                                 │         │
│  │  ┌──────────────────────────────────────────┐ │         │
│  │  │  In-Memory Tokenizer Cache (LRU)         │ │         │
│  │  │  qwen-3.5-server → Tokenizer(50MB)       │ │         │
│  │  │  deepseek-r1     → Tokenizer(89MB)       │ │         │
│  │  └──────────────────────────────────────────┘ │         │
│  │                                                 │         │
│  └────────────────────────────────────────────────┘         │
│                                                              │
└────────────────────────────────────────────────────────────┘
```

**Key Separation:**
- **Router (Go)**: Quota enforcement, request routing
- **Tokenizer Sidecar (Python)**: Tokenization only, independent lifecycle
- **Communication**: HTTP over localhost (no external network calls for core functionality)

---

#### Lifecycle Flow

##### 1. Deployment & Initialization

When the Kthena router starts:

- Router pod includes tokenizer sidecar container (conditional via Helm)
- Sidecar starts with empty cache
- Router's `tokenizer.Manager` connects to sidecar via `localhost:50051`
- Tokenizers are loaded automatically as ModelServers are registered.

##### 2. Operator Annotates a ModelServer

```yaml
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: qwen-3.5-server
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"
spec:
  model:
    model_id: "hf:Qwen/Qwen3.5-397B-A17B"  # HuggingFace model ID
  # or
  # model_id: "ms://deepseek-ai/DeepSeek-R1"  # ModelScope ID
  # or
  # model_id: "local:qwen-tokenizer"  # mounted PVC path
```

**Router Detects Change:**
- `store.RegisterCallback` for `ModelServer` fires on annotation
- Router's `tokenizer.Manager` calls:
  ```json
  POST http://localhost:50051/v1/load
  {
    "model_server_id": "qwen-3.5-server",
    "model_id": "hf:Qwen/Qwen3.5-397B-A17B"
  }
  ```

##### 3. Sidecar Loads Tokenizer

Sidecar processes the load request:

- **HuggingFace Hub** (`hf:` prefix):
  ```python
  # Downloads tokenizer.json from HF Hub
  from huggingface_hub import hf_hub_download
  tokenizer_path = hf_hub_download(
      repo_id="Qwen/Qwen3.5-397B-A17B",
      filename="tokenizer.json",
      cache_dir="/tmp/tokenizers"
  )
  ```

- **ModelScope Registry** (`ms://` prefix):
  ```python
  # Downloads from ModelScope registry
  from modelscope.hub.api import HubApi
  api = HubApi()
  tokenizer_path = api.get_model_file(
      model_id="deepseek-ai/DeepSeek-R1",
      file_name="tokenizer.json"
  )
  ```

- **Local PVC** (`local:` prefix):
  ```python
  # Reads from mounted PVC
  tokenizer_path = f"/models/{model_id}/tokenizer.json"
  ```

**Tokenizer Cached:**
```python
self.tokenizers_cache["qwen-3.5-server"] = Tokenizer.from_file(tokenizer_path)
```

**Response:**
```json
{
  "status": "loaded",
  "model_server_id": "qwen-3.5-server",
  "model_id": "hf:Qwen/Qwen3.5-397B-A17B",
  "size_mb": 142,
  "loaded_at": "2026-07-13T10:23:45Z"
}
```

##### 4. Rate Limiter Uses Local Tokenization

When a client request arrives:

**Router's RateLimiter:**
```go
if tokenizer.Manager.IsAnnotated(modelServerID) {
    // Call local sidecar for accurate count
    tokenCount, err := tokenizer.Manager.Encode(modelServerID, prompt)
    if err != nil {
        // Tokenizer unavailable; block request
        return 503, "Tokenizer Unavailable"
    }
    quotaUsed = tokenCount
} else {
    // Fallback to heuristic
    quotaUsed = len(prompt) / 4
}
```

**HTTP Call to Sidecar:**
```json
POST http://localhost:50051/v1/encode
{
  "model_server_id": "qwen-3.5-server",
  "text": "Explain quantum computing in simple terms...",
  "return_tokens": false
}

Response (200 OK):
{
  "token_count": 47,
  "token_ids": null,
  "model_server_id": "qwen-3.5-server"
}
```

**Latency Profile:**
- Local HTTP call: ~1-2ms (no network, localhost)
- Tokenization: ~5-10ms (in-memory)
- Total: ~6-12ms per request (vs. 50-200ms for remote service)

##### 5. Autoscaler Access (Issue #1100)

Autoscaler can call the same tokenizer for intelligent scaling decisions:

```go
// Autoscaler scheduling logic
func (s *Autoscaler) ShouldScale(modelID string, queue []*Request) bool {
    tokenizer := s.tokenizerClient  // same sidecar
    
    totalTokens := 0
    for _, req := range queue {
        count, _ := tokenizer.Encode(modelID, req.Prompt)
        totalTokens += count
    }
    
    // Scale decision based on actual token load
    return totalTokens > s.thresholdTokens
}
```

**Benefits:**
- No duplicate tokenization (autoscaler reuses router's tokenizer)
- Accurate scaling decisions (not heuristic-based)
- Same low latency (localhost call)
- Decoupled from model inference (autoscaler not blocked by backend availability)

##### 6. KV Cache Integration & Scheduling

The local tokenizer enables low-latency integration with the KV cache-aware scheduler plugin:

```go
// Scheduler scoring flow utilizing local tokenizer
func (p *KVCacheAwarePlugin) Score(podInfo *datastore.PodInfo, req *Request) (int, error) {
    // Step 1: Get token sequence from local tokenizer sidecar
    tokens, err := p.tokenizer.Encode(req.ModelServerID, req.Prompt)
    if err != nil {
        return 0, err
    }
    
    // Step 2: Divide tokens into semantic blocks (e.g., 128 tokens per block)
    blocks := p.blockProcessor.Divide(tokens)
    
    // Step 3: Query Redis for pods containing matching prefix blocks
    matchingPods := p.redisClient.QueryBlocks(blocks)
    
    // Step 4: Calculate score based on consecutive cached prefix blocks
    score := p.calculateConsecutiveMatches(matchingPods, podInfo)
    return score, nil
}
```

**Benefits of Local Tokenizer for KV Cache Scheduling:**
- **Zero Remote Latency** — Pod scoring runs in the scheduling path for every request. Running tokenization locally (~1-2ms) ensures that scheduling latency is kept minimal.
- **Resource Isolation** — Querying the backend inference engine for tokenization during scheduling would create a circular dependency and heavily degrade routing throughput. Local tokenization prevents any impact on inference backend load.
- **De-duplicated Token IDs** — Once tokenized for scoring, the resulting token IDs can be forwarded directly to the target pod's inference engine (e.g., using `prompt_token_ids`), avoiding a duplicate tokenization pass in the backend's prefill phase.

##### 7. ModelServer Deletion or Annotation Removal

When a model is deleted or annotation is removed:

```json
POST http://localhost:50051/v1/unload
{
  "model_server_id": "qwen-3.5-server"
}

Response (200 OK):
{
  "status": "unloaded",
  "model_server_id": "qwen-3.5-server",
  "freed_mb": 142
}
```

Tokenizer is removed from cache; memory is freed.

---

### Design Details

Operator controls which models use local tokenization:

```yaml
metadata:
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"  # require local tokenization
```

| Annotation | Behavior |
|-----------|----------|
| Absent | Use `len(prompt)/4` heuristic (backward compatible) |
| `"true"` | Require local tokenization; block if unavailable |
| `"false"` or any other value | Use heuristic; ignore sidecar |

**Important:** Annotation must be exactly `"true"` (case-sensitive).

---

#### Rate Limiter Integration

**Before:**
```go
// Old: simple heuristic
quotaUsed = len(prompt) / 4
```

**After:**
```go
// New: check for local tokenizer
count, err := r.tokenizerManager.Encode(modelServerID, prompt)
if err != nil {
    return errors.New("Tokenizer Unavailable (HTTP 503)")
}
quotaUsed = count
```

---

#### Autoscaler Integration (Issue #1100)

The autoscaler can query the same local tokenizer for scaling decisions:

```go
type AutoscalerConfig struct {
    TokenizerEndpoint string  // "http://localhost:50051"
}

func (a *Autoscaler) EvaluateScaling(modelID string, pendingRequests []*Request) ScalingDecision {
    // Accurate token count instead of heuristic
    totalTokens := 0
    for _, req := range pendingRequests {
        count, _ := a.tokenizer.Encode(modelID, req.Prompt)
        totalTokens += count
    }
    
    // Scale based on actual token load
    if totalTokens > a.Config.ScaleUpThreshold {
        return ScalingDecision{Action: "scale_up", replicas: currentReplicas + 1}
    }
    
    return ScalingDecision{Action: "none"}
}
```

**Advantages:**
- **Accurate Decisions** — scaling based on actual tokens, not estimation
- **Low Latency** — local HTTP call (~10ms) is negligible for autoscaler polling cycles
- **No Duplication** — autoscaler and router share the same tokenizer
- **Independent Scaling** — tokenizer can scale separately from model inference

---

### User Stories

#### Story 1: Enable Low-Latency Tokenization for High-Throughput Service

A PaaS operator runs a multi-tenant LLM service with 10k+ requests/second. They need tokenization to be fast and local; remote tokenization adds unacceptable latency (50-200ms per request = 500k-2M seconds of added latency per day).

**Flow:**
1. Operator sets `kthenaRouter.tokenizerSidecar.enabled: true` in Helm
2. Operator adds annotation `kthena.volcano.sh/tokenizer-enabled: "true"` to production models
3. Router detects annotation and loads tokenizer from HuggingFace Hub via sidecar
4. Subsequent requests use local tokenization (~10ms per request vs. 150ms remote)
5. Daily latency savings: ~140ms × 10k req/s × 86,400s = ~120 million milliseconds (33+ hours of latency saved per day)

**Result:** Measurable improvement in end-user request latency and throughput.

#### Story 2: Autoscaler Makes Intelligent Scaling Decisions

An autoscaler (issue #1100) needs accurate token counts to decide when to scale model replicas. Current heuristic estimates lead to over-scaling or under-scaling.

**Flow:**
1. Autoscaler is configured to use local tokenizer at `http://localhost:50051`
2. Autoscaler monitors pending request queue every 10 seconds
3. For each pending request, calls `/v1/encode` to get actual token count
4. Computes total token load; compares against configured threshold
5. Scales model replicas based on actual token demand (not request count)
6. Result: Better resource utilization; fewer over-provisioned replicas

**Example:**
- Request A: 5 tokens (simple query)
- Request B: 500 tokens (complex reasoning task)
- Heuristic estimate: 2 × 1000 / 4 = 500 tokens (incorrect)
- Actual: 505 tokens (correct)
- Autoscaler scales more accurately with real count

#### Story 3: Integrate with KV Cache for Request Optimization

An operator deploys Kthena with KV cache-aware plugins. Local tokenization enables tight integration: cache lookups happen before inference.

**Flow:**
1. Operator enables sidecar and annotates model
2. Router's KV cache plugin receives tokenized input from local sidecar
3. Plugin checks cache for token sequence; finds match
4. Returns cached output, bypassing inference entirely (~200ms saved)
5. Unmatched requests proceed to inference only after cache miss

**Result:** Significant latency reduction for repeated prompts; inference backend is not bothered with cache logic.

### Design Details

#### Component Changes

**Router (kthena-router) — Go:**

New package `pkg/tokenizer/`:
```go
// Manager coordinates tokenizer lifecycle
type Manager struct {
    httpClient   *http.Client
    endpoint     string  // "http://localhost:50051"
    }

// Load tokenizer for a model
func (m *Manager) Load(modelServerID, modelID string) error {
    req := LoadRequest{ModelServerID: modelServerID, ModelID: modelID}
    // POST /v1/load; return error if fails
}

// Encode text to token count
func (m *Manager) Encode(modelServerID string, text string) (int, error) {
    req := EncodeRequest{ModelServerID: modelServerID, Text: text, ReturnTokens: false}
    // POST /v1/encode; return token count
}

// Unload tokenizer for a model
func (m *Manager) Unload(modelServerID string) error {
    req := UnloadRequest{ModelServerID: modelServerID}
    // POST /v1/unload
}

```

Modified `RateLimiter.CheckQuota()`:
```go
func (rl *RateLimiter) CheckQuota(modelServerID, prompt string) (tokens int, err error) {
    if rl.tokenizer.IsAnnotated(modelServerID) {
        // Use sidecar
        tokens, err = rl.tokenizer.Encode(modelServerID, prompt)
        if err != nil {
            return 0, fmt.Errorf("Tokenizer Unavailable: %w", err)
        }
    } else {
        // Fallback to heuristic
        tokens = len(prompt) / 4
    }
    return tokens, nil
}
```

Enhanced `store.RegisterCallback` for `ModelServer`:
```go
func (s *Store) RegisterCallback(modelServer *ModelServer, event EventType) {
    if event == EventUpdate {
        annotation := modelServer.Annotations["kthena.volcano.sh/tokenizer-enabled"]
        if annotation == "true" {
            s.tokenizer.Load(modelServer.Name, modelServer.Spec.ModelID)
        } else {
            s.tokenizer.Unload(modelServer.Name)
        }
    } else if event == EventDelete {
        s.tokenizer.Unload(modelServer.Name)
    }
}
```

**Sidecar (tokenizer-sidecar) — Python + FastAPI:**

```python
# main.py
from fastapi import FastAPI, HTTPException
from huggingface_hub import hf_hub_download
import os
import json

app = FastAPI()

class TokenizerManager:
    def __init__(self):
        self.cache = {}  # model_server_id -> tokenizer
        self.max_cached = 5  # LRU cache limit
    
    def load(self, model_server_id: str, model_id: str):
        """Load tokenizer from HF Hub, ModelScope, or local PVC"""
        try:
            if model_id.startswith("hf:"):
                repo_id = model_id.replace("hf:", "")
                path = hf_hub_download(repo_id, "tokenizer.json")
            elif model_id.startswith("ms://"):
                # ModelScope download
                repo_id = model_id.replace("ms://", "")
                # Use modelscope downloader
                path = download_from_modelscope(repo_id)
            elif model_id.startswith("local:"):
                path = f"/models/{model_id.replace('local:', '')}/tokenizer.json"
            
            from tokenizers import Tokenizer
            tokenizer = Tokenizer.from_file(path)
            
            # Implement LRU eviction if needed
            if len(self.cache) >= self.max_cached:
                oldest_key = next(iter(self.cache))
                del self.cache[oldest_key]
            
            self.cache[model_server_id] = {
                "tokenizer": tokenizer,
                "model_id": model_id,
                "size_mb": os.path.getsize(path) / (1024*1024),
                "loaded_at": datetime.now()
            }
            return True
        except Exception as e:
            raise ValueError(f"Failed to load tokenizer: {str(e)}")
    
    def encode(self, model_server_id: str, text: str):
        """Tokenize text"""
        if model_server_id not in self.cache:
            raise KeyError(f"Tokenizer not loaded for {model_server_id}")
        
        tokenizer = self.cache[model_server_id]["tokenizer"]
        encoding = tokenizer.encode(text)
        return len(encoding.ids)
    
    def unload(self, model_server_id: str):
        """Remove tokenizer from cache"""
        if model_server_id in self.cache:
            del self.cache[model_server_id]

manager = TokenizerManager()

@app.post("/v1/load")
async def load_tokenizer(request: LoadRequest):
    try:
        manager.load(request.model_server_id, request.model_id)
        return {
            "status": "loaded",
            "model_server_id": request.model_server_id,
            "size_mb": manager.cache[request.model_server_id]["size_mb"],
            "loaded_at": manager.cache[request.model_server_id]["loaded_at"]
        }
    except Exception as e:
        raise HTTPException(status_code=400, detail=str(e))

@app.post("/v1/encode")
async def encode(request: EncodeRequest):
    try:
        count = manager.encode(request.model_server_id, request.text)
        return {"token_count": count, "model_server_id": request.model_server_id}
    except KeyError:
        raise HTTPException(status_code=404, detail=f"Tokenizer not loaded")
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@app.post("/v1/unload")
async def unload_tokenizer(request: UnloadRequest):
    manager.unload(request.model_server_id)
    return {"status": "unloaded", "model_server_id": request.model_server_id}

@app.get("/v1/list")
async def list_tokenizers():
    return {
        "loaded_models": [
            {
                "model_server_id": k,
                "model_id": v["model_id"],
                "size_mb": v["size_mb"],
                "loaded_at": v["loaded_at"]
            }
            for k, v in manager.cache.items()
        ]
    }
```

**Helm Chart:**

```yaml
# templates/router-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kthena-router
spec:
  template:
    spec:
      containers:
      - name: router
        image: kthena-router:latest
        env:
        - name: TOKENIZER_ENDPOINT
          value: "http://localhost:50051"
      
      # Conditional sidecar container
      {{ if .Values.kthenaRouter.tokenizerSidecar.enabled }}
      - name: tokenizer-sidecar
        image: {{ .Values.kthenaRouter.tokenizerSidecar.image }}
        ports:
        - containerPort: {{ .Values.kthenaRouter.tokenizerSidecar.port }}
        resources:
          requests:
            memory: {{ .Values.kthenaRouter.tokenizerSidecar.resources.requests.memory }}
            cpu: {{ .Values.kthenaRouter.tokenizerSidecar.resources.requests.cpu }}
          limits:
            memory: {{ .Values.kthenaRouter.tokenizerSidecar.resources.limits.memory }}
            cpu: {{ .Values.kthenaRouter.tokenizerSidecar.resources.limits.cpu }}
        livenessProbe:
          httpGet:
            path: /health
            port: 50051
          initialDelaySeconds: 10
          periodSeconds: 10
      {{ end }}
```

---

#### API Specifications

**POST /v1/load — Load Tokenizer**
```json
Request:
{
  "model_server_id": "qwen-3.5-server",
  "model_id": "hf:Qwen/Qwen3.5-397B-A17B"
}

Response (200 OK):
{
  "status": "loaded",
  "model_server_id": "qwen-3.5-server",
  "model_id": "hf:Qwen/Qwen3.5-397B-A17B",
  "size_mb": 142,
  "loaded_at": "2026-07-13T10:23:45Z"
}

Response (400 Bad Request):
{
  "error": "Failed to download tokenizer from HF Hub: model not found"
}
```

**POST /v1/encode — Tokenize Text**
```json
Request:
{
  "model_server_id": "qwen-3.5-server",
  "text": "Explain quantum computing in simple terms",
  "return_tokens": false
}

Response (200 OK):
{
  "token_count": 47,
  "token_ids": null,
  "model_server_id": "qwen-3.5-server"
}

Response (404 Not Found):
{
  "error": "Tokenizer not loaded for server qwen-3.5-server"
}
```

**POST /v1/unload — Unload Tokenizer**
```json
Request:
{
  "model_server_id": "qwen-3.5-server"
}

Response (200 OK):
{
  "status": "unloaded",
  "model_server_id": "qwen-3.5-server",
  "freed_mb": 142
}
```

**GET /v1/list — List Loaded Tokenizers**
```json
Response (200 OK):
{
  "loaded_models": [
    {
      "model_server_id": "qwen-3.5-server",
      "model_id": "hf:Qwen/Qwen3.5-397B-A17B",
      "size_mb": 142,
      "loaded_at": "2026-07-13T10:23:45Z"
    },
    {
      "model_server_id": "deepseek-r1",
      "model_id": "ms://deepseek-ai/DeepSeek-R1",
      "size_mb": 89,
      "loaded_at": "2026-07-13T10:28:10Z"
    }
  ]
}
```

---

### Performance Analysis

#### Latency Comparison

| Scenario | Latency | Impact |
|----------|---------|--------|
| Remote Tokenization (OpenAI API) | 50-200ms | High; adds 50-200ms per request |
| Local Sidecar Tokenization | 6-12ms | Low; minimal impact on request latency |
| Heuristic Estimate (`len(prompt)/4`) | <1ms | Very low, but inaccurate |

**Example: 10,000 requests/sec**
- Remote tokenization: 500-2,000 seconds of added latency per second
- Local sidecar: 60-120 seconds of added latency per second
- Savings: ~80-95% reduction in tokenization latency

#### Memory Footprint

- Each tokenizer: 50-200MB (depending on model)
- Sidecar with 5 cached tokenizers: ~500MB-1GB
- Helm resource limits: 2GB (configurable)
- LRU eviction: Automatically removes least-used tokenizers when cache is full

#### Scaling Strategy

**Request Path:** Router → Sidecar (HTTP localhost, ~10ms)
- Sidecar is not a bottleneck (single request ~1-2ms encode time)
- Even with high throughput, sidecar keeps up

**Scaling Decision Path:** Autoscaler → Sidecar (HTTP localhost, ~10ms per request)
- Autoscaler polls every 10-60 seconds (configurable)
- 100 pending requests × 10ms = 1 second of encoding
- Negligible impact on autoscaler cycle time

---

### Test Plan

#### Unit Tests

- `Manager.Load()` with HF Hub model ID (mocked HTTP)
- `Manager.Load()` with local PVC path (mocked filesystem)
- `Manager.Encode()` returns correct token count
- `Manager.Encode()` throws error if model not loaded
- `Manager.Unload()` removes tokenizer from cache
- `Manager.IsAnnotated()` returns correct status
- LRU cache evicts oldest tokenizer when limit reached
- RateLimiter uses sidecar if model is annotated
- RateLimiter falls back to heuristic if model is not annotated
- RateLimiter blocks request (HTTP 503) if sidecar is unavailable

#### Integration Tests

- Sidecar starts and responds to health checks
- `/v1/load` endpoint downloads and caches tokenizer
- `/v1/encode` endpoint returns correct counts
- `/v1/unload` endpoint removes tokenizer
- `/v1/list` endpoint shows all loaded tokenizers
- Annotation changes trigger load/unload calls

#### End-to-End Scenario

1. Deploy Kthena with `tokenizerSidecar.enabled: true`
2. Verify sidecar is running and responding to `/health`
3. Create ModelServer with `tokenizer-enabled: "true"` annotation
4. Verify router calls `/v1/load` and sidecar loads tokenizer
5. Send 100 requests with varying prompt lengths
6. Verify token counts are accurate (not heuristic estimates)
7. Verify quota deductions match sidecar counts
8. Simulate sidecar crash; verify requests to annotated models are blocked
9. Restart sidecar; verify requests resume working
10. Remove annotation; verify fallback to heuristic
11. Disable sidecar via Helm; verify all models use heuristic
12. Verify autoscaler can query same tokenizer for scaling decisions

#### Performance Benchmarks

- Single tokenization: <5ms (in-memory)
- 10,000 concurrent tokenization requests: avg ~10ms latency
- Sidecar memory usage: 500MB-1GB for 5 cached tokenizers
- Cache lookup: <1ms

---

### Deployment Guide

#### Prerequisites

- Kthena router running on Kubernetes
- Docker/container registry access
- PVC (optional, for local tokenizers)

#### Step 1: Build Sidecar Image

```bash
git clone https://github.com/volcano-sh/kthena.git
cd kthena/sidecar/tokenizer
docker build -t tokenizer-sidecar:latest .
docker push your-registry/tokenizer-sidecar:latest
```

#### Step 2: Enable Sidecar in Helm

```bash
# values.yaml
kthenaRouter:
  tokenizerSidecar:
    enabled: true
    image: your-registry/tokenizer-sidecar:latest
    port: 50051
    resources:
      requests:
        memory: "256Mi"
        cpu: "100m"
      limits:
        memory: "2Gi"
        cpu: "1000m"
```

#### Step 3: Annotate Models

```yaml
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: qwen-3.5-server
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"
spec:
  model:
    model_id: "hf:Qwen/Qwen3.5-397B-A17B"
  # ... rest of spec
```

#### Step 4: Deploy

```bash
helm upgrade --install kthena ./charts/kthena -f values.yaml
# Wait for rollout
kubectl rollout status deployment/kthena-router
```

#### Step 5: Verify

```bash
# Check sidecar is running
kubectl logs -f deployment/kthena-router -c tokenizer-sidecar

# Query loaded tokenizers
kubectl port-forward svc/kthena-router 50051:50051
curl http://localhost:50051/v1/list
```

---

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Sidecar crashes; requests blocked | **By design** — operator ensures sidecar availability via K8s restart policies and liveness probes |
| Tokenizer download fails | Load endpoint returns 400; operator must fix config or network and retry |
| Memory leak in sidecar | LRU cache + explicit unload; memory monitoring via Prometheus metrics |
| Load timeout | Set reasonable timeout (5-10s); load failure is logged; operator can retry or disable annotation |
| Annotation typo (`"True"` vs `"true"`) | Documentation emphasizes exact case; validation in router rejects non-exact values |
| Operator enables sidecar but doesn't deploy it | Requests to annotated models are blocked (HTTP 503); operator must either disable sidecar or deploy it |
| High memory usage (many tokenizers) | LRU cache limits to 5 tokenizers; adjust `max_cached` or increase resource limits |
| Tokenizer version mismatch | Document supported tokenizer formats; pin versions in model_id |

---

### Alternative Approaches

#### Alternative 1: Centralized Tokenization Service

Deploy tokenizer as a separate microservice (not sidecar) accessible to all pods.

**Rejected Because:**
- Network latency: 50-200ms per request (main motivation for this proposal)
- Single point of failure: if service is down, all models are affected
- Scaling complexity: service scales independently, adding operational overhead
- KV cache integration: difficult to integrate cache logic with separate service

#### Alternative 2: Embedded Tokenizer in Router

Bundle tokenizer library directly into router binary (no sidecar).

**Rejected Because:**
- Router pod memory increases by 50-200MB per model (doesn't scale well)
- Can't separate tokenizer lifecycle from router (can't upgrade independently)
- Difficult to add KV cache plugin integration
- Coupling conflicts with architecture goals

#### Alternative 3: Use External MaaS (OpenAI, Anthropic)

Call external tokenization APIs.

**Rejected Because:**
- Network latency: 50-200ms per request (defeats purpose of this proposal)
- Network calls: external dependency; routing not under operator control
- Cost: external API calls add cost per request
- Compliance: data may leave infrastructure boundary

#### Alternative 4: Query Backend Inference Engine (e.g., vLLM) for Tokenization

Query the backend inference engine pods for tokenization before routing or scheduling.

**Rejected Because:**
- **Influence on Backend Inference Engine**: Querying the backend inference engine for tokenization requests competes with actual model inference (prefill/decode) execution queues. High concurrency of tokenization tasks can degrade the throughput and latency of the backend inference engine.
- **Network Latency**: Even though the inference engine is in the cluster, querying it introduces network round-trips over the cluster network (adding 10-50ms latency), which degrades the scoring performance of the KV cache-aware plugin.
- **Duplicate Tokenizing**: The backend engine would still have to tokenize the raw prompt again during the prefill phase unless we manage complex token pass-throughs, resulting in redundant processing overhead.

---

### Conclusion

This proposal delivers **low-latency, local tokenization** that enables:
- **Performance** — ~90% reduction in tokenization latency vs. remote services
- **Separation & Isolation** — independent scaling, lifecycle management, and zero resource contention/influence on backend inference engines
- **Autoscaler Support** — accurate token counts for intelligent scaling decisions (issue #1100)
- **KV Cache Integration & Scheduling** — fast prefix matching and consecutive block scoring for cache-aware request routing
- **Bypassing Redundant Work** — de-duplicated tokenization via direct token ID pass-throughs to backend engines
- **Backward Compatibility** — existing models continue to work with heuristics

The sidecar design is **minimal, core infrastructure** — operators can enable it for production workloads and disable it for development.

---

### Related Issues

- **#1100** — Autoscaler integration (requires accurate token counts)
- **#1266** — Token accuracy discussion
- **#1161** — Original token counting proposal

---

### Appendix: Configuration Examples

#### Example 1: High-Throughput Production Deployment

```yaml
# values.yaml
kthenaRouter:
  tokenizerSidecar:
    enabled: true
    image: tokenizer-sidecar:v1.0
    resources:
      requests:
        memory: "512Mi"
        cpu: "250m"
      limits:
        memory: "2Gi"
        cpu: "1000m"

---
# model-qwen.yaml
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: qwen-production
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"
spec:
  model:
    model_id: "hf:Qwen/Qwen3.5-397B-A17B"
  inference:
    # inference config
```

#### Example 2: Development Environment (No Tokenizer Overhead)

```yaml
# values.yaml
kthenaRouter:
  tokenizerSidecar:
    enabled: false  # Minimal resource usage

---
# model-dev.yaml
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: qwen-dev
  # No tokenizer annotation; uses heuristic
spec:
  model:
    model_id: "hf:Qwen/Qwen3.5-397B-A17B"
```

#### Example 3: Multi-Model Deployment with Mixed Tokenization

```yaml
# values.yaml
kthenaRouter:
  tokenizerSidecar:
    enabled: true
    resources:
      limits:
        memory: "2Gi"  # Accommodate multiple tokenizers

---
# Critical model: require accurate tokenization
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: qwen-critical
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"
spec:
  model:
    model_id: "hf:Qwen/Qwen3.5-397B-A17B"

---
# Standard model: use heuristic
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: llama-standard
  # No annotation
spec:
  model:
    model_id: "hf:meta-llama/Llama-2-70b"

---
# Experimental model: local tokenizer on PVC
apiVersion: kthena.volcano.sh/v1
kind: ModelServer
metadata:
  name: custom-model
  annotations:
    kthena.volcano.sh/tokenizer-enabled: "true"
spec:
  model:
    model_id: "local:custom-tokenizer"  # Mounted PVC
```