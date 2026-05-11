# Router Routing

This page describes the router routing features and capabilities in Kthena, based on real-world examples and configurations.

## Overview

Kthena Router provides sophisticated traffic routing capabilities that enable intelligent forwarding of inference requests to appropriate backend models. The routing system is built around two core Custom Resources (CRs):

- **ModelServer**: Defines backend inference service instances with their associated pods, models, and traffic policies. It uses **workloadSelector.matchLabels** to select which pods belong to it (pods must match all given labels); this applies to every ModelServer.
- **ModelRoute**: Defines routing rules based on request characteristics such as model name, LoRA adapters, HTTP headers, and weight distribution

For a detailed definition of the ModelServer and ModelRoute CRs, please refer to the [ModelRoute and ModelRoute Reference](../reference/crd/networking.serving.volcano.sh.md) pages.

The router supports various routing strategies, from simple model-based forwarding to complex weighted distribution, header-based routing, and PD-Disaggregated (Prefill/Decode) routing. This flexibility allows for advanced deployment patterns including canary releases, A/B testing, load balancing across heterogeneous model deployments, and PD-Disaggregated inference for lower latency and better resource utilization.

## Preparation

Before diving into the routing configurations, let's set up the environment and understand the prerequisites. All the configuration examples in this document can be found in the [examples/kthena-router](https://github.com/volcano-sh/kthena/tree/main/examples/kthena-router) directory of the Kthena repository.

### Environment Setup

To simplify deployment and reduce the requirements for demonstration environments, we use a **mock LLM server** instead of deploying real models. This mock server implements the vLLM standard interface and returns mock data, making it perfect for testing and learning purposes.

### Prerequisites

- Kubernetes cluster with Kthena installed
- Access to the Kthena examples repository
- Basic understanding of router CRDs (ModelServer and ModelRoute)

### Getting Started

1. Deploy mock LLM inference engine if you do not have a real GPU/NPU environment at the moment. [mock deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/LLM-Mock-ds1.5b.yaml) and [mock deepseek-ai/DeepSeek-R1-Distill-Qwen-7B](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/LLM-Mock-ds7b.yaml)

2. Deploy all kinds of ModelServer, such as `deepseek-r1-1-5b`, `deepseek-r1-7b`, etc., as backends of different routing strategies.

3. All routing examples in this guide use these mock services, so you can experiment with different routing strategies without the overhead of real model deployment.

## Routing Scenarios

### 1. Simple Model-Based Routing

**Scenario**: Direct all requests for a specific model to a single backend service.

**Traffic Processing**: When a request comes in for model "deepseek-r1", the router matches this criterion and forwards all traffic to the 1.5B ModelServer. This is the most straightforward routing pattern.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-simple
  namespace: default
spec:
  modelName: "deepseek-simple"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"
```

**Flow Description**: 
1. Request arrives for model name "deepseek-r1"
2. Router matches the modelName field in the ModelRoute
3. 100% of traffic is directed to `deepseek-r1-1-5b`
4. The ModelServer serves requests using vLLM inference engine with 10s timeout

**Try it out**:
```bash
export MODEL="deepseek-simple"

curl http://$ROUTER_IP/v1/completions \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"prompt\": \"San Francisco is a\",
        \"temperature\": 0
    }"
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B!"}],"created":1756366365,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":218,"prompt_tokens":1,"time":0.0,"total_tokens":219}}
```


### 2. LoRA-Aware Routing

**Scenario**: Route requests requiring specific LoRA adapters to specialized ModelServers optimized for LoRA workloads.

**Traffic Processing**: When a request specifies LoRA adapters (lora-A or lora-B), the router routes it to ModelServers configured to handle these specific adapters.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-lora
  namespace: default
spec:
  loraAdapters:
  - "lora-A"
  - "lora-B"
  rules:
  - name: "lora-route"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"
```

**Flow Description**:
1. Request arrives with LoRA adapter requirement (`lora-A` or `lora-B`)
2. Router matches the LoRA adapter against the supported list
3. Routes to `deepseek-r1-1-5b` ModelServer configured for LoRA workloads
4. ModelServer efficiently handles LoRA adapter loading and inference

**Try it out**:
```bash
export MODEL="lora-A"

curl http://$ROUTER_IP/v1/completions \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"prompt\": \"San Francisco is a\",
        \"temperature\": 0
    }"
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from lora-A!"}],"created":1756366636,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"lora-A","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":120,"prompt_tokens":1,"time":0.0,"total_tokens":121}}
```


### 3. Weight-Based Traffic Distribution

**Scenario**: Gradually roll out new model versions by splitting traffic between different versions using weighted distribution.

**Traffic Processing**: The router uses weighted round-robin to distribute requests. For every 100 requests, approximately 70 will go to version 1 and 30 to version 2. This allows safe validation of new model versions with controlled risk.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-subset
  namespace: default
spec:
  modelName: "deepseek-subset"
  rules:
  - name: "deepseek-r1-route"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b-v1"
      weight: 70
    - modelServerName: "deepseek-r1-1-5b-v2"
      weight: 30
```

**Flow Description**:
1. Request arrives for model "deepseek-r1"
2. Router applies weighted distribution algorithm
3. 70% of requests → `deepseek-r1-1-5b-v1` (stable version)
4. 30% of requests → `deepseek-r1-1-5b-v2` (new version being tested)
5. This enables controlled testing of new model versions

**NOTE**: This scenario need to deploy canary version of [ModelServer](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelServer-ds1.5b-Canary.yaml) and [mock deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/LLM-Mock-ds1.5b-Canary.yaml) to test.

**Try it out**:
```bash
export MODEL="deepseek-subset"

for i in $(seq 1 100);
do
    curl http://$ROUTER_IP/v1/completions \
        -H "Content-Type: application/json" \
        -d "{
            \"model\": \"$MODEL\",
            \"prompt\": \"San Francisco is a\",
            \"temperature\": 0
        }";
done
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":313,"prompt_tokens":1,"time":0.0,"total_tokens":314}}
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":313,"prompt_tokens":1,"time":0.0,"total_tokens":314}}
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v1!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v1","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":47,"prompt_tokens":1,"time":0.0,"total_tokens":48}}
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v1!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v1","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":118,"prompt_tokens":1,"time":0.0,"total_tokens":119}}
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":285,"prompt_tokens":1,"time":0.0,"total_tokens":286}}
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v2","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":409,"prompt_tokens":1,"time":0.0,"total_tokens":410}}
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v1!"}],"created":1756371124,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B-v1","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":343,"prompt_tokens":1,"time":0.0,"total_tokens":344}}
...
```


### 4. Header-Based Multi-Model Routing

**Scenario**: Route traffic to different model sizes based on user tier, enabling premium users to access more powerful models.

**Traffic Processing**: The router evaluates incoming requests in the order rules are defined. Premium users (identified by `user-type: premium` header) are routed to the 7B model, while regular users fall back to the 1.5B model.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-multi-models
  namespace: default
spec:
  modelName: "deepseek-multi-models"
  rules:
  - name: "premium"
    modelMatch:
      headers:
        user-type:
          exact: premium
    targetModels:
    - modelServerName: "deepseek-r1-7b"
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b"
```

**Flow Description**:
1. Request arrives for model "deepseek-r1" with headers
2. Router first checks if `user-type: premium` header exists with exact match
3. If premium header found → Routes to `deepseek-r1-7b` (7B model using SGLang)
4. If no premium header → Falls back to `deepseek-r1-1-5b` (1.5B model using vLLM)
5. Premium users get access to the more powerful 7B model for better performance

**Try it out**:
```bash
export MODEL="deepseek-multi-models"

curl http://$ROUTER_IP/v1/completions \
    -H "Content-Type: application/json" \
    -H "user-type: premium" \
    -d "{
        \"model\": \"$MODEL\",
        \"prompt\": \"San Francisco is a\",
        \"temperature\": 0
    }"
{"choices":[{"finish_reason":"length","index":0,"logprobs":null,"text":"This is simulated message from deepseek-ai/DeepSeek-R1-Distill-Qwen-7B!"}],"created":1756367891,"id":"cmpl-uqkvlQyYK7bGYrRHQ0eXlWi7","model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-7B","object":"text_completion","system_fingerprint":"fp_44709d6fcb","usage":{"completion_tokens":71,"prompt_tokens":1,"time":0.0,"total_tokens":72}}
```

### 5. PD-Disaggregated Routing

**Scenario**: Route inference requests to Prefill/Decode disaggregated backends, where prefill instances handle prefill and decode instances handle decode stages independently.

**Traffic Processing**: When a request arrives for a PD-Disaggregated model, the router selects prefill and decode pods via `pdGroup`, proxies prefill to the prefill instance and decode to the decode instance, coordinating KV state exchange between them.

**ModelServer** (with `pdGroup`):

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: deepseek-r1-1-5b-pd-disaggregation
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      app: deepseek-r1-1-5b
    pdGroup:
      groupKey: "modelserving.volcano.sh/group-name"
      prefillLabels:
        modelserving.volcano.sh/rolename: "P-instance"
      decodeLabels:
        modelserving.volcano.sh/rolename: "D-instance"
  workloadPort:
    port: 8000
  model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B"
  inferenceEngine: "vLLM"
  trafficPolicy:
    timeout: 10s
```

**How `workloadSelector` works**

Two layers: **matchLabels** (general) selects which pods belong to this ModelServer; **pdGroup** (PD-only) then groups and roles them.

- **matchLabels**: Pods must match all key-value pairs. Used by every ModelServer.
- **pdGroup** (only for PD-Disaggregated):
  - **groupKey** — Label key whose value defines a PD group. Prefill and decode pods with the same value are paired (shared KV state).
  - **prefillLabels** / **decodeLabels** — Among those pods, match these labels to classify as prefill or decode.

The router sends prefill and decode for a request to a paired prefill pod and decode pod (same `groupKey` value).

**ModelRoute**:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: deepseek-r1-1-5b-pd-disaggregation
  namespace: default
spec:
  modelName: "deepseek-r1-1-5b-pd-disaggregation"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "deepseek-r1-1-5b-pd-disaggregation"
```

**Flow Description**:
1. Request arrives for the PD-Disaggregated model
2. Router matches the ModelRoute and resolves the target ModelServer
3. Via `pdGroup`, router selects a prefill-pod and a decode-pod (same `groupKey` value)
4. Prefill runs on prefill instance; decode runs on decode instance, with KV state exchanged between them (configure `kvConnector` for nixl/mooncake when needed)
5. Response returned to client

**NOTE**: Deploy [ModelServing](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelServing-ds1.5b-pd-disaggregation.yaml) with PD roles first, then apply [ModelServer](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelServer-ds1.5b-pd-disaggregation.yaml) and [ModelRoute](https://github.com/volcano-sh/kthena/blob/main/examples/kthena-router/ModelRoute-ds1.5b-pd-disaggregation.yaml).

---

This comprehensive routing system enables flexible, scalable, and maintainable model serving infrastructure that can adapt to various deployment patterns and user requirements.
