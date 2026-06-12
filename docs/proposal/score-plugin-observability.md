---
title: Observability for prefix-cache and kvcache-aware Score Plugins
authors:
  - "@kube-gopher"
reviewers:
  - TBD
approvers:
  - TBD
creation-date: 2026-06-13
---

## Observability for prefix-cache and kvcache-aware Score Plugins

### Summary

The kthena-router scheduler relies on two cache-oriented score plugins — `prefix-cache` (`pkg/kthena-router/scheduler/plugins/prefix.go`) and `kvcache-aware` (`pkg/kthena-router/scheduler/plugins/kvcache_aware.go`) — to steer requests toward pods that are likely to have a warm cache. Both plugins make decisions whose quality is invisible at runtime: the only signal today is `klog.V(4)` output, which is impractical during load testing and offers no aggregated or historical view.

This proposal adds Prometheus instrumentation to both plugins, exported through the router's existing `/metrics` endpoint, plus a sample Grafana dashboard. The metrics cover cache hit/miss behaviour, match-length distribution, internal latency breakdown (Redis, tokenization), error rates, and cache occupancy/eviction. They are registered through the existing central `metrics.Metrics` struct (`pkg/kthena-router/metrics/metrics.go`) so that naming, labelling, and registration stay consistent with the rest of the router.

### Motivation

Both plugins perform caching/matching logic that critically affects scheduling quality, yet expose no runtime telemetry:

- **`prefix-cache`** is fundamentally a cache. Its effectiveness is defined by hit rate, match length, occupancy, and eviction pressure — none of which are observable.
- **`kvcache-aware`** depends on a tokenizer round-trip and batched Redis lookups (`kvcache_aware.go:204-230`) for block matching. Tokenizer and Redis latency directly bound router throughput and scoring accuracy, but are only ever logged.

Without telemetry it is difficult to (1) evaluate plugin effectiveness under load, (2) locate performance bottlenecks (Redis latency, tokenizer latency), and (3) tune configuration parameters (`blockSizeToHash`, `maxBlocksToMatch`, cache capacity).

#### Goals

- Export Prometheus metrics for both plugins via the router's existing `/metrics` endpoint.
- Make cache hit rate, match length, internal latency, and error rate queryable and aggregatable, labelled by `model`.
- Reuse existing router metric infrastructure (central `Metrics` struct + `promauto`) and naming conventions (`kthena_router_*` prefix).
- Ship a sample Grafana dashboard for load-test analysis.
- Introduce no measurable regression in `Score()` latency.

#### Non-Goals

- Per-request distributed tracing (OpenTelemetry spans). Listed as future work.
- Restructuring the prefix store or the Redis key schema.
- A `pod`-level label on any metric (rejected on cardinality grounds; see Risks).
- Instrumenting other score plugins (`gpu`, `least-latency`, `least-request`, `lora-affinity`); they are already covered adequately by the generic per-plugin duration metric and are out of scope here.

### Proposal

Add two groups of plugin-scoped metrics, all prefixed `kthena_router_` and labelled with `model` where applicable, recorded from within each plugin. Hit/miss/match/latency metrics are recorded on the request path through the `MetricsRecorder` already available on `framework.Context`; occupancy/eviction metrics are maintained out-of-band (pod deletion and LRU eviction run outside any request) against the global `metrics.DefaultMetrics`.

A key design constraint discovered during review: total `Score()` duration is **already exported** generically as `kthena_router_scheduler_plugin_duration_seconds{plugin,type="score"}`, recorded for every score plugin at `scheduler_impl.go:217-224`. This proposal therefore does **not** add a per-plugin total-Score histogram (that would duplicate it) and instead instruments only the sub-phases the generic metric cannot break down.

#### Metrics: `prefix-cache`

| Metric                                          | Type     | Labels  | Description                                                                                  |
|------------------------------------------------|----------|---------|----------------------------------------------------------------------------------------------|
| `kthena_router_prefix_cache_hits_total`        | Counter  | `model` | Score calls with ≥1 matching pod (`matchByName` non-empty, `prefix.go:172`)                  |
| `kthena_router_prefix_cache_misses_total`      | Counter  | `model` | Score calls that hashed a non-empty prompt but found zero matches                            |
| `kthena_router_prefix_cache_blocks_matched`    | Histogram| `model` | Longest prefix match length (in blocks) per request                                          |
| `kthena_router_prefix_cache_evictions_total`   | Counter  | `model` | Hash entries evicted from a pod LRU due to capacity pressure                                 |
| `kthena_router_prefix_cache_entries`           | Gauge    | —       | Current number of cached hash entries (maintained counter)                                   |

#### Metrics: `kvcache-aware`

| Metric                                                 | Type     | Labels                | Description                                                                                    |
|--------------------------------------------------------|----------|-----------------------|------------------------------------------------------------------------------------------------|
| `kthena_router_kvcache_aware_hits_total`               | Counter  | `model`               | Score calls with ≥1 block match (`podScores` non-empty, `kvcache_aware.go:232`)                |
| `kthena_router_kvcache_aware_misses_total`             | Counter  | `model`               | Score calls that produced block hashes but matched zero blocks                                 |
| `kthena_router_kvcache_aware_blocks_matched`           | Histogram| `model`               | Longest prefix-block match length per request (`lastMatchedBlock+1`)                           |
| `kthena_router_kvcache_aware_redis_duration_seconds`   | Histogram| `model`               | Time in the batched Redis lookup (`kvcache_aware.go:222-224`)                                  |
| `kthena_router_kvcache_aware_tokenize_duration_seconds`| Histogram| `model`               | Time spent tokenizing the prompt (`kvcache_aware.go:204-206`)                                  |
| `kthena_router_kvcache_aware_errors_total`             | Counter  | `model`, `stage`      | Score calls that aborted on error, by stage (`tokenize`, `redis`)                              |

> **Note on `errors_total`:** Several `Score()` paths return `nil` that are *not* cache misses — tokenization failure (`kvcache_aware.go:209-212`) and Redis failure (`kvcache_aware.go:225-227`). Folding these into `misses_total` would corrupt the hit-rate signal, so they are counted separately and labelled by stage. This directly serves the bottleneck-diagnosis goal.

#### Grafana Dashboard

Ship a sample dashboard (JSON) under `docs/proposal/images/` or `examples/observability/` visualising, per model: hit rate (`rate(hits)/rate(hits+misses)`), match-length distribution, Redis and tokenizer latency quantiles, error rate by stage, and prefix-cache occupancy/eviction trend.

### Notes/Constraints

- **Miss definition excludes non-attempts.** Empty/nil prompt and "no hashes generated" early returns (`prefix.go:164`, `kvcache_aware.go:198-220`) count as neither hit nor miss. Only a genuine lookup that produced zero matches is a miss.
- **Two eviction paths exist and only one is an eviction.** Capacity eviction fires the LRU `onEvict` callback (`prefix_store.go:201-204`); pod deletion removes entries directly via `onHashEvicted` (`prefix_store.go:97-124`), bypassing that callback. `evictions_total` counts **only** capacity evictions. The `entries` gauge is decremented on both paths.
- **`entries` is a maintained counter, not a scrape-time scan.** Prefix state is spread across per-pod LRUs plus 32-way sharded per-model maps (`prefix_store.go:68-79`); summing it at scrape time would take many read locks and contend with `Score()`/`Add()`. Instead an `atomic.Int64` is incremented on insert and decremented on eviction/removal, read by a `GaugeFunc` (same pattern as `ActiveRequests`, `metrics.go:241-249`).
- **Occupancy/eviction metrics use the global `DefaultMetrics`,** because eviction and pod deletion run outside request context and have no `MetricsRecorder`.