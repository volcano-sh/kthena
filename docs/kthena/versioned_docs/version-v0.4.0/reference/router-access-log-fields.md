# Router Access Log Fields Reference

This document provides a comprehensive reference for all fields available in Kthena Router access logs.

## Overview

Kthena Router generates structured access logs for all AI inference requests. These logs provide detailed information about request processing, including timing breakdowns, routing decisions, token usage, and error information.

## Log Format

Access logs are available in two formats:
- **JSON**: Structured JSON format suitable for log aggregation and analysis
- **Text**: Human-readable format for development and debugging

### Text Format Structure

The text format follows this structure:
```
[timestamp] "METHOD /path PROTOCOL" status_code [error=type:message] model_name=name model_route=route model_server=server selected_pod=pod request_id=id tokens=input/output timings=total(req+upstream+resp)ms
```

Key features of the text format:
- **Error placement**: Error information appears immediately after the status code when present
- **Timing format**: Shows total time with breakdown in parentheses: `timings=2350ms(45+2180+5)`
- **Compact representation**: All information on a single line for easy parsing

## Field Reference

### Standard HTTP Fields

These fields follow the Envoy access log format for compatibility with existing log processing tools.

| Field         | Type               | Description                                      | Example                    |
| ------------- | ------------------ | ------------------------------------------------ | -------------------------- |
| `timestamp`   | `string` (RFC3339) | ISO 8601 timestamp when the request was received | `2024-01-15T10:30:45.123Z` |
| `method`      | `string`           | HTTP method used for the request                 | `POST`, `GET`              |
| `path`        | `string`           | Request path including query parameters          | `/v1/chat/completions`     |
| `protocol`    | `string`           | HTTP protocol version                            | `HTTP/1.1`, `HTTP/2`       |
| `status_code` | `integer`          | HTTP response status code                        | `200`, `400`, `500`        |

### Error Information

Error information is included when a request fails and appears immediately after the status code.

| Field           | Type     | Description            | Example                                    |
| --------------- | -------- | ---------------------- | ------------------------------------------ |
| `error.type`    | `string` | Error category or type | `timeout`, `rate_limit`, `model_not_found` |
| `error.message` | `string` | Detailed error message | `Model inference timeout after 30s`        |

#### Common Error Types

| Error Type              | Description                          | Typical Status Code |
| ----------------------- | ------------------------------------ | ------------------- |
| `timeout`               | Request exceeded configured timeout  | `504`               |
| `rate_limit`            | Request was rate limited             | `429`               |
| `model_not_found`       | Requested model is not available     | `404`               |
| `authentication_failed` | Authentication credentials invalid   | `401`               |
| `authorization_failed`  | User lacks required permissions      | `403`               |
| `upstream_error`        | Error from model inference backend   | `502`, `503`        |
| `invalid_request`       | Malformed request body or parameters | `400`               |

### AI-Specific Routing Information

These fields provide information about how the request was routed through the AI router.

| Field          | Type     | Description                               | Example                                |
| -------------- | -------- | ----------------------------------------- | -------------------------------------- |
| `model_name`   | `string` | Name of the AI model requested            | `llama2-7b`, `gpt-3.5-turbo`           |
| `model_route`  | `string` | Name of the ModelRoute resource used      | `default/llama2-route-v1`              |
| `model_server` | `string` | ModelServer that handled the request      | `default/llama2-server`                |
| `selected_pod` | `string` | Specific pod that processed the inference | `llama2-deployment-5f7b8c9d-xk2p4`     |
| `request_id`   | `string` | Unique identifier for request tracing     | `550e8400-e29b-41d4-a716-446655440000` |

### Token Information

Token usage metrics for the inference request.

| Field           | Type      | Description                            | Example |
| --------------- | --------- | -------------------------------------- | ------- |
| `input_tokens`  | `integer` | Number of tokens in the request prompt | `150`   |
| `output_tokens` | `integer` | Number of tokens generated in response | `75`    |

### Timing Breakdown

All timing values are in milliseconds and provide detailed performance metrics.

| Field                          | Type      | Description                                     | Example |
| ------------------------------ | --------- | ----------------------------------------------- | ------- |
| `duration_total`               | `integer` | Total end-to-end request processing time (ms)   | `2350`  |
| `duration_request_processing`  | `integer` | Router request processing overhead (ms)        | `45`    |
| `duration_upstream_processing` | `integer` | Model inference time on backend pod (ms)        | `2180`  |
| `duration_response_processing` | `integer` | Response processing and serialization time (ms) | `5`     |

#### Timing Phases

1. **Request Processing**: Time spent parsing the request, authentication, rate limiting, and routing decisions
2. **Upstream Processing**: Time spent on actual model inference in the backend pod
3. **Response Processing**: Time spent formatting and serializing the response

## Example Access Logs

### Successful Request (JSON Format)

```json
{
  "timestamp": "2024-01-15T10:30:45.123Z",
  "method": "POST",
  "path": "/v1/chat/completions",
  "protocol": "HTTP/1.1",
  "status_code": 200,
  "model_name": "llama2-7b",
  "model_route": "default/llama2-route-v1",
  "model_server": "default/llama2-server",
  "selected_pod": "llama2-deployment-5f7b8c9d-xk2p4",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "input_tokens": 150,
  "output_tokens": 75,
  "duration_total": 2350,
  "duration_request_processing": 45,
  "duration_upstream_processing": 2180,
  "duration_response_processing": 5
}
```

### Failed Request with Error (JSON Format)

```json
{
  "timestamp": "2024-01-15T10:35:22.456Z",
  "method": "POST",
  "path": "/v1/chat/completions",
  "protocol": "HTTP/1.1",
  "status_code": 504,
  "error": {
    "type": "timeout",
    "message": "Model inference timeout after 30s"
  },
  "model_name": "llama2-7b",
  "model_route": "default/llama2-route-v1",
  "model_server": "default/llama2-server",
  "selected_pod": "llama2-deployment-5f7b8c9d-xk2p4",
  "request_id": "660e8400-e29b-41d4-a716-446655440001",
  "input_tokens": 200,
  "output_tokens": 0,
  "duration_total": 30050,
  "duration_request_processing": 50,
  "duration_upstream_processing": 30000,
  "duration_response_processing": 0
}
```

### Text Format Example

```
[2024-01-15T10:30:45.123Z] "POST /v1/chat/completions HTTP/1.1" 200 model_name=llama2-7b model_route=default/llama2-route-v1 model_server=default/llama2-server selected_pod=llama2-deployment-5f7b8c9d-xk2p4 request_id=550e8400-e29b-41d4-a716-446655440000 tokens=150/75 timings=2350ms(45+2180+5)
```

### Text Format with Error

```
[2024-01-15T10:35:22.456Z] "POST /v1/chat/completions HTTP/1.1" 504 error=timeout:Model inference timeout after 30s model_name=llama2-7b model_route=default/llama2-route-v1 model_server=default/llama2-server selected_pod=llama2-deployment-5f7b8c9d-xk2p4 request_id=660e8400-e29b-41d4-a716-446655440001 tokens=200/0 timings=30050ms(50+30000+0)
```

## Configuration

Access logging is configured through environment variables in the kthena router deployment:

### Environment Variables

| Variable             | Description                      | Default  | Valid Values                     |
| -------------------- | -------------------------------- | -------- | -------------------------------- |
| `ACCESS_LOG_ENABLED` | Enable or disable access logging | `true`   | `true`, `false`                  |
| `ACCESS_LOG_FORMAT`  | Log output format                | `text`   | `json`, `text`                   |
| `ACCESS_LOG_OUTPUT`  | Where to write logs              | `stdout` | `stdout`, `stderr`, or file path |
