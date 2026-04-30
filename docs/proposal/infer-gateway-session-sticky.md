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

This proposal specifies **session sticky** (session affinity) for the **model router** in **infer-gateway**: requests that present the same **session key** are routed to the same **ModelServer Pod** (by Pod name), for as long as the mapping is valid under TTL and the Pod remains in the router’s eligible endpoint set. The feature is **opt-in** on `ModelRoute` (per-route **sources** and **TTL** only), composes with existing scheduling and plugins, and uses an explicit **session → Pod name** mapping. Whether the mapping store is **in-process memory** or **shared Redis** is a **router process / deployment flag**, not part of the `ModelRoute` API. **Failover** clears or replaces stale bindings when the mapped Pod is no longer selectable.

### Motivation

Session stickiness matters for AI inference when operators need to:

- Preserve **conversation context** across multiple HTTP requests.
- Keep **model or cache state** on a specific instance for related calls.
- Obtain **stable behavior** for stateful or quasi-stateful models.
- Reduce latency by **reusing instance-local caches**.

Kubernetes Service `sessionAffinity: ClientIP` only keys on **source IP** and does not support HTTP headers, JWT claims, cookies, or query parameters. Affinity is therefore defined at the **gateway/router** layer: bind a session key to a backend for a **TTL**, then re-select when the binding is missing, expired, or invalid—analogous in spirit to kube-proxy’s session affinity, with a richer key.

#### Goals

1. Session sticky is implemented in the **model router**, integrated with existing route matching and scheduling.
2. **Backward compatibility**: if **`spec.sessionSticky` is nil/omitted** (the default), behavior is unchanged—no separate `enabled` flag.
3. **Session key** can be derived from **HTTP headers**, **query parameters**, **cookies**, or **JWT claims**, using an **ordered list** of sources under **`spec.sessionSticky`** when it is set.
4. When a mapping exists and the target Pod is still among filtered, schedulable endpoints, **the same session key routes to that Pod**.
5. **Declarative configuration** via an **optional** **`ModelRoute.spec.sessionSticky` pointer** (`omitempty` in JSON/YAML: absent or `null` means sticky is off; a non-null object means sticky is on for that route), with admission validation for consistency.
6. **Horizontal scaling** of gateway replicas: operators run the router with a **shared Redis** backing store (configured on the **router**), and **TTL** on keys; per-route CRD only names **how** to extract the session key and for how long bindings last (`sessionAffinitySeconds`).
7. **Failover**: if the mapped Pod is not available to the router, the stale mapping is removed (or bypassed), a new Pod is chosen, a new mapping is written, and the event is **logged**.

#### Non-goals (this version)

- Session sticky for **PD disaggregation** mode: affinity applies only when the router is scheduling **aggregated** ModelServer Pods (`PDGroup` absent). PD paths ignore sticky hints by design.

### Proposal

#### User stories

**Story 1** — An operator sets **`spec.sessionSticky`** (non-null) on a `ModelRoute` with a header source `X-Session-ID`. Requests with the same header value hit the same ModelServer Pod until TTL expires or that Pod leaves the endpoint set.

**Story 2** — An operator runs **multiple infer-gateway replicas** and starts each router with **Redis** enabled (address via **router** config/flags, not on `ModelRoute`); the same session key then sticks to the same Pod regardless of which replica handles the request.

#### Architecture

1. **Session key extraction**  
   For a matched `ModelRoute` with **non-nil `spec.sessionSticky`**, the router evaluates **`spec.sessionSticky.sources` in list order** and uses the **first non-empty** string as the session key. Types:
   - **Header** / **Query** / **Cookie**: read from the incoming request.
   - **JWTClaim**: read a string claim from the **Authorization Bearer** token using the **same JWKS and validation path** as the router’s JWT authenticator. If JWT auth is disabled, the token is missing/invalid, or the claim is absent, the value is treated as empty for that source.

2. **Session-to-Pod mapping**  
   The mapping is **`session_key` → Pod name** (the name of the Kubernetes Pod selected as upstream). Key derivation for shared stores: includes route identity and a **hash of the session key** so raw session material does not appear in the key.  
   The **pluggable store** (in-memory **vs** **Redis**) is selected by **router runtime flags** (see **Router process configuration** below): in-memory is suitable for single-replica or dev; Redis shares state across **infer-gateway** replicas. The stored value is Pod name; per-request **TTL refresh** uses each route’s **`sessionAffinitySeconds`** (default **10800** when unset in spec).

3. **TTL refresh**  
   The store exposes **Get**, **Set**, and **Delete**. There is no separate **RefreshTTL** RPC: after each successful backend choice for a request that has a non-empty session key, the router **Set**s the mapping again with the **full configured TTL**, which refreshes expiry on Redis and in-memory backends.

4. **Routing**  
   - **`sessionSticky` set**, **non-empty** session key, **aggregated** mode: **Get** mapping; if the mapped Pod name appears in the current filtered pod list, the scheduler **pins** selection to that Pod (still running score plugins on that candidate).  
   - If the mapped Pod is **not** in the filtered set: **Delete** the mapping (when applicable), **log**, then run normal scheduling and **Set** a new mapping for the chosen Pod.  
   - `sessionSticky` **nil/omitted**, **empty** session key after extraction, or **PD disaggregation**: existing scheduling only.

#### Notes / constraints


#### Risks and mitigations


### Design details

#### `ModelRoute` API

In Go/OpenAPI terms, **`sessionSticky` is a pointer to `SessionSticky`** (JSON **`omitempty` / YAML absence or `null`**). **Nil/omitted** = session sticky off; **non-nil** = on for that route. No `enabled` boolean: presence encodes the switch.

| Field | Purpose |
|-------|---------|
| `sessionAffinitySeconds` | TTL for the binding; optional, default **10800**; minimum **1** when set. |
| `sources` | Ordered list (max **16**) of **`SessionKeySource`**: **`type`** (`Header`, `Query`, `Cookie`, `JWTClaim`) and **`name`** (header name, query key, cookie name, or claim name). **Required and non-empty when `sessionSticky` is non-nil** (enforced by validation). |

Root spec: **`SessionSticky`** is a sibling of **`Rules`** / **`RateLimit`** on **`ModelRouteSpec`**.

```go
// ModelRouteSpec (excerpt) — session affinity is configured on the root spec.
type ModelRouteSpec struct {
	ModelName     string                      `json:"modelName,omitempty"`
	LoraAdapters  []string                    `json:"loraAdapters,omitempty"`
	ParentRefs    []gatewayv1.ParentReference `json:"parentRefs,omitempty"`
	Rules         []*Rule                     `json:"rules"`
	RateLimit     *RateLimit                  `json:"rateLimit,omitempty"`
	SessionSticky *SessionSticky              `json:"sessionSticky,omitempty"`
}

// SessionSticky — only per-route extraction + TTL; store backend is not here.
type SessionSticky struct {
	SessionAffinitySeconds *int32              `json:"sessionAffinitySeconds,omitempty"`
	Sources                []SessionKeySource  `json:"sources,omitempty"`
}

type SessionKeySourceType string

const (
	SessionKeySourceHeader   SessionKeySourceType = "Header"
	SessionKeySourceQuery    SessionKeySourceType = "Query"
	SessionKeySourceCookie   SessionKeySourceType = "Cookie"
	SessionKeySourceJWTClaim SessionKeySourceType = "JWTClaim"
)

// SessionKeySource defines one session key extraction rule.
type SessionKeySource struct {
	// Type of extraction.
	// +kubebuilder:validation:Required
	Type SessionKeySourceType `json:"type"`
	Name string               `json:"name"`
}
```

**Router: session key extraction** — iterate **`Sources` in order**; first non-empty value wins.

```go
func ExtractSessionKey(c *gin.Context, spec *networkingv1alpha1.SessionSticky, authenticator *auth.JWTAuthenticator) string {
	if spec == nil || len(spec.Sources) == 0 {
		return ""
	}
	for _, src := range spec.Sources {
		if v := extractOne(c, src, authenticator); v != "" {
			return v
		}
	}
	return ""
}
```

**`extractOne`** : resolves **one** `SessionKeySource` by **`Type`**—header, query, cookie, or JWT claim via **`authenticator`**—using **`Name`**, trimmed or **empty** if missing. **`ExtractSessionKey`** only walks **`Sources`** and returns the **first** non-empty value.

**Router process configuration (not `ModelRoute`)** — choose mapping backend and optional Redis, e.g. flags such as:

| Concern | Notes |
|--------|--------|
| Sticky **store** | **Memory** (default) vs **Redis**; must be the same for all router replicas in a given deployment (cluster ops concern). |
| **Redis** | When Redis is selected: **`address`** (or equivalent) as `host:port`, TLS/password if needed — same as other router-side shared dependencies. **Validation** on router start: e.g. fail fast if Redis mode and address missing/unreachable. |

#### Validation

- **Webhook (`ModelRoute`)**: when **`spec.sessionSticky` is non-nil**, **`sources` must be non-empty**; **`sessionAffinitySeconds`**, if set, must be **≥ 1** (no validation of store backend on the CRD).
- **Router binary**: validates **Memory vs Redis** and **Redis address** (and connectivity) from **process flags / config** at startup.

#### Observability

- **Logs**: session sticky store errors, failover when mapped Pod is not in endpoints, Redis connection issues.
- **Response header**: **`X-Kthena-Backend-Pod`** exposes the selected upstream Pod name on the router response (useful for tests and debugging).

### Test plan

#### Unit tests

- Session key extraction: **Header**, **Query**; **source ordering** (first non-empty wins); when **`spec.sessionSticky` is nil**, sticky path is unused; when non-nil but every source yields empty, treat as no session key.
- In-memory store **Set** / **Get** with TTL.

#### End-to-end acceptance (`test/e2e/router/`)
It should be noted that the relevant scoring plugins need to be disabled for the related e2e tests.

| ID | Scenario | Expected outcome |
|----|----------|------------------|
| E2E-SS-01 | **Basic stickiness**: **non-null** `spec.sessionSticky` with header source; repeated requests with the same session header. | All requests reach the **same** Pod. |
| E2E-SS-02 | **Isolation**: two different session keys; then reuse the first key. | Each key stays on its own Pod; returning to the first key does **not** adopt the second key’s binding. |
| E2E-SS-03 | **No key**: **non-null** `sessionSticky`; requests omit the configured header. | No sticky error; over many requests, at least **two** distinct backend Pods (spread like plain LB). |
| E2E-SS-04 | **`sessionSticky` absent** (or `null`): same session header on every request (as if a client always sent a sticky key). | At least **two** distinct backend Pods observed over many requests (header does not pin; no sticky config). |
| E2E-SS-05 | **Query** source only. | Same query value → same Pod. |
| E2E-SS-06 | **Cookie** source. | Same cookie → same Pod. |
| E2E-SS-07 | **TTL**: short `sessionAffinitySeconds`; requests before and after expiry. | Within TTL, session stays on one Pod; after expiry, behavior allows **re-binding** (test allows same Pod by chance but asserts the TTL path). |
| E2E-SS-08 | **Failover**: delete the Pod currently selected for a sticky session; retry with the same session key. | Request succeeds; selected Pod is **no longer** the deleted one (new healthy backend). |
| E2E-SS-09 | **Multi-replica + Redis store**: two router replicas; **router** flags enable **Redis** sticky store with shared address; **`ModelRoute` has no store fields**—only **`sources`** / **`sessionAffinitySeconds`**. | Same session key → **same** Pod regardless of which replica handles the request. |
| E2E-SS-10 | **Admission**: **`sessionSticky` non-null** (object present) with **empty** `sources`. | Create **rejected** (`Invalid` or `BadRequest`). |
| E2E-SS-11 | **Precedence**: header and query both present; list order picks header before query. | The **first** matching source in `sources` wins (header value used). |

### References

- Kubernetes kube-proxy session affinity (conceptual analog): `pkg/proxy/iptables/proxier.go` (`writeServiceToEndpointRules`, `recent` module for `ClientIP`).
- Kthena router E2E: `test/e2e/router/`.
