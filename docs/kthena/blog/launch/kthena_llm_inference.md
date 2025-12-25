---
slug: launch-blog-post
title: "Volcano Community Launches Kthena Sub-project: Redefining Intelligent LLM Inference"
authors: [hzxuzhonghu]
date: 2025-09-01
tags: []
---

# Volcano Community Launches Kthena Sub-project: Redefining Intelligent LLM Inference

Today, we’re excited to announce to global developers and MLOps engineers the arrival of a new sub-project in the Volcano community **Kthena**.

Kthena is a cloud-native, high-performance LLM inference routing, orchestration, and scheduling system designed specifically for Kubernetes. It aims to solve the core challenges of deploying and serving LLMs at scale in production environments. Through its unique features such as **KV Cache-aware scheduling** and **Prefill/Decode separation routing**, Kthena significantly improves GPU resource utilization, reduces inference latency, and provides enterprises with unprecedented flexibility and control.

As a sub-project of Volcano, Kthena is dedicated to helping Volcano expand its boundaries beyond AI training, creating a complete integrated solution for both training and inference.

[**Project Address**](https://github.com/volcano-sh/kthena)

<!-- truncate -->

## The "Last Mile" Challenge of Production LLM Serving

Large Language Models (LLMs) are reshaping industries at an unprecedented pace, but deploying them efficiently and cost-effectively in production environments—especially on cloud-native platforms based on Kubernetes—remains fraught with difficulties. Developers generally face the following challenges:

1. **Low Resource Utilization**: LLM inference, particularly its unique KV Cache mechanism, consumes dynamic and massive amounts of GPU memory. Traditional load balancing typically uses Round-Robin algorithms, which are unaware of these load characteristics, leading to high costs where GPU resources sit idle while requests are queued.
2. **Difficulty Balancing Latency and Throughput**: LLM inference consists of two phases: **Prefill** (processing input prompts) and **Decode** (generating tokens). The former is compute-intensive, while the latter is memory-bound. Scheduling them together often prevents targeted optimization, affecting overall service response speed and throughput. Consequently, Prefill/Decode (PD) separation has become mainstream, but efficient routing and scheduling remains a challenge.
3. **Complex Multi-tenant and Multi-model Management**: In enterprise environments, it is often necessary to serve multiple business units, different model versions, or LoRA-fine-tuned models simultaneously. Implementing fair scheduling, priority management, and dynamic routing is a complex engineering problem; some industry solutions even map AI gateways one-to-one with large models.
4. **Lack of Native Kubernetes Integration**: Many existing solutions are either external systems disconnected from the Kubernetes ecosystem or overly complex, failing to meet the simplicity and operational flexibility required for production-grade use.

## Kthena: The Intelligent Brain of Cloud-Native LLM Inference

To overcome these challenges, Kthena was born. It is not intended to replace existing LLM serving frameworks (such as vLLM or SGLang), but rather to serve as an intelligent **traffic hub** and **scheduling center** above them, deeply integrated into Kubernetes.

![Architecture](images/kthena-arch.svg)

Kthena consists of two major components:

1) **Kthena Router**: An independent, high-performance multi-model router responsible for receiving inference requests and intelligently distributing them to backend `ModelServer` instances based on `ModelRoute` rules.

2) **Kthena Controller Manager**: A controller in the Kubernetes control plane, composed of multiple controllers responsible for orchestration and lifecycle management of LLM workloads. It continuously reconciles and links multiple CRDs (such as `ModelBooster`, `ModelServing`, `AutoScalingPolicy`/`AutoScalingPolicyBinding`, and `ModelRoute`/`ModelServer`) to transform declarative APIs into runtime resources.

The `ModelServing` controller orchestrates `ServingGroup` and `Prefill/Decode` role groupings; supports topology-aware scheduling, Gang scheduling, rolling upgrades, and fault recovery; and implements elastic scaling based on `AutoScalingPolicy`.

This architecture makes Kthena a highly programmable bridge connecting user requests with LLM inference backends.

## Core Features and Advantages

Kthena’s strength lies in its core capabilities specifically designed for LLM inference scenarios.

### 1) Production-Grade Inference Orchestration (ModelServing)

![orchestration](images/model-serving.svg)

- **Three-layer LLM workload model**: `ModelServing -> ServingGroup -> Role`. A single API supports multiple deployment forms, including native LLM deployment, PD separation, and large-scale EP (Expert Parallelism).
- **Prefill-Decode Separation Deployment**: Schedules compute-intensive Prefill instances onto nodes with high compute capability, and memory-bound Decode instances onto nodes with high-bandwidth memory, enabling better resource matching and lower end-to-end latency. Prefill and Decode can scale independently to adapt to mixed traffic patterns.
- **Multiple Parallel Paradigms**: Flexible configuration of TP/PP/DP/EP to maximize utilization and meet SLO targets.
- **Topology-aware + Gang Scheduling**: Gang scheduling ensures atomic placement of related Pods (ServingGroup/Role), reducing tail latency and avoiding wasted fragmented resources; topology-aware scheduling improves parallel communication efficiency.

### 2) Out-of-the-Box Model Onboarding (ModelBooster)

- Provides deployment templates for mainstream models, including PD separation.
- Automatically generates routing policies and lifecycle resources such as `ModelRoute`, `ModelServer`, `ModelServing`, and `Autoscaling`.
- Covers common deployment scenarios while allowing fine-grained customization via `ModelServing`.

### 3) Intelligent, Model-Aware Routing (Kthena Router)

- **Multi-model routing**: OpenAI API compatible; routes based on headers or request content.
- **Pluggable scheduling**: Least Request, Minimum Latency, KV Cache-aware, Prefix Cache-aware, LoRA affinity, GPU utilization-aware, Fair Scheduling, and more.
- **Hot-pluggable LoRA without interruption**: Detects adapters loaded by inference engines and routes without downtime.
- **Traffic governance**: weight-based routing, canary release, token-level rate limiting, and failover.
- **All-in-one routing architecture**: no need to deploy Envoy Gateway; natively supports PD-separated traffic and consolidates multi-layer routing for simpler operations.

### 4) Cost-Driven Auto-Scaling (Autoscaler)

- **Homogeneous scaling**: stable and burst modes, scaling by business metrics (CPU/GPU/Memory/custom).
- **Heterogeneous deployment optimization**: cost-capability greedy placement across multiple inference engines and heterogeneous accelerators.

### 5) Support for Mainstream Inference Engines and Heterogeneous Hardware

- Supports vLLM, SGLang, Triton/TGI, etc. with unified API abstraction and standardized metrics.
- Supports hybrid deployments across GPU/NPU, working with heterogeneous autoscaling to balance cost and SLO.

### 6) Built-in Traffic Control and Fair Scheduling

- **Fair scheduling**: based on priority and historical token consumption; prevents starvation.
- **Rate limiting**: fine-grained controls by user, model, and token length.

## Extreme Performance Improvements

Based on the scheduling plugin architecture of Kthena Router, in scenarios with long system prompts (e.g., 4096 tokens), the **KV Cache-aware + Least Request** strategy compared to a random baseline:

- Throughput can be increased by approximately **2.73×**.
- TTFT (Time to First Token) is reduced by approximately **73.5%**.
- End-to-end latency is reduced by more than **60%**.

| Plugin Configuration | Throughput (req/s) | TTFT (s) | End-to-End Latency (s) |
| --- | ---:| ---:| ---:|
| Least Request + KVCacheAware | **32.22** | **9.22** | **0.57** |
| Least Request + Prefix Cache | 23.87 | 12.47 | 0.83 |
| Random | 11.81 | 25.23 | 2.15 |

The gap in short-prompt scenarios converges with prompt length, but in multi-turn conversations, templated generation, and business scenarios with highly similar prefixes, KV Cache-aware routing delivers significant gains. Actual improvements depend on model size, prompt length, and hardware, but the “mix-and-match, scenario-based selection” approach has proven effective.

## Future Roadmap

Kthena received attention and support from community users in its early planning and development stages, but this is only the beginning. We plan to support more efficient scheduling algorithms, broader best practices for large model deployment, and will continue to focus on large-scale deployment and performance optimization for LLM inference.

## Start Exploring Kthena Now

- **GitHub Repository**: https://github.com/volcano-sh/kthena
- **Official Website**: https://kthena.volcano.sh/
- **Community (Slack)**: https://cloud-native.slack.com/archives/C011GJDQS0N

Let’s work together to give LLMs cloud-native wings and release the full potential of AI.
