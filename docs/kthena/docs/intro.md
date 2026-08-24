---
sidebar_position: 1
---

# Kthena

**Kthena** is a lightweight, Kubernetes-native AI serving platform that turns a cluster into an enterprise-grade inference cloud. Instead of stitching together load balancers, autoscalers, and model servers by hand, you declare *what* you want — models, traffic rules, scaling targets — and Kthena's control plane reconciles the rest.

Kthena ships as **two independent, self-contained components** — workload controllers and a router. Install one, the other, or both; each is useful on its own.

> **Declarative CRDs. Any engine. Every scale.**
> Deploy with a single `ModelBooster` for a one-stop experience, or compose fine-grained primitives — `ModelRoute`, `ModelServer`, `ModelServing`, `AutoScalingPolicy`, and `AutoScalingPolicyBinding` — for full control. From a single-GPU/NPU prototype to a multi-node, prefill/decode disaggregated fleet.

---

## Why Kthena?

| Challenge                                   | Kthena's Answer                                                                                                                         |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| Heavy platforms with sprawling dependencies | Two self-contained Go binaries with a minimal dependency surface — fast to install, cheap to run, simple to upgrade                     |
| Adopting only part of a platform            | Fully decoupled planes: deploy the workload controllers, the router, or both — neither depends on the other at runtime                  |
| Managing multiple inference engines         | Unified CRD layer that abstracts vLLM, SGLang, Triton, and TorchServe behind a consistent API                                           |
| Balancing latency vs. throughput            | Request-level scheduler with pluggable scoring — KV-cache awareness, prefix-cache matching, LoRA affinity, least-request, least-latency |
| Scaling large models cost-effectively       | Prefill/Decode disaggregation with independent scaling ratios and cost-aware autoscaling                                                |
| Safe model updates in production            | Rolling upgrades with partition control, canary releases, and automated failover                                                        |

---

## Key Features

### Lightweight & Modular

Kthena is built to be adopted incrementally and to stay out of your way.

- **Small footprint** — Two self-contained Go binaries with a minimal dependency surface, quick to install and simple to upgrade.
- **Independently deployable planes** — The **workload** controllers (`ModelServing`, `AutoscalingPolicy`) and the **networking** router (`ModelRoute`, `ModelServer`) are separate Helm subcharts with separate CRD groups and separate release lifecycles.
- **No cross-plane runtime coupling** — Each component talks only to the Kubernetes API, never to the other. Run the router in front of workloads managed by Deployments or another operator; or run the controllers and expose pods through your own gateway.
- **Opt-in extras** — Gang scheduling (Volcano), webhooks, Gateway API support, and TLS are all optional, so a minimal install stays minimal.

See [Installation](./getting-started/installation.md#component-scoped-installation) for component-scoped install commands.

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
