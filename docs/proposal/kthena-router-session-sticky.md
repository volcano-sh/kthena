---
title: Session Sticky (Session Affinity) for Kthena Router
authors:
- "@FAUST-BENCHOU"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-04-01

---

## Session Sticky (Session Affinity) for Kthena Router

### Summary

This proposal specifies **session sticky** (session affinity) for **Kthena Router**: after `ModelRoute` selects a `ModelServer`, requests that present the same **session key** are routed to the same backend of that `ModelServer`, for as long as the mapping is valid under TTL and the backend remains selectable. For an aggregated ModelServer, the backend is one Pod. For a PD-disaggregated ModelServer, the backend is the complete **Prefill/Decode Pod pair** selected from the same PD group. The feature is **opt-in** on `ModelServer` (per-server **sources** and **TTL** only). The mapping store is **in-process memory** or **shared Redis**, selected by **router configuration**, not by the CRD. **Failover** clears or replaces stale bindings when the mapped Pod or pair is no longer selectable.

Session sticky does **not** override `ModelRoute` weighted selection among ModelServers. This matches Istio: VirtualService (our `ModelRoute`) chooses the destination; DestinationRule traffic policy (our `ModelServer.spec.trafficPolicy`) applies affinity inside that destination.

### Motivation

Session stickiness matters for AI inference when operators need to:

- Preserve **conversation context** across multiple HTTP requests.
- Keep **model or cache state** on a specific instance for related calls.
- Reduce latency by **reusing instance-local caches**.

Kubernetes Service `sessionAffinity: ClientIP` only keys on **source IP**. Affinity is therefore defined at the **Kthena Router** layer: bind a session key to a backend Pod for a **TTL**, then re-select when the binding is missing, expired, or invalid.

#### Goals

1. Session sticky is implemented in **Kthena Router**, applied **after** `ModelRoute` matching and destination selection, then integrated with existing scheduling.
2. **Backward compatibility**: if **`spec.trafficPolicy.sessionSticky` is nil/omitted**, behavior is unchanged.
3. **Session key** can be derived from **HTTP headers**, **query parameters**, **cookies**, or **JWT claims**, using an ordered list of sources.
4. When a mapping exists and the backend is still selectable **in the already-chosen ModelServer**, the same session key routes to that Pod or complete PD pair.
5. **Declarative configuration** via optional **`ModelServer.spec.trafficPolicy.sessionSticky`**. Store backend is **not** on the CRD.
6. **Horizontal scaling**: all router replicas share the same Redis store when Redis is configured.
7. **PD support**: for a PD ModelServer, bind the session to a complete Prefill/Decode pair from the same PD group, preserving the pair for subsequent requests.
8. **Failover**: if the mapped Pod or either side of the mapped PD pair is not available, the stale mapping is removed, a new backend or pair in the same ModelServer is chosen, and the event is logged.

#### Non-goals (this version)

- Pinning a session across **multiple ModelServers**. Weighted / canary selection on `ModelRoute` stays independent of sticky.
- Choosing a different PD pair solely for KV-cache scoring when the bound pair remains selectable. A valid bound pair is pinned and Score is skipped; normal scoring applies only when no valid binding exists.

### Proposal

#### User stories

**Story 1** — An operator sets **`spec.trafficPolicy.sessionSticky`** on a `ModelServer` with a header source `X-Session-ID`. After `ModelRoute` selects that ModelServer, requests with the same header value hit the same Pod, or the same Prefill/Decode pair for a PD ModelServer, until TTL expires or the backend leaves the selectable endpoint set.

**Story 2** — An operator runs **multiple Kthena Router replicas** with **Redis** enabled in router config (not on the CRD). The same session key sticks to the same Pod, or complete PD pair, of that ModelServer regardless of which replica handles the request.

**Story 3** — A `ModelRoute` splits traffic 50/50 between two ModelServers. The same sticky header does **not** collapse that split: sticky only pins the Pod **inside** whichever ModelServer the route selected for that request.

#### Architecture

Session sticky is **not** a scheduler plugin. It looks up a binding before `Schedule`, supplies an aggregated Pod or PD pair hint to the scheduler, and commits the selected backend.

**Request path**

1. Match `ModelRoute` and select a destination by **rule weights**. Sticky does not rematch or prefer a ModelServer.
2. If the chosen `ModelServer` has `trafficPolicy.sessionSticky`, take the first non-empty source (`Header` / `Query` / `Cookie` / `JWTClaim`) as the session key. Empty key: skip sticky.
3. `Get` binding keyed by **ModelServer identity + session key**. If present, pass the bound Pod or complete PD pair into `Schedule`.
4. `Schedule` (aggregated, non-PD): Filter runs. If `StickyPodName` is still in the list, `BestPods` is that Pod and Score is skipped. Otherwise the pin is cleared and Score runs.
5. `Schedule` (PD sticky): same pin-and-skip-Score rule as aggregated, but for a complete pair:
   1. Require both sticky Decode and Prefill names; a half pair is never preferred.
   2. Sticky Decode must remain in the filtered decode list.
   3. Load Prefills in that Decode's PD group and run Filter on them.
   4. Sticky Prefill must still be selectable in that group.
   5. Both sides OK → set `DecodePods`/`PrefillPods` to that pair only and return (no Score, no topN fill).
   6. Otherwise clear sticky names and score new pairs normally.
6. `Commit` the selected backend with the ModelServer TTL (store details below), then proxy the request using the existing retry behavior.

**How this interacts with other scheduling factors**

| Case | What happens |
|------|----------------|
| Sticky Pod **survives Filter** | `BestPods` is that Pod. Score plugins are skipped. |
| Sticky Pod **fails Filter** (overloaded, gone, …) | Pin is dropped. Score ranks the remaining Pods as usual. The new winner is committed. |
| No binding / empty session key | Filter then Score as today. The selected Pod or pair is committed if a key exists. |
| **PD** (`PDGroup` set) | Sticky binds the complete Prefill/Decode pair. A valid pair is pinned as a unit and Score is skipped; if either side is unavailable, the binding is cleared and a new pair is scored. |
| **Multi-target ModelRoute** | Each request still follows weights. Sticky for ModelServer A never forces traffic onto A when the route selected B. |

#### Session map storage

The store is a TTL map: **opaque key → Binding `{modelServer, pod, prefillPod}`**. `pod` remains the selected backend Pod for aggregated ModelServers and represents the Decode Pod for PD-disaggregated ModelServers; `prefillPod` is empty for aggregated ModelServers.

| Item | Value |
|------|--------|
| Key | `kthena/sticky/` + `sha256(namespace/name\|sessionKey)` (`ModelServer` identity + hashed session material; raw session key is not stored in the Redis/memory key) |
| Value | ModelServer **short name**, and for PD the **Prefill Pod** and **Decode Pod** names |
| TTL | `ModelServer.spec.trafficPolicy.sessionSticky.sessionAffinitySeconds` (default **300**); each scheduled sticky request **Set/Commit**s the full TTL (sliding expiry) |
| API | `Get` / `Delete` / `Commit`; no separate Refresh RPC |

**Memory** (default): process-local map plus a background sweeper. Suitable for single replica or tests. Replicas do **not** share bindings.

**Redis**: Hash fields `modelServer`, `pod`, and `prefillPod`, plus key TTL. Lua commit is atomic:

- missing → `HSET` + `EXPIRE`, return the new binding
- same binding → `EXPIRE` only (refresh)
- different binding → return the existing fields (do not overwrite)

Router config (not `ModelServer`):

```yaml
sessionSticky:
  backend: memory   # or redis
  redis:
    address: host:port
```

All replicas in a deployment must use the same backend. Redis mode fails fast at startup if address is missing or unreachable.

#### Notes / constraints

- Binding is scoped per `ModelServer` (namespace/name in the store key), not per `ModelRoute` and not cluster-global.
- The same session key on two ModelServers produces two independent bindings.
- ModelServer name in the binding is the same-namespace short name; Pod names are unique within that namespace.
- A PD binding is valid only when both Pods exist, pass the current filters, and belong to the same PD group. The binding is invalidated as a unit if either side fails these checks.

#### Risks and mitigations

- **Split brain without Redis**: memory store is per process; multi-replica must use Redis.
- **Stale Pod or PD pair**: filter miss clears the binding; commit writes the newly selected Pod or PD pair.
- **Concurrent first request**: Redis keeps the first writer's binding. The in-flight request still uses its selected Pod or pair. Later requests follow the stored binding.

### Design details

#### `ModelServer` API

**`sessionSticky` is a pointer** (`omitempty`) on `TrafficPolicy`. Nil/omitted = off; non-nil = on. No `enabled` boolean.

| Field | Purpose |
|-------|---------|
| `sessionAffinitySeconds` | TTL in seconds; optional, default **300**; minimum **1** when set. |
| `sources` | Ordered list (max **16**). Evaluated in order; the first non-empty value is the session key. Later sources are ignored. Required and non-empty when `sessionSticky` is set. |

```go
type TrafficPolicy struct {
	Timeout       *metav1.Duration `json:"timeout,omitempty"`
	Retry         *Retry           `json:"retry,omitempty"`
	SessionSticky *SessionSticky   `json:"sessionSticky,omitempty"`
}

type SessionSticky struct {
	SessionAffinitySeconds *int32             `json:"sessionAffinitySeconds,omitempty"`
	Sources                []SessionKeySource `json:"sources,omitempty"`
}

type SessionKeySourceType string

const (
	SessionKeySourceHeader   SessionKeySourceType = "Header"
	SessionKeySourceQuery    SessionKeySourceType = "Query"
	SessionKeySourceCookie   SessionKeySourceType = "Cookie"
	SessionKeySourceJWTClaim SessionKeySourceType = "JWTClaim"
)

type SessionKeySource struct {
	Type SessionKeySourceType `json:"type"`
	Name string               `json:"name"`
}
```

Header names are case-insensitive; Cookie names are case-sensitive.

#### Validation

- **Webhook (`ModelServer`)**: when `spec.trafficPolicy.sessionSticky` is non-nil, `sources` must be non-empty; `sessionAffinitySeconds`, if set, must be ≥ 1. No cross-object PD rejection at admission (object lifecycle ordering).
- **Router**: validates memory vs Redis and Redis address/connectivity at startup.

#### Observability

- **Logs**: store errors, failover when a mapped Pod or PD pair is not selectable, and Redis issues.
- **E2E visibility**: PD E2E reads the complete binding from its configured Redis store. No test-only access-log field is added for either Pod.

### Test plan

#### Unit tests

- Session key extraction: Header, Query; source ordering; nil `sessionSticky`; all sources empty.
- In-memory store Set/Get/Commit with TTL; Redis commit does not overwrite a different winner; PD bindings require a valid Prefill/Decode pair.
- PD scheduling pins a valid bound pair (skips Score) and rejects a pair when either side is unavailable or belongs to a different PD group. A half pair is never preferred.

#### End-to-end acceptance (`test/e2e/router/`)

Scoring plugins that would hide stickiness should be disabled for these cases. E2E that asserts backend identity requires the debug `X-Kthena-Backend-Pod` flag.

Sticky E2E uses a dedicated `ModelServer` (same backend pods as the 1.5B mock) so the shared 1.5B `ModelServer` stays unsticky for other tests.

| ID | Scenario | Expected outcome |
|----|----------|------------------|
| E2E-SS-01 | Header, Query, and Cookie sources; Header listed first when Header and Query are both present. | Same session key pins the same Pod or complete PD pair; Header wins over Query. |
| E2E-SS-02 | Two session keys, then reuse the first. | Each key stays on its Pod; first key does not adopt the second. |
| E2E-SS-03 | `sessionSticky` set; header omitted. | No error; spread across at least two Pods. |
| E2E-SS-04 | `sessionSticky` absent on the selected ModelServer; sticky-like header present. | Header does not pin; at least two Pods. |
| E2E-SS-05 | Short TTL; requests before and after expiry. | Sticky within TTL; re-bind after expiry. |
| E2E-SS-06 | Delete the bound Pod; retry same key. | New healthy Pod or, for PD, a new valid pair. |
| E2E-SS-07 | `sessionSticky` non-null with empty `sources` on ModelServer. | Admission rejected. |
| E2E-SS-08 | PD ModelServer with `sessionSticky` set; repeat the same session key, then make the bound Prefill Pod unselectable and retry. | The same Prefill/Decode pair is selected while both Pods remain selectable; the stale pair is replaced with a new valid pair after Prefill becomes unavailable. |
| E2E-SS-09 | `ModelRoute` 50/50 across a sticky ModelServer and another ModelServer; same sticky header. | Traffic still spreads across both ModelServers. |
| E2E-SS-10 | Two router replicas + Redis store with an aggregated or PD ModelServer. | The same session key resolves to the same Pod or complete PD pair on both replicas. |

### References

- Kubernetes kube-proxy session affinity (conceptual analog).
- Istio DestinationRule consistent-hash load balancing (affinity on the destination, not the route).
- Kthena Router E2E: `test/e2e/router/`.
- Implementation: `pkg/kthena-router/sessionsticky/`, `pkg/kthena-router/router/router.go`, and aggregated/PD sticky handling in `pkg/kthena-router/scheduler/scheduler_impl.go`.
