import LightboxImage from '@site/src/components/LightboxImage';
import kthenaRouterArch from '../assets/diagrams/kthena-router-arch.svg';
import kthenaRouterComponents from '../assets/diagrams/kthena-router-components.svg';

# Kthena Router

Kthena Router is a standalone router component designed to provide unified access to Large Language Models (LLMs). It supports both privately deployed LLMs and public AI service providers such as OpenAI, DeepSeek, HuggingFace, and others.

Our goal is to deliver a lightweight, user-friendly, and extensible LLM inference router that enables users to rapidly build and deploy in production environments with minimal dependencies, thereby reducing maintenance costs and improving operational efficiency.

## Overview

<LightboxImage src={kthenaRouterArch} alt="arch"></LightboxImage>

Kthena Router is deployed as a standalone binary that can seamlessly integrate with existing gateway infrastructure or serve as a direct traffic entry point for handling AI workloads independently.

For backend model access, the router supports both external public AI service providers (such as OpenAI, Google Gemini) and privately deployed models within the cluster.

For privately deployed models in particular, the router supports mainstream inference frameworks including vLLM, SGLang, and others. By continuously monitoring the metrics interfaces of inference engines running on model pods, it obtains real-time model status information, including currently loaded LoRA details, KV cache utilization, and other key metrics. This information enables intelligent routing decisions that significantly improve inference service throughput while reducing latency.

## Core Components

<LightboxImage src={kthenaRouterComponents} alt="arch"></LightboxImage>

**Router**: The core execution framework responsible for request reception, processing, and forwarding.

**Listener**: Manages HTTP/HTTPS listeners and handles incoming traffic on specified ports. It provides flexible configuration for different protocols and can bind to multiple addresses to serve various types of requests.

**Controller**: Synchronizes and processes Pods and Custom Resources (CRs) such as ModelRoute and ModelServer.

**Filters**: Contains two sub-modules:
- **Auth**: Handles traffic authentication and authorization
- **RateLimit**: Manages various rate limiting strategies including input tokens, output tokens, and user tier-based rate limiting

**Backend**: Provides an abstraction layer for accessing various inference engines, masking differences in metrics interface access methods and metric naming conventions across different inference frameworks.

**Metrics Fetcher**: Continuously collects real-time metrics from inference engine endpoints running on model pods. It gathers critical performance data including KV cache utilization, current LoRA model status, request queue lengths, and latency metrics (TTFT/TPOT). This component ensures up-to-date information is available for intelligent routing decisions.

**Datastore**: A unified data storage layer that provides easy access to ModelServer-to-Pod associations, as well as information about Base Models/LoRA configurations and runtime metrics within pods.

**Scheduler**: Implements traffic scheduling algorithms consisting of a scheduling framework and various scheduling algorithm plugins. The framework integrates and runs different scheduling plugins to filter and score the pod collections corresponding to ModelServers, selecting the globally optimal pod as the final access target.

Initial scheduling plugins include:
- **Least KV Cache Usage**
- **Least Latency**: TPOT (Time Per Output Token), TTFT (Time To First Token)
- **Least Pending Request**
- **Prefix Cache Aware**
- **LoRA Affinity**
- **Fairness Scheduling**


## Features

**Standalone Binary**: Deployed as an independent binary (not as a plugin for existing proxies like Envoy), ensuring lightweight operation, minimal dependencies, user-friendly experience, and simple deployment.

**Seamless Integration**: Seamlessly compatible with existing API Gateway infrastructure, whether based on Nginx, Envoy, or other proxies, maximizing reuse of existing API Gateway capabilities such as authentication and authorization.

**Model-Aware Routing**: Leverages metrics obtained from inference engines to enable better AI traffic scheduling and improved inference performance.

**LoRA-Aware Load Balancing**: Intelligently route to pods that have already loaded the desired LoRA adapter to reduce adapter swap latency from hundreds of milliseconds to near-zero.

**Rich Load Balancing Algorithms**: Supports Session Affinity, Prefix Cache Aware, KV Cache Aware, and Heterogeneous GPU Hardware Aware algorithms to enhance inference service SLO and reduce inference costs.

**Inference Engine Agnostic**: Compatible with various mainstream frameworks including vLLM, SGLang, TGI, and others.

**Model-Based Canary Deployments and A/B Testing**: Enables gradual rollouts and testing strategies at the model level.

**Authentication/Authorization**: Supports common authentication methods including API Key, JWT, and OAuth Authorization.

**Comprehensive Rate Limiting**: Implements diverse rate limiting strategies for input tokens, output tokens, and role-based limiting.

**Kubernetes Gateway API Compatibility**: Compatible with the inference extension of the upstream Kubernetes community's Gateway API.


## API

### 1. ModelRoute

ModelRoute defines traffic routing strategies based on request characteristics such as Model Name, LoRA Name, HTTP URL, Headers, and other relevant information to forward traffic to the appropriate ModelServer.

### 2. ModelServer

ModelServer defines inference service instances and traffic access policies. It uses WorkloadSelector to identify the pods where models are located, specifies the inference engine used for model deployment through InferenceFramework, and defines specific policies for accessing model pods through TrafficPolicy.
