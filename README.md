# Kthena

<p align="center">
  <img src="docs/proposal/images/kthena-arch.svg" alt="Kthena Architecture" width="800"/>
</p>

<p align="center">
  <strong>The Lightweight, Modular, Enterprise-Grade LLM Serving Platform That Makes AI Infrastructure Simple, Scalable, and Cost-Efficient</strong>
</p>

<p align="center">
| <a href="https://kthena.volcano.sh/">Documentation</a> | <a href="https://kthena.volcano.sh/blog">Blog</a> | <a href="#">White Paper</a> | <a href="#">Slack</a> |

</p>

<div align="center">

[![Go Check](https://github.com/volcano-sh/kthena/actions/workflows/go-check.yml/badge.svg)](https://github.com/volcano-sh/kthena/actions/workflows/go-check.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/volcano-sh/kthena)](https://goreportcard.com/report/github.com/volcano-sh/kthena)
![GitHub Release](https://img.shields.io/github/v/release/volcano-sh/kthena?sort=semver)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/volcano-sh/kthena)
</div>

## Overview

**Kthena** is a lightweight, Kubernetes-native LLM inference platform that transforms how organizations deploy and manage Large Language Models in production. Built with declarative model lifecycle management and intelligent request routing, it provides high performance and enterprise-grade scalability for LLM inference workloads.

The platform extends Kubernetes with purpose-built Custom Resource Definitions (CRDs) for managing LLM workloads, supporting multiple inference engines (vLLM, SGLang, Triton) and advanced serving patterns like prefill-decode disaggregation. Kthena's architecture separates control plane operations (model lifecycle, autoscaling policies) from data plane traffic routing through an intelligent router, enabling teams to manage complex LLM deployments with familiar cloud-native patterns while delivering cost-driven autoscaling, heterogeneous accelerators support, and multi-backend inference engines.

Kthena is deliberately **lightweight and composable**. The entire platform is two self-contained Go binaries with a small dependency surface, and its two planes are fully decoupled: install the **workload controllers** to manage the model lifecycle, install the **router** to handle inference traffic, or install both. Each side stands on its own and neither depends on the other at runtime — adopt just the piece you need today, and add the other whenever you're ready.

## Key Features

### **Lightweight & Modular**
- **Small footprint**: Two self-contained Go binaries with a minimal dependency surface — quick to install, cheap to run, and simple to upgrade.
- **Independently deployable planes**: The controller manager (workload) and the router (networking) are separate Helm subcharts with separate CRD groups and release lifecycles. Deploy either one alone, or both together.
- **No runtime coupling**: Each component talks only to the Kubernetes API, never to the other, so a partial install is a first-class scenario, not a degraded mode.
- **Bring your own stack**: Keep your existing API gateway, ingress, and observability tooling — Kthena plugs into them instead of replacing them.
- **Opt-in capabilities**: Gang scheduling (Volcano), webhooks, Gateway API support, and TLS are all optional, so a minimal install stays minimal.

### **Production-Ready LLM Serving**
Deploy and scale Large Language Models with enterprise-grade reliability, supporting vLLM, SGLang, Triton, and TorchServe inference engines through consistent Kubernetes-native APIs.

### **Simplified LLM Management**
- **Prefill-Decode Disaggregation**: Separate compute-intensive prefill operations from token generation decode processes to optimize hardware utilization and meet latency-based SLOs.
- **Cost-Driven Autoscaling**: Intelligent scaling based on multiple metrics (CPU, GPU, memory, custom) with configurable budget constraints and cost optimization policies
- **Zero-Downtime Updates**: Rolling model updates with configurable strategies
- **Dynamic LoRA Management**: Hot-swap adapters without service interruption  

### **Built-in Network Topology-Aware Scheduling**
Network topology-aware scheduling places inference instances within the same network domain to maximize inter-instance communication bandwidth and enhance inference performance.

### **Built-in Gang Scheduling**
Gang scheduling ensures atomic scheduling of distributed inference groups like xPyD, preventing resource waste from partial deployments.

### Intelligent Routing & Traffic Control
- Multi-model routing with pluggable load-balancing algorithms, including model load aware and KV-cache aware strategies.
- PD group aware request distribution for xPyD (x-prefill/y-decode) deployment patterns.
- Rich traffic policies, including canary releases, weighted traffic distribution, token-based rate limiting, and automated failover.
- LoRA adapter aware routing without inference outage

## Architecture

Kthena implements a Kubernetes-native architecture with a clear split between the control plane and the data plane. Each plane is an independent component with its own CRD group, its own Helm subchart, and its own release lifecycle — **either one can be deployed and used on its own**. It contains the following key components:

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
| Full platform                | Both subcharts        | Required for the one-stop `ModelBooster` API, which cascades into both CRD groups.                     |

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

Get up and running with Kthena in minutes. This [guide](docs/kthena/docs/getting-started/quick-start.md) will walk you through installing the platform and deploying your first LLM model. You can install the full platform, or only the component you need — see [Modular Deployment](#modular-deployment) and the [installation guide](docs/kthena/docs/getting-started/installation.md).

### Install from code

If you don't have a kubernetes cluster, try one-click install from code base:

```bash
./hack/local-up-kthena.sh
```

Run `./hack/local-up-kthena.sh --help` for more options.

## Community

Kthena is an open source project that welcomes contributions from developers, platform engineers, and AI practitioners.

**Get Involved:**
- **Issues**: Report bugs and request features on [GitHub Issues](https://github.com/volcano-sh/kthena/issues)
- **Discussions**: Join conversations on [GitHub Discussions](https://github.com/volcano-sh/kthena/discussions)
- **Documentation**: Help improve guides and examples

## Contributing

Contributions are welcome! Here's how to get started:

### Contribution Guidelines

- **Code**: Follow Go conventions and include tests for new features
- **Documentation**: Update relevant docs and examples
- **Issues**: Use GitHub Issues for bug reports and feature requests
- **Pull Requests**: Ensure CI passes and include clear descriptions

See [CONTRIBUTING.md](./CONTRIBUTING.md) for detailed guidelines.

## Meeting

Regular Community Meeting:

- Community weekly meeting for Asia: 16:00 - 17:00 (UTC+8) Wednesday. [Convert to your timezone](https://dateful.com/time-zone-converter?t=16&tz2=UTC%2B8).

Resources:
- [Meeting notes and agenda](https://docs.google.com/document/d/1bph_MA1UU3tKCV9T8XmJ0cIH3o85uAxExep412fDToE/edit?tab=t.0)
- [Meeting link](https://zoom-lfx.platform.linuxfoundation.org/meeting/99772189625?password=c4530aed-d08f-4871-9477-091980f37d50)
- [Meeting Calendar](https://calendar.google.com/calendar/u/0/newembed?src=volcano.sh.bot@gmail.com&csspa=1)

## License

Kthena is licensed under the [Apache 2.0 License](LICENSE).
