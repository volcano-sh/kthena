# Kthena Networking Chart

This chart deploys the Kthena networking components, including the kthena-router and webhook.

## Configuration

### Kthena Router

The kthena-router is the main component that handles serving requests and provides fairness scheduling.

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

#### Fairness Scheduling Configuration

The fairness scheduling system ensures equitable resource allocation among users based on their token usage history.

```yaml
kthenaRouter:
  fairness:
    # Enable fairness scheduling (default: false)
    enabled: true
    
    # Sliding window duration for token tracking (default: "1h")
    # Valid formats: 1m, 5m, 10m, 30m, 1h
    windowSize: "10m"
    
    # Token weights for priority calculation
    # Input token weight (default: 1.0)
    inputTokenWeight: 1.0
    
    # Output token weight (default: 2.0)
    # Typically higher than input weight due to generation cost
    outputTokenWeight: 2.0
```

#### Configuration Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `kthenaRouter.fairness.enabled` | boolean | `false` | Enable fairness scheduling |
| `kthenaRouter.fairness.windowSize` | string | `"5m"` | Sliding window duration (1m-1h) |
| `kthenaRouter.fairness.inputTokenWeight` | float | `1.0` | Weight for input tokens (≥0) |
| `kthenaRouter.fairness.outputTokenWeight` | float | `2.0` | Weight for output tokens (≥0) |

#### Backend Metric Port Configuration

Use these values to configure model backend metric ports in `routerConfiguration`:

```yaml
kthenaRouter:
  backend:
    sglang:
      metricPort: 30000
    vllm:
      metricPort: 8000
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `kthenaRouter.backend.sglang.metricPort` | int | `30000` | Metric port used for SGLang backends |
| `kthenaRouter.backend.vllm.metricPort` | int | `8000` | Metric port used for vLLM backends |

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

## Installation

### Basic Installation
```bash
helm install kthena ./charts/kthena
```

### With Fairness Scheduling
```bash
helm install kthena ./charts/kthena \
  --set networking.kthenaRouter.fairness.enabled=true \
  --set networking.kthenaRouter.fairness.windowSize=10m \
  --set networking.kthenaRouter.fairness.outputTokenWeight=3.0
```
