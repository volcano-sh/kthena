---
sidebar_position: 1
---

# Kthena

**Kthena** is a Kubernetes-native AI serving platform that turns a cluster into a production-grade inference cloud. Instead of stitching together load balancers, autoscalers, and model servers by hand, you declare *what* you want — models, traffic rules, scaling targets — and Kthena's control plane reconciles the rest.

> **Declarative CRDs. Any engine. Every scale.**
> Deploy with a single `ModelBooster` for a one-stop experience, or compose fine-grained primitives — `ModelRoute`, `ModelServer`, `ModelServing`, `AutoScalingPolicy`, and `AutoScalingPolicyBinding` — for full control. From a single-GPU/NPU prototype to a multi-node, prefill/decode disaggregated fleet.

---

## Why Kthena?

| Challenge | Kthena's Answer |
|---|---|
| Managing multiple inference engines | Unified CRD layer that abstracts vLLM, SGLang, Triton, and TorchServe behind a consistent API |
| Balancing latency vs. throughput | Request-level scheduler with pluggable scoring — KV-cache awareness, prefix-cache matching, LoRA affinity, least-request, least-latency |
| Scaling large models cost-effectively | Prefill/Decode disaggregation with independent scaling ratios and cost-aware autoscaling |
| Safe model updates in production | Rolling upgrades with partition control, canary releases, and automated failover |

---

## Key Features

### Multi-Backend Inference Engine

Kthena treats inference engines as pluggable backends behind a single Kubernetes-native API surface.

- **Engine Support** — Native integration with **vLLM**, **SGLang**, **Triton**, and **TorchServe**. Switch engines without rewriting manifests.
- **Serving Patterns** — Run standard replicated serving *or* disaggregated prefill/decode topologies across heterogeneous accelerators (H100, A100, NPU, etc.).
- **Intelligent Routing** — Pluggable scheduler with filter and score plugins: least request, least latency, LoRA affinity, prefix-cache matching, KV-cache awareness, and PD-group-aware routing — all at the *request* level.
- **Traffic Management** — Canary releases with weighted traffic splits, token-based rate limiting, per-model fair queuing, and automated failover policies.
- **LoRA Adapter Management** — Hot-load, unload, and route LoRA adapters dynamically without restarting or draining pods.
- **Rolling Updates** — Zero-downtime model upgrades with configurable partition-based rollout strategies.

### Prefill-Decode Disaggregation

Large-model inference has two fundamentally different workloads — compute-heavy prompt processing (prefill) and memory-bound token generation (decode). Kthena lets you split them into separately scaled `ServingGroup` roles.

- **Workload Separation** — Dedicate prefill nodes for maximum compute throughput and decode nodes for lowest latency, each with its own replica count and hardware profile.
- **KV Cache Coordination** — Seamless KV-cache transfer between prefill and decode pods via **LMCache**, **MoonCake**, or **NIXL** connectors — no application-level plumbing required.
- **PD-Aware Routing** — The Kthena Router understands PD groups: it selects a decode pod first, then pairs it with a compatible prefill pod in the same group, ensuring co-located cache hits and minimal data movement.

### Cost-Driven Autoscaling

Kthena's autoscaler goes beyond simple metric thresholds — it factors in cost, SLOs, and heterogeneous hardware.

- **Multi-Metric Scaling** — Scale on custom metrics, CPU, memory, GPU utilization, and budget constraints in a single policy.
- **Flexible Policies** — Combine stable scaling with a **panic mode** fast-path for traffic spikes, plus configurable stabilization windows to avoid flapping.
- **Policy Binding** — Attach autoscaling policies to any `ModelServing` workload — not just a single resource type — with support for cost-aware distribution across heterogeneous instance pools (e.g., H100 + A100).

### Observability & Monitoring

- **Prometheus Metrics** — Built-in metrics for router latency (TTFT / TPOT), queue depth, cache hit rates, and per-model throughput.
- **Request Tracking** — End-to-end request tracing across the authentication → scheduling → proxy pipeline.
- **Access Log** — Structured access logging for every request, including model, latency, token counts, and upstream pod.
- **Health Checks** — Continuous liveness, readiness, and engine-specific health probes for every inference pod.
