---
sidebar_position: 1
---

# Kthena

**Kthena** 是一个 Kubernetes 原生 AI 服务平台，可将集群转变为生产级推理云。你只需声明所需的模型、流量规则和扩缩容目标，Kthena 控制平面便会协调实现这些目标，无需手动组合负载均衡器、自动扩缩容组件和模型服务器。

> **声明式 CRD，支持多种引擎，适用于各种规模。**
> 通过组合 `ModelRoute`、`ModelServer`、`ModelServing`、`AutoScalingPolicy` 和 `AutoScalingPolicyBinding` 等细粒度资源，实现全面控制。从单 GPU/NPU 原型到多节点 Prefill/Decode 分离式集群，均可按需部署。

---

## 为什么选择 Kthena？ {#why-kthena}

| 挑战 | Kthena 的解决方案 |
| --- | --- |
| 管理多种推理引擎 | 通过统一的 CRD 层，为 vLLM、SGLang、Triton 和 TorchServe 提供一致的 API |
| 平衡延迟与吞吐量 | 请求级调度器支持可插拔的评分机制，包括 KV 缓存感知、前缀缓存匹配、LoRA 亲和性、最少请求数和最低延迟 |
| 经济高效地扩缩容大模型 | 支持 Prefill/Decode 分离、独立的扩缩容比例和成本感知的自动扩缩容 |
| 安全更新生产环境中的模型 | 支持分区控制的滚动升级、金丝雀发布和自动故障转移 |

---

## 核心特性 {#key-features}

### 多后端推理引擎 {#multi-backend-inference-engine}

Kthena 将推理引擎作为可插拔的后端，通过统一的 Kubernetes 原生 API 进行管理。

- **引擎支持**：原生集成 **vLLM**、**SGLang**、**Triton** 和 **TorchServe**，切换引擎无需重写资源清单。
- **服务模式**：支持在异构加速器（H100、A100、NPU 等）上运行常规多副本服务或 Prefill/Decode 分离式拓扑。
- **智能路由**：可插拔的调度器提供过滤和评分插件，支持最少请求数、最低延迟、LoRA 亲和性、前缀缓存匹配、KV 缓存感知和 PD 分组感知路由，均作用于请求级别。
- **流量管理**：支持按权重分配流量的金丝雀发布、基于 Token 的限流、按模型公平排队和自动故障转移策略。
- **LoRA 适配器管理**：支持 LoRA 适配器的动态热加载、卸载和请求路由，无需重启 Pod，也无需停止接收新请求并等待现有请求处理完毕。
- **滚动更新**：通过可配置的分区发布策略，实现模型的零停机升级。

### Prefill-Decode 分离 {#prefill-decode-disaggregation}

大模型推理包含两种本质不同的工作负载：计算密集型的预填充（Prefill，即提示词处理）和受内存带宽限制的解码（Decode，即 Token 生成）。Kthena 支持将两者拆分为可独立扩缩容的 `ServingGroup` 角色。

- **工作负载分离**：为 Prefill 分配专用节点以提高计算吞吐量，为 Decode 分配专用节点以降低延迟，两者可分别配置副本数和硬件规格。
- **KV 缓存协调**：通过 **LMCache**、**MoonCake** 或 **NIXL** 连接器，在 Prefill 和 Decode Pod 之间无缝传输 KV 缓存，无需在应用层自行实现对接。
- **PD 感知路由**：Kthena Router 能够识别 PD 分组，先选择 Decode Pod，再匹配同组内兼容的 Prefill Pod，以利用组内缓存并尽量减少数据传输。

### 成本驱动的自动扩缩容 {#cost-driven-autoscaling}

Kthena 的自动扩缩容组件不仅依据简单的指标阈值，还会考虑成本、服务等级目标（SLO）和异构硬件。

- **多指标扩缩容**：在同一策略中结合自定义指标、CPU、内存、GPU 利用率和预算约束进行扩缩容。
- **灵活的策略**：结合稳定扩缩容与应急模式（panic mode），快速应对流量突增，并通过可配置的稳定窗口避免副本数频繁波动。
- **策略绑定**：可为任意 `ModelServing` 工作负载绑定自动扩缩容策略，策略不局限于单一资源类型，并支持在异构实例池（如 H100 + A100）之间按成本分配资源。

### 可观测性与监控 {#observability--monitoring}

- **Prometheus 指标**：内置路由器延迟（TTFT / TPOT）、队列深度、缓存命中率和各模型吞吐量等指标。
- **请求跟踪**：在认证 → 调度 → 代理的完整处理流程中，实现端到端请求跟踪。
- **访问日志**：为每个请求记录结构化访问日志，包括模型、延迟、Token 数量和上游 Pod。
- **健康检查**：持续对每个推理 Pod 执行存活、就绪以及引擎专用的健康探测。
