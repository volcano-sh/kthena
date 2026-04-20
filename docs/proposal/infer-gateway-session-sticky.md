---
title: Session Sticky (Session Affinity) for the Model Router in infer-gateway
authors:
- "@FAUST-BENCHOU"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-04-01

---

## Session Sticky (Session Affinity) for the Model Router in infer-gateway

### Summary

This proposal specifies **session sticky** (session affinity) for the **model router** in **infer-gateway**: requests that present the same **session key** are routed to the same **ModelServer Pod** (by Pod name), for as long as the mapping is valid under TTL and the Pod remains in the router’s eligible endpoint set. The feature is **opt-in** on `ModelRoute`, composes with existing scheduling and plugins, and uses an explicit **session → Pod name** mapping with **TTL**, backed by **process memory** or **Redis** for multi-replica gateways. **Failover** clears or replaces stale bindings when the mapped Pod is no longer selectable.

### Motivation

Session stickiness matters for AI inference when operators need to:

- Preserve **conversation context** across multiple HTTP requests.
- Keep **model or cache state** on a specific instance for related calls.
- Obtain **stable behavior** for stateful or quasi-stateful models.
- Reduce latency by **reusing instance-local caches**.

Kubernetes Service `sessionAffinity: ClientIP` only keys on **source IP** and does not support HTTP headers, JWT claims, cookies, or query parameters. Affinity is therefore defined at the **gateway/router** layer: bind a session key to a backend for a **TTL**, then re-select when the binding is missing, expired, or invalid—analogous in spirit to kube-proxy’s session affinity, with a richer key.

#### Goals

1. Session sticky is implemented in the **model router**, integrated with existing route matching and scheduling.
2. **Backward compatibility**: if sticky is disabled or not configured, behavior is unchanged.
3. **Session key** can be derived from **HTTP headers**, **query parameters**, **cookies**, or **JWT claims**, using an **ordered list** of sources on the `ModelRoute`.
4. When a mapping exists and the target Pod is still among filtered, schedulable endpoints, **the same session key routes to that Pod**.
5. **Declarative configuration** on **`ModelRoute.spec.sessionSticky`**, with admission validation for consistency.
6. **Horizontal scaling** of gateway replicas via a **shared Redis** mapping store and **TTL** on keys.
7. **Failover**: if the mapped Pod is not available to the router, the stale mapping is removed (or bypassed), a new Pod is chosen, a new mapping is written, and the event is **logged**.

#### Non-goals (this version)

- Session sticky for **PD disaggregation** mode: affinity applies only when the router is scheduling **aggregated** ModelServer Pods (`PDGroup` absent). PD paths ignore sticky hints by design.

### Proposal

#### Architecture

1. **Session key extraction**  
   For a matched `ModelRoute` with sticky enabled, the router evaluates **`spec.sessionSticky.sources` in list order** and uses the **first non-empty** string as the session key. Types:
   - **Header** / **Query** / **Cookie**: read from the incoming request.
   - **JWTClaim**: read a string claim from the **Authorization Bearer** token using the **same JWKS and validation path** as the router’s JWT authenticator. If JWT auth is disabled, the token is missing/invalid, or the claim is absent, the value is treated as empty for that source.

2. **Session-to-Pod mapping**  
   The mapping is **`session_key` → Pod name** (the name of the Kubernetes Pod selected as upstream). Redis keys derive from the route identity and a **hash of the session key** so raw session material is not stored in the key. Stores:
   - **Memory** (`SessionStickyBackend: Memory`): single-process; suitable for single replica or development.
   - **Redis** (`SessionStickyBackend: Redis`): shared across replicas; value is Pod name; **TTL** matches `sessionAffinitySeconds` (default **10800** seconds).

3. **TTL refresh**  
   The store exposes **Get**, **Set**, and **Delete**. There is no separate **RefreshTTL** RPC: after each successful backend choice for a request that has a non-empty session key, the router **Set**s the mapping again with the **full configured TTL**, which refreshes expiry on Redis and in-memory backends.

4. **Routing**  
   - Sticky **enabled**, **non-empty** session key, **aggregated** mode: **Get** mapping; if the mapped Pod name appears in the current filtered pod list, the scheduler **pins** selection to that Pod (still running score plugins on that candidate).  
   - If the mapped Pod is **not** in the filtered set: **Delete** the mapping (when applicable), **log**, then run normal scheduling and **Set** a new mapping for the chosen Pod.  
   - Empty session key, sticky disabled, or **PD disaggregation**: existing scheduling only.

#### User stories

**Story 1** — An operator sets `sessionSticky` on a `ModelRoute` with a header source `X-Session-ID`. Requests with the same header value hit the same ModelServer Pod until TTL expires or that Pod leaves the endpoint set.

**Story 2** — An operator runs **multiple infer-gateway replicas**, sets `backend: Redis` and `redis.address`, and relies on the shared store so the same session key sticks regardless of which replica handles the request.

#### Notes / constraints


#### Risks and mitigations


### Design details

#### Components

1. **Extraction**  
   Implemented in the router’s load-balancing path for the resolved `ModelRoute`, once headers, URI, cookies, and (for JWT) the shared authenticator are available.

2. **Mapping store**  
   Interface: **Get**, **Set**, **Delete**. Implementations: in-memory map with per-entry expiry; Redis string keys with TTL.

3. **Scheduling integration**  
   The scheduler framework carries an optional **sticky Pod name**. If set and that Pod remains in the filtered set, aggregated scheduling **prefers** that Pod; otherwise the hint is cleared and normal scoring applies.

4. **Failover**  
   Structured logs when a mapped Pod is no longer valid; mapping removed before re-selection.

5. **`ModelRoute` API (`spec.sessionSticky`)**

| Field | Purpose |
|-------|---------|
| `enabled` | Turns sticky routing on for this route. |
| `sessionAffinitySeconds` | TTL for the binding; optional, default **10800**; minimum **1** when set. |
| `backend` | `Memory` (default) or `Redis`. |
| `redis` | Required when `backend` is `Redis`: **`address`** as `host:port`. |
| `sources` | Ordered list (max **16**) of **`SessionKeySource`**: **`type`** (`Header`, `Query`, `Cookie`, `JWTClaim`) and **`name`** (header name, query key, cookie name, or claim name). |

6. **Validation**  
   Validating webhook: when `enabled` is true, **`sources` must be non-empty**; if `backend` is **Redis**, **`redis.address` must be set**; **`sessionAffinitySeconds`**, if set, must be **≥ 1**.

#### Observability

- **Logs**: session sticky store errors, failover when mapped Pod is not in endpoints, Redis connection issues.
- **Response header**: **`X-Kthena-Backend-Pod`** exposes the selected upstream Pod name on the router response (useful for tests and debugging).

### Test plan

#### Unit tests

- Session key extraction: **Header**, **Query**, `enabled: false` → empty key, and **source ordering** (first non-empty wins).
- In-memory store **Set** / **Get** with TTL.

#### End-to-end acceptance (`test/e2e/router/`)
It should be noted that the relevant scoring plugins need to be disabled for the related e2e tests.

| ID | Scenario | Expected outcome |
|----|----------|------------------|
| E2E-SS-01 | **Basic stickiness**: sticky enabled; repeated requests with the same configured header session key. | All requests reach the **same** Pod. |
| E2E-SS-02 | **Isolation**: two different session keys; then reuse the first key. | Each key stays on its own Pod; returning to the first key does **not** adopt the second key’s binding. |
| E2E-SS-03 | **No key**: sticky enabled; requests omit the configured header. | No sticky error; over many requests, at least **two** distinct backend Pods (spread like plain LB). |
| E2E-SS-04 | **`enabled: false`**: same session header on every request (sources still configured). | At least **two** distinct backend Pods observed over many requests (header does not pin). |
| E2E-SS-05 | **Query** source only. | Same query value → same Pod. |
| E2E-SS-06 | **Cookie** source. | Same cookie → same Pod. |
| E2E-SS-07 | **TTL**: short `sessionAffinitySeconds`; requests before and after expiry. | Within TTL, session stays on one Pod; after expiry, behavior allows **re-binding** (test allows same Pod by chance but asserts the TTL path). |
| E2E-SS-08 | **Failover**: delete the Pod currently selected for a sticky session; retry with the same session key. | Request succeeds; selected Pod is **no longer** the deleted one (new healthy backend). |
| E2E-SS-09 | **Redis** with **two** router replicas sharing one Redis. | Same session key → **same** Pod regardless of which replica handles the request. |
| E2E-SS-10 | **Admission**: `sessionSticky.enabled: true` with **empty** `sources`. | Create **rejected** (`Invalid` or `BadRequest`). |
| E2E-SS-11 | **Precedence**: header and query both present; list order picks header before query. | The **first** matching source in `sources` wins (header value used). |

### References

- Kubernetes kube-proxy session affinity (conceptual analog): `pkg/proxy/iptables/proxier.go` (`writeServiceToEndpointRules`, `recent` module for `ClientIP`).
- Kthena router E2E: `test/e2e/router/`.
