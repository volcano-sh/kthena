---
slug: release-v1.0.0
title: "Kthena v1.0.0 Released: Production-Ready Kubernetes-Native LLM Serving"
authors: [LiZhenCheng9527]
tags: [release]
date: 2026-06-30
---

## Summary

We are excited to announce **Kthena v1.0.0**, a major milestone for Kubernetes-native LLM inference. This release focuses on production readiness across the serving stack: more accurate Gateway API routing, first-class role-level autoscaling for prefill/decode disaggregated workloads, safer role-level rolling updates, better router scheduling signals, session boost for multi-turn conversation workloads, richer cache-aware router observability with Prometheus metrics and example dashboards, and a more complete CLI experience.

Kthena v1.0.0 also includes an important autoscaling API consolidation. `AutoscalingPolicyBinding` has been removed, and target configuration now lives directly in `AutoscalingPolicy` through `homogeneousTarget`, `heterogeneousTarget`, and `disaggregatedTarget`.

<!-- truncate -->

## Release Highlights

### Key Features Overview

- **AutoscalingPolicy consolidation and P/D coordinated disaggregated autoscaling:** `AutoscalingPolicyBinding` is removed, and autoscaling target configuration is consolidated into `AutoscalingPolicy`; `disaggregatedTarget` enables coordinated role-level autoscaling for prefill/decode workloads, allowing each role to scale from its own metrics while keeping P/D replica ratios within healthy bounds through optional ratio constraints.
- **Session boost for multi-turn conversations:** The router can prioritize follow-up requests from recently completed sessions, improving the chance of reusing warm KV cache under concurrent agentic and chat workloads.
- **Router scheduling and observability:** Per-pod in-flight request tracking, Redis-backed cross-router synchronization, configurable pod metrics scraping, and cache-aware Prometheus metrics improve scheduling accuracy and operational visibility.
- **Role-level rolling update availability control:** Enhancements to the `RoleRollingUpdate` feature. Now each role can set `maxUnavailable` to control the pace of the upgrade.
- **Gateway API and HTTPRoute correctness:** Kthena router now honors HTTPRoute hostnames, keeps matched route rules consistent for backend selection and URL rewrites, fixes `PathPrefix` semantics, and respects Gateway listener `allowedRoutes`.
- **CLI and OpenAI-compatible API improvements:** The CLI now exposes richer status output and supports `ModelRoute` and `ModelServer`, while the router adds an OpenAI-compatible `GET /v1/models` endpoint.

### AutoscalingPolicy Consolidation and P/D Coordinated Disaggregated Autoscaling

Kthena v1.0.0 introduces a simpler and more powerful autoscaling API. Users now configure what to scale, how to collect metrics, and scaling boundaries in a single `AutoscalingPolicy` resource.

The new `disaggregatedTarget` mode is designed for **coordinated P/D autoscaling** in role-based ModelServing deployments, especially prefill/decode disaggregated inference. Prefill and decode roles can make scaling decisions from their own metrics, while the autoscaler applies shared constraints so the two sides grow and shrink together instead of drifting independently. Each role can define its own replica range, metrics, and metric sources, and operators can configure a `ratioConstraint` to keep the P/D replica ratio within a healthy range.

Example `disaggregatedTarget` configuration:

```yaml
spec:
  disaggregatedTarget:
    targetRef:
      apiVersion: workload.serving.volcano.sh/v1alpha1
      kind: ModelServing
      name: vllm-qwen-pd-ms
    roles:
      prefill:
        minReplicas: 1
        maxReplicas: 8
        metrics:
          - name: prefill_waiting_requests
            targetValue: "1"
        metricSources:
          prefill_waiting_requests:
            prometheus:
              serverURL: http://kube-prometheus-stack-prometheus.test.svc.cluster.local:9090
              query: sum(vllm:num_requests_waiting{namespace="autoscale-demo", service="vllm-prefill"})
      decode:
        minReplicas: 1
        maxReplicas: 16
        metrics:
          - name: decode_gpu_cache_usage
            targetValue: "0.75"
        metricSources:
          decode_gpu_cache_usage:
            prometheus:
              serverURL: http://kube-prometheus-stack-prometheus.test.svc.cluster.local:9090
              query: sum(vllm:gpu_cache_usage_perc{namespace="autoscale-demo", service="vllm-decode"})
    ratioConstraint:
      numeratorRole: prefill
      denominatorRole: decode
      minRatio: "0.25"
      maxRatio: "1"
```

Related changes:

- Proposal:
  - [Proposal of merge `autoscalingPolicybingding` into `autoscalingPolicy` #1172](https://github.com/volcano-sh/kthena/pull/1172)
- PRs:
  - [merge autoscalingpolicybinding to autoscalingpolicy #1203](https://github.com/volcano-sh/kthena/pull/1203)
  - [Implementation of PD disaggregation auto-scaler #1258](https://github.com/volcano-sh/kthena/pull/1258)
- Contributors: [@LiZhenCheng9527](https://github.com/LiZhenCheng9527)

### Session Boost for Multi-Turn Conversation Workloads

Kthena v1.0.0 adds session boost support to improve multi-round chat, agentic workflows, and RAG chains where each request depends on previous responses. In these workloads, follow-up requests often reuse a large shared prefix. If they wait behind unrelated traffic for too long, corresponding backend KV cache entries may be evicted and time-to-first-token can increase.

Session boost lets the router track recently completed sessions and prioritize follow-up requests from those sessions in the waiting queue. The implementation keeps this behavior separate from user-fairness scheduling and uses dedicated session-boost configuration, including session header selection, bounded recent-session tracking, inflight admission limits, and an optional grace period for advanced cache-hit optimization.

The feature is designed to improve cache reuse opportunities under concurrent multi-turn traffic without claiming pod affinity by itself. For the strongest KV cache benefit, operators should also ensure that session-aware or cache-aware routing can place follow-up requests on backends that still hold the warm cache.

Example Helm configuration:

```yaml
networking:
  kthenaRouter:
    sessionBoost:
      enabled: true
      header: X-Session-ID
      maxSessions: 4096
      inflightPerPod: 16
      gracePeriod: 0s
```

Related changes:

- Issue: [Improve multi-round conversation case #1190](https://github.com/volcano-sh/kthena/issues/1190)
- PRs:
  - [session boost queue to optimize multi conversation scenario #1183](https://github.com/volcano-sh/kthena/pull/1183)
- Contributors: [@YaoZengzeng](https://github.com/YaoZengzeng), [@hzxuzhonghu](https://github.com/hzxuzhonghu), [@FAUST-BENCHOU](https://github.com/FAUST-BENCHOU), [@LiZhenCheng9527](https://github.com/LiZhenCheng9527)

### Smarter Router Scheduling and Cache-Aware Observability

The router now has better load signals for scheduling decisions. Kthena tracks per-pod in-flight requests and can synchronize these counters across router replicas through Redis, allowing the `least-request` plugin to make decisions based on real-time load instead of only local router state.

Cache-aware scheduling is also much more observable. The `prefix-cache` and `kvcache-aware` score plugins now export Prometheus metrics on the router's existing `/metrics` endpoint, replacing klog-only visibility with queryable, model-labelled time series for load testing and production tuning.

For cache effectiveness, Kthena records match-ratio histograms instead of simple hit/miss counters. `kthena_router_prefix_cache_match_ratio` and `kthena_router_kvcache_aware_match_ratio` report the fraction of a prompt's blocks already available on the best-matching candidate pod, with `0` representing a real miss. This makes hit rate derivable from the `le="0.0"` bucket while also showing how much of the prefix was reused.

Related changes:

- Proposal:
  - [Observability for prefix-cache and kvcache-aware Score Plugins](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/cache-observability.md)
- PRs:
  - [feat(router): add per-pod in-flight request tracking with Redis sync #962](https://github.com/volcano-sh/kthena/pull/962)
  - [Add SGLang tokenizer support for KV-cache-aware scheduling #997](https://github.com/volcano-sh/kthena/pull/997)
  - [router: add observability metrics for prefix-cache and kvcache-aware score plugins #1194](https://github.com/volcano-sh/kthena/pull/1194)
  - [feat(router): make pod metrics update interval configurable #1151](https://github.com/volcano-sh/kthena/pull/1151)
  - [perf(router): cache parsed prompt to avoid redundant ParsePrompt call #1123](https://github.com/volcano-sh/kthena/pull/1123)
  - [fix: parallelize pod metrics scraping loop with bounded concurrency #1255](https://github.com/volcano-sh/kthena/pull/1255)
- Contributors: [@hzxuzhonghu](https://github.com/hzxuzhonghu), [@blenbot](https://github.com/blenbot), [@kube-gopher](https://github.com/kube-gopher), [@rajnish-jais](https://github.com/rajnish-jais), [@nabrahma](https://github.com/nabrahma)

### Role RollingUpdate Availability Control

Kthena v0.4.0 introduced `RoleRollingUpdate`, but role updates could still delete all outdated Role replicas in a ServingGroup at once. When `spec.replicas` was `1`, this could make the service temporarily unavailable during a role-level rollout.

Kthena v1.0.0 adds per-Role `maxUnavailable` support for `RoleRollingUpdate`. Operators can now control the step size of each Role update independently, using either an absolute number or a percentage. This gives role-level rolling updates the same availability-oriented control that ServingGroup-level updates already had, while keeping Role-level and ServingGroup-level rollout budgets separate.

Example Role-level rollout configuration:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
  template:
    roles:
    - name: prefill
      replicas: 2
      maxUnavailable: 1
      # entryTemplate and workerTemplate omitted
    - name: decode
      replicas: 4
      maxUnavailable: 25%
      # entryTemplate and workerTemplate omitted
```

Related changes:

- Issue: [Control the number of unavailable Role replicas in RoleRollingUpdate #1188](https://github.com/volcano-sh/kthena/issues/1188)
- PRs:
  - [Role rollingupdate support maxUnavailable settings #1239](https://github.com/volcano-sh/kthena/pull/1239)
- Contributors: [@hzxuzhonghu](https://github.com/hzxuzhonghu), [@LiZhenCheng9527](https://github.com/LiZhenCheng9527)

### Gateway API and HTTPRoute Correctness

Kthena-router now handles Gateway API traffic with stronger correctness guarantees. The router honors `HTTPRoute.spec.hostnames`, keeps the matched HTTPRoute rule associated with backend selection and URL rewrite filters, and chooses more specific path rules within a route. This avoids accidentally routing a request through a backend or filter from a different rule than the one that matched the request.

This release also fixes Gateway API `PathPrefix` matching semantics and makes the router respect Gateway listener `allowedRoutes` before accepting HTTPRoutes.

Related changes:

- PRs:
  - [feat: honor HTTPRoute hostnames and matched rule selection #1174](https://github.com/volcano-sh/kthena/pull/1174)
  - [Fix HTTPRoute PathPrefix matching #1119](https://github.com/volcano-sh/kthena/pull/1119)
  - [fix(router): respect Gateway allowedRoutes #1263](https://github.com/volcano-sh/kthena/pull/1263)
- Contributors: [@zhy76](https://github.com/zhy76), [@Monti-27](https://github.com/Monti-27), [@avinxshKD](https://github.com/avinxshKD)

### CLI and OpenAI-Compatible API Improvements

The `kthena` CLI now surfaces more useful status information and supports more resource types:

- `kthena get model-servings` now shows `READY` and `STATUS` columns.
- `kthena get model-boosters` now shows a `STATUS` column.
- `kthena get model-routes` and `kthena get model-servers` are now supported.
- `kthena describe model-route` and `kthena describe model-server` are now supported.

The router also adds an OpenAI-compatible `GET /v1/models` endpoint, returning available model names in the standard list response shape.

Related changes:

- PRs:
  - [feat: add STATUS and READY columns to kthena get output #978](https://github.com/volcano-sh/kthena/pull/978)
  - [feat: add CLI support for ModelRoute and ModelServer resources #981](https://github.com/volcano-sh/kthena/pull/981)
  - [feat: support /v1/models endpoint #996](https://github.com/volcano-sh/kthena/pull/996)
- Contributors: [@anirudh240](https://github.com/anirudh240), [@madmecodes](https://github.com/madmecodes)

## Additional Enhancements

- Added KEDA/HPA compatibility for `ModelServing` by setting the scale subresource label selector. [#839](https://github.com/volcano-sh/kthena/pull/839)
- Added a controller-manager debug port for dumping cached ServingGroup and Role configuration. [#900](https://github.com/volcano-sh/kthena/pull/900)
- Added SGLang Dynamo mocker coverage and SGLang inference simulator integration. [#920](https://github.com/volcano-sh/kthena/pull/920), [#1231](https://github.com/volcano-sh/kthena/pull/1231)
- Added router `pprof` endpoint support. [#1057](https://github.com/volcano-sh/kthena/pull/1057)
- Added `debugPort` Helm chart support for controller-manager. [#1032](https://github.com/volcano-sh/kthena/pull/1032)
- Improved ModelBooster GPU and offline environment support. [#972](https://github.com/volcano-sh/kthena/pull/972), [#1141](https://github.com/volcano-sh/kthena/pull/1141), [#1146](https://github.com/volcano-sh/kthena/pull/1146), [#945](https://github.com/volcano-sh/kthena/pull/945)
- Added GPU usage plugin E2E coverage. [#1199](https://github.com/volcano-sh/kthena/pull/1199)
- Refreshed the quick-start documentation and recommends starting with ModelServing. [#1260](https://github.com/volcano-sh/kthena/pull/1260)
- Added DeepSeek-v4 model-serving examples. [#936](https://github.com/volcano-sh/kthena/pull/936), [#937](https://github.com/volcano-sh/kthena/pull/937)
- Added KV-cache-aware scheduler plugin documentation. [#910](https://github.com/volcano-sh/kthena/pull/910)

## Stability and Correctness Highlights

- **Router request handling correctness:** Fixes in JWT auth, empty request validation, streaming content-type parsing, retry behavior, mid-stream error propagation, IPv6 backend URLs, and HTTPRoute matching make the router more predictable under production traffic.
- **Gateway API reconciliation correctness:** The router now respects HTTPRoute hostnames, matched rule selection, path-prefix semantics, and Gateway listener namespace admission rules, reducing mismatches between Gateway API configuration and runtime routing behavior.
- **Autoscaler and metrics reliability:** Autoscaler metric collection now uses the target-referenced namespace first, checks HTTP response status before parsing metrics, stabilizes scale-down with the maximum sliding window, and supports bounded concurrent pod metric scraping.
- **Controller concurrency safety:** ModelBooster and ModelServing controller fixes address data races, concurrent map writes, stale pod bindings, duplicate role deletion, and unsafe PodInfo access.
- **Webhook and deployment stability:** Webhook certificate generation, cert-manager CA injection, webhook names, validator messages, and quick-start example defaults were corrected to reduce installation and onboarding failures.
- **Test and CI hardening:** Additional unit and E2E coverage was added for controllers, autoscaler, router debug modules, ModelBooster lifecycle operations, SGLang PD routing, and status-aware scale-down behavior.

## Bug Fixes

### Router

- Fixed router auth panic when the JWKS cache is empty. [#1219](https://github.com/volcano-sh/kthena/pull/1219)
- Required the Bearer scheme for JWT auth. [#1035](https://github.com/volcano-sh/kthena/pull/1035)
- Rejected empty router model requests. [#1036](https://github.com/volcano-sh/kthena/pull/1036)
- Corrected streaming content-type detection when parameters are present. [#1145](https://github.com/volcano-sh/kthena/pull/1145)
- Fixed retries sending empty bodies to fallback pods in the aggregated proxy path. [#1031](https://github.com/volcano-sh/kthena/pull/1031)
- Returned mid-stream and copy errors from proxy requests. [#1049](https://github.com/volcano-sh/kthena/pull/1049)
- Added IPv6 pod backend URL support. [#1071](https://github.com/volcano-sh/kthena/pull/1071)
- Fixed stale KV cache ownership. [#1224](https://github.com/volcano-sh/kthena/pull/1224)
- Removed an unnecessary goroutine in the ModelPrefixStore LRU eviction callback. [#1243](https://github.com/volcano-sh/kthena/pull/1243)

### Autoscaler and Metrics

- Used the target-referenced namespace first in autoscaler metric collection. [#1068](https://github.com/volcano-sh/kthena/pull/1068)
- Checked HTTP status before parsing metrics responses. [#1142](https://github.com/volcano-sh/kthena/pull/1142)
- Used the maximum sliding window for autoscaler scale-down stabilization. [#946](https://github.com/volcano-sh/kthena/pull/946)
- Used gauge values for SGLang metrics. [#976](https://github.com/volcano-sh/kthena/pull/976)
- Treated zero TTFT/TPOT as uninitialized in least-latency scoring. [#1040](https://github.com/volcano-sh/kthena/pull/1040)
- Used ModelServer's configured workload port for metrics instead of hardcoded defaults. [#1205](https://github.com/volcano-sh/kthena/pull/1205)

### ModelServing and ModelBooster Controllers

- Used controller owner references for ModelBooster children. [#1054](https://github.com/volcano-sh/kthena/pull/1054)
- Fixed a ModelBoosterController data race and concurrent-map-write panic. [#1085](https://github.com/volcano-sh/kthena/pull/1085)
- Fixed stale ModelServer pod bindings. [#1126](https://github.com/volcano-sh/kthena/pull/1126)
- Fixed concurrent grace-map access in error-pod handling. [#1157](https://github.com/volcano-sh/kthena/pull/1157)
- Guarded PodInfo access against data races. [#1167](https://github.com/volcano-sh/kthena/pull/1167)
- Avoided duplicate role deletion while a role is already deleting. [#1269](https://github.com/volcano-sh/kthena/pull/1269)
- Fixed ModelServing webhook panic on missing replicas. [#1055](https://github.com/volcano-sh/kthena/pull/1055)
- Synced PodGroup NetworkTopology when ModelServing topology changes. [#1088](https://github.com/volcano-sh/kthena/pull/1088)

### Connectors and PD Paths

- Rebuilt NIXL prefill/decode request bodies on every proxy call. [#947](https://github.com/volcano-sh/kthena/pull/947)
- Rebuilt SGLang prefill/decode request bodies on every proxy call. [#984](https://github.com/volcano-sh/kthena/pull/984)
- Added streaming error handling improvements. [#1236](https://github.com/volcano-sh/kthena/pull/1236)

### Helm, Webhooks, and Examples

- Fixed cert-manager CA injection annotation for controller-manager webhooks. [#1018](https://github.com/volcano-sh/kthena/pull/1018)
- Used random serial numbers for webhook certificates. [#1160](https://github.com/volcano-sh/kthena/pull/1160)
- Corrected webhook names and validator messages. [#1152](https://github.com/volcano-sh/kthena/pull/1152), [#1159](https://github.com/volcano-sh/kthena/pull/1159)
- Fixed quick-start ModelBooster `cacheURI` and Hugging Face endpoint examples. [#1246](https://github.com/volcano-sh/kthena/pull/1246)

## API Changes and Upgrade Notes

### Breaking Change: AutoscalingPolicyBinding Removed

`AutoscalingPolicyBinding` has been removed from CRDs, generated clients, informers, listers, apply configurations, and Helm chart embedded CRDs.

Users should migrate autoscaling target configuration into one of the following `AutoscalingPolicy.spec` fields:

- `homogeneousTarget`
- `heterogeneousTarget`
- `disaggregatedTarget`

For further details, please refer to the [CRD documentation](https://kthena.volcano.sh/docs/next/reference/crd/workload.serving.volcano.sh) and the [Proposal](https://github.com/volcano-sh/kthena/pull/1172).

### New ModelBooster and Router Configuration

- `ModelBackend.runtimeClassName` configures the Kubernetes RuntimeClass used by generated backend pods.
- `ModelWorker.tolerations` configures worker pod tolerations.
- `KTHENA_SKIP_ENGINE_DEPENDENCY_INSTALL=true` skips startup-time connector dependency installation for offline or prebuilt engine images.
- `METRICS_SCRAPE_INTERVAL` controls the router pod metrics scrape interval.
- Controller-manager Helm values now support `debugPort`.

### Build Environment

The project toolchain, Dockerfiles, CI workflows, and development documentation have been upgraded to Go `1.26.4`. See [feat: comprehensive upgrade to Go 1.26.4 #1244](https://github.com/volcano-sh/kthena/pull/1244).

## Tests, Docs, and Infrastructure

Kthena v1.0.0 includes broad test, documentation, and infrastructure improvements:

- Added unit tests for ModelRouteController and GatewayController. [#992](https://github.com/volcano-sh/kthena/pull/992)
- Added E2E coverage for updating and deleting ModelBooster. [#1000](https://github.com/volcano-sh/kthena/pull/1000)
- Added E2E coverage for status-aware scale-down behavior. [#982](https://github.com/volcano-sh/kthena/pull/982)
- Added SGLang PD router E2E coverage. [#994](https://github.com/volcano-sh/kthena/pull/994)
- Added autoscaler, config, and router debug unit tests. [#903](https://github.com/volcano-sh/kthena/pull/903)
- Fixed flaky controller and router tests. [#950](https://github.com/volcano-sh/kthena/pull/950), [#1102](https://github.com/volcano-sh/kthena/pull/1102), [#1162](https://github.com/volcano-sh/kthena/pull/1162)
- Used the PodGroup informer cache when listing existing PodGroups. [#1081](https://github.com/volcano-sh/kthena/pull/1081)

## Upgrade Instructions

To upgrade to Kthena v1.0.0 after the release is published:

### 1. Review Breaking API Changes

Before upgrading, review any existing autoscaling resources in your cluster. `AutoscalingPolicyBinding` is removed in v1.0.0, so clusters using the old two-resource autoscaling model must migrate to the new single-resource `AutoscalingPolicy` model.

Check whether your cluster still has old binding resources:

```bash
kubectl get autoscalingpolicybindings.workload.serving.volcano.sh --all-namespaces
```

If any resources are returned, migrate their target and metric-source configuration into one of the following `AutoscalingPolicy.spec` fields before or during the upgrade:

- `homogeneousTarget`
- `heterogeneousTarget`
- `disaggregatedTarget`

For prefill/decode disaggregated workloads, use `disaggregatedTarget.roles` and, when needed, `disaggregatedTarget.ratioConstraint`.

### 2. Back Up Existing Kthena Resources

Back up Kthena custom resources before applying the new CRDs and controllers:

```bash
kubectl get modelservings.workload.serving.volcano.sh --all-namespaces -o yaml > modelservings-backup.yaml
kubectl get modelboosters.workload.serving.volcano.sh --all-namespaces -o yaml > modelboosters-backup.yaml
kubectl get autoscalingpolicies.workload.serving.volcano.sh --all-namespaces -o yaml > autoscalingpolicies-backup.yaml
kubectl get modelroutes.networking.serving.volcano.sh --all-namespaces -o yaml > modelroutes-backup.yaml
kubectl get modelservers.networking.serving.volcano.sh --all-namespaces -o yaml > modelservers-backup.yaml
```

If you still have `AutoscalingPolicyBinding` resources, back them up before removing or migrating them:

```bash
kubectl get autoscalingpolicybindings.workload.serving.volcano.sh --all-namespaces -o yaml > autoscalingpolicybindings-backup.yaml
```

### 3. Upgrade Kthena

#### Using Helm

For OCI-based Helm installation from GHCR:

```bash
helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena \
  --version v1.0.0 \
  --namespace kthena-system
```

If this is a fresh installation instead of an upgrade:

```bash
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena \
  --version v1.0.0 \
  --namespace kthena-system \
  --create-namespace
```

If you install from the release chart package:

```bash
curl -L -o kthena.tgz https://github.com/volcano-sh/kthena/releases/download/v1.0.0/kthena.tgz
helm upgrade kthena kthena.tgz --namespace kthena-system
```

### 4. Verify the Upgrade

Check that all Kthena components are running:

```bash
kubectl get pods -n kthena-system
kubectl get svc -n kthena-system
kubectl get crd | grep serving.volcano.sh
```

Verify workload, networking, and autoscaling resources:

```bash
kubectl get modelservings.workload.serving.volcano.sh --all-namespaces
kubectl get modelboosters.workload.serving.volcano.sh --all-namespaces
kubectl get autoscalingpolicies.workload.serving.volcano.sh --all-namespaces
kubectl get modelroutes.networking.serving.volcano.sh --all-namespaces
kubectl get modelservers.networking.serving.volcano.sh --all-namespaces
```

### Upgrade Notes

- `AutoscalingPolicyBinding` is removed. Migrate to `AutoscalingPolicy.spec.homogeneousTarget`, `spec.heterogeneousTarget`, or `spec.disaggregatedTarget` before relying on v1.0.0 autoscaling behavior.
- `disaggregatedTarget` supports role-level autoscaling and optional role ratio constraints. Fixed roles can use `minReplicas == maxReplicas`.
- Existing ModelBooster behavior remains backward compatible by default. Set `KTHENA_SKIP_ENGINE_DEPENDENCY_INSTALL=true` only when using offline or prebuilt engine images that already include connector dependencies.
- GPU clusters that require custom runtimes can set `ModelBackend.runtimeClassName`; tainted GPU nodes can be targeted with `ModelWorker.tolerations`.
- Router pod metrics scraping can be tuned with `METRICS_SCRAPE_INTERVAL`. Keep the default unless you need fresher scheduling signals or lower scrape overhead.
- Development and build prerequisites now use Go `1.26.4`; this affects contributors and image builders, not normal Helm or manifest-based runtime upgrades.

## Thank You, Contributors

Thank you to everyone who contributed to Kthena v1.0.0 across controllers, router, autoscaler, CLI, Helm charts, examples, docs, CI, generated clients, and tests.

Special thanks to contributors including [@Abirdcfly](https://github.com/Abirdcfly), [@Alivestars04](https://github.com/Alivestars04), [@anirudh240](https://github.com/anirudh240), [@avinxshKD](https://github.com/avinxshKD), [@blenbot](https://github.com/blenbot), [@FAUST-BENCHOU](https://github.com/FAUST-BENCHOU), [@hzxuzhonghu](https://github.com/hzxuzhonghu), [@JagjeevanAK](https://github.com/JagjeevanAK), [@katara-Jayprakash](https://github.com/katara-Jayprakash), [@kube-gopher](https://github.com/kube-gopher), [@LiZhenCheng9527](https://github.com/LiZhenCheng9527), [@madmecodes](https://github.com/madmecodes), [@nabrahma](https://github.com/nabrahma), [@nXtCyberNet](https://github.com/nXtCyberNet), [@rajnish-jais](https://github.com/rajnish-jais), [@Sanchit2662](https://github.com/Sanchit2662), [@verma-garv](https://github.com/verma-garv), [@WHOIM1205](https://github.com/WHOIM1205), [@xrwang8](https://github.com/xrwang8), [@zhy76](https://github.com/zhy76), and [@YaoZengzeng](https://github.com/YaoZengzeng).

We warmly invite developers, operators, and AI infrastructure teams to try Kthena v1.0.0 and help shape the next generation of cloud native LLM serving.
