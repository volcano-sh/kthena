# Local Tokenizer Sidecar Service for Performance and Architectural Separation

---

**title:** Local Tokenizer Sidecar Service for Performance and Architectural Separation

**authors:**
- @nXtCyberNet

**reviewers:**
- @hzxuzhonghu

**approvers:**
- @hzxuzhonghu

**creation-date:** 2026-06-16

**last-updated:** 2026-07-13

---

## Summary

This proposal introduces a containerized tokenizer service built with Python, FastAPI, and Hugging Face's tokenizers library. It supports two deployment modes:

- **UDS-based sidecar** (co-located with the router)
- **Independent HTTP service** for flexible scaling

The service exposes clean endpoints for tokenization and delivers the following improvements:

- **Latency & Scheduling:** Enables the KV cache-aware scheduling plugin to perform rapid and accurate token counting for pod scoring before routing.
- **Inference Isolation:** Offloads tokenization from backend inference engines (e.g., vLLM), preventing tokenization requests from consuming execution queues or slots.
- **Accurate Rate Limiting:** Replaces the crude `len(prompt)/4` heuristic with precise token counts for quota enforcement.
- **KV Cache Optimization:** Supplies exact token counts to the scheduler for better load balancing and cache-aware decisions.
- **Optional Optimization:** Can forward `prompt_token_ids` to supported backends (disabled by default, opt-in via env).
- **Architectural Separation:** Decouples tokenizer lifecycle from model serving, allowing independent scaling, upgrades, and caching.

---

## Motivation

Today's rate limiting relies on the inaccurate `len(prompt) / 4` heuristic, which lacks the precision needed for production workloads and creates redundant computation across the request pipeline.

The KV cache-aware scheduling plugin already requires complete token sequences for intelligent pod scoring. This proposal performs tokenization once during scheduling, then reuses the results for both scheduling and rate limiting — eliminating duplicate work and replacing heuristics with exact token accounting.

Tokenizer instances are automatically managed through ModelServer lifecycle events (create, update, delete). The router loads, reloads, or unloads the appropriate tokenizer without any manual intervention.

---

## Performance Bottlenecks & Interference

- **Network Latency & Scheduler Blocking:** Calling remote or in-cluster tokenization endpoints adds 50-200ms latency, directly blocking routing decisions. Under load (queue depth, slow model loading, or CPU saturation), additional queuing can add 10-50ms per request, cascading delays across the scheduling path.

- **Backend Resource Contention:** Using the inference engine (vLLM) for tokenization causes tokenization requests to compete with prefill and decode tasks for GPU slots and queues, degrading overall inference performance.

- **Redundant Tokenization:** The same prompt is currently tokenized up to three times - by the rate limiter, KV cache scheduler (pod scoring), and backend during prefill. This wastes CPU cycles and adds 7-14ms of unnecessary latency per request. At 10k RPS, this creates 70-140 seconds of aggregate overhead per second.

- **Lost Token IDs:** Each component independently tokenizes the prompt and discards the results, forcing repeated work and breaking context between layers.

---

## Architectural Constraints

- **Tight Coupling:** Quota enforcement and scheduling logic are currently coupled with model inference backends.
- **Cache-Aware Limitations:** The KV cache scheduler requires accurate token sequences for block matching. Without a local tokenizer, it must call the slower vLLM endpoint, especially costly for long prompts.

---

## Goals

- Deliver low-latency local tokenization (<1ms via UDS sidecar, 1-2ms via HTTP service).
- Enable fast, accurate token counting for the KV cache-aware scheduler before routing.
- Achieve single-pass tokenization with reuse: rate limiter → KV cache plugin → optional prompt_token_ids forwarding to backend.
- Fully decouple tokenization from inference engines to eliminate resource contention.
- Support independent tokenizer lifecycle, scaling, and upgrades.
- Flexibly support multiple tokenizer sources (PVC, Hugging Face Hub, ModelScope, or model_repo_url annotation).
- Provide two deployment modes (UDS sidecar or independent HTTP) selectable per ModelServer via annotation.
- Make prompt_token_ids forwarding to vLLM optional and configurable per model.

### Non-Goals

- Support for external MaaS tokenization endpoints (remain available as fallback)
- Compliance/billing-specific accuracy requirements
- Modify existing ModelRoute behavior for models without tokenizer annotation

---

## Proposal

### 1. UDS-Based Sidecar (co-located in Router Pod)

- Tokenizer runs as a sidecar container in the same pod as the router
- Communication via Unix Domain Socket (`/tmp/tokenizer.sock`)
- Ultra-low latency (<1ms), zero network overhead
- Cannot scale independently from the router

### 2. HTTP-Based Independent Service (Separate Pods)

- Tokenizer runs as its own Kubernetes Deployment
- Communication via HTTP through cluster Service DNS
- Low latency (1-2ms)
- Supports independent scaling and fault isolation

---

## Architecture

### UDS-Based Sidecar Model

The router pod contains two containers sharing a tmpfs volume for the Unix socket:

- **Router container (Go):** Handles rate limiting, routing, and scheduling
- **Tokenizer sidecar (Python + FastAPI):** Performs tokenization

#### Data Flow

```
Request → Rate Limiter → Manager.Encode() → UDS socket → Tokenizer returns token count (<1ms)
```

**Benefits:**
- Minimal latency
- Simple deployment
- No network calls

**Drawbacks:**
- Shares resources with router
- No independent scaling
- Pod restart affects both

---

### HTTP-Based Independent Service Model

Tokenizer runs as a separate Deployment in the same namespace:

- Router connects to tokenizer Service (port 8080) via cluster DNS
- Supports multiple replicas with load balancing
- Optional shared PVC for model storage

#### Data Flow

```
Request → Rate Limiter → HTTP POST to tokenizer Service → Returns token count (1-2ms)
```

**Benefits:**
- Independent scaling
- Fault isolation
- Flexible node placement

**Drawbacks:**
- Slightly higher latency due to network
- More Kubernetes objects to manage

---

## Core Design Principles

- **Local-First:** Tokenizer runs either in-pod (UDS sidecar) or nearby (HTTP service) - avoiding external network calls.
- **Automatic Lifecycle Management:** Tokenizer is automatically loaded, reloaded, or unloaded based on ModelServer create/update/delete events.
- **Strong Isolation:** Tokenization is fully decoupled from inference backends, eliminating GPU/CPU resource contention.
- **Single-Pass Tokenization:** Tokenization results are reused across rate limiter and KV cache scheduler.
- **Flexible Model Sources:** Supports PVC-mounted models, Hugging Face Hub, ModelScope, and model_repo_url annotation.

---

## Design Details

### Model URI Propagation from ModelBooster

ModelBooster automatically injects the resolved model repository URI into the ModelServer as an annotation:

```yaml
annotations:
  kthena.volcano.sh/model-repo-id: "hf://Qwen/Qwen3.5-397B-A17B"
```

The router watches ModelServer lifecycle events and loads/unloads the corresponding tokenizer. It falls back to `ModelServer.Spec.Model` for backward compatibility.

---

### Deployment Mode Selection

Operators configure the mode via Helm values:

#### UDS Sidecar Mode

```yaml
kthenaRouter:
  tokenizer:
    mode: "uds"
    sidecar:
      enabled: true
      image: tokenizer-sidecar:v1.0
```

#### HTTP Independent Mode

```yaml
kthenaRouter:
  tokenizer:
    mode: "http"

tokenizer:
  replicas: 2
  image: tokenizer-service:v1.0
```

**Important:** UDS mode requires the tokenizer to run as a sidecar in the same pod. HTTP mode is mandatory for separate pods. Mismatched configurations will cause failures.

---

### Implementation Details

- **UDS Sidecar:** Uses Unix Domain Socket (`/tmp/tokenizer.sock`) with JSON-line protocol. Delivers synchronous sub-millisecond responses.
- **HTTP Service:** Built with FastAPI + uvicorn. Uses standard REST endpoints (`/v1/load`, `/v1/encode`, `/v1/unload`, `/health`), connection pooling, configurable timeouts, and Kubernetes probes.

---

### Rate Limiter Integration

```go
if tokenizer.IsAnnotated(modelServerID) {
    count, _ := tokenizer.Encode(modelServerID, prompt)  // UDS or HTTP
    quotaUsed = count
} else {
    quotaUsed = len(prompt) / 4  // fallback
}
```

---

### KV Cache Scheduling Integration

The KV cache-aware scheduler plugin calls the local tokenizer for fast token counting:

- **UDS:** <1ms latency
- **HTTP:** 1-2ms latency

This enables accurate cache hit estimation without involving the inference backend.

---

### Token ID Forwarding to Backend (Optional)

Disabled by default. Can be enabled globally via Helm or per-model via annotation:

```
kthena.volcano.sh/forward-token-ids: "true"
```

When enabled:

- Router computes token IDs during rate limiting
- Forwards `prompt_token_ids` to compatible backends (supported by both vLLM and SGLANG)
- Backend skips redundant tokenization

This is kept opt-in due to added complexity and limited backend support.

---

## User Stories

### Story 1: High-Throughput Production with UDS Sidecar

A PaaS operator runs a multi-tenant LLM service handling 10k+ requests per second and needs ultra-low latency tokenization.

#### Flow

1. Operator enables UDS mode in Helm values.
2. Router pod starts with the tokenizer sidecar.
3. Operator adds `kthena.volcano.sh/tokenizer-enabled: "true"` annotation to production ModelServers.
4. Router automatically loads the tokenizer over UDS socket.
5. All requests use local tokenization (<1ms vs. previous 150ms remote).

**Result:** Reduces per-request latency by ~140ms at scale, significantly improving end-to-end latency and overall throughput.

---

### Story 2: Independent Scaling with HTTP Service

An operator needs to scale tokenization independently as request volume grows and the tokenizer becomes a bottleneck.

#### Flow

1. Operator configures HTTP mode and the tokenizer Service endpoint.
2. Router connects to the separate tokenizer Deployment (initially 2 replicas).
3. As CPU utilization on tokenizer pods reaches 80%, the operator scales up:
   ```bash
   kubectl scale deployment tokenizer --replicas=5
   ```
4. Traffic is automatically load-balanced across the additional pods.

**Result:** Tokenizer scales independently without restarting the router, maintaining stable performance under varying loads.

---

## Performance Analysis

### Latency Comparison

| Scenario | Latency | Impact |
|----------|---------|--------|
| **UDS Sidecar** | **<1 ms** | Minimal, direct Unix Domain Socket communication |
| **HTTP Independent** | **1-2 ms** | Low, limited to Kubernetes cluster networking |
| **Remote Tokenization** | **50-200 ms** | High, network latency and external service overhead |
| **Heuristic Estimate** | **<1 ms** | Very low latency, but inaccurate token estimation |

---

### Memory Footprint

#### UDS Sidecar

- **Single tokenizer:** 50-200 MB (model dependent)
- **Five loaded models:** 500 MB-1 GB
- **Recommended memory limit:** 2 GB (configurable via Helm)
- **Eviction policy:** LRU automatically unloads least recently used tokenizers

#### HTTP Independent

- Router pod has **zero tokenizer memory overhead**
- Each tokenizer pod consumes **500 MB-1 GB**
- Total memory usage scales linearly with the number of tokenizer replicas

---

### Scaling Strategy

#### UDS Sidecar

- One tokenizer instance per router pod
- Scales automatically with router replicas
- Best suited for consistent, high-throughput deployments
- Simpler operational model with minimal communication latency

#### HTTP Independent

- Supports multiple tokenizer replicas (2-5+)
- Each replica delivers **1-2 ms** tokenization latency
- Can be scaled independently using **Horizontal Pod Autoscaler (HPA)** or manual scaling
- Provides higher aggregate throughput and improved resource utilization

---

## Test Plan

### Unit Tests

- Manager.Load() loads tokenizer from configured source
- Manager.Encode() returns correct token count
- Manager.Encode() returns error when tokenizer is not loaded
- Manager.Unload() removes tokenizer from cache
- LRU cache evicts least recently used tokenizer
- RateLimiter uses tokenizer for annotated models
- RateLimiter falls back to heuristic for non-annotated models
- KV Cache plugin receives accurate token count
- Per-model configuration (mode, repository, forward-token-ids) is respected

### UDS Mode Tests

- Router communicates successfully with tokenizer through Unix Domain Socket
- Connection failure returns appropriate error
- Sidecar restart allows subsequent requests to succeed

### HTTP Mode Tests

- Router communicates successfully with tokenizer service
- HTTP timeout is handled correctly
- Service unavailable (HTTP 503) returns appropriate error

### Integration Tests

- Tokenizer service starts successfully
- `/v1/load` loads tokenizer
- `/v1/encode` returns correct token count
- `/v1/unload` removes tokenizer
- Annotation changes trigger tokenizer load/unload
- Both UDS and HTTP modes produce identical token counts

### End-to-End Validation

#### UDS Mode

1. Deploy router with tokenizer sidecar
2. Create annotated ModelServer
3. Verify accurate token counting
4. Restart sidecar and verify recovery

#### HTTP Mode

1. Deploy tokenizer service
2. Configure router for HTTP mode
3. Verify accurate token counting
4. Scale tokenizer replicas
5. Verify uninterrupted request processing

---

## Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Sidecar crashes (UDS mode) | Kubernetes restart policy with liveness and readiness probes |
| Tokenizer service unavailable (HTTP mode) | Deploy multiple replicas behind a Kubernetes Service with readiness probes |
| Tokenizer fails to load model | Return clear error, retain heuristic fallback for models without tokenizer enabled |
| Invalid or unreachable `model-repo-id` | Validate configuration during model load and surface descriptive errors |
| `forward-token-ids` enabled for unsupported backend | Feature is disabled by default; document supported backends |
| Memory growth from multiple loaded tokenizers | LRU cache with configurable memory limits and explicit unload support |
| Tokenizer request timeout | Configurable request timeout with retry through normal request flow |
| High tokenizer load | Scale router replicas (UDS) or tokenizer replicas independently (HTTP) |

---

## Alternatives Considered

### Alternative 1: Embedded Tokenizer in Router (Rejected)

Bundle tokenizer library directly into router binary (no sidecar).

**Rejection Rationale:** Pod memory increases by 50-200MB per model, cannot scale independently, difficult to upgrade separately, tight coupling conflicts with architecture goals.

---

## Conclusion

This proposal introduces accurate local tokenization for Kthena Router through two deployment models:

- **UDS Sidecar:** Ultra-low latency with minimal deployment overhead.
- **HTTP Independent Service:** Independent scaling, fault isolation, and flexible deployment.

The design provides:

- Accurate token accounting for rate limiting and quota enforcement
- Fast token counting for KV cache-aware scheduling
- Per-model tokenizer source selection (`model-repo-id`)
- Configurable deployment mode (UDS or HTTP)
- Optional forwarding of token IDs to compatible backends
- Backward compatibility through heuristic fallback for models without tokenizer enabled

Together, these changes remove dependence on remote tokenization services while maintaining flexibility for different deployment environments.