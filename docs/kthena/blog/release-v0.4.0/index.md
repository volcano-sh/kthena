---
slug: release-v0.4.0
title: "Kthena v0.4.0 Released: More Robust and Feature-Rich Version"
authors: [hzxuzhonghu, LiZhenCheng9527, YaoZengzeng]
tags: [release]
date: 2026-04-14
---

# Announcing Kthena 0.4.0

Thanks to the incredible dedication and collective efforts of our contributors over the past two months, Kthena’s stability has reached new heights. We want to express our deepest gratitude to everyone who contributed to this milestone. Today, we are thrilled to announce the official release of **Kthena 0.4.0**—our most robust and feature-rich version yet!

Beyond rock-solid stability, Kthena 0.4.0 introduces a wave of exciting new features designed to streamline your LLM workloads and empower your AI infrastructure.

<!-- truncate -->

## Improved Observability

### Role Status Visibility

To minimize kube-apiserver load, Kthena's `ModelServing` utilizes a local store to cache the status of `ServingGroup`s and `Role`s. While highly efficient, we realized this limited observability during debugging.

In v0.4.0, we've broken the black box. We now [expose role status directly via Kubernetes Events](https://github.com/volcano-sh/kthena/pull/676), dramatically enhancing the observability of `ModelServing`. Looking ahead, we plan to directly embed this crucial Role information into the `ModelServing` Status, giving you complete, transparent control over your deployments.

### Comprehensive Access Logging

Router observability is now easier than ever. The Router now generates a detailed access log, capturing rich [routing metadata](https://github.com/volcano-sh/kthena/pull/621) for every single request. Here is an example of a Router log:

```sh
[2026-04-16T07:33:08.435627146Z] "POST /v1/chat/completions HTTP/1.1" 200 model_name=deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B model_router=deepseek-r1-1-1.5b model_server=deepseek-r1 selected_pod=deepseek-r1-1-5b-6989c66877-p6jvv request_id=ad683d1b-6011-4b0f-b9b5-cbb18d43c57b gateway=dev/default http_route=kthena-e2e-gie-8eoas/llm-route inference_pool=kthena-e2e-gie-8eoas/deepseek-r1-1-5b tokens=10/38 timings=3ms(0+2+0)
```

Compared to previous versions, we have introduced the `gateway`, `http_route`, and `inference_pool` fields to provide deeper visibility into Gateway and Gateway Inference Extension traffic.

## A Faster, Smarter Router

### Deterministic & Efficient Model Selection

Previously, mapping multiple `ModelRoute` resources to a single model could trigger route conflicts—leading to ambiguous rule matching and inconsistent target selection. Because Kubernetes' built-in CRD validation cannot enforce global cross-object uniqueness, we tackled this gracefully at the routing layer.

Kthena 0.4.0 introduces a robust [conflict-resolution mechanism](https://github.com/volcano-sh/kthena/pull/779). When duplicate `ModelRoute`s exist, the router deterministically prioritizes the oldest (typically prebuilt) route, treating newer duplicates as lower priority. Predictable, rock-solid routing every time.

### Configurable Prefix-Caching

One size doesn't fit all. That's why Kthena replaced hardcoded prefix-cache parameters with a fully [configurable prefix-matching system](https://github.com/volcano-sh/kthena/pull/844). You now have granular control over prefix-cache behavior with the following parameters:

- **Block Size (for hash processing):** Controls the granularity of the prefix match. Smaller blocks offer more precise matching but increase CPU overhead, while larger blocks process faster.
- **Max Block Limits:** Sets a ceiling on how much of a given prompt is hashed. This protects the router from computational bottlenecks and latency spikes when handling excessively long incoming prompts.
- **Cache Capacity:** Defines how many prefix entries the router can remember. Increasing this improves cache hit rates for highly diverse workloads, at the cost of slightly higher memory footprint.
- **Top-K Results:** Determines how many candidate instances are considered when a match is found. Tuning this allows for better load balancing, ensuring traffic is distributed smoothly across multiple nodes instead of overwhelming a single active instance.

By fine-tuning these settings, you can tailor Kthena's routing performance to match your specific models and business LLM workloads.

## Granular, Resource-Efficient Rolling Updates

Historically, Kthena executed rolling updates at the entire `ServingGroup` level. For massive LLM applications, completely rebuilding a `ServingGroup` is an incredibly resource-intensive and time-consuming process.

To solve this, we are introducing [role-based rolling updates](https://github.com/volcano-sh/kthena/pull/802). You no longer need to update an entire `ServingGroup` when only a specific `Role` requires changes (which is also why we introduced `RoleRecreate` in the recovery policy). Starting from 0.4.0, you can dynamically adjust your `rolloutStrategy`—drastically cutting down resource consumption, speeding up deployment times.

## A Thriving, Connected Ecosystem

We are deeply committed to building Kthena alongside the wider open-source community. In 0.4.0, we have extended kthena's model downloader with [ModelScope protocol support](https://github.com/volcano-sh/kthena/pull/861) to provide a seamless, high-speed alternative to Hugging Face for developers facing network constraints in China.

Additionally, `ModelServing` is now thoroughly verified to support leading inference engines like **vLLM** and **SGLang**. As Kthena continues to weave deeper into the Cloud Native AI ecosystem, we warmly invite anyone interested to join us in building this vibrant future together!
