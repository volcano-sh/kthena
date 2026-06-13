# Score-plugin observability

Prometheus metrics and a sample Grafana dashboard for the kthena-router
`prefix-cache` and `kvcache-aware` score plugins. See the design proposal in
[`docs/proposal/score-plugin-observability.md`](../../docs/proposal/score-plugin-observability.md).

## Metrics

All metrics are exported on the router's existing `/metrics` endpoint and are
labelled with `model` (the `kvcache-aware` error counter is additionally
labelled with `stage`).

### `prefix-cache`

| Metric | Type |
|--------|------|
| `kthena_router_prefix_cache_hits_total` | Counter |
| `kthena_router_prefix_cache_misses_total` | Counter |
| `kthena_router_prefix_cache_blocks_matched` | Histogram |
| `kthena_router_prefix_cache_evictions_total` | Counter |
| `kthena_router_prefix_cache_entries` | Gauge |

### `kvcache-aware`

| Metric | Type |
|--------|------|
| `kthena_router_kvcache_aware_hits_total` | Counter |
| `kthena_router_kvcache_aware_misses_total` | Counter |
| `kthena_router_kvcache_aware_blocks_matched` | Histogram |
| `kthena_router_kvcache_aware_redis_duration_seconds` | Histogram |
| `kthena_router_kvcache_aware_tokenize_duration_seconds` | Histogram |
| `kthena_router_kvcache_aware_errors_total` | Counter (`stage`: `tokenize`, `redis`) |

> Total `Score()` duration is already exported generically as
> `kthena_router_scheduler_plugin_duration_seconds{plugin="prefix-cache"|"kvcache-aware", type="score"}`,
> so it is not duplicated here.

## Scraping with kube-prometheus-stack

Apply the ServiceMonitor (adjust namespace and the `release` label to match your
Prometheus):

```bash
kubectl apply -f router-servicemonitor.yaml
```

Verify the new series are present:

```bash
kubectl -n kthena-system port-forward svc/kthena-router 8080:80 &
curl -s localhost:8080/metrics | grep -E 'kthena_router_(prefix_cache|kvcache_aware)'
```

## Grafana dashboard

Import `grafana-dashboard-score-plugins.json` (Dashboards → Import) and select
your Prometheus data source. The dashboard has a `model` template variable and
panels for hit rate, match-length distribution, Redis/tokenizer latency,
eviction rate, occupancy, and error rate by stage.
