# Kthena Networking Chart

This chart deploys the Kthena networking components, including the kthena-router and webhook.

## Configuration

### Kthena Router

The kthena-router is the main component that handles serving requests and provides priority-queue request scheduling.

#### Basic Configuration

```yaml
kthenaRouter:
  enabled: true
  replicas: 1
  image:
    repository: ghcr.io/volcano-sh/kthena-router
    tag: latest
    pullPolicy: IfNotPresent
```

#### Request Scheduling Configuration

The router schedules requests through a per-model queue using one of two mutually
exclusive strategies:

- **User Fairness**: orders requests by each user's recent token usage so
  that no single user dominates a model under contention.
- **Session Boost**: promotes follow-up requests from recently-completed conversation
  sessions to maximize prefix cache hits for multi-turn workloads.

The two strategies are mutually exclusive. Enable user fairness with
`fairness.enabled: true`, or session boost with `sessionBoost.enabled: true`, but
not both. Enabling both is a configuration error.

```yaml
kthenaRouter:
  fairness:
    # Enable user-fairness scheduling
    enabled: true

    # Sliding window duration for token tracking (default: "1h")
    # Valid formats: 1m, 5m, 10m, 30m, 1h
    windowSize: "10m"
    # Token weights for priority calculation
    inputTokenWeight: 1.0
    outputTokenWeight: 2.0
    # Global total inflight limit admitted through the fairness gate (0 = QPS mode)
    maxConcurrent: 0
```

#### Configuration Parameters

| Parameter                                  | Type    | Default          | Description                                                        |
| ------------------------------------------ | ------- | ---------------- | ------------------------------------------------------------------ |
| `kthenaRouter.fairness.enabled`            | boolean | `false`          | Enable user-fairness scheduling (mutually exclusive with boost)    |
| `kthenaRouter.fairness.windowSize`         | string  | `"1h"`           | Fairness: sliding window duration (1m-1h)                          |
| `kthenaRouter.fairness.inputTokenWeight`   | float   | `1.0`            | Fairness: weight for input tokens (≥0)                             |
| `kthenaRouter.fairness.outputTokenWeight`  | float   | `2.0`            | Fairness: weight for output tokens (≥0)                            |
| `kthenaRouter.fairness.maxConcurrent`      | int     | `0`              | Fairness: global inflight limit (`0` falls back to QPS mode)       |
| `kthenaRouter.sessionBoost.enabled`        | boolean | `false`          | Enable session-boost scheduling (mutually exclusive with fairness) |
| `kthenaRouter.sessionBoost.header`         | string  | `"X-Session-ID"` | HTTP header used to identify conversation sessions                 |
| `kthenaRouter.sessionBoost.maxSessions`    | int     | `4096`           | Max recently-completed sessions kept warm (LRU-evicted)            |
| `kthenaRouter.sessionBoost.inflightPerPod` | int     | `16`             | Inflight requests per backend pod; total = perPod x pod count      |
| `kthenaRouter.sessionBoost.gracePeriod`    | string  | `"0s"`           | Wait time for a same-session follow-up (disabled by default)       |

#### Session Boost Configuration

Session boost optimizes multi-turn conversation latency by prioritizing follow-up
requests from the same session (maximizing prefix cache hits). It is mutually exclusive
with user fairness.

```yaml
kthenaRouter:
  sessionBoost:
    enabled: true
    header: "X-Session-ID"
    maxSessions: 4096         # LRU cache of recently-completed sessions kept warm
    inflightPerPod: 16        # total inflight = perPod x backend pod count
```

#### Configuration Scenarios

##### Development Environment
```yaml
kthenaRouter:
  fairness:
    enabled: true
    windowSize: "2m"          # Short window for quick feedback
    inputTokenWeight: 1.0     # Equal weights for simplicity
    outputTokenWeight: 1.0
```

##### Production Environment
```yaml
kthenaRouter:
  fairness:
    enabled: true
    windowSize: "10m"         # Balanced window size
    inputTokenWeight: 1.0     # Realistic cost ratios
    outputTokenWeight: 2.5
```

##### Cost-Sensitive Environment
```yaml
kthenaRouter:
  fairness:
    enabled: true
    windowSize: "30m"         # Longer window for stability
    inputTokenWeight: 1.0     # High output weight for cost control
    outputTokenWeight: 4.0
```

### TLS Configuration

```yaml
kthenaRouter:
  tls:
    enabled: true
    dnsName: "your-domain.com"
    secretName: "kthena-router-tls"
```

### Resource Configuration

```yaml
kthenaRouter:
  resource:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### Drain Timeout

```yaml
kthenaRouter:
  terminationGracePeriodSeconds: 330
  drainTimeout: 5m
```

| Parameter                                    | Type   | Default | Description                                                             |
| -------------------------------------------- | ------ | ------- | ----------------------------------------------------------------------- |
| `kthenaRouter.terminationGracePeriodSeconds` | int    | `330`   | Pod termination grace period for the router                             |
| `kthenaRouter.drainTimeout`                  | string | `"5m"`  | Time allowed for the router to drain in-flight requests before shutdown |

## Installation

### Basic Installation
```bash
helm install kthena ./charts/kthena
```

### With User-Fairness Scheduling
```bash
helm install kthena ./charts/kthena \
  --set networking.kthenaRouter.fairness.enabled=true \
  --set networking.kthenaRouter.fairness.windowSize=10m \
  --set networking.kthenaRouter.fairness.outputTokenWeight=3.0
```
