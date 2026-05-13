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

This proposal specifies **session sticky** (session affinity) for **Kthena Router**: after `ModelRoute` selects a `ModelServer`, requests that present the same **session key** are routed to the same **Pod of that ModelServer**, for as long as the mapping is valid under TTL and the Pod remains selectable. The feature is **opt-in** on `ModelServer` (per-server **sources** and **TTL** only). The mapping store is **in-process memory** or **shared Redis**, selected by **router configuration**, not by the CRD. **Failover** clears or replaces stale bindings when the mapped Pod is no longer selectable.

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
4. When a mapping exists and the Pod is still selectable **in the already-chosen ModelServer**, the same session key routes to that Pod.
5. **Declarative configuration** via optional **`ModelServer.spec.trafficPolicy.sessionSticky`**. Store backend is **not** on the CRD.
6. **Horizontal scaling**: all router replicas share the same Redis store when Redis is configured.
7. **Failover**: if the mapped Pod is not available, the stale mapping is removed, a new Pod in the same ModelServer is chosen, and the event is logged.

#### Non-goals (this version)

- Session sticky for **PD disaggregation**. If the resolved ModelServer has `WorkloadSelector.PDGroup`, sticky is **bypassed at runtime** and a warning is logged.
- Pinning a session across **multiple ModelServers**. Weighted / canary selection on `ModelRoute` stays independent of sticky.
- **Future work**: PD-specific session sticky semantics in a follow-up proposal.

### Proposal

#### User stories

**Story 1** — An operator sets **`spec.trafficPolicy.sessionSticky`** on a `ModelServer` with a header source `X-Session-ID`. After `ModelRoute` selects that ModelServer, requests with the same header value hit the same Pod until TTL expires or that Pod leaves the endpoint set.

**Story 2** — An operator runs **multiple Kthena Router replicas** with **Redis** enabled in router config (not on the CRD). The same session key sticks to the same Pod of that ModelServer regardless of which replica handles the request.

**Story 3** — A `ModelRoute` splits traffic 50/50 between two ModelServers. The same sticky header does **not** collapse that split: sticky only pins the Pod **inside** whichever ModelServer the route selected for that request.

#### Architecture

Session sticky is **not** a scheduler plugin. It only **looks up a binding** before `Schedule`, **pins one Pod after Filter**, then **writes the chosen Pod** after `Schedule`. Filter and Score plugins are unchanged.

**Request path**

1. Match `ModelRoute` and select a destination by **rule weights**. Sticky does not rematch or prefer a ModelServer.
2. If the chosen `ModelServer` has `trafficPolicy.sessionSticky`, take the first non-empty source (`Header` / `Query` / `Cookie` / `JWTClaim`) as the session key. Empty key: skip sticky.
3. `Get` binding keyed by **ModelServer identity + session key**. If present, pass `StickyPodName = binding.Pod` into `Schedule`.
4. `Schedule` (aggregated, non-PD): **Filter plugins** run on that ModelServer's candidate list → if `StickyPodName` is still in that list, shrink it to that one Pod → **Score plugins** run on whatever list remains.
5. After a Pod is chosen, `Commit` `{ModelServer, Pod}` with the ModelServer TTL (store details below). Then proxy.

**How this interacts with other scheduling factors**

| Case | What happens |
|------|----------------|
| Sticky Pod **survives Filter** | Candidate list becomes that one Pod. Score still runs, but cannot pick a different Pod. |
| Sticky Pod **fails Filter** (overloaded, gone, …) | Pin is dropped. Score ranks the remaining Pods as usual. The new winner is committed. |
| No binding / empty session key | Filter then Score as today. First successful Pod is committed if a key exists. |
| **PD** (`PDGroup` set) | Sticky is skipped (no pin, no commit). PD Filter/Score is unchanged. |
| **Multi-target ModelRoute** | Each request still follows weights. Sticky for ModelServer A never forces traffic onto A when the route selected B. |

Pinning is **after Filter, before Score**. Other plugins are not reordered and do not need sticky-specific logic.

#### Session map storage

The store is a TTL map: **opaque key → Binding `{modelServer, pod}`**.

| Item | Value |
|------|--------|
| Key | `kthena/sticky/` + `sha256(namespace/name\|sessionKey)` (`ModelServer` identity + hashed session material; raw session key is not stored in the Redis/memory key) |
| Value | ModelServer **short name** and **Pod name** |
| TTL | `ModelServer.spec.trafficPolicy.sessionSticky.sessionAffinitySeconds` (default **300**); each successful request **Set/Commit**s the full TTL (sliding expiry) |
| API | `Get` / `Delete` / `Commit`; no separate Refresh RPC |

**Memory** (default): process-local map plus a background sweeper. Suitable for single replica or tests. Replicas do **not** share bindings.

**Redis**: Hash fields `modelServer` and `pod`, plus key TTL. Lua commit is atomic:

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
- ModelServer name in the binding is the same-namespace short name; Pod name is unique within that namespace.

#### Risks and mitigations

- **Split brain without Redis**: memory store is per process; multi-replica must use Redis.
- **Stale Pod**: filter miss clears the pin; commit writes the newly selected Pod.
- **Concurrent first request**: Redis Lua returns the first writer; the loser adopts that Pod if it is still selectable.

### Design details

#### `ModelServer` API

**`sessionSticky` is a pointer** (`omitempty`) on `TrafficPolicy`. Nil/omitted = off; non-nil = on. No `enabled` boolean.

| Field | Purpose |
|-------|---------|
| `sessionAffinitySeconds` | TTL in seconds; optional, default **300**; minimum **1** when set. |
| `sources` | Ordered list (max **16**) of `SessionKeySource`. **Required and non-empty when `sessionSticky` is non-nil**. |

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

- **Logs**: store errors, failover when mapped Pod is not selectable, Redis issues, PD bypass.
- **Response header (test/debug only)**: `X-Kthena-Backend-Pod` is off by default; enable only via a router debug flag for e2e.

### Test plan

#### Unit tests

- Session key extraction: Header, Query; source ordering; nil `sessionSticky`; all sources empty.
- In-memory store Set/Get/Commit with TTL; Redis commit does not overwrite a different winner.

#### End-to-end acceptance (`test/e2e/router/`)

Scoring plugins that would hide stickiness should be disabled for these cases. E2E that asserts backend identity requires the debug `X-Kthena-Backend-Pod` flag.

Sticky E2E uses a dedicated `ModelServer` (same backend pods as the 1.5B mock) so the shared 1.5B `ModelServer` stays unsticky for other tests.

| ID | Scenario | Expected outcome |
|----|----------|------------------|
| E2E-SS-01 | Header, Query, and Cookie sources; Header listed first when Header and Query are both present. | Same session key pins the same Pod; Header wins over Query. |
| E2E-SS-02 | Two session keys, then reuse the first. | Each key stays on its Pod; first key does not adopt the second. |
| E2E-SS-03 | `sessionSticky` set; header omitted. | No error; spread across at least two Pods. |
| E2E-SS-04 | `sessionSticky` absent on the selected ModelServer; sticky-like header present. | Header does not pin; at least two Pods. |
| E2E-SS-05 | Short TTL; requests before and after expiry. | Sticky within TTL; re-bind after expiry. |
| E2E-SS-06 | Delete the bound Pod; retry same key. | New healthy Pod. |
| E2E-SS-07 | `sessionSticky` non-null with empty `sources` on ModelServer. | Admission rejected. |
| E2E-SS-08 | PD ModelServer at runtime with `sessionSticky` set. | Sticky bypassed; warning log. |
| E2E-SS-09 | `ModelRoute` 50/50 across a sticky ModelServer and another ModelServer; same sticky header. | Traffic still spreads across both ModelServers. |
| E2E-SS-10 | Two router replicas + Redis store. | Same session key → same Pod on both replicas. |

### References

- Kubernetes kube-proxy session affinity (conceptual analog).
- Istio DestinationRule consistent-hash load balancing (affinity on the destination, not the route).
- Kthena Router E2E: `test/e2e/router/`.
- Implementation: `pkg/kthena-router/sessionsticky/`, `pkg/kthena-router/router/router.go`, scheduler `StickyPodName` pin in `pkg/kthena-router/scheduler/scheduler_impl.go`.
