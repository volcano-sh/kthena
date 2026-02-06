# Router Observability

## Overview & Purpose

The **kthena-router** serves as the central data-plane gateway for all inference traffic in the Kthena LLM inference platform.  
It is responsible for request routing, load balancing, scheduling, fairness queuing, rate limiting, token accounting, and (when applicable) disaggregated prefill/decode forwarding.

Without strong observability, diagnosing issues such as:

- Why is this model slow?
- Which users are being unfairly delayed?
- Where are the 5xx errors coming from?
- Is the scheduler making good pod selections?
- Are we hitting rate limits or resource exhaustion?

becomes extremely difficult and time-consuming.

This observability framework provides production-grade visibility through three main channels:

1. **Prometheus metrics** — quantitative signals for dashboards, alerting, and trending
2. **Structured access logs** — rich per-request forensic details
3. **Debug endpoints** — instant insight into routing configuration and system state

Together they enable fast root-cause analysis, performance tuning, capacity planning, cost monitoring, and abuse detection.

## Metrics

### Endpoint

- **Metrics Port**: `8080` (default)  
- **Metrics Path**: `/metrics`  

**Note:** The Prometheus metrics are exposed on port **8080** by default.  
The debug endpoints (`/debug/config_dump/*`) are served on port **15000**.

### Core Request & Latency Metrics

| Metric Name                                          | Type      | Description                                                  | Labels                                      | Buckets                                                                 |
|------------------------------------------------------|-----------|--------------------------------------------------------------|---------------------------------------------|-------------------------------------------------------------------------|
| `kthena_router_requests_total`                       | Counter   | Total requests processed                                     | `model`, `path`, `status_code`, `error_type` | —                                                                       |
| `kthena_router_request_duration_seconds`             | Histogram | End-to-end latency (client → response)                       | `model`, `path`, `status_code`              | 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60        |
| `kthena_router_request_prefill_duration_seconds`     | Histogram | Prefill (prompt processing) phase duration                   | `model`, `path`, `status_code`              | same as above                                                           |
| `kthena_router_request_decode_duration_seconds`      | Histogram | Decode (token generation) phase duration                     | `model`, `path`, `status_code`              | same as above                                                           |
| `kthena_router_active_downstream_requests`           | Gauge     | Currently active client requests                             | `model`                                     | —                                                                       |
| `kthena_router_active_upstream_requests`             | Gauge     | Currently active requests to inference pods                  | `model_route`, `model_server`               | —                                                                       |

### Token & Usage Metrics

| Metric Name                            | Type    | Description                                      | Labels                              |
|----------------------------------------|---------|--------------------------------------------------|-------------------------------------|
| `kthena_router_tokens_total`           | Counter | Total tokens processed (input + output)          | `model`, `path`, `token_type` (input/output) |

### Scheduler & Fairness Metrics

| Metric Name                                           | Type      | Description                                            | Labels                        | Buckets                                                                |
|-------------------------------------------------------|-----------|--------------------------------------------------------|-------------------------------|------------------------------------------------------------------------|
| `kthena_router_scheduler_plugin_duration_seconds`     | Histogram | Execution time per scheduler plugin                    | `model`, `plugin`, `type`     | 0.001, 0.005, 0.01, 0.05, 0.1, 0.5                                     |
| `kthena_router_fairness_queue_size`                   | Gauge     | Current queued requests per model/user                 | `model`, `user_id`            | —                                                                      |
| `kthena_router_fairness_queue_duration_seconds`       | Histogram | Time spent waiting in fairness/priority queue          | `model`, `user_id`            | 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5             |

### Rate Limiting & Protection

| Metric Name                                      | Type    | Description                                          | Labels                        |
|--------------------------------------------------|---------|------------------------------------------------------|-------------------------------|
| `kthena_router_rate_limit_exceeded_total`        | Counter | Requests rejected due to rate limiting               | `model`, `limit_type`, `path` |

## Access Logs

### Recommended Format: Structured JSON

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
  "duration_total_ms": 3840,
  "duration_queue_ms": 180,
  "duration_request_processing_ms": 65,
  "duration_upstream_ms": 3480,
  "duration_response_processing_ms": 115,
  "error": null
}
```

## Configuration

Observability features are configured via the Kthena Router's deployment or ConfigMap.  
Most settings are controlled through environment variables or the router's configuration file (depending on your deployment method).

### Access Log Configuration

```yaml
accessLogger:
  enabled: true
  format: "json"          # "json" (strongly recommended) or "text"
  output: "stdout"        # "stdout", "stderr", or file path
```

#### Equivalent environment variables (recommended for most deployments)

```yaml
env:
- name: ACCESS_LOG_ENABLED
  value: "true"
- name: ACCESS_LOG_FORMAT
  value: "json"
- name: ACCESS_LOG_OUTPUT
  value: "stdout"
```

#### Metrics Configuration

```yaml
observability:
  metrics:
    enabled: true
    port: 8080           # Default metrics port
    path: /metrics
```

## Debug Endpoints

All available on the same `:15000` port

| Endpoint | Description |
| --- | --- |
| `/debug/config_dump/modelroutes` | All ModelRoute resources |
| `/debug/config_dump/modelservers` | All ModelServer resources |
| `/debug/config_dump/pods` | Current view of healthy/ready inference pods |
| `/debug/config_dump/namespaces/{ns}/modelroutes/{name}` | Detailed single ModelRoute |
| `/debug/config_dump/namespaces/{ns}/modelservers/{name}` | Detailed single ModelServer |

## Quick Start – Observability in Action

```bash
# Forward metrics port (8080)
kubectl port-forward -n kthena-system svc/kthena-router 8080:8080 &

# Forward debug port (15000) when needed
kubectl port-forward -n kthena-system svc/kthena-router 15000:15000 &

# Watch real-time request rate by model
watch -n 2 'curl -s http://localhost:8080/metrics | grep kthena_router_requests_total | sort'

# Tail logs and filter access log entries (JSON lines only)
kubectl logs -n kthena-system deployment/kthena-router -f \
  | grep -E '^{.*}$' | jq .    # Only process valid JSON lines

# Alternative: look for model-related entries in all logs
kubectl logs -n kthena-system deployment/kthena-router -f \
  | grep -E "model_name|request_id|duration_total"
```

**Important note about logs**  

- Router logs usually contain both regular application logs (plain text) and structured access logs (JSON).  
- Piping everything directly to `jq` will cause errors on non-JSON lines.  
- Use `grep` to filter JSON lines first, or use a log processor (like fluentd, vector, or loki) in production.

## Troubleshooting Guide

### Preparation

```bash
# Forward metrics port (8080) for metrics queries
kubectl port-forward -n kthena-system svc/kthena-router 8080:8080 &

# Forward debug port (15000) for debug endpoints
kubectl port-forward -n kthena-system svc/kthena-router 15000:15000 &

# Live structured logs (recommended, with filtering)
kubectl logs -n kthena-system deployment/kthena-router -f \
  | grep -E '^{.*}$' | jq .
```

#### 1. High Error Rate (5xx, timeouts, internal server errors)

Count errors by status & model:

```bash
curl -s http://localhost:8080/metrics | grep 'status_code=5' | sort -nr
```

Top affected models:

```bash
curl -s http://localhost:8080/metrics \
  | grep 'status_code=5' \
  | grep -o 'model="[^"]*"' | sort | uniq -c | sort -nr
```

Inspect recent failed requests:

```bash
kubectl logs -n kthena-system deployment/kthena-router --since=30m \
  | jq 'select(.status_code >= 500) | {ts: .timestamp, model: .model_name, err: .error, pod: .selected_pod, dur: .duration_total_ms}'
```

Check upstream health:

```bash
curl http://localhost:15000/debug/config_dump/modelservers | jq .
curl http://localhost:15000/debug/config_dump/pods | jq .
```

#### 2. High Latency / Slow TTFT or Generation Speed

Latency percentiles (p50/p95/p99):

```bash
curl -s http://localhost:8080/metrics \
  | grep -E 'kthena_router_request_duration_seconds_(bucket|sum|count)'
```

Find slowest requests:

```bash
kubectl logs -n kthena-system deployment/kthena-router --since=20m \
  | jq 'select(.duration_total_ms > 4000) | {model: .model_name, total: .duration_total_ms, upstream: .duration_upstream_ms, pod: .selected_pod}'
```

Check queue pressure:

```bash
watch -n 2 'curl -s http://localhost:8080/metrics | grep -E "(active_downstream|fairness_queue_size)"'
```

#### 3. Queue Buildup / Fairness / Throttling

Live queue monitoring:

```bash
watch -n 3 'curl -s http://localhost:8080/metrics | grep fairness_queue_size'
```

Queue wait time distribution:

```bash
curl -s http://localhost:8080/metrics | grep fairness_queue_duration_seconds
```

Find throttled/rejected requests:

```bash
kubectl logs -n kthena-system deployment/kthena-router --since=1h \
  | jq 'select(.error? | .type? == ("rate_limit","throttled","queue_full"))'
```

#### 4. Wrong Routing / 404 / Pod Selection Issues

Validate full routing table:

```bash
curl http://localhost:15000/debug/config_dump/modelroutes | jq .
```

Check pod readiness:  

```bash
curl http://localhost:15000/debug/config_dump/pods | jq .
```

Trace a specific request:

```bash
# Use request_id from client or error message
kubectl logs -n kthena-system deployment/kthena-router \
  | jq 'select(.request_id == "a1b2c3d4-...")'
```

#### 5. Token Usage / Cost / Abuse Monitoring

Current token consumption rate:

```bash
curl -s http://localhost:8080/metrics | grep kthena_router_tokens_total
```

High-token requests:

```bash
kubectl logs -n kthena-system deployment/kthena-router --since=2h \
  | jq 'select(.input_tokens > 3000 or .output_tokens > 1500)'
```
