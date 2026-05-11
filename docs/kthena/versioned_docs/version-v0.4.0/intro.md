---
sidebar_position: 1
---

# Kthena

Welcome to **Kthena**, a Kubernetes-native AI serving platform that provides scalable and efficient model serving capabilities.

Kthena enables you to deploy, manage, and scale AI models in Kubernetes environments with advanced features like intelligent routing, auto-scaling, and multi-model serving.

## Key Features

### **Multi-Backend Inference Engine**
-   **Engine Support**: Native support for vLLM, SGLang, Triton, TorchServe inference engines with consistent Kubernetes-native APIs
-   **Serving Patterns**: Support for both standard and disaggregated serving patterns across heterogeneous hardware accelerators
-   **Advanced Load Balancing**: Pluggable scheduling algorithms including least request, least latency, random, LoRA affinity, prefix-cache, KV-cache and PD Groups aware routing
-   **Traffic Management**: Supports canary releases, weighted traffic distribution, token-based rate limiting, and automated failover policies
-   **LoRA Adapter Management**: Dynamic LoRA adapter routing and management without service interruption
-   **Rolling Updates**: Zero-downtime model updates with configurable rollout strategies

### **Prefill-Decode Disaggregation**
-   **Workload Separation**: Optimize large model serving by separating prefill and decode workloads for enhanced performance
-   **KV Cache Coordination**: Seamless coordination through LMCache, MoonCake, or NIXL connectors for optimized resource utilization
-   **Intelligent Routing**: Model-aware request distribution with PD Groups awareness for disaggregated serving patterns

### **Cost-Driven Autoscaling**
-   **Multi-Metric Scaling**: Autoscaling based on custom metrics, CPU, memory, GPU utilization, and budget constraints
-   **Flexible Policies**: Flexible scaling behaviors with panic mode, stable scaling policies, and configurable scaling behaviors
-   **Policy Binding**: Granular autoscaling policy assignment to specific model deployments not limited to `ModelInfer`

### **Observability & Monitoring**
-   **Prometheus Metrics**: Built-in metrics collection for router performance and model serving
-   **Request Tracking**: Detailed request routing and performance monitoring
-   **Health Checks**: Comprehensive health checks for all model servers
