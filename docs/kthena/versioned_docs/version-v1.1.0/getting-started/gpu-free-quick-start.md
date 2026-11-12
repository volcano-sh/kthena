---
sidebar_position: 3
---

# GPU-Free Quick Start

Try out Kthena on a Kubernetes cluster **without GPUs or NPUs**! This guide deploys a mock inference backend that mimics the vLLM API, exposes it through a `ModelServer` and a `ModelRoute`, and sends a test inference request through the Kthena router.

## Prerequisites

- A CPU-only Kubernetes cluster, such as a local [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) cluster
- Kthena installed on your Kubernetes cluster (see [Installation](./installation.md))
- Access to a Kubernetes cluster with `kubectl` configured
- Pod in Kubernetes can access the internet
- [volcano](https://volcano.sh/en/docs/installation/) is installed.

## Step 1: Deploy the Mock Inference Backend

The repository ships a mock backend that emulates a vLLM server: it exposes the same OpenAI-compatible HTTP API and metrics endpoint, but returns simulated responses instead of running a real model — no GPU, no model weights.

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/refs/heads/main/examples/kthena-router/LLM-Mock-ds1.5b.yaml
```

Wait for the mock Pods to become ready:

```bash
kubectl wait --for=condition=Ready pod -l app=deepseek-r1-1-5b --timeout=180s
kubectl get pods -l app=deepseek-r1-1-5b
```

Expected output:

```text
NAME                                READY   STATUS    RESTARTS   AGE
deepseek-r1-1-5b-xxxxxxxxx-xxxxx    1/1     Running   0          1m
deepseek-r1-1-5b-xxxxxxxxx-xxxxx    1/1     Running   0          1m
deepseek-r1-1-5b-xxxxxxxxx-xxxxx    1/1     Running   0          1m
```

## Step 2: Create the ModelServer and ModelRoute

A `ModelServer` tells the router which Pods serve a model (via a workload selector and port). A `ModelRoute` declares a routable model name and maps it to one or more `ModelServer` targets.

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/refs/heads/main/examples/kthena-router/ModelServer-ds1.5b.yaml
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/refs/heads/main/examples/kthena-router/ModelRouteSimple.yaml
```

Verify both resources exist:

```bash
kubectl get modelservers,modelroutes
```

Expected output:

```text
NAME                                                              AGE
modelserver.networking.serving.volcano.sh/deepseek-r1-1-5b        1m

NAME                                                              AGE
modelroute.networking.serving.volcano.sh/deepseek-simple          1m
```

## Step 3: Port-Forward the Kthena Router

The router Service is of type `LoadBalancer`. On clusters without a load-balancer implementation (such as a default Kind cluster) its external IP stays `<pending>`, so use a port-forward for local access:

```bash
kubectl port-forward -n kthena-system svc/kthena-router 8080:80
```

If port 8080 is already taken on your machine (for example, Podman occupies it by default), forward a different local port instead, such as `kubectl port-forward -n kthena-system svc/kthena-router 18080:80`, and use that port in the next step.

Keep this command running and continue in a second terminal.

## Step 4: Send a Test Inference Request

The `ModelRoute` above registers the model name `deepseek-simple`. Send an OpenAI-style completion request through the router (the current mock image requires `max_tokens` and `"stream": true`):

```bash
curl -N http://localhost:8080/v1/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "deepseek-simple",
        "prompt": "San Francisco is a",
        "max_tokens": 5,
        "temperature": 0,
        "stream": true
    }'
```

Expected response is a stream of SSE chunks with simulated tokens, ending in `[DONE]`:

```text
data: {"id":"cmpl-4282ab27-171f-4ccf-82a3-adbebc84151a","choices":[{"text":"En","index":0}],"created":1786508011,"model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B","system_fingerprint":null,"object":"text_completion","usage":null}

...

data: {"id":"cmpl-4282ab27-171f-4ccf-82a3-adbebc84151a","choices":[{"text":"","index":0,"finish_reason":"length"}],"created":1786508011,"model":"deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B","system_fingerprint":null,"object":"text_completion","usage":null,"nvext":{"timing":{"request_received_ms":1786508011068,"total_time_ms":54.605582999999996}}}

data: [DONE]
```

Note that the response reports the backend model name (`deepseek-ai/DeepSeek-R1-Distill-Qwen-1.5B`) even though the request used the routed name `deepseek-simple`: the router matched the `model` field against the `ModelRoute`, rewrote it to the `ModelServer`'s base model, picked a ready backend Pod from the `ModelServer`'s selector, and forwarded the request.
