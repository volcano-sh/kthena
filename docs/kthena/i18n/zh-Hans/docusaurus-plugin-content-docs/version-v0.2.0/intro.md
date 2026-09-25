---
sidebar_position: 1
---

# Kthena

欢迎使用 **Kthena**！Kthena 是一个 Kubernetes 原生 AI 服务平台，提供可扩展、高效的模型服务能力。

Kthena 帮助你在 Kubernetes 环境中部署和管理 AI 模型，并对模型服务进行扩缩容，同时提供智能路由、自动扩缩容和多模型服务等高级功能。

## 核心特性 {#key-features}

### 多后端推理引擎 {#multi-backend-inference-engine}

- **引擎支持**：原生支持 vLLM、SGLang、Triton 和 TorchServe 推理引擎，提供一致的 Kubernetes 原生 API。
- **服务模式**：支持在异构硬件加速器上运行常规部署和分离式部署的模型服务。
- **高级负载均衡**：提供可插拔的调度算法，支持最少请求数、最低延迟、随机调度、LoRA 亲和性，以及前缀缓存、KV 缓存和 PD 分组感知路由。
- **流量管理**：支持金丝雀发布、按权重分配流量、基于 Token 的限流和自动故障转移策略。
- **LoRA 适配器管理**：在不中断服务的情况下，动态管理 LoRA 适配器，并将请求路由到相应的适配器。
- **滚动更新**：通过可配置的发布策略实现模型的零停机更新。

### Prefill-Decode 分离 {#prefill-decode-disaggregation}

- **工作负载分离**：将预填充（Prefill）和解码（Decode）工作负载分离，优化大模型服务性能。
- **KV 缓存协调**：通过 LMCache、MoonCake 或 NIXL 连接器实现无缝协调，优化资源利用率。
- **智能路由**：根据模型分配请求，并通过 PD 分组感知能力支持分离式服务模式。

### 成本驱动的自动扩缩容 {#cost-driven-autoscaling}

- **多指标扩缩容**：根据自定义指标、CPU、内存、GPU 利用率和预算约束进行自动扩缩容。
- **灵活的策略**：支持应急模式（panic mode）、稳定扩缩容策略以及可配置的扩缩容行为。
- **策略绑定**：为特定模型部署配置细粒度的自动扩缩容策略，适用对象不限于 `ModelServing`。

### 可观测性与监控 {#observability--monitoring}

- **Prometheus 指标**：内置路由器性能和模型服务的指标采集功能。
- **请求跟踪**：提供详细的请求路由跟踪和性能监控。
- **健康检查**：为所有模型服务器提供全面的健康检查。
