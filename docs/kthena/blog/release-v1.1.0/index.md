---
slug: release-v1.1.0
title: "Kthena v1.1.0 Released: One Router for Every Model, Safer and Faster ModelServing Rollouts"
authors: [LiZhenCheng9527]
tags: [release]
date: 2026-11-11
---

## Summary

We are excited to announce **Kthena v1.1.0**. Building on the production-readiness work in v1.0.0, this release widens what the Kthena Router can front and makes ModelServing runs safer and more stable.

On the router side, Kthena can now route to **third-party model APIs** through the new `ExternalModelProvider` resource, pin multi-turn conversations to a backend with **session sticky routing**, run **without a Kubernetes API server**, and score KV cache locality through an **in-memory index** and a **dedicated tokenizer service**. On the ModelServing side, rolling updates gain **`maxSurge`**, **in-place image updates**, **coordinated Role rollouts**, per-Role **partition**, and **eviction protection**.

This release also changes a few defaults compared with v1.0.0, most notably the router `/metrics` port, the opt-in `headless-service` plugin, and the `recoveryPolicy: None` semantics. Please read the Upgrade Notes before upgrading. The `ModelBooster` API is deprecated; new deployments should use `ModelServing` together with `ModelRoute` and `ModelServer`.

<!-- truncate -->

## Release Highlights

### Key Features Overview

- **External model provider routing:** A new `ExternalModelProvider` CRD lets one router entry point serve both in-cluster models and third-party OpenAI-compatible and Anthropic APIs, with credentials in Secrets and shared auth, rate limiting, metrics, and weighted routing.
- **Session sticky routing:** `ModelServer.spec.trafficPolicy.sessionSticky` pins requests with the same session key to the same Pod, or the same Prefill/Decode pair, with memory or Redis binding stores.
- **ModelServing rollout controls:** `maxSurge` for ServingGroup and Role rolling updates, an `InPlace` update method, coordinated proportional Role rollouts, per-Role `partition`, and an `eviction protection webhook`.
- **KV-cache-aware routing without Redis and without engine tokenization:** An in-memory KV cache index streamed from runtime sidecars, plus an optional GPU-free tokenizer service.
- **Standalone router:** The router can read `ModelRoute`, `ModelServer`, and `ExternalModelProvider` manifests from a local directory and run without a cluster.
- **ModelBooster deprecation:** `ModelBooster` is deprecated as of v1.1; admission webhooks and the CLI now warn users.

### External Model Provider Routing (Alpha)

Kthena Router is positioned as a unified LLM entry point, but before v1.1 it could only route to Pod-backed `ModelServer` backends. Teams that mix self-hosted models with public APIs had to run a second gateway for the external part, with separate auth, rate limiting, and observability.

v1.1 adds the `ExternalModelProvider` CRD. A `ModelRoute` target can now reference an external provider instead of a `ModelServer`. The router skips Pod scheduling and calls the provider over HTTPS, while reusing request parsing, authentication, rate limiting, `ModelRoute` matching, weighted traffic splitting, access logs, and metrics. External provider requests also have dedicated observability and token accounting.

Key capabilities:

- OpenAI-compatible providers: `/v1/chat/completions`, `/v1/completions`, `/v1/responses`.
- Anthropic Messages API: `/v1/messages`.
- Streaming SSE passthrough, with native request bodies (tools, multimodal fields) forwarded unchanged.
- Credentials are referenced from Kubernetes Secrets that carry the label `networking.serving.volcano.sh/external-model-provider-credential: "true"`.
- Provider credentials and headers are validated at admission.

Note: `ExternalModelProvider` can forward a Secret value to the configured `baseURL`. Grant write access only to cluster administrators and restrict router egress.

Example:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-provider
  namespace: default
spec:
  providerType: OpenAI
  baseURL: "https://api.openai.com"
  model: "gpt-4o-mini"
  auth:
    secretRef:
      name: openai-api-key
      key: api-key
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: openai-external-route
  namespace: default
spec:
  modelName: "openai-chat"
  rules:
  - name: "default"
    targetModels:
    - externalModelProviderName: "openai-provider"
```

Related changes:

- Proposal: [Kthena Router Support for Third-Party Model APIs](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/kthena-router-third-party-model-api.md) ([#1494](https://github.com/volcano-sh/kthena/pull/1494))
- User guide: [External Model Providers](https://github.com/volcano-sh/kthena/blob/main/docs/kthena/docs/user-guide/external-model-provider.md)
- PRs:
  - [feat(router): add external model provider routing #1369](https://github.com/volcano-sh/kthena/pull/1369)
  - [feat(router): add external provider observability and accounting #1370](https://github.com/volcano-sh/kthena/pull/1370)
  - [test(router): add external provider mock E2E and docs #1371](https://github.com/volcano-sh/kthena/pull/1371)
  - [router: normalize external provider credentials #1638](https://github.com/volcano-sh/kthena/pull/1638)
  - [router: reject invalid provider header values at admission #1697](https://github.com/volcano-sh/kthena/pull/1697)
- Contributors: [@Oxidaner](https://github.com/Oxidaner), [@0YHR0](https://github.com/0YHR0)

### Session Sticky Routing

Session boost in v1.0.0 reorders the router queue for follow-up requests but does not pin Pods. Multi-turn chat and agent workloads benefit most when follow-up requests land on the backend that already holds their prefix KV cache.

Session sticky routing binds a session key to a backend for a sliding TTL. For an aggregated `ModelServer` the backend is one Pod; for a PD-disaggregated `ModelServer` it is the complete Prefill/Decode pair. It applies after `ModelRoute` selects a `ModelServer` and does not alter weighted or canary selection.

Key capabilities:

- Ordered session key sources: `Header`, `Query`, `Cookie`, and `JWTClaim` (first non-empty value wins).
- Sliding TTL via `sessionAffinitySeconds` (default 300).
- `memory` (default) or `redis` binding store configured on the router; use Redis with multiple router replicas.
- Complements session boost, which only reorders the queue and does not pin Pods.

Example:

```yaml
spec:
  trafficPolicy:
    timeout: 10s
    sessionSticky:
      sessionAffinitySeconds: 300
      sources:
        - type: Header
          name: X-Session-ID
        - type: Query
          name: session_id
```

```yaml
networking:
  kthenaRouter:
    sessionSticky:
      backend: redis
      redis:
        address: "redis:6379"
```

Related changes:

- Proposal: [Session Sticky Routing](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/kthena-router-session-sticky.md)
- User guide: [Session Sticky Routing](https://github.com/volcano-sh/kthena/blob/main/docs/kthena/docs/user-guide/session-sticky.md)
- PRs:
  - [feat: Implement Session Sticky Support in Model Router #1144](https://github.com/volcano-sh/kthena/pull/1144)
  - [feat(router): support PD session sticky routing #1816](https://github.com/volcano-sh/kthena/pull/1816)
  - [router: make the session sticky memory store safe to close concurrently #1817](https://github.com/volcano-sh/kthena/pull/1817)
- Contributors: [@FAUST-BENCHOU](https://github.com/FAUST-BENCHOU), [@aeron-gh](https://github.com/aeron-gh)

### ModelServing Rollout Controls: maxSurge, In-Place Update, Coordinated Roles, and Eviction Protection

Rolling updates in v1.0.0 were limited by `maxUnavailable`: replacing a healthy outdated ServingGroup or Role meant removing capacity before replacement capacity was ready. For large multi-GPU inference groups, that is expensive or impossible to absorb.

v1.1 adds several controls:

- **`maxSurge`** for both `ServingGroupRollingUpdate` (under `rollingUpdateConfiguration`) and `RoleRollingUpdate` (per Role). Updated capacity is created first and counts towards availability before outdated capacity is removed. `maxUnavailable` may be `0` when `maxSurge` is above `0`. The implementation is count-driven and recovers from observed state after a controller restart.
- **`rolloutStrategy.updateMethod: InPlace` (Alpha)** patches container images on existing Pods when only regular container images change; otherwise it falls back to recreation and emits an `InPlaceUpdateFallback` event. `progressDeadlineSeconds` bounds how long an in-place update may take to become ready.
- **Coordinated proportional Role rolling updates (Alpha)** (`rolloutStrategy.roleCoordination`) keep selected Roles, for example prefill and decode, progressing in step. `maxSkew` bounds the difference between their target-version Ready ratios, and `dependencies` makes a dependent Role wait for the Roles it depends on. The policy is immutable after creation, and a `CoordinatedRoleRolloutBlocked` condition reports when progress is waiting.
- **Per-Role `partition`** for `RoleRollingUpdate`, protecting the first N Role instances from an update.
- **Eviction protection (Alpha)** through `rolloutStrategy.evictionStrategy`, which keeps a minimum number of ready ServingGroups (`minAvailable`) or ready Role instances (`roleMinAvailable`) during node eviction. It is backed by an optional pods/eviction webhook, disabled by default (`workload.controllerManager.evictionWebhook.enabled`).
- Role-level scale and rollout fixes, including reserving `RoleDeleting` ordinals during scale-up and enforcing rollout configuration granularity.

Example:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
    updateMethod: InPlace
    evictionStrategy:
      protectionLevel: Role
      roleMinAvailable:
        prefill: 1
        decode: 50%
  template:
    roles:
    - name: prefill
      replicas: 2
      maxUnavailable: 0
      maxSurge: 1
      # entryTemplate and workerTemplate omitted
```

With coordination enabled under `RoleRollingUpdate`:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
    roleCoordination:
      roles: [prefill, decode]
      maxSkew: 10%
      dependencies:
      - role: prefill
        dependsOn: [decode]
```

Under `ServingGroupRollingUpdate`, set `rolloutStrategy.rollingUpdateConfiguration.maxSurge` (and `maxUnavailable`, `partition`) instead. With `RoleRollingUpdate`, `rollingUpdateConfiguration` is accepted but ignored; configure `maxUnavailable`, `maxSurge`, and `partition` on each Role. `partition` is applied to absolute ordinals, so it also protects replicas correctly when ordinals are sparse after bin-pack scale-down. Rollout budgets are validated consistently in both modes (for example, `maxUnavailable` cannot exceed `replicas`).

Related changes:

- Proposals: [Coordinated Role rolling updates](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/coordinated-role-rolling-update.md), [Support maxSurge](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/support-maxSurge.md), [Revision history and rollback](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/modelserving-revision-history-and-rollback.md) ([#1607](https://github.com/volcano-sh/kthena/pull/1607)), [Role-scoped network topology](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/modelserving-role-scoped-network-topology.md)
- PRs:
  - [Support partition for Role in RoleRollingUpdate #1311](https://github.com/volcano-sh/kthena/pull/1311)
  - [feat: support coordinated proportional Role rolling updates #1676](https://github.com/volcano-sh/kthena/pull/1676)
  - [feat: manage headless services through ModelServing plugin hooks #1801](https://github.com/volcano-sh/kthena/pull/1801)
  - [ModelServing support maxSurge #1626](https://github.com/volcano-sh/kthena/pull/1626)
  - [feat: add ModelServing revision core #1643](https://github.com/volcano-sh/kthena/pull/1643)
  - [feat: add ModelServing eviction protection #1485](https://github.com/volcano-sh/kthena/pull/1485)
  - [model-serving: add rolloutStrategy.updateMethod and in-place eligibility checks #1882](https://github.com/volcano-sh/kthena/pull/1882)
  - [model-serving: add the in-place rolling update controller #1883](https://github.com/volcano-sh/kthena/pull/1883)
  - [fix: enforce rollout configuration granularity for ModelServing #1515](https://github.com/volcano-sh/kthena/pull/1515)
  - [fix: reserve RoleDeleting ordinals during scale-up #1355](https://github.com/volcano-sh/kthena/pull/1355)
  - [Fix partition handling and historical replica recovery #1987](https://github.com/volcano-sh/kthena/pull/1987)
  - [Make rollout validation consistent across both modes #1988](https://github.com/volcano-sh/kthena/pull/1988)
- Contributors: [@LiZhenCheng9527](https://github.com/LiZhenCheng9527), [@kube-gopher](https://github.com/kube-gopher), [@acsoto](https://github.com/acsoto), [@VanderChen](https://github.com/VanderChen), [@haoeeeee](https://github.com/haoeeeee), [@FAUST-BENCHOU](https://github.com/FAUST-BENCHOU)

### KV-Cache-Aware Routing: In-Memory Index and Dedicated Tokenizer Service

The `kvcache-aware` score plugin previously required a Redis round-trip for every block-ownership lookup and a `/tokenize` call to a randomly chosen GPU engine Pod for every scheduling decision. Both sit on the routing hot path.

v1.1 makes both optional:

- **In-memory index (Alpha)** (`indexMode: memory`): runtime sidecars, started with `KV_EVENT_SYNC_MODE=memory`, stream KV events to every router over a router-initiated connection. Lookups become local memory reads with no Redis deployment. Each router replica keeps its own index, and events are only accepted on connections the router opens itself (see Security); stale entries are garbage-collected after 24 hours, and ownership written before a Pod restart is ignored.
- **Tokenizer service (Alpha)** (disabled by default): a GPU-free service that runs `vllm launch render` per mapped model and serves vLLM-compatible `/tokenize`. It runs as a router sidecar or as a standalone Deployment. If it is unavailable, the router falls back to engine tokenization by default.

Router and sidecar configuration for memory mode:

```yaml
# Runtime sidecar container
env:
- name: KV_EVENT_SYNC_MODE
  value: "memory"
```

```yaml
# Router ConfigMap, kvcache-aware plugin arguments
indexMode: memory
```

Tokenizer service Helm values:

```yaml
networking:
  kthenaRouter:
    tokenizerService:
      enabled: true
      mode: sidecar          # or standalone
      models:
        deepseek-v3: deepseek-ai/DeepSeek-V3
```

Related changes:

- Proposal: [Dedicated Tokenizer Service](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/dedicated-tokenizer-service.md)
- User guides: [KV Cache Aware Plugin](https://github.com/volcano-sh/kthena/blob/main/docs/kthena/docs/user-guide/kvcache-aware.md), [Tokenizer Service](https://github.com/volcano-sh/kthena/blob/main/docs/kthena/docs/user-guide/tokenizer-service.md)
- PRs:
  - [router: in-memory KV cache index for kvcache-aware plugin #1725](https://github.com/volcano-sh/kthena/pull/1725)
  - [kvcache-aware: stream memory-mode KV events over a router-initiated connection #1867](https://github.com/volcano-sh/kthena/pull/1867)
  - [router: dedicated local tokenizer service for kvcache-aware routing #1758](https://github.com/volcano-sh/kthena/pull/1758)
  - [router: skip KV cache ownership written before a pod restart #1463](https://github.com/volcano-sh/kthena/pull/1463)
  - [runtime: use Redis SCAN instead of KEYS #1431](https://github.com/volcano-sh/kthena/pull/1431)
  - [router: validate KV-cache tokenizer ports #1633](https://github.com/volcano-sh/kthena/pull/1633)
- Contributors: [@YaoZengzeng](https://github.com/YaoZengzeng), [@aeron-gh](https://github.com/aeron-gh), [@0YHR0](https://github.com/0YHR0), [@nabrahma](https://github.com/nabrahma), [@avinxshKD](https://github.com/avinxshKD)

### Standalone Router (Alpha)

Kthena Router can now run without a Kubernetes API server. With `--config-source=file`, it reads `ModelRoute`, `ModelServer`, `ExternalModelProvider`, and `Secret` manifests from a directory, and `ModelServer.spec.endpoints` lists serving instances explicitly. This suits local development, benchmarking, and container-only deployments. Manifests are validated with the same rules as the admission webhook, and an invalid snapshot is rejected while the last good one keeps serving.

```bash
kthena-router \
  --config-source=file \
  --config-dir=/etc/kthena/resources \
  --config-sync-period=10s
```

The admission webhook is disabled automatically and `--enable-gateway-api` cannot be combined with this mode.

Related changes:

- User guide: [Standalone Router](https://github.com/volcano-sh/kthena/blob/main/docs/kthena/docs/user-guide/standalone-router.md)
- PRs:
  - [router: run standalone without a Kubernetes API server #1705](https://github.com/volcano-sh/kthena/pull/1705)
- Contributors: [@YaoZengzeng](https://github.com/YaoZengzeng)

### ModelBooster Deprecation

The `ModelBooster` API is deprecated as of v1.1. The Go types are annotated as deprecated, the admission webhook returns deprecation warnings, the CLI prints a deprecation notice, and the documentation now points to `ModelServing`, `ModelRoute`, and `ModelServer`. The API is still served in this release.

Related changes:

- PRs:
  - [api: deprecate the ModelBooster API #1726](https://github.com/volcano-sh/kthena/pull/1726)
  - [webhook: add ModelBooster deprecation warnings #1735](https://github.com/volcano-sh/kthena/pull/1735)
  - [CLI Add ModelBooster deprecation description #1844](https://github.com/volcano-sh/kthena/pull/1844)
- Contributors: [@kube-gopher](https://github.com/kube-gopher), [@chenmeibin-dev](https://github.com/chenmeibin-dev)

## Additional Enhancements

- Added a per-`ModelServer` upstream connection pool configuration (`spec.trafficPolicy.connectionPool`) and widened the default upstream transport pool. [#1759](https://github.com/volcano-sh/kthena/pull/1759), [#1654](https://github.com/volcano-sh/kthena/pull/1654)
- Added OpenAI `/v1/responses` routing for in-cluster backends, including PD-disaggregated targets. [#1764](https://github.com/volcano-sh/kthena/pull/1764)
- ModelServer `trafficPolicy.timeout` is now honored, including for PD-disaggregated routing. [#1603](https://github.com/volcano-sh/kthena/pull/1603), [#1711](https://github.com/volcano-sh/kthena/pull/1711)
- Improved scale-out fairness of the `least-latency` plugin by adjusting the cold-start default score, and validated score plugin weights. [#1554](https://github.com/volcano-sh/kthena/pull/1554), [#1393](https://github.com/volcano-sh/kthena/pull/1393), [#1859](https://github.com/volcano-sh/kthena/pull/1859)
- Added a Kthena router overview Grafana dashboard. [#1692](https://github.com/volcano-sh/kthena/pull/1692)
- Added router access-log settings, a configurable router Service type, tolerations, replicas in Helm values, and a debug container port for the router. [#1568](https://github.com/volcano-sh/kthena/pull/1568), [#1786](https://github.com/volcano-sh/kthena/pull/1786), [#1440](https://github.com/volcano-sh/kthena/pull/1440), [#1336](https://github.com/volcano-sh/kthena/pull/1336)
- Added structured contextual fields to controller logs. [#1362](https://github.com/volcano-sh/kthena/pull/1362), [#1566](https://github.com/volcano-sh/kthena/pull/1566)
- Added printer columns to CRDs and `ModelRoute`/`ModelServer` support in `kthena create manifest`. [#1437](https://github.com/volcano-sh/kthena/pull/1437), [#1418](https://github.com/volcano-sh/kthena/pull/1418)
- Made the autoscaler reconcile period configurable and validated pod metric source endpoints and `minReplicas`/`maxReplicas`. [#1303](https://github.com/volcano-sh/kthena/pull/1303), [#1526](https://github.com/volcano-sh/kthena/pull/1526), [#1794](https://github.com/volcano-sh/kthena/pull/1794)
- Added a `pod.Spec` validator and allowed AWS default credentials in the downloader. [#1790](https://github.com/volcano-sh/kthena/pull/1790), [#1691](https://github.com/volcano-sh/kthena/pull/1691)
- CLI now honors `KUBECONFIG` and the kubeconfig context namespace. [#1651](https://github.com/volcano-sh/kthena/pull/1651), [#1602](https://github.com/volcano-sh/kthena/pull/1602)
- Documentation: Dynamo and llm-d Router integration guides, downloader, session sticky, tokenizer service, GPU-free quick start, and reorganized PD-disaggregation guides. [#1728](https://github.com/volcano-sh/kthena/pull/1728), [#1694](https://github.com/volcano-sh/kthena/pull/1694), [#1875](https://github.com/volcano-sh/kthena/pull/1875), [#1608](https://github.com/volcano-sh/kthena/pull/1608), [#1559](https://github.com/volcano-sh/kthena/pull/1559)

## Bug Fixes

### Router and Gateway API

- Applied Gateway API precedence across matching HTTPRoutes, matched parentless `ModelRoute`s to Gateways, respected `ModelRoute` Gateway parent refs and listeners, supported optional `sectionName` and `port` in listener matching, and matched nested subdomains for wildcard listeners. [#1540](https://github.com/volcano-sh/kthena/pull/1540), [#1457](https://github.com/volcano-sh/kthena/pull/1457), [#1346](https://github.com/volcano-sh/kthena/pull/1346), [#1517](https://github.com/volcano-sh/kthena/pull/1517), [#1414](https://github.com/volcano-sh/kthena/pull/1414), [#1625](https://github.com/volcano-sh/kthena/pull/1625), [#1659](https://github.com/volcano-sh/kthena/pull/1659), [#1229](https://github.com/volcano-sh/kthena/pull/1229)
- Honored HTTPRoute `InferencePool` backend weights and returned 503 when no backend is eligible or available. [#1471](https://github.com/volcano-sh/kthena/pull/1471), [#1379](https://github.com/volcano-sh/kthena/pull/1379)
- Fixed healthy Pods not being picked when no score plugin scores them, and PD scheduling bypassing `least-request` filters. [#1865](https://github.com/volcano-sh/kthena/pull/1865), [#1385](https://github.com/volcano-sh/kthena/pull/1385)
- Rejected unregistered models before fairness and session-boost queuing; cancelled pending requests when the fairness queue closes; woke queues after each metrics scrape. [#1642](https://github.com/volcano-sh/kthena/pull/1642), [#1377](https://github.com/volcano-sh/kthena/pull/1377), [#1853](https://github.com/volcano-sh/kthena/pull/1853)
- Fixed PD-disaggregated routing: capped `min_tokens` for prefill requests, generated a fresh SGLang bootstrap room per retry, avoided retrying already-written responses, and refreshed PD categorization on Pod label updates. [#1904](https://github.com/volcano-sh/kthena/pull/1904), [#1424](https://github.com/volcano-sh/kthena/pull/1424), [#1685](https://github.com/volcano-sh/kthena/pull/1685), [#1766](https://github.com/volcano-sh/kthena/pull/1766)
- Fixed metrics handling: bounded `path` and `upstream_model` label cardinality, handled histogram resets after backend restarts, and fixed unknown-model metrics. [#1821](https://github.com/volcano-sh/kthena/pull/1821), [#1486](https://github.com/volcano-sh/kthena/pull/1486), [#1398](https://github.com/volcano-sh/kthena/pull/1398)
- Hardened observability endpoints, scoped controller and router Secret RBAC to least privilege, and skipped JWT issuer validation when no issuer is configured. [#1488](https://github.com/volcano-sh/kthena/pull/1488), [#1480](https://github.com/volcano-sh/kthena/pull/1480), [#1831](https://github.com/volcano-sh/kthena/pull/1831)
- Fixed the wrong model name being passed to the engine and the served model name in `kvcache-aware`, skipped prefix-cache recording when prompt hashes are empty, and fixed `PodInfo.Pod` data races in session sticky routing. [#1956](https://github.com/volcano-sh/kthena/pull/1956), [#1998](https://github.com/volcano-sh/kthena/pull/1998), [#1921](https://github.com/volcano-sh/kthena/pull/1921)
- Fixed IPv6 pod metric and tokenizer URLs, the vLLM ZMQ topic filter, and a tokenizer panic when deleting a `ModelServer` whose renderer failed to start. [#1495](https://github.com/volcano-sh/kthena/pull/1495), [#1534](https://github.com/volcano-sh/kthena/pull/1534), [#1886](https://github.com/volcano-sh/kthena/pull/1886)
- Fixed concurrency and lifecycle issues: datastore callbacks map locking, token tracker lock contention, a concurrent-safe random score plugin, workqueue retry reset, and Pod delete events on `ModelServer` eviction. [#1784](https://github.com/volcano-sh/kthena/pull/1784), [#1297](https://github.com/volcano-sh/kthena/pull/1297), [#1650](https://github.com/volcano-sh/kthena/pull/1650), [#1788](https://github.com/volcano-sh/kthena/pull/1788), [#1798](https://github.com/volcano-sh/kthena/pull/1798)

### ModelServing, Autoscaler, and ModelBooster

- Preserved semantic ControllerRevisions across upgrades so upgrading the controller does not trigger spurious rollouts, skipped semantically unchanged PodGroup updates, and validated immutable `ModelServing` field updates in the webhook. [#1799](https://github.com/volcano-sh/kthena/pull/1799), [#1952](https://github.com/volcano-sh/kthena/pull/1952)
- Never delete faulty Pods when `recoveryPolicy` is `None` or `restartGracePeriodSeconds` is `-1`. [#2006](https://github.com/volcano-sh/kthena/pull/2006)
- Refilled deleted Pods for `RecoveryPolicy=None`, retried failed headless Service creation, and guarded Pod deletion by UID. [#1669](https://github.com/volcano-sh/kthena/pull/1669), [#1408](https://github.com/volcano-sh/kthena/pull/1408), [#1427](https://github.com/volcano-sh/kthena/pull/1427)
- Fixed condition reason, message, and timestamp refresh when a condition is superseded, and `graceMap` cleanup when `markPodUnavailable` fails. [#1744](https://github.com/volcano-sh/kthena/pull/1744), [#1742](https://github.com/volcano-sh/kthena/pull/1742)
- Stopped the `LeaderWorkerSet` integration from rewriting a `ModelServing` it does not own, and allowed the controller to delete old `ModelServing`, `ModelServer`, and `ModelRoute` objects under RBAC. [#1917](https://github.com/volcano-sh/kthena/pull/1917), [#1623](https://github.com/volcano-sh/kthena/pull/1623), [#1646](https://github.com/volcano-sh/kthena/pull/1646)
- Allowed a role with zero replicas under `RoleRollingUpdate` in the webhook and tolerated a missing ServingGroup entry during rolling update. [#1750](https://github.com/volcano-sh/kthena/pull/1750), [#1757](https://github.com/volcano-sh/kthena/pull/1757)
- Autoscaler: fixed heterogeneous scale-down getting stuck, target distinction by namespace, ratio aggregation across heterogeneous backends, pod metric sums, and ready-pod metric preservation during mixed readiness. [#1700](https://github.com/volcano-sh/kthena/pull/1700), [#1445](https://github.com/volcano-sh/kthena/pull/1445), [#1272](https://github.com/volcano-sh/kthena/pull/1272), [#1843](https://github.com/volcano-sh/kthena/pull/1843), [#1405](https://github.com/volcano-sh/kthena/pull/1405)
- ModelBooster: fixed `spec.name` routing lookup, engine port propagation, negative `workerReplicas` for vLLM, and rejected inconsistent PD `kv_connector` configs. [#1490](https://github.com/volcano-sh/kthena/pull/1490), [#1446](https://github.com/volcano-sh/kthena/pull/1446), [#1629](https://github.com/volcano-sh/kthena/pull/1629), [#1455](https://github.com/volcano-sh/kthena/pull/1455)
- Required `workloadPort` in `ModelServer` and validated the `ModelServer` workload selector. [#1635](https://github.com/volcano-sh/kthena/pull/1635), [#1287](https://github.com/volcano-sh/kthena/pull/1287)
- Helm and downloader: fixed the router TLS chart mount path, Redis password key lookup, controller manager webhook toggle, downloader lock inode stability, and S3 credential failures. [#1351](https://github.com/volcano-sh/kthena/pull/1351), [#1683](https://github.com/volcano-sh/kthena/pull/1683), [#1662](https://github.com/volcano-sh/kthena/pull/1662), [#1715](https://github.com/volcano-sh/kthena/pull/1715), [#1596](https://github.com/volcano-sh/kthena/pull/1596)

## Security

### Unauthenticated KV event ingestion in the in-memory index (fixed before release)

The in-memory KV cache index introduced in [#1725](https://github.com/volcano-sh/kthena/pull/1725) originally had runtime sidecars push KV events to a router `POST /kvcache/event` endpoint. That endpoint had no authentication, and the router trusted the `pod_identifier` and `model_name` supplied by the sender. Any client able to reach the endpoint could impersonate an inference Pod, register a large number of fabricated KV cache blocks for it, and make `kvcache-aware` scoring route requests to a backend of its choosing.

This was fixed by [#1867](https://github.com/volcano-sh/kthena/pull/1867). The router no longer exposes an events endpoint. Instead, it opens a connection to the runtime sidecar of each selected Pod and the sidecar streams KV events back on that connection, so events are always attributed to the Pod the router dialed and nothing in a stream can claim another Pod's identity. Store times are capped at the time of receipt, and each Pod can hold at most `maxBlocksPerPod` blocks.

Impact: #1725 was not part of v1.0.0, so no v1.0.x release contained the vulnerability and the exposure was limited to the v1.1 development line. Deployments using the default Redis index mode were not affected.

Thanks to [@btxu-db](https://github.com/btxu-db) for reporting this vulnerability.

## Upgrade Notes

### Changes in default behavior compared with v1.0.0

- **Router metrics moved off the inference port.** `/metrics` is now served on a dedicated port (`--metrics-port`, default `9090`; Helm `networking.kthenaRouter.metrics.port`) and exposed through the new `kthena-router-metrics` ClusterIP Service. It is no longer served on the public inference listener by default. Update Prometheus scrape configs, or set `networking.kthenaRouter.metrics.exposeOnRouterPort: true` (`--expose-metrics-on-router-port`) for legacy compatibility.
- **`headless-service` is an explicit plugin.** v1.0.0 created Entry Services and injected `ENTRY_ADDRESS` without any plugin. In v1.1, a `ModelServing` receives these generated DNS settings only if it declares `plugins: [{name: headless-service, type: BuiltIn}]`. Existing objects without it stop getting them when Pods are recreated, and `plugins` is immutable, so migrate by creating a new object.
- **`recoveryPolicy: None` no longer deletes Pods.** It previously followed default Pod or Deployment behavior; now a failed or restarted Pod is never deleted by the controller and is left to the kubelet. `restartGracePeriodSeconds: -1` is now accepted and tolerates failed Pods indefinitely (values below `-1` are rejected). A Pod deleted by others is still recreated.
- **`partition` uses absolute ordinals.** It protects replicas with ordinal in `[0, partition)` even when the ordinal set is sparse.
- **Stricter admission.** `ModelServer.spec.workloadPort` is required; score plugin weights must be positive, and `maxWaitingRequests` for `least-request` must not be negative; invalid `ExternalModelProvider` headers are rejected; webhooks validate immutable `ModelServing` updates and rollout budgets in both rollout modes. Existing manifests that violate these rules fail on the next update.
- **Router upstream connection pool is larger.** The idle connections per backend default to `64` (Go's default is `2`); tune it per `ModelServer` with `trafficPolicy.connectionPool`.
- **Scheduling and routing results can differ.** The `least-latency` plugin uses a new cold-start default score, so scale-out Pods receive traffic more fairly. Pods that no score plugin scores are now still selectable. A matched route returns `503` when no backend is available. Unregistered models are rejected before fairness or session-boost queuing. The router readiness probe now uses `/readyz`.
- **Helm.** The router Service type is now configurable (default remains `LoadBalancer`), `tolerations` and `replicas` values were added, the unused top-level `webhook` values were removed, and the Redis password is read from the `REDIS_PASSWORD` Secret key. Re-apply the CRDs because they changed.
- **Deprecation.** `ModelBooster` is deprecated; plan migration to `ModelServing`, `ModelRoute`, and `ModelServer`.

### Opt-in features

The following features are **Alpha**: `ExternalModelProvider`, `rolloutStrategy.updateMethod: InPlace`, `rolloutStrategy.roleCoordination`, the tokenizer service, `indexMode: memory`, and the standalone router (`--config-source=file`). They are disabled or unused by default, and their APIs, fields, flags, and behavior may change in future releases. Review them before enabling in production. All other features in this release are not Alpha.

`rolloutStrategy.updateMethod: InPlace`, `rolloutStrategy.evictionStrategy` (and its webhook), `roleCoordination`, `maxSurge`, `ExternalModelProvider`, session sticky, the tokenizer service, and `indexMode: memory` are opt-in. Existing behavior is unchanged when they are not configured. `maxUnavailable` may be `0` only when `maxSurge` resolves above `0`.

If you use external providers, restrict `ExternalModelProvider` write permission to administrators.

## Contributors

Thank you to everyone who contributed to this release, including @aeron-gh, @avinxshKD, @acsoto, @0YHR0, @LiZhenCheng9527, @vanshika2720, @kube-gopher, @nabrahma, @YaoZengzeng, @iamtanishqjain, @FAUST-BENCHOU, @omlahore, @miantalha45, @chenhuiluo, @AdeshDeshmukh, @Abhayanthk, @Oxidaner, @chenmeibin-dev, @vivek41-glitch, @safiya2610, @btxu-db, @bhuvan-somisetty, @anirudh240, @VanderChen, @haoeeeee, @Amansingh0807, @sumukhj1219, @Srijan76-code, @hzxuzhonghu, @chacha923, and all first-time contributors.
