---
slug: router-blog-post
title: A Deep Dive into the Kthena Router
authors: [YaoZengzeng]
tags: []
---

# A Deep Dive into the Kthena Router

## 1. Introduction

As Large Language Models (LLMs) become increasingly central to modern applications, the infrastructure supporting them must evolve to meet demanding performance, scalability, and cost requirements. Deploying LLMs in production presents unique challenges: models are resource-intensive, inference workloads vary significantly, and users expect low latency with high throughput. Traditional load balancers and API gateways, while excellent for conventional web services, lack the awareness needed to intelligently route AI inference traffic.

**Kthena Router** addresses these challenges head-on. It is a Kubernetes-native, standalone inference router purpose-built for LLM serving workloads. Unlike generic proxies or load balancers, Kthena Router is model-aware, making intelligent routing decisions based on real-time metrics from inference engines. This enables sophisticated traffic management strategies that significantly improve throughput, reduce latency, and lower operational costs.

The router seamlessly integrates with existing API gateway infrastructure while providing advanced capabilities specifically designed for AI workloads:

- **Model-Aware Routing**: Leverages real-time metrics from inference engines (vLLM, SGLang, TGI) to make intelligent routing decisions
- **LoRA-Aware Load Balancing**: Intelligently route to pods that have already loaded the desired LoRA adapter to reduce adapter swap latency from hundreds of milliseconds to near-zero
- **Advanced Scheduling Algorithms**: Includes Prefix Cache Aware, KV Cache Aware and Fairness Scheduling, etc.
- **Prefill-Decode Disaggregation**: Native support for xPyD (x-prefill/y-decode) deployment patterns

Kthena Router is deployed as a standalone binary with minimal dependencies, ensuring lightweight operation and straightforward deployment. It continuously monitors inference engine metrics to obtain real-time information about model status, including currently loaded LoRA adapters, KV cache utilization, request queue lengths, and latency metrics (TTFT/TPOT). This real-time awareness enables the router to make optimal routing decisions that traditional load balancers simply cannot achieve.

<!-- truncate -->

## 2. Architecture

Kthena Router implements a clean, modular architecture designed for performance and extensibility. The system consists of several core components working together to provide intelligent request routing.

![Kthena Router Architecture](../../docs/assets/diagrams/kthena-router-arch.svg)

### 2.1 Core Components Overview

**Router**: The core execution framework responsible for receiving, processing, and forwarding requests. It orchestrates the interaction between all other components and maintains the request lifecycle from initial reception to final response.

**Listener**: Manages HTTP/HTTPS listeners and handles incoming traffic on specified ports. It provides flexible configuration for different protocols and can bind to multiple addresses to serve various types of requests. The listener ensures efficient connection handling and supports both streaming and non-streaming request patterns.

**Controller**: A Kubernetes-native component that synchronizes and processes Pods and Custom Resources (CRs) such as `ModelRoute` and `ModelServer`. The controller watches for changes in the cluster and updates the router's internal state accordingly, ensuring routing decisions are always based on current cluster topology.

**Filters**: Contains two critical sub-modules that process requests before they reach the backend:
- **Auth**: Handles traffic authentication and authorization, supporting API Key, JWT
- **RateLimit**: Manages comprehensive rate limiting strategies including input tokens and output tokens limiting

**Backend**: Provides an abstraction layer for accessing various inference engines. It masks differences in metrics interface access methods and metric naming conventions across frameworks like vLLM, SGLang, and TGI, presenting a unified interface to the scheduler.

**Metrics Fetcher**: Continuously collects real-time metrics from inference engine endpoints running on model pods. It gathers critical performance data including:
- KV cache utilization
- Current LoRA model status
- Request queue lengths
- Latency metrics (TTFT - Time To First Token, TPOT - Time Per Output Token)

**Datastore**: A unified data storage layer that provides efficient access to ModelServer-to-Pod associations, Base Model/LoRA configurations, and runtime metrics. It serves as the central repository for all routing-relevant information and supports callbacks for real-time updates.

**Scheduler**: The brain of the router, implementing sophisticated traffic scheduling algorithms. It consists of a scheduling framework and various pluggable scheduling algorithm plugins. The framework integrates and runs different scheduling plugins to filter and score pod collections corresponding to `ModelServers`, selecting the globally optimal pod as the final access target.

![Kthena Router Components](../../docs/assets/diagrams/kthena-router-components.svg)

## 3. Router API

Kthena Router's routing behavior is controlled by two key Custom Resource Definitions (CRDs): **ModelServer** and **ModelRoute**. These declarative APIs allow you to define sophisticated routing strategies using familiar Kubernetes patterns.

### 3.1 ModelRoute

**ModelRoute** defines traffic routing rules based on request characteristics. It determines which ModelServer(s) should handle requests based on model names, LoRA adapters, HTTP headers, and other criteria.

Key fields include:
- **ModelName**: The model name to match in incoming requests
- **LoRAAdapters**: List of LoRA adapter names this route supports
- **Rules**: An ordered list of routing rules, each containing:
  - **ModelMatch**: Conditions for matching requests (headers, URIs, etc.)
  - **TargetModels**: List of ModelServers to route to, with optional weights
- **RateLimit**: Token-based rate limiting configuration

More details about `ModelRoute` can be found in the [definition](https://github.com/volcano-sh/kthena/blob/main/charts/kthena/charts/networking/crds/networking.serving.volcano.sh_modelroutes.yaml).


### 3.2 ModelServer

**ModelServer** defines an inference service instance and its access policies. It identifies the pods running your models, specifies the inference framework being used, and defines how traffic should be handled.

Key fields include:
- **WorkloadSelector**: Identifies pods via labels and supports PD (Prefill-Decode) group specifications
- **Model**: Specifies the base model name that the server hosts
- **InferenceFramework**: Indicates the inference engine (vLLM, SGLang, TGI, etc.)
- **WorkloadPort**: Defines the port on which the inference service listens
- **TrafficPolicy**: Configures timeout, retry policies, and other traffic handling behaviors
- **KVConnector**: Specifies the KV connector type for PD disaggregated deployments (HTTP, Nixl, LMCache, Mooncake)

More details about `ModelServer` can be found in the [definition](https://github.com/volcano-sh/kthena/blob/main/charts/kthena/charts/networking/crds/networking.serving.volcano.sh_modelservers.yaml).

### 3.3 Example: Header-Based Multi-Model Routing

For tiered service offerings, route users to different model sizes based on headers:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-multi-models
  namespace: default
spec:
  modelName: "deepseek-multi-models"
  rules:
  - name: "premium"
    modelMatch:
      headers:
        user-type:
          exact: premium
    targetModels:
    - modelServerName: "deepseek-r1-7b"
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: deepseek-r1-7b
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      app: deepseek-r1-7b
  workloadPort:
    port: 8000
  model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B"
  inferenceEngine: "vLLM"
  trafficPolicy:
    timeout: 10s
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: deepseek-r1-1-5b
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      app: deepseek-r1-1-5b
  workloadPort:
    port: 8000
  model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B"
  inferenceEngine: "vLLM"
  trafficPolicy:
    timeout: 10s
```

**Traffic Flow**:
1. Request arrives with model name "deepseek-multi-models"
2. Router checks if `user-type: premium` header exists
3. Premium users → routed to larger 7B model for better quality
4. Regular users → routed to smaller 1.5B model for cost efficiency

Testing premium routing:
```bash
curl http://$ROUTER_IP/v1/completions \
    -H "Content-Type: application/json" \
    -H "user-type: premium" \
    -d '{"model": "deepseek-multi-models", "prompt": "Explain quantum computing"}'
```

This example demonstrates how `ModelRoute` and `ModelServer` CRDs provide flexible, declarative control over sophisticated routing strategies, all managed through standard Kubernetes APIs.

## 4. Core Features

### 4.1 Intelligent Scheduling Plugins

What truly differentiates Kthena Router from traditional load balancers is its suite of model-aware scheduling plugins. These plugins leverage real-time inference engine metrics to make intelligent routing decisions that dramatically improve performance.

#### 4.1.1 Prefix Cache Aware Scheduling

Modern inference engines like vLLM and SGLang implement prefix caching, where commonly used prompt prefixes are cached to avoid redundant computation. The Prefix Cache Aware plugin maximizes cache hit rates by routing requests with similar prefixes to the same pods.

**How it works**:
- Extracts prompt prefixes from incoming requests
- Maintains a mapping of prefixes to pods that have processed them
- Routes new requests with matching prefixes to pods likely to have cached KV states
- Significantly reduces Time To First Token (TTFT) for repeated or similar prompts

#### 4.1.2 KV Cache Aware Scheduling

The KV Cache Aware plugin (`kvcache-aware`) routes requests to pods that are most likely to have matching KV cache entries, using token-block based matching with Redis-based distributed coordination. This maximizes cache hits and reduces redundant prefill computation.

**How it works**:
- The Kthena Runtime sidecar subscribes to vLLM ZMQ kv-events and writes token block hashes into Redis
- The router tokenizes incoming prompts, divides them into fixed-size blocks, and hashes each block
- Redis is queried to find which pods have cached each token block
- Pods are scored based on consecutive block matches from the beginning of the prompt
- Requires Redis and the Kthena Runtime sidecar deployed alongside vLLM pods

#### 4.1.3 LoRA Affinity Scheduling

LoRA (Low-Rank Adaptation) adapters enable fine-tuned model behavior without redeploying base models. However, loading and unloading adapters introduces latency. The LoRA Affinity plugin minimizes this overhead.

**How it works**:
- Tracks which LoRA adapters are currently loaded on each pod
- Routes requests requiring a specific LoRA to pods that already have it loaded
- Falls back to pods with available adapter slots if no pods have the adapter cached
- Reduces adapter swap latency from hundreds of milliseconds to near-zero


#### 4.1.4 Least Latency Scheduling

The Least Latency plugin routes requests to the fastest available pods based on real-time latency metrics.

**Metrics considered**:
- **TTFT (Time To First Token)**: Important for streaming responses and user-perceived latency
- **TPOT (Time Per Output Token)**: Critical for overall generation speed

#### 4.1.5 Least Request Scheduling

The Least Request plugin considers the number of requests waiting to be processed and the number of requests running to route to the least busy pods.

**How it works**:
- Monitors the `num_requests_running` and `num_requests_waiting` metrics from inference engines
- Calculates total pending work for each pod
- Routes new requests to the least busy pod
- Prevents hot-spotting and ensures even load distribution

#### 4.1.6 Plugin Configurations

These plugins work together through the scheduler framework. You can configure which plugins are enabled and their relative weights through the router configuration.

The scheduler framework runs enabled plugins in sequence:
1. **Filtering**: Plugins eliminate unsuitable pods (e.g., insufficient cache, wrong LoRA)
2. **Scoring**: Plugins score remaining pods based on their criteria
3. **Selection**: The highest-scoring pod(s) are selected for the request

This composable architecture allows you to tailor routing behavior to your specific workload requirements.

### 4.2 Fairness Scheduling

The Fairness Scheduling ensures equitable resource allocation across users based on token consumption history.

**How it works**:
- Tracks cumulative token usage (input + output) per user per model
- Assigns request priority inversely proportional to historical usage
- Queues requests and processes them in priority order
- Prevents any single user from monopolizing resources

**Use cases**:
- Multi-tenant platforms with shared infrastructure
- Research clusters with fair-share policies
- SLA-driven systems requiring usage-based throttling

### 4.3 Prefill-Decode Disaggregation Support

For advanced deployment patterns, Kthena Router natively supports prefill-decode disaggregation (xPyD), where the compute-intensive prefill phase is separated from the token generation decode phase.

**How it works**:
- Identifies PD group configurations from ModelServer CRDs
- Routes prefill requests to prefill-optimized pods
- Transfers KV cache state via configurable connectors (HTTP, Redis, etc.)
- Routes decode requests to decode-optimized pods
- Coordinates the two-phase process transparently to the client

**Benefits**:
- Optimizes hardware utilization by matching workload characteristics to hardware
- Reduces latency by using specialized hardware for each phase
- Improves cost-efficiency through better resource allocation

### 4.4 Token-Based Rate Limiting

Kthena Router provides comprehensive rate limiting capabilities to protect your inference infrastructure from overload and ensure fair resource allocation across users.

- **Input Token Limits**: Control the rate of incoming prompt tokens per user or API key
- **Output Token Limits**: Limit generated tokens to manage compute costs
- **Local Rate Limits**: Enforces a limit on a per-router-instance basis.
- **Global Rate Limits**: Enforces a shared limit across all router instances, using a central store like Redis.

### 4.5 Observability

Kthena Router provides comprehensive observability capabilities designed for production LLM serving:

- **Metrics**: Exposes detailed metrics including request latency, token consumption, scheduler plugin performance, and rate limiting statistics at `/metrics` endpoint
- **Structured Access Logs**: Records complete request lifecycle with routing decisions, timing breakdowns, and token tracking in JSON or text format
- **Debug Endpoints**: Provides `/debug/config_dump/*` APIs to inspect internal state, ModelRoute/ModelServer configurations, and real-time pod metrics
- **Standard Integration**: Seamlessly works with Prometheus, Grafana, ELK, and other observability stacks for monitoring, alerting, and troubleshooting


## 5. Performance

The **ScorePlugin** module in **Kthena Router** leverages a configurable, pluggable architecture to enable multi-dimensional scoring and intelligent routing of inference requests.To demonstrate the impact of intelligent scheduling, we construct a standardized benchmarking environment based on the **DeepSeek-R1-Distill-Qwen-7B** model to evaluate the performance of different scheduling strategies under both long and short system prompt scenarios.

Experimental results demonstrate that in **long system prompt scenarios**, the **KVCacheAware Plugin + Least Request Plugin** combination achieves **2.73× higher throughput** and reduces **TTFT latency by 73.5%**, significantly optimizing overall inference service performance and validating the core value of cache-aware scheduling for large-scale model inference.

### 5.1 Experimental Setup

A standardized benchmarking environment was built using the **DeepSeek-R1-Distill-Qwen-7B** model to evaluate the performance of different scheduling strategies.

**Table 1: Experimental Environment Configuration**

| Parameter              | Value                                   |
| :--------------------- | :-------------------------------------- |
| Model                  | deepseek-ai/DeepSeek-R1-Distill-Qwen-7B |
| Block size             | 128                                     |
| Max model length       | 32,768                                  |
| Max batch token count  | 65,536                                  |
| Replicas               | 3                                       |
| GPU memory utilization | 0.9                                     |
| Max sequence count     | 256                                     |
| Dataset                | generated-shared-prefix                 |
| Request groups         | 256                                     |
| Requests per group     | 32                                      |
| Request rate           | 800 req/s                               |
| Max concurrency        | 300                                     |

### 5.2 Long System Prompt Scenario (4096 tokens)

**Table 2: Performance Metrics – Long Prompt**

| Plugin Configuration         | Runs  | Success Rate (%) | Throughput (req/s) | Latency (s) | TTFT (s) |
| :--------------------------- | :---: | :--------------: | :----------------: | :---------: | :------: |
| Least Request + KVCacheAware |   3   |      100.0       |     **32.22**      |  **9.22**   | **0.57** |
| Least Request + Prefix Cache |   3   |      100.0       |       23.87        |    12.47    |   0.83   |
| Random                       |   3   |      100.0       |       11.81        |    25.23    |   2.15   |
| Least Request                |   3   |      100.0       |        9.86        |    30.13    |  12.46   |
| GPU Usage                    |   3   |      100.0       |        9.56        |    30.92    |  13.14   |
| Least Latency                |   3   |      100.0       |        9.47        |    31.44    |  11.07   |

## 6. Conclusion

Kthena Router represents a significant leap forward in LLM serving infrastructure. By moving beyond simple load balancing to model-aware, metrics-driven routing, it unlocks substantial performance improvements and cost savings that were previously unattainable. 

It is open source and available today. The [documentation](https://volcano-sh.github.io/kthena/) provides comprehensive guides for installation, configuration, and deployment. The [examples directory](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router) contains ready-to-use configurations for common scenarios.

Whether you're running a single model or managing a complex multi-tenant LLM platform, Kthena Router provides the intelligent routing capabilities needed to maximize performance, minimize costs, and deliver excellent user experiences.
