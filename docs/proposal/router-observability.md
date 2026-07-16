# Proposal: Router Observability

## Goals

- **Metrics definition**: Define all the metrics about the running status of the router, LLM requests processing, and performance optimization.
- **Access log format**: Define the format of the access log to intuitively display the key results of the request processing.
- **Debug interface**: Define the debug interface to observe the router's internal status.

## 1. Introduction

The kthena-router serves as a critical component in the AI inference system, managing model routing, request scheduling, and resource allocation. Effective observability is essential for:

1. **Performance Monitoring**: Track request latencies, token processing rates, and resource utilization to ensure optimal system performance.

2. **Troubleshooting**: Quickly identify and diagnose issues through detailed metrics, logs, and debug information.

3. **Capacity Planning**: Analyze usage patterns and resource consumption to make informed scaling decisions.

4. **Cost Optimization**: Monitor token usage and model utilization to optimize resource allocation and costs.

This proposal outlines a comprehensive observability framework that combines:

- **Prometheus Metrics**: Detailed metrics covering HTTP requests, token processing, rate limiting, and errors
- **Structured Access Logs**: Request lifecycle logging with timing breakdowns and routing decisions
- **Debug Endpoints**: Internal state exposure for troubleshooting and diagnostics

The framework is designed to integrate with standard observability tools while providing AI-specific insights needed for managing inference workloads at scale.


## 2. Technical Implementation

### 2.1 Metrics definition

The router exposes the following metrics to monitor request processing:

#### Design Principles

**Essential Labels**: Include key dimensions for effective monitoring and debugging:
- `method`: HTTP method (GET, POST) - useful for differentiating request types
- `path`: API path (/v1/chat/completions, /v1/completions) - track different API usage
- `status_code`: HTTP response status code (200, 400, 500) - monitor success/failure patterns
- `model`: AI model name - essential for AI workload monitoring
- `error_type`: Specific error categories for detailed troubleshooting
- `model_route`: Namespaced ModelRoute name, or `none`
- `backend_type`: Bounded destination kind (`model_server`, `external_provider`, `inference_pool`, `unresolved`, or `none`)
- `backend_name`: Namespaced backend resource name, or `none`
- `upstream_model`: Configured upstream model, or `none`

**Label Cardinality Management**: Keep label values bounded to avoid high cardinality issues:
- Limited set of endpoints and methods
- Standard HTTP status codes
- Controlled model catalog
- Predefined error types

#### Request Processing Metrics

**HTTP Request Metrics**
- `kthena_router_requests_total{model="<model_name>",path="<path>",status_code="<code>",error_type="<error_type>",model_route="<route_name>",backend_type="<type>",backend_name="<backend_name>",upstream_model="<upstream_model>"}` (Counter)
  - Total number of HTTP requests processed by the router
  - Labels: 
    - `model`: AI model name
    - `path`: Request path (/v1/chat/completions, /v1/completions, etc.)
    - `status_code`: HTTP response status code (200, 400, 500, etc.)
    - `error_type`: Type of error (validation, timeout, internal, rate_limit, etc.)
    - `model_route`, `backend_type`, `backend_name`, `upstream_model`: Bounded destination identity

- `kthena_router_request_duration_seconds{model="<model_name>",path="<path>",status_code="<code>",model_route="<route_name>",backend_type="<type>",backend_name="<backend_name>",upstream_model="<upstream_model>"}` (Histogram)
  - End-to-end request processing latency distribution for all requests
  - Labels:
    - `model`: AI model name
    - `path`: Request path (/v1/chat/completions, /v1/completions, etc.)
    - `status_code`: HTTP response status code (200, 400, 500, etc.)
    - `model_route`, `backend_type`, `backend_name`, `upstream_model`: Bounded destination identity
  - Buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60]

- `kthena_router_request_prefill_duration_seconds{model="<model_name>",path="<path>",status_code="<code>"}` (Histogram)
  - Prefill phase processing latency distribution for PD-disaggregated requests
  - Labels:
    - `model`: AI model name
    - `path`: Request path (/v1/chat/completions, /v1/completions, etc.)
    - `status_code`: HTTP response status code (200, 400, 500, etc.)
  - Buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60]

- `kthena_router_request_decode_duration_seconds{model="<model_name>",path="<path>",status_code="<code>"}` (Histogram)
  - Decode phase processing latency distribution for PD-disaggregated requests
  - Labels:
    - `model`: AI model name
    - `path`: Request path (/v1/chat/completions, /v1/completions, etc.)
    - `status_code`: HTTP response status code (200, 400, 500, etc.)
  - Buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60]

- `kthena_router_active_downstream_requests{model="<model_name>"}` (Gauge)
  - Current number of active downstream requests (from clients to router)
  - Labels:
    - `model`: AI model name

- `kthena_router_active_upstream_requests{model_server="<server_name>",model_route="<route_name>",backend_type="<type>",backend_name="<backend_name>",upstream_model="<upstream_model>"}` (Gauge)
  - Current number of active upstream requests (from router to backend pods)
  - Labels:
    - `model_route`: ModelRoute name handling the requests
    - `model_server`: ModelServer name processing the requests
    - `backend_type`, `backend_name`, `upstream_model`: Destination-neutral backend identity; `model_server` is retained for query compatibility

**AI-Specific Token Metrics**
- `kthena_router_tokens_total{model="<model_name>",path="<path>",token_type="input|output",model_route="<route_name>",backend_type="<type>",backend_name="<backend_name>",upstream_model="<upstream_model>"}` (Counter)
  - Total tokens processed/generated
  - Labels:
    - `model`: AI model name
    - `path`: Request path (/v1/chat/completions, /v1/completions, etc.)
    - `token_type`: Token type ("input" for processed tokens, "output" for generated tokens)
    - `model_route`, `backend_type`, `backend_name`, `upstream_model`: Bounded destination identity

**Scheduler Plugin Metrics**
- `kthena_router_scheduler_plugin_duration_seconds{model="<model_name>",plugin="<plugin_name>",type="filter|score"}` (Histogram)
  - Processing time per scheduler plugin
  - Labels:
    - `model`: AI model name
    - `plugin`: Plugin name
    - `type`: Plugin type ("filter" for filtering plugins, "score" for scoring plugins)
  - Buckets: [0.001, 0.005, 0.01, 0.05, 0.1, 0.5]

**Rate Limiting Metrics**  
- `kthena_router_rate_limit_exceeded_total{model="<model_name>",limit_type="input_tokens|output_tokens|requests",path="<path>"}` (Counter)
  - Number of requests rejected due to rate limiting
  - Labels:
    - `model`: AI model name
    - `limit_type`: Type of rate limit (input_tokens, output_tokens, requests)
    - `path`: Request path (/v1/chat/completions, /v1/completions, etc.)

**Fairness Queue Metrics**
- `kthena_router_fairness_queue_size{model="<model_name>",user_id="<user_id>"}` (Gauge)
  - Current fairness queue size for pending requests
  - Labels:
    - `model`: AI model name
    - `user_id`: User identifier for the fairness scheduling

- `kthena_router_fairness_queue_duration_seconds{model="<model_name>",user_id="<user_id>"}` (Histogram)
  - Time requests spend in fairness queue before processing
  - Labels:
    - `model`: AI model name
    - `user_id`: User identifier for the fairness scheduling
  - Buckets: [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5]

All metrics are exposed at the `/metrics` endpoint in Prometheus format. The metrics provide comprehensive visibility into:

**Key Observability Dimensions**
- **Request Processing**: HTTP request patterns, latency, and success rates
- **AI Workload Specific**: Token consumption, model performance, and inference costs  
- **Resource Utilization**: Connections, queuing, and rate limiting
- **Error Tracking**: Detailed error categorization and failure patterns


### 2.2 Access log format

The router generates structured access logs for each request, following Envoy's access log format with AI-specific extensions to track model routing and processing stages.

#### Log Format

**Default Format (JSON)**
```json
{
  "timestamp": "2024-01-15T10:30:45.123Z",
  "method": "POST",
  "path": "/v1/chat/completions",
  "protocol": "HTTP/1.1",
  "status_code": 200,
  
  // AI-specific routing information
  "model_name": "llama2-7b",
  "model_route": "default/llama2-route-v1",
  "model_server": "default/llama2-server",
  "selected_pod": "llama2-deployment-5f7b8c9d-xk2p4",
  "backend_type": "model_server",
  "backend_name": "default/llama2-server",
  "upstream_model": "llama2-7b",
  "upstream_status_code": 200,
  "upstream_attempts": 1,
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  
  // Token information
  "input_tokens": 150,
  "output_tokens": 75,
  
  // Timing breakdown (in milliseconds) - flattened structure
  "duration_total": 2350,
  "duration_request_processing": 45,
  "duration_upstream_processing": 2180,
  "duration_response_processing": 5,
  
  // Error information (if applicable)
  "error": {
    "type": "timeout",
    "message": "Model inference timeout after 30s"
  }
}
```

#### Text Format (Alternative)
For environments preferring text logs, a structured text format is also supported:
```
[2024-01-15T10:30:45.123Z] "POST /v1/chat/completions HTTP/1.1" 200 model_name=llama2-7b model_route=default/llama2-route-v1 model_server=default/llama2-server selected_pod=llama2-deployment-5f7b8c9d-xk2p4 backend_type=model_server backend_name=default/llama2-server upstream_model=llama2-7b upstream_status_code=200 upstream_attempts=1 request_id=550e8400-e29b-41d4-a716-446655440000 tokens=150/75 timings=2350ms(45+2180+5)
```

**Text Format Features:**
- **Error placement**: Error information (if present) appears immediately after status code: `error=type:message`
- **Timing format**: Shows total time with detailed breakdown in parentheses: `timings=total(req+upstream+resp)ms`
- **Compact representation**: All information on a single line for easy parsing and grep operations
- **Key-value pairs**: All fields after the basic HTTP line are in `key=value` format

#### Key Fields Explanation

**Standard HTTP Fields** (Following Envoy format):
- `timestamp`: Request start time in ISO 8601 format with nanosecond precision
- `method`, `path`, `protocol`: Standard HTTP request information
- `status_code`: HTTP response status code

**AI-Specific Routing Fields**:
- `model_name`: The AI model requested in the request body
- `model_route`: Which ModelRoute CR was matched for this request (namespace/name format)
- `model_server`: Which ModelServer CR was selected for routing (namespace/name format)
- `selected_pod`: The specific pod that processed the inference request
- `backend_type`: Destination kind shared by ModelServer, ExternalModelProvider, and InferencePool paths
- `backend_name`: Namespaced backend resource name
- `upstream_model`: Model identifier sent to the selected backend
- `upstream_status_code`: HTTP status returned by the accepted upstream attempt
- `upstream_attempts`: Number of upstream attempts made for the request
- `error_origin`: Bounded failure origin (`router` or `upstream`), omitted for successful requests
- `request_id`: Unique request identifier (generated if not provided in x-request-id header)

**Token Tracking**:
- `input_tokens`: Number of tokens in the input prompt (omitted if 0)
- `output_tokens`: Number of tokens in the response (omitted if 0)

**Detailed Timing Breakdown** (all times in milliseconds, flattened structure):
- `duration_total`: End-to-end request processing time
- `duration_request_processing`: Time spent in router request processing (parsing, routing, etc.)
- `duration_upstream_processing`: Actual model inference time on the backend pod
- `duration_response_processing`: Time spent processing and formatting the response

**Error Information**:
- `error`: Detailed error information for failed requests (type and message, omitted if no error)

#### Configuration

Access logging can be configured through the AccessLoggerConfig:

```go
type AccessLoggerConfig struct {
    Format  LogFormat `json:"format" yaml:"format"`   // "json" or "text"
    Output  string    `json:"output" yaml:"output"`   // "stdout", "stderr", or file path
    Enabled bool      `json:"enabled" yaml:"enabled"` // Enable/disable logging
}
```

**Default Configuration:**
- Format: JSON
- Output: stdout  
- Enabled: true

Logs are written to stdout by default and can be configured to write to files or external log collectors. When logging is disabled, a no-op logger is used to avoid performance overhead.

### 2.3 Debug interface

The router exposes a debug interface at `/debug/config_dump` to help operators inspect internal state and troubleshoot issues. This interface provides access to the router's datastore information, allowing examination of ModelRoutes, ModelServers, and Pod details.

#### Debug Endpoints

The following debug endpoints are available:

**List Resources**
- `/debug/config_dump/modelroutes` - List all ModelRoute configurations
- `/debug/config_dump/modelservers` - List all ModelServer configurations 
- `/debug/config_dump/pods` - List all Pod information

**Get Specific Resource**
- `/debug/config_dump/namespaces/{namespace}/modelroutes/{name}` - Get details of a specific ModelRoute
- `/debug/config_dump/namespaces/{namespace}/modelservers/{name}` - Get details of a specific ModelServer
- `/debug/config_dump/namespaces/{namespace}/pods/{name}` - Get details of a specific Pod

#### Example Responses

**GET /debug/config_dump/modelroutes**
```json
{
  "modelroutes": [
    {
      "name": "llama2-route",
      "namespace": "default",
      "spec": {
        "modelName": "llama2-7b",
        "loraAdapters": ["lora-adapter-1", "lora-adapter-2"],
        "rules": [
          {
            "name": "default-rule",
            "modelMatch": {
              "body": {
                "model": "llama2-7b"
              }
            },
            "targetModels": [
              {
                "modelServer": {
                  "name": "llama2-server",
                  "namespace": "default"
                },
                "weight": 100
              }
            ]
          }
        ],
        "rateLimit": {
          "local": {
            "inputTokensPerSecond": 1000,
            "outputTokensPerSecond": 500
          }
        }
      }
    }
  ]
}
```

**GET /debug/config_dump/namespaces/default/modelroutes/llama2-route**
```json
{
  "name": "llama2-route",
  "namespace": "default",
  "spec": {
    "modelName": "llama2-7b",
    "loraAdapters": ["lora-adapter-1", "lora-adapter-2"],
    "rules": [
      {
        "name": "default-rule",
        "modelMatch": {
          "headers": {
            "authorization": {
              "prefix": "Bearer "
            }
          },
          "uri": {
            "prefix": "/v1/chat/completions"
          },
          "body": {
            "model": "llama2-7b"
          }
        },
        "targetModels": [
          {
            "modelServer": {
              "name": "llama2-server",
              "namespace": "default"
            },
            "weight": 100
          }
        ]
      }
    ],
    "rateLimit": {
      "local": {
        "inputTokensPerSecond": 1000,
        "outputTokensPerSecond": 500
      }
    }
  }
}
```

**GET /debug/config_dump/modelservers**
```json
{
  "modelservers": [
    {
      "name": "llama2-server",
      "namespace": "default",
      "spec": {
        "model": "llama2-7b-chat",
        "inferenceEngine": "vLLM",
        "workloadSelector": {
          "matchLabels": {
            "app": "llama2",
            "version": "v1"
          }
        },
        "workloadPort": {
          "port": 8000,
          "protocol": "http"
        },
        "trafficPolicy": {
          "loadBalancer": {
            "simple": "LEAST_REQUEST"
          }
        }
      }
    }
  ]
}
```

**GET /debug/config_dump/namespaces/default/modelservers/llama2-server**
```json
{
  "name": "llama2-server",
  "namespace": "default",
  "spec": {
    "model": "llama2-7b-chat",
    "inferenceEngine": "vLLM",
    "workloadSelector": {
      "matchLabels": {
        "app": "llama2",
        "version": "v1"
      },
      "pdGroup": {
        "groupKey": "group-id",
        "prefillLabels": {
          "role": "prefill"
        },
        "decodeLabels": {
          "role": "decode"
        }
      }
    },
    "workloadPort": {
      "port": 8000,
      "protocol": "http"
    },
    "trafficPolicy": {
      "loadBalancer": {
        "simple": "LEAST_REQUEST"
      }
    },
    "kvConnector": {
      "type": "redis",
      "redis": {
        "address": "redis.default.svc.cluster.local:6379",
        "db": 0
      }
    }
  },
  "associatedPods": [
    "default/llama2-deployment-5f7b8c9d-xk2p4",
    "default/llama2-deployment-5f7b8c9d-mn8q7"
  ]
}
```

**GET /debug/config_dump/pods**
```json
{
  "pods": [
    {
      "name": "llama2-deployment-5f7b8c9d-xk2p4",
      "namespace": "default",
      "podIP": "10.244.2.20",
      "nodeName": "worker-node-1",
      "phase": "Running",
      "engine": "vLLM",
      "metrics": {
        "gpuCacheUsage": 0.75,
        "requestWaitingNum": 3,
        "requestRunningNum": 2,
        "tpot": 0.045,
        "ttft": 1.2
      },
      "models": ["llama2-7b", "lora-adapter-1", "lora-adapter-2"],
      "modelServers": ["default/llama2-server"]
    }
  ]
}
```

**GET /debug/config_dump/namespaces/default/pods/llama2-deployment-5f7b8c9d-xk2p4**
```json
{
  "name": "llama2-deployment-5f7b8c9d-xk2p4",
  "namespace": "default",
  "podInfo": {
    "podIP": "10.244.2.20",
    "nodeName": "worker-node-1",
    "phase": "Running",
    "startTime": "2024-01-15T10:00:00Z",
    "labels": {
      "app": "llama2",
      "version": "v1",
      "role": "inference"
    }
  },
  "engine": "vLLM",
  "metrics": {
    "gpuCacheUsage": 0.75,
    "requestWaitingNum": 3,
    "requestRunningNum": 2,
    "tpot": 0.045,
    "ttft": 1.2
  },
  "models": ["llama2-7b", "lora-adapter-1", "lora-adapter-2"],
  "modelServers": ["default/llama2-server"]
}
```

## 3. Conclusion

This proposal defines a comprehensive observability framework for the kthena-router that provides:

1. **Metrics**: Prometheus-compatible metrics covering HTTP requests, AI-specific token processing, rate limiting, and error tracking with carefully chosen labels to avoid high cardinality issues.

2. **Access Logs**: Structured logging format that captures the complete AI inference request lifecycle, including model routing decisions, pod selection, and detailed timing breakdowns for performance analysis.

3. **Debug Interface**: RESTful debug endpoints that expose internal datastore state for troubleshooting, including ModelRoute configurations, ModelServer details, and Pod metrics.

The framework enables operators to:
- Monitor system performance and health in real-time
- Troubleshoot routing and scheduling issues effectively  
- Analyze token consumption patterns for cost optimization
- Track rate limiting effectiveness and resource utilization
- Correlate metrics, logs, and debug information for comprehensive observability

All components are designed to integrate seamlessly with standard observability tools (Prometheus, Grafana, log collectors) while providing AI-specific insights essential for managing inference workloads.
