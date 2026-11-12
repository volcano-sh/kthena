# Session Sticky Routing

Session sticky pins requests that share the same **session key** to the same backend of a `ModelServer` for a configurable TTL. For an aggregated ModelServer the backend is one Pod; for a PD-disaggregated ModelServer it is the complete Prefill/Decode pair from the same PD group.

Sticky applies **after** `ModelRoute` selects a ModelServer. It does not change weighted or canary selection among ModelServers.

## When to use

Use session sticky when follow-up requests should reuse the same backend (for example to keep a warm prefix / KV cache). It pairs well with [session boost](./session-boost), which only reorders the queue and does not pin Pods by itself.

Omit `trafficPolicy.sessionSticky` when you want normal load balancing inside the ModelServer.

## How it works

1. `ModelRoute` selects a destination ModelServer.
2. If that ModelServer has `sessionSticky`, the router evaluates `sources` in order and takes the first non-empty value as the session key. An empty key skips sticky for that request.
3. The router looks up a binding keyed by ModelServer identity + session key.
4. If a valid binding remains selectable, scheduling pins that Pod (or Prefill/Decode pair); the pinned backend bypasses scoring. Otherwise the stale binding is cleared and a new backend is chosen, then committed.
5. Each successful sticky schedule refreshes the TTL (sliding expiry).

## Configure ModelServer

Enable sticky with a non-nil `spec.trafficPolicy.sessionSticky`. There is no separate `enabled` flag.

| Field | Description |
| ----- | ----------- |
| `sources` | Ordered list (max 16). Required and non-empty when sticky is set. First non-empty extraction wins. |
| `sessionAffinitySeconds` | Binding TTL in seconds. Optional; default **300**. Minimum **1** when set. |

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: deepseek-r1-1-5b-sticky
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      app: deepseek-r1-1-5b
  workloadPort:
    port: 8000
  model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B"
  inferenceEngine: "vLLM"
  trafficPolicy:
    timeout: 10s
    sessionSticky:
      sessionAffinitySeconds: 300
      sources:
        - type: Header
          name: X-Session-ID
        - type: Query
          name: session_id
        - type: Cookie
          name: session_sid
```

Admission rejects `sessionSticky` with an empty `sources` list.

### Session key sources

| `type` | `name` | Notes |
| ------ | ------ | ----- |
| `Header` | Header name | Case-insensitive. |
| `Query` | Query parameter | |
| `Cookie` | Cookie name | Case-sensitive. |
| `JWTClaim` | Claim name | Requires router JWT auth so the claim is available. |

## Store backend (router config)

The binding store is configured on the **router**, not on the ModelServer CRD.

| Backend | Behavior |
| ------- | -------- |
| `memory` (default) | Per-process map. Replicas do **not** share bindings. |
| `redis` | Shared across replicas. Startup fails if `redis.address` is missing or unreachable. |

Helm values:

```yaml
networking:
  kthenaRouter:
    sessionSticky:
      backend: memory   # or redis
      redis:
        address: "redis:6379"   # required when backend is redis
```

Equivalent ConfigMap snippet under `routerConfiguration`:

```yaml
sessionSticky:
  backend: redis
  redis:
    address: redis:6379
```

Use Redis whenever you run more than one router replica and need cross-replica stickiness.

## End-to-end example

Assume a `ModelRoute` targets `deepseek-r1-1-5b-sticky`, and sticky is configured with header `X-Session-ID` as above.

```bash
# Same session key → same backend (verify as below)
curl -s http://$ROUTER_IP/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: conv-1" \
  -d '{"model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B","messages":[{"role":"user","content":"Hi"}]}'

curl -s http://$ROUTER_IP/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: conv-1" \
  -d '{"model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B","messages":[{"role":"user","content":"Hi again"}]}'
```

Different session keys get independent bindings. Omitting the session key disables sticky for that request only; other requests for the ModelServer still stick when they present a key.

Confirm stickiness as follows:

- **Aggregated ModelServers**: compare `selected_pod` in router access logs across requests that share the session key.
- **PD-disaggregated ModelServers**: `selected_pod` is empty because PD scheduling does not set `BestPods`. With `backend: redis`, inspect the binding (`pod` / `prefillPod` hash fields) for the mapping key; otherwise use higher-verbosity router logs.

After the bound Pod (or either side of a PD pair) becomes unselectable, the next request with the same key rebinds and the store is updated.

## PD-disaggregated ModelServers

When the ModelServer has a `PDGroup`, sticky binds the **complete Prefill/Decode pair**. A half pair is never preferred. If either side fails filters or leaves the selectable set, the binding is cleared and a new pair is selected.

## Limitations

- Sticky does not override `ModelRoute` weights among ModelServers.
- Memory store is not shared across router replicas; use Redis for multi-replica affinity.
- Bindings are scoped per ModelServer. The same session key on two ModelServers yields two independent bindings.
- Design details: [session sticky proposal](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/kthena-router-session-sticky.md).
