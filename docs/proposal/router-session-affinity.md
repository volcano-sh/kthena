---
title: Router Session Affinity
authors:
- "@JagjeevanAK"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-04-25

---

## Router Session Affinity

### Summary

This proposal adds `session-affinity` support to the Kthena router so requests that carry the same session identifier can be routed to the same backend pod within an already selected backend. The initial design keeps `ModelRoute` destination selection unchanged and introduces pod-level stickiness only after the router has already selected a `ModelServer` or `InferencePool`.

The proposal addresses the original feature request in issue [#310](https://github.com/volcano-sh/kthena/issues/310), which asked for sticky routing in the model router, and builds on the earlier documentation effort in PR [#873](https://github.com/volcano-sh/kthena/pull/873). Compared with the broader ideas in that issue, this proposal intentionally narrows the first phase to a low-risk router-side implementation with in-memory state, a configurable request header source, and no CRD changes.

### Motivation

Some inference workloads benefit when a sequence of related requests is handled by the same backend pod. Common cases include multi-turn conversations, workloads that benefit from warm prompt caches, and systems that maintain pod-local state or perform better when follow-up requests hit the same runtime instance. Today the Kthena router supports several scheduler plugins, but it does not provide an explicit session stickiness mechanism even though the need has already been raised by users.

Issue [#310](https://github.com/volcano-sh/kthena/issues/310) describes the user need at a feature level and suggests several possible session sources and storage approaches. PR [#873](https://github.com/volcano-sh/kthena/pull/873) opened the discussion from a documentation angle. What is still missing is a concrete, scoped design that fits the current router architecture and can be delivered without destabilizing existing routing behavior. This proposal fills that gap.

#### Goals

- Add a router scheduler plugin named `session-affinity`.
- Keep weighted `ModelRoute` destination selection behavior unchanged.
- Apply stickiness only at pod selection time inside the already selected backend.
- Support a configurable request header as the session key source.
- Use a simple, single-router, in-memory TTL mapping in the first phase.
- Avoid CRD changes, webhook regeneration, and API surface expansion.
- Integrate cleanly with the existing scheduler plugin framework and post-schedule hooks.

#### Non-Goals

- Route-level stickiness across weighted `ModelRoute` destinations.
- Distributed or HA-safe session storage in the first phase.
- Cookie, JWT claim, or query-parameter extraction in the first phase.
- New `ModelRoute` or `ModelServer` spec fields.
- Affinity guarantees across router replicas.
- Changing existing scheduler semantics when no session key is present.

### Proposal

The router will expose session affinity as a normal scheduler score plugin called `session-affinity`. The plugin will not participate in backend selection. Instead, it will influence pod choice only after the router has already resolved a request to a specific `ModelServer` or `InferencePool`.

At request time, the router extracts a session identifier from a configurable HTTP header. The default header is `X-Session-ID`. If the header is empty or absent, the plugin behaves as a no-op and routing proceeds exactly as it does today.

The router also computes an `AffinityScopeKey` for the selected backend:

- For `ModelServer` traffic, the scope is `modelserver/namespace/name` of the selected `ModelServer`.
- For `InferencePool` traffic, the scope is `inferencepool/namespace/name` of the matched `InferencePool`.

The kind prefix makes the scope globally unique across backend types and prevents collisions when names overlap across `ModelServer` and `InferencePool` resources.

The scheduler plugin maintains an in-memory mapping:

- Key: `(AffinityScopeKey, SessionKey)`
- Value: selected pod identity

For stable key derivation and future shared-store interoperability, session key material is normalized through SHA-256 before composing internal affinity keys.

When a request is scored:

- If no binding exists, the plugin returns zero scores and lets the normal scoring stack decide.
- If a binding exists and the bound pod is still in the candidate set, that pod receives the maximum plugin score and all others receive zero.
- If the binding points to a pod that is no longer a candidate, the binding is removed and the request falls back to normal scoring.

After a request is successfully proxied, the router runs scheduler post-hooks. The `session-affinity` plugin uses that point to persist or refresh the binding. This keeps the binding aligned with successful routing outcomes and avoids creating stickiness entries for failed attempts.

#### User Stories

##### Story 1

As an operator serving chat workloads, I want repeated requests carrying the same session header to hit the same pod so that pod-local caches and runtime context are more likely to be reused.

##### Story 2

As an operator already using weighted `ModelRoute` routing, I want session affinity to improve pod reuse without changing how traffic is split across route destinations.

##### Story 3

As a maintainer of the scheduler framework, I want new post-schedule plugins to register through the same plugin pipeline instead of relying on special-case wiring.

#### Notes/Constraints/Caveats

- This is intentionally a pod-level affinity feature, not a route-level persistence feature.
- Pin semantics are configurable with `pinMode`.
- `soft` mode uses score contribution only, so higher-weighted plugins can still override.
- `hard` mode pins selection to the bound pod before weighted score aggregation.
- In multi-router deployments, bindings are local to each router instance in this phase.
- A session binding is refreshed only after a successful upstream path reaches post-schedule hooks.
- For prefill/decode disaggregation, affinity remains backend-local and persists successful decode/prefill choices independently under `.../decode` and `.../prefill` scope suffixes.
- In `soft` mode, operators should assign `session-affinity` a sufficiently high weight when they want an existing binding to dominate other scoring plugins.
- Relative to the broader direction discussed in PR #873, v1 intentionally narrows scope: no per-route `sources` list, no `JWTClaim` extraction, and no Redis-backed shared stickiness in this phase.

#### Risks and Mitigations

**Risk: user expectations around “sticky routing” may be broader than the implementation.**  
Mitigation: document clearly that v1 does not change weighted `ModelRoute` destination selection and does not provide cross-router affinity.

**Risk: stale bindings may point to deleted or unavailable pods.**  
Mitigation: validate the bound pod against current candidates during scoring and lazily remove stale entries.

**Risk: local in-memory storage weakens behavior in HA deployments.**  
Mitigation: make the phase-1 limitation explicit and leave room for a future distributed store.

**Risk: refreshing TTL on every sticky hit can cause write amplification when a shared backend (for example Redis) is introduced.**  
Mitigation: for v1 this is process-local in-memory state; if/when shared storage is added, evaluate lighter refresh strategies (for example expiry-only refresh or threshold-based refresh).

**Risk: memory growth under high unique-session churn.**  
Mitigation: keep affinity state bounded with `maxEntries` and evict old/stale entries.

**Risk: interoperability drift if shared-store key hash behavior changes across versions.**  
Mitigation: key derivation is standardized on SHA-256 and should remain stable across implementations.

**Risk: plugin-specific hook wiring could become harder to maintain.**  
Mitigation: generalize post-schedule hook collection so any score plugin implementing `PostScheduleHook` can participate without custom scheduler code.

### Design Details

#### Router Context Changes

Extend `pkg/kthena-router/scheduler/framework.Context` with:

- `SessionKey string`
- `AffinityScopeKey string`

These fields are populated by the router before scheduling begins.

#### Session Key Extraction

The router reads a configurable header name from scheduler plugin args:

```yaml
pluginConfig:
  - name: session-affinity
    args:
      headerName: X-Session-ID
      ttl: 30m
      pinMode: soft
```

Default values:

- `headerName: X-Session-ID`
- `ttl: 30m`
- `pinMode: soft`

If the header is missing or resolves to an empty string after trimming, the plugin becomes a no-op for that request.

#### Scheduler Plugin Behavior

The new plugin is named `session-affinity` and implements:

- `framework.ScorePlugin`
- `framework.PostScheduleHook`

Score behavior:

1. Read `(AffinityScopeKey, SessionKey)` from the scheduler context.
2. Look up the current binding in a concurrency-safe in-memory store.
3. If the binding exists and matches a current candidate pod, give that pod the maximum plugin score.
4. If the binding does not exist, return zero scores for all candidates.
5. If the binding is stale, delete it and return zero scores.

Post-schedule behavior:

1. Run only after successful proxy flow reaches the scheduler post-hooks.
2. Persist the selected pod under `(AffinityScopeKey, SessionKey)`.
3. Refresh TTL on reuse.

#### Sticky State Store

The first implementation uses a router-local store with:

- concurrency-safe access
- lazy expiration
- idle TTL eviction
- bounded capacity (`maxEntries`) with LRU-style eviction under pressure

This design keeps the feature simple and avoids introducing a new external dependency. A future phase can replace the store implementation with Redis or another shared backend without changing the high-level plugin contract.

#### Plugin Registration and Hook Collection

The scheduler factory already supports score and filter plugins, but post-schedule behavior has historically been wired around `prefix-cache`. This proposal generalizes that path:

- score plugins continue to be created through the registry
- plugins that also implement `framework.PostScheduleHook` are collected automatically
- existing `prefix-cache` behavior continues to work under the same generalized mechanism

This keeps `session-affinity` aligned with the rest of the framework instead of introducing another one-off path.

#### API and Configuration Surface

No CRD changes are required.

New scheduler plugin:

- `session-affinity`

Supported args:

- `headerName: string`
- `ttl: duration string`
- `maxEntries: int`
- `pinMode: string` (`soft` or `hard`)

Recommended scoring guidance:

- use a relatively large weight for `session-affinity` when an existing binding should dominate other score plugins

#### Maintainer Review Alignment

- **Soft vs hard pin semantics are explicit.** `pinMode` controls whether sticky behavior is score-based (`soft`) or scheduler-enforced (`hard`).
- **Affinity scope is globally unique across backend kinds.** Scope keys are kind-prefixed (`modelserver/...` and `inferencepool/...`) to prevent collisions.
- **Store growth is bounded in v1.** Session state is capped by `maxEntries` and evicts stale/old entries.
- **InferencePool session extraction path is validated.** Router tests cover session header propagation for both `ModelServer` and `InferencePool` flows.

#### Beyond V1 Roadmap

The following items are intentionally deferred and should be delivered in follow-up phases with explicit design review:

1. Per-route multi-source extraction (`Header`, `Query`, `Cookie`, `JWTClaim`) using deterministic source order.
2. Shared-store backend (Redis) for multi-router affinity.
3. Shared-store key namespace/versioning strategy while reusing the standardized SHA-256 key derivation.
4. Reduced write amplification strategy for shared-store TTL refresh (for example expiry-only or threshold-based refresh).

#### Test Plan

Unit tests:

- no session header results in no-op scoring
- existing valid binding gives maximum score to the bound pod
- stale binding is removed when the pod is not in the candidate set
- post-schedule hook stores the selected pod
- TTL expiry removes old bindings from effect

Scheduler integration tests:

- config parsing recognizes `session-affinity`
- plugin registration succeeds
- generalized post-hook collection works for both `session-affinity` and `prefix-cache`

Router tests:

- request header extraction populates `SessionKey`
- `AffinityScopeKey` is set correctly for `ModelServer`
- `AffinityScopeKey` is set correctly for `InferencePool`
- missing or empty header does not alter existing routing behavior

Regression expectations:

- current routing remains unchanged when the plugin is not configured
- weighted `ModelRoute` selection behaves exactly as it does today
- existing scheduler plugins continue to behave as before

### Alternatives

#### Add session affinity to `ModelRoute` or `ModelServer` CRDs

This would expose a more explicit API, but it increases design and rollout cost by requiring API review, schema work, and additional validation. For the first phase, router configuration is enough.

#### Implement route-level sticky destination selection

This would more directly match some interpretations of issue `#310`, but it is a materially larger behavior change because it interacts with weighted traffic policies, canary rollouts, and route semantics. It should be discussed separately if needed.

#### Support multiple session sources in v1

Issue `#310` mentions headers, JWT claims, query parameters, cookies, and custom logic. Supporting all of them in the first phase would expand scope without validating the core scheduling behavior first. A header-based source is simpler, explicit, and easy to test.

#### Use Redis or another distributed mapping store from the start

This would improve multi-router consistency, but it introduces additional operational dependency and failure modes. The first phase favors a narrow implementation that matches the current single-router assumption.

#### Keep the proposal at a high level only

PR [#873](https://github.com/volcano-sh/kthena/pull/873) started the design discussion and captured the user need, but it stopped short of defining the router-scoped behavior, scheduler integration points, and testing boundaries needed for implementation. This proposal keeps the original motivation while tightening the design into an implementable first phase.
