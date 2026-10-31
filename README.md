# Kthena

<p align="center">
  <img src="docs/proposal/images/kthena-arch.svg" alt="Kthena Architecture" width="800"/>
</p>

<p align="center">
  <strong>Kubernetes-native LLM Serving Platform: declarative model lifecycle and intelligent request routing</strong>
</p>

<div align="center">

[![Go Check](https://github.com/volcano-sh/kthena/actions/workflows/go-check.yml/badge.svg)](https://github.com/volcano-sh/kthena/actions/workflows/go-check.yml)
![Go Version](https://img.shields.io/github/go-mod/go-version/volcano-sh/kthena)
![GitHub Release](https://img.shields.io/github/v/release/volcano-sh/kthena?sort=semver)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/volcano-sh/kthena)
</div>

## Overview

**Kthena** is a lightweight, Kubernetes-native LLM inference platform that transforms how organizations deploy and manage Large Language Models in production. Built with declarative model lifecycle management and intelligent request routing, it provides high performance and enterprise-grade scalability for LLM inference workloads.

The platform extends Kubernetes with purpose-built Custom Resource Definitions (CRDs) for managing LLM workloads on inference engines such as vLLM and SGLang, including advanced serving patterns like prefill-decode disaggregation and cost-optimized autoscaling across heterogeneous accelerators.

Kthena is deliberately **lightweight and composable**. The entire platform is two self-contained Go binaries with a small dependency surface, and its two planes are fully decoupled: install the **workload controllers** to manage the model lifecycle, install the **router** to handle inference traffic, or install both. Neither depends on the other at runtime, and optional capabilities such as gang scheduling, webhooks, Gateway API support, and TLS stay off until you need them.

## Key Features

### Model Lifecycle Management
- **Role-based deployment**: `ModelServing` deploys a model as serving groups of roles (for example prefill and decode), each with entry and worker pods.
- **Prefill-decode disaggregation**: Separate compute-intensive prefill from token-generation decode to optimize hardware utilization and meet latency-based SLOs.
- **Rolling updates**: Roll out model changes with configurable strategies and partitions.
- **Autoscaling**: Scale on metrics scraped from pods or queried from Prometheus, including PD-disaggregated scaling and cost-optimized scaling across heterogeneous hardware.

### Scheduling (with Volcano)
- **Gang scheduling**: Schedule distributed inference groups such as xPyD atomically, preventing resource waste from partial deployments.
- **Network topology-aware scheduling**: Place inference instances within the same network domain to maximize inter-instance communication bandwidth.

### Intelligent Routing & Traffic Control
- **Pluggable load balancing**: Multi-model routing with filter and score plugins, including model-load-aware and KV-cache-aware strategies.
- **PD-aware routing**: Request distribution for xPyD (x-prefill/y-decode) deployments, with KV transfer over HTTP, NIXL, Mooncake, or SGLang connectors.
- **Traffic policies**: Canary releases, weighted traffic distribution, and failover.
- **Rate limiting**: Token-based rate limiting, enforced locally or globally via Redis.
- **Fairness and session stickiness**: Optional fairness queuing, and session-sticky routing backed by memory or Redis.
- **LoRA-aware routing**: Route requests to LoRA adapters without inference outage.
- **External model providers**: Route to OpenAI- and Anthropic-compatible APIs alongside self-hosted models.
- **Authentication and Gateway API**: JWT authentication, plus optional Gateway API and Gateway API Inference Extension integration.

## Architecture

Kthena implements a Kubernetes-native architecture with a clear split between the control plane and the data plane. Each plane has its own CRD group, Helm subchart, and release lifecycle. It contains the following key components:

- **Kthena-controller-manager**: 
  The control plane component governing the LLM inference lifecycle. It continuously reconciles Kthena CRDs to deploy, scale, and upgrade inference replicas across the cluster while exposing advanced scheduling policies that integrate directly with the [Volcano scheduler](https://github.com/volcano-sh/volcano/).   
- **Kthena-router**:
  The data plane entry point for inference traffic. It classifies each request by model name, custom headers, or URI patterns, then applies load-balancing policies and traffic controls to dispatch requests to the right inference instance. Native support for prefill–decode disaggregation routing while keeping high throughput and low latency.

### Modular Deployment

The two components talk to Kubernetes, not to each other, so you can mix and match:

| You want to...               | Install               | Notes                                                                                                  |
| ---------------------------- | --------------------- | ------------------------------------------------------------------------------------------------------ |
| Manage model workloads only  | `workload` subchart   | Use `ModelServing` / `AutoscalingPolicy` and expose pods with your own gateway or `Service`.           |
| Route inference traffic only | `networking` subchart | Point `ModelServer` at any pods — Deployments, StatefulSets, or workloads managed by another operator. |
| Full platform                | Both subcharts        | Manage workloads with `ModelServing` and route traffic with `ModelServer` and `ModelRoute`.          |

> **ModelBooster deprecation:** ModelBooster is deprecated in v1.1; use ModelServing, ModelServer, and ModelRoute. Removal is no earlier than v1.5. See the [deployment guidance](docs/kthena/docs/user-guide/model-deployment.md#modelbooster-deprecation). Existing ModelBooster deployments remain supported in v1.1 and require both subcharts.

```bash
# Workload controllers only (no router)
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena \
  --namespace kthena-system --create-namespace \
  --set networking.enabled=false

# Router only (no workload controllers)
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena \
  --namespace kthena-system --create-namespace \
  --set workload.enabled=false
```

For more details, please refer to [Kthena Architecture](https://kthena.volcano.sh/docs/architecture/architecture)

> [!Note]
> The router component is a reference implementation, because Gateway Inference Extension does not natively support prefill-decode disaggregation. The Kthena router is still under active iteration, and it can be deployed behind a standard API gateway.


## Getting Started

Install the full platform with Helm:

```bash
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena \
  --namespace kthena-system --create-namespace
```

This [guide](docs/kthena/docs/getting-started/quick-start.md) will walk you through installing the platform and deploying your first LLM model. You can install the full platform, or only the component you need — see [Modular Deployment](#modular-deployment) and the [installation guide](docs/kthena/docs/getting-started/installation.md).

### Install from source

To try Kthena from source without an Kubernetes cluster, run:

```bash
./hack/local-up-kthena.sh
```

Run `./hack/local-up-kthena.sh --help` for more options.

## Community and Contributing

Kthena welcomes contributions from developers, platform engineers, and AI practitioners.

- **Issues**: Report bugs and request features on [GitHub Issues](https://github.com/volcano-sh/kthena/issues).
- **Discussions**: Join conversations on [GitHub Discussions](https://github.com/volcano-sh/kthena/discussions).
- **Contributing**: See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup and pull request guidelines.

## Meeting

Regular Community Meeting:

- Community weekly meeting for Asia: 16:00 - 17:00 (UTC+8) Wednesday. [Convert to your timezone](https://dateful.com/time-zone-converter?t=16&tz2=UTC%2B8).

Resources:
- [Meeting notes and agenda](https://docs.google.com/document/d/1bph_MA1UU3tKCV9T8XmJ0cIH3o85uAxExep412fDToE/edit?tab=t.0)
- [Meeting link](https://zoom-lfx.platform.linuxfoundation.org/meeting/99772189625?password=c4530aed-d08f-4871-9477-091980f37d50)
- [Meeting Calendar](https://calendar.google.com/calendar/u/0/newembed?src=volcano.sh.bot@gmail.com&csspa=1)

## License

Kthena is licensed under the [Apache 2.0 License](LICENSE).
