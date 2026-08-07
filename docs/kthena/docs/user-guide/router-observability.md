# Router Observability

The Kthena router exposes three complementary observability surfaces:

- Prometheus metrics for alerting, dashboards, and capacity analysis
- One access-log record for each inference request
- Loopback-only configuration-dump and pprof endpoints for live diagnosis

## Endpoint map

The current Helm chart serves health and metrics routes on the inference
listener. The debug listener is a separate loopback-only server inside the
router pod.

| Listener | Default | Endpoints | Exposure |
| --- | --- | --- | --- |
| Router | Container port `8080`; Service port `80` | `/healthz`, `/readyz`, `/metrics`, inference APIs | The `kthena-router` LoadBalancer Service |
| Debug | `localhost:15000` | `/debug/config_dump/*`, `/debug/pprof/*` | Pod loopback only; no Service port |

The chart does not currently provide an `observability.metrics` values block.
The metrics path is fixed at `/metrics`, and its port follows
`networking.kthenaRouter.port`. Do not set the previously documented
`observability.metrics` keys; Helm ignores them.

To inspect these endpoints without relying on their external exposure:

```bash
# The Service listens on 80 and forwards to the router's default port, 8080.
kubectl port-forward -n kthena-system service/kthena-router 8080:80

# Run separately when debug access is needed. This selects a router pod and
# reaches the process's loopback-only listener from inside its network namespace.
kubectl port-forward -n kthena-system deployment/kthena-router 15000:15000
```

If the release namespace is not `kthena-system`, replace it in the commands.

## Metrics

The tables below list every Kthena-owned router metric family registered by the
current binary. Histograms additionally expose Prometheus `_bucket`, `_sum`, and
`_count` series. Standard Go runtime, process, and Prometheus handler metrics are
also present; those collector-provided families can vary with dependency
versions.

Some metrics appear only after the corresponding feature or request path has
been exercised.

### Requests, traffic, and rate limiting

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kthena_router_requests_total` | Counter | `model`, `path`, `status_code`, `error_type` | Completed HTTP requests |
| `kthena_router_request_duration_seconds` | Histogram | `model`, `path`, `status_code` | End-to-end request latency |
| `kthena_router_request_prefill_duration_seconds` | Histogram | `model`, `path`, `status_code` | Prefill latency for prefill/decode-disaggregated requests |
| `kthena_router_request_decode_duration_seconds` | Histogram | `model`, `path`, `status_code` | Decode latency for prefill/decode-disaggregated requests |
| `kthena_router_tokens_total` | Counter | `model`, `path`, `token_type` | Input or output tokens; `token_type` is `input` or `output` |
| `kthena_router_rate_limit_exceeded_total` | Counter | `model`, `limit_type`, `path` | Requests rejected by input-token, output-token, or request rate limits |
| `kthena_router_active_requests` | Gauge | none | All requests currently handled by this router process |
| `kthena_router_active_downstream_requests` | Gauge | `model` | Active client-to-router requests |
| `kthena_router_active_upstream_requests` | Gauge | `model_server`, `model_route` | Active router-to-backend requests |

### Scheduler and user-fairness queue

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kthena_router_scheduler_plugin_duration_seconds` | Histogram | `model`, `plugin`, `type` | Scheduler plugin execution time; `type` is `filter` or `score` |
| `kthena_router_fairness_queue_size` | Gauge | `model`, `user_id` | Pending requests in the fairness queue |
| `kthena_router_fairness_queue_duration_seconds` | Histogram | `model`, `user_id` | Time spent waiting in the fairness queue |
| `kthena_router_fairness_queue_cancelled_total` | Counter | `model`, `user_id` | Requests cancelled or timed out while queued |
| `kthena_router_fairness_queue_dequeue_total` | Counter | `model`, `user_id` | Requests successfully dequeued |
| `kthena_router_fairness_queue_inflight` | Gauge | `model` | Requests admitted through the fairness semaphore |
| `kthena_router_fairness_queue_priority_refresh_total` | Counter | `model` | Dequeue-time priority refresh and reinsert operations |
| `kthena_router_fairness_queue_heap_rebuild_total` | Counter | `model` | Full heap rebuilds caused by priority drift |

`user_id` values originate from authenticated request identity. Treat them as
sensitive, and account for their cardinality when retaining or federating these
series.

### Tokenizer and cache-aware scheduling

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kthena_router_tokenizer_unsupported_engine_total` | Counter | `model`, `engine` | Tokenizer lookups for which no pod used a supported engine |
| `kthena_router_prefix_cache_match_ratio` | Histogram | `model` | Best prefix-cache match ratio for each scheduling attempt |
| `kthena_router_prefix_cache_evictions_total` | Counter | `model` | Per-pod prefix-cache entries evicted at capacity |
| `kthena_router_prefix_cache_entries` | Gauge | none | Current `(prefix block, pod)` entries across all local caches |
| `kthena_router_kvcache_aware_match_ratio` | Histogram | `model` | Best external KV-cache match ratio for each attempt |
| `kthena_router_kvcache_aware_redis_duration_seconds` | Histogram | `model` | Batched Redis lookup latency |
| `kthena_router_kvcache_aware_tokenize_duration_seconds` | Histogram | `model` | Prompt tokenization latency for KV-cache-aware matching |
| `kthena_router_kvcache_aware_errors_total` | Counter | `model`, `stage` | Aborted KV-cache-aware attempts; `stage` is `tokenize` or `redis` |

### Session-boost queue

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `kthena_router_session_boost_queue_size` | Gauge | `model` | Pending requests in the session-boost queue |
| `kthena_router_session_boost_queue_duration_seconds` | Histogram | `model` | Time spent waiting in the session-boost queue |
| `kthena_router_session_boost_queue_cancelled_total` | Counter | `model` | Requests cancelled or timed out while queued |
| `kthena_router_session_boost_queue_dequeue_total` | Counter | `model` | Requests successfully dequeued |
| `kthena_router_session_boost_queue_inflight` | Gauge | `model` | Requests admitted through the session-boost queue |

### Histogram buckets

| Metrics | Buckets |
| --- | --- |
| Request, prefill, and decode durations | `0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`, `30`, `60` seconds |
| Scheduler plugin duration | `0.001`, `0.005`, `0.01`, `0.05`, `0.1`, `0.5` seconds |
| Fairness and session-boost queue durations | `0.001`, `0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5` seconds |
| Prefix-cache and KV-cache match ratios | `0`, `0.1`, `0.25`, `0.5`, `0.75`, `0.9`, `0.95`, `0.99`, `1.0` |
| KV-cache Redis and tokenization durations | `0.0005`, `0.001`, `0.0025`, `0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5` seconds |

## Access logs

Helm enables access logging in `text` format by default. This is the
compatibility default and is not changed by this documentation update. JSON is
recommended for structured ingestion, but operators must opt in explicitly.

```yaml
networking:
  kthenaRouter:
    accessLog:
      enabled: true
      format: json
      output: stdout
```

For a standalone deployment, the equivalent environment variables are:

```yaml
env:
  - name: ACCESS_LOG_ENABLED
    value: "true"
  - name: ACCESS_LOG_FORMAT
    value: "json"
  - name: ACCESS_LOG_OUTPUT
    value: "stdout"
```

The valid formats are `text` and `json`. Output can be `stdout`, `stderr`, or a
file path. Prefer `stdout` in Kubernetes so the container runtime and log agent
can collect the records; a file path is local to the container filesystem.

The JSON field names below match the emitted contract:

```json
{
  "timestamp": "2026-01-09T14:35:22.147Z",
  "method": "POST",
  "path": "/v1/chat/completions",
  "protocol": "HTTP/1.1",
  "status_code": 200,
  "model_name": "llama3-70b-instruct",
  "model_route": "prod/llama3-70b-route",
  "model_server": "prod/llama3-70b-server",
  "selected_pod": "llama3-70b-deployment-7b9f4c2d-kjx9p",
  "request_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "input_tokens": 412,
  "output_tokens": 189,
  "duration_total": 3840,
  "duration_request_processing": 65,
  "duration_upstream_processing": 3480,
  "duration_response_processing": 115
}
```

All duration fields are integer milliseconds. There is no `duration_queue`
field. `duration_total` is authoritative and can exceed the sum of the marked
processing phases, including when routing or queue time falls between phase
checkpoints. See the [access-log field reference](../reference/router-access-log-fields.md)
for optional routing fields and current error types.

When `ACCESS_LOG_OUTPUT` is `stdout` or `stderr`, `kubectl logs` can contain
access-log records alongside other process logs. The following examples
therefore filter JSON lines before invoking `jq`. If access logs are written to
a file path, they do not appear in `kubectl logs`; inspect the mounted file or
the configured log collector instead.

```bash
kubectl logs -n kthena-system deployment/kthena-router -f \
  | grep -E '^\{.*\}$' \
  | jq .
```

## Health endpoints

| Endpoint | Successful response | Purpose |
| --- | --- | --- |
| `/healthz` | HTTP `200`, `{"message":"ok"}` | Process liveness |
| `/readyz` | HTTP `200`, `{"message":"router is ready"}` | Controller and datastore readiness; returns HTTP `503` until ready |

## Debug and pprof endpoints

The debug server binds to `localhost:15000` by default. It is intentionally not
published by the Helm Service. A port-forward grants access to sensitive routing
state and runtime profiles; keep it open only while diagnosing a trusted cluster.

### Configuration dump

| Endpoint | Description |
| --- | --- |
| `/debug/config_dump/modelroutes` | All ModelRoute resources known to the router |
| `/debug/config_dump/modelservers` | All ModelServer resources known to the router |
| `/debug/config_dump/pods` | All inference pods known to the router |
| `/debug/config_dump/gateways` | All Gateway resources known to the router |
| `/debug/config_dump/httproutes` | All HTTPRoute resources known to the router |
| `/debug/config_dump/inferencepools` | All InferencePool resources known to the router |
| `/debug/config_dump/namespaces/{namespace}/modelroutes/{name}` | One namespaced ModelRoute |
| `/debug/config_dump/namespaces/{namespace}/modelservers/{name}` | One namespaced ModelServer |
| `/debug/config_dump/namespaces/{namespace}/pods/{name}` | One namespaced pod |
| `/debug/config_dump/namespaces/{namespace}/gateways/{name}` | One namespaced Gateway |
| `/debug/config_dump/namespaces/{namespace}/httproutes/{name}` | One namespaced HTTPRoute |
| `/debug/config_dump/namespaces/{namespace}/inferencepools/{name}` | One namespaced InferencePool |

### Runtime profiling

| Endpoint | Description |
| --- | --- |
| `/debug/pprof/` | pprof index |
| `/debug/pprof/profile` | CPU profile |
| `/debug/pprof/goroutine` | Goroutine profile |
| `/debug/pprof/heap` | Heap profile |
| `/debug/pprof/allocs` | Allocation profile |
| `/debug/pprof/block` | Blocking profile |
| `/debug/pprof/mutex` | Mutex-contention profile |

## Troubleshooting examples

After starting both port-forwards from the [endpoint map](#endpoint-map):

```bash
# Request counters by model and result.
curl -s http://localhost:8080/metrics \
  | grep '^kthena_router_requests_total'

# Router configuration and currently known pods.
curl -s http://localhost:15000/debug/config_dump/modelservers | jq .
curl -s http://localhost:15000/debug/config_dump/pods | jq .

# Recent 5xx access records (requires JSON access-log format).
kubectl logs -n kthena-system deployment/kthena-router --since=30m \
  | grep -E '^\{.*\}$' \
  | jq 'select(.status_code >= 500) | {timestamp, model: .model_name, error, pod: .selected_pod, duration: .duration_total}'

# Requests slower than four seconds (requires JSON access-log format).
kubectl logs -n kthena-system deployment/kthena-router --since=20m \
  | grep -E '^\{.*\}$' \
  | jq 'select(.duration_total > 4000) | {model: .model_name, total: .duration_total, upstream: .duration_upstream_processing, pod: .selected_pod}'
```
