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

The kthena-router scheduler relies on two cache-oriented score plugins — `prefix-cache` and `kvcache-aware` — to steer requests toward pods that are likely to have a warm cache. Both plugins make decisions whose quality is invisible at runtime: the only signal today is verbose debug logging, which is impractical during load testing and offers no aggregated or historical view.

This proposal adds Prometheus instrumentation to both plugins, exported through the router's existing `/metrics` endpoint, plus a sample Grafana dashboard. The metrics cover cache match quality (a hit/miss-and-depth match-ratio distribution), internal latency breakdown (Redis, tokenization), error rates, and cache occupancy/eviction. They are registered through the router's existing central metrics registry so that naming, labelling, and registration stay consistent with the rest of the router.

### Motivation

Both plugins perform caching/matching logic that critically affects scheduling quality, yet expose no runtime telemetry:

- **`prefix-cache`** is fundamentally a cache. Its effectiveness is defined by hit rate, match ratio, occupancy, and eviction pressure — none of which are observable.
- **`kvcache-aware`** depends on a tokenizer round-trip and batched Redis lookups for block matching. Tokenizer and Redis latency directly bound router throughput and scoring accuracy, but are only ever logged.

Without telemetry it is difficult to (1) evaluate plugin effectiveness under load, (2) locate performance bottlenecks (Redis latency, tokenizer latency), and (3) tune configuration parameters (`blockSizeToHash`, `maxBlocksToMatch`, cache capacity).

#### Goals

- Export Prometheus metrics for both plugins via the router's existing `/metrics` endpoint.
- Make cache hit rate, match ratio, internal latency, and error rate queryable and aggregatable, labelled by `model`.
- Reuse the router's existing metric infrastructure and naming conventions (`kthena_router_*` prefix).
- Ship a sample Grafana dashboard for load-test analysis.
- Introduce no measurable regression in scoring latency.

#### Non-Goals

- Per-request distributed tracing (OpenTelemetry spans). Listed as future work.
- Restructuring the prefix store or the Redis key schema.
- A `pod`-level label on any metric (rejected on cardinality grounds; see Risks).
- Instrumenting other score plugins (`gpu`, `least-latency`, `least-request`, `lora-affinity`); they are already covered adequately by the generic per-plugin duration metric and are out of scope here.

### Proposal

Add two groups of plugin-scoped metrics, all prefixed `kthena_router_` and labelled with `model` where applicable, recorded from within each plugin. Match-quality and latency metrics are recorded on the request path through the per-request metrics recorder already available to each plugin; occupancy and eviction metrics are maintained out-of-band — pod deletion and cache eviction run outside any request — against the router's global metrics registry.

Cache effectiveness is captured as a **match-ratio histogram** rather than separate hit/miss counters plus an absolute match-length histogram. The match ratio (fraction of the prompt's blocks the best-matching candidate pod had cached, `0` on a miss) is a per-event *magnitude*, which a histogram is the correct instrument for: a counter would collapse a 1-block match and a full match into the same `+1`, losing the very signal a cache needs. The histogram subsumes the hit/miss counters — its `le="0.0"` bucket is the miss count, so hit rate is derivable as `1 - (rate(..._bucket{le="0.0"}) / rate(..._count))` — and also expresses *how much* prefix was reused, as a percentage.

A key design constraint discovered during review: total scoring duration is **already exported** generically as `kthena_router_scheduler_plugin_duration_seconds{plugin,type="score"}`, recorded for every score plugin. This proposal therefore does **not** add a per-plugin total-scoring histogram (that would duplicate it) and instead instruments only the sub-phases the generic metric cannot break down.

#### Metrics: `prefix-cache`

| Metric                                          | Type     | Labels  | Description                                                                                  |
|------------------------------------------------|----------|---------|----------------------------------------------------------------------------------------------|
| `kthena_router_prefix_cache_match_ratio`       | Histogram| `model` | Fraction (`0…1`) of the prompt's blocks the best-matching candidate pod had already cached, one observation per match attempt, `0` on a miss. Reflects locality *available* in the candidate set, not necessarily the pod finally routed to. Bucketed with finer resolution near `0` and `1`, where matches tend to cluster. Hit rate is derivable from the `le="0.0"` bucket (`1 - rate(_bucket{le="0.0"}) / rate(_count)`); the upper buckets show *how much* prefix was reused. |
| `kthena_router_prefix_cache_evictions_total`   | Counter  | `model` | Number of (prefix block, pod) entries evicted from a per-pod cache when it reached capacity. Excludes entries removed because a pod was deleted (see Notes). |
| `kthena_router_prefix_cache_entries`           | Gauge    | —       | Total prefix-cache occupancy: the number of (prefix block, pod) entries currently stored, summed over every pod's cache at scrape time. The router records, per pod, which recent prefix blocks that pod has served, so a block held by N pods counts N times. Bounded by `(#pods with entries) × maxHashCacheSize`; once every per-pod cache is full the value plateaus (1-for-1 eviction) and changes only as pods are added or deleted. |

#### Metrics: `kvcache-aware`

| Metric                                                 | Type     | Labels                | Description                                                                                    |
|--------------------------------------------------------|----------|-----------------------|------------------------------------------------------------------------------------------------|
| `kthena_router_kvcache_aware_match_ratio`              | Histogram| `model`               | Fraction (`0…1`) of the prompt's blocks whose KV cache the best-matching candidate pod already held, one observation per match attempt, `0` on a miss. Reflects locality *available* in the candidate set, not necessarily the pod finally routed to. Hit rate is derivable from the `le="0.0"` bucket; the upper buckets show how much KV prefix was reused. |
| `kthena_router_kvcache_aware_redis_duration_seconds`   | Histogram| `model`               | Latency of the batched Redis block-lookup performed during a match attempt. |
| `kthena_router_kvcache_aware_tokenize_duration_seconds`| Histogram| `model`               | Latency of tokenizing the prompt during a match attempt. |
| `kthena_router_kvcache_aware_errors_total`             | Counter  | `model`, `stage`      | Number of match attempts aborted by an error, labelled by failing stage (`tokenize` or `redis`). Counted separately so transient failures do not distort the hit rate. |

> **Note on `errors_total`:** Some match attempts abort early on an error rather than producing a cache miss — a tokenization failure or a Redis failure. These never reach the `match_ratio` observation, so they neither inflate misses nor count as a `0` match; they are tracked separately and labelled by stage. This directly serves the bottleneck-diagnosis goal.

#### Grafana Dashboard

Ship a sample dashboard (JSON) under `examples/observability/` visualising, per model: hit rate (`1 - rate(match_ratio_bucket{le="0.0"}) / rate(match_ratio_count)`), match-ratio distribution (p50/p90/p99), Redis and tokenizer latency quantiles, error rate by stage, and prefix-cache occupancy/eviction trend.

### Notes/Constraints

- **Non-attempts record nothing.** An empty prompt, or a prompt that produces no hashable blocks, is skipped before any metric is recorded, so it produces no `match_ratio` observation at all — it is neither a miss (`0`) nor a hit. Only a genuine match attempt over a non-empty prompt records a sample, and a miss is exactly a `0` observation (the `le="0.0"` bucket).
- **Two eviction paths exist and only one is an eviction.** Capacity eviction — when a per-pod cache is full — is the only path counted by `evictions_total`. Entries also disappear when a pod is deleted, but that removal is deliberately not counted as an eviction. Both paths shrink the per-pod caches, so both are reflected in the `entries` gauge (see below).
- **`entries` is computed by a scrape-time scan, not a maintained counter.** The gauge is computed lazily at scrape time by summing the current size of every per-pod cache, rather than being maintained incrementally on every insert and eviction. This keeps the hot insert and eviction paths free of extra bookkeeping, at the cost of a small read-lock held only on the (infrequent) scrape path. A running counter updated on every cache mutation was considered but rejected as unnecessary complexity for a value that is only read at scrape time.
- **Occupancy and eviction metrics are recorded against the router's global metrics registry,** because eviction and pod deletion happen outside any request and have no per-request recorder.
- **Miss bucket selector is `le="0.0"`, not `le="0"`.** Prometheus normalizes histogram bucket boundaries to a canonical float form on ingestion, so the `0` boundary is stored as `le="0.0"` (and `1` as `le="1.0"`). Hit-rate queries and alerts must select `le="0.0"`; `le="0"` silently matches nothing. The sample dashboard uses the normalized form.