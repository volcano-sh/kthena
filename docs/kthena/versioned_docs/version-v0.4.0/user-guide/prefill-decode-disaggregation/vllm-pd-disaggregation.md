# vLLM Prefill-Decode Disaggregation (GPU)

This page describes how to deploy prefill-decode disaggregated inference using [vLLM](https://github.com/vllm-project/vllm) with Kthena on a GPU cluster.

## Prerequisites

- Kubernetes cluster with Kthena installed
- NVIDIA GPU-enabled nodes with the appropriate device plugin configured
- Access to the vLLM container image (`ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2`)
- The target model (e.g. `Qwen/Qwen3-0.6B`) accessible from the cluster (the deployment uses a downloader init container to pull the model)

## Deployment

When using the ModelServing approach, you need to manually create the following resources in order:

1. **ModelServing** — Manages Pods for prefill and decode roles
2. **ModelServer** — Manages the networking layer and PD-group routing
3. **ModelRoute** — Provides request routing to the ModelServer

### 1. ModelServing Configuration

The `ModelServing` resource defines two roles: `prefill` and `decode`. Each role runs a separate vLLM server with the `NixlConnector` as the KV-transfer backend. A downloader init container fetches the model weights before the server starts.

```sh
kubectl apply -f - <<'EOF'
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: vllm-qwen-06b
  namespace: default
spec:
  schedulerName: volcano
  replicas: 1
  recoveryPolicy: ServingGroupRecreate
  template:
    restartGracePeriodSeconds: 60
    roles:
      - name: prefill
        replicas: 1
        entryTemplate:
          spec:
            initContainers:
              - name: downloader
                imagePullPolicy: IfNotPresent
                image: ghcr.io/volcano-sh/downloader:latest
                args:
                  - --source
                  - Qwen/Qwen3-0.6B
                  - --output-dir
                  - /models/Qwen3-0.6B/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: prefill
                image: ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2
                command: ["sh", "-c"]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_producer"}'
                env:
                  - name: PYTHONHASHSEED
                    value: "1047"
                  - name: VLLM_NIXL_SIDE_CHANNEL_HOST
                    value: "0.0.0.0"
                  - name: VLLM_NIXL_SIDE_CHANNEL_PORT
                    value: "5558"
                  - name: VLLM_WORKER_MULTIPROC_METHOD
                    value: spawn
                  - name: VLLM_ENABLE_V1_MULTIPROCESSING
                    value: "0"
                  - name: GLOO_SOCKET_IFNAME
                    value: eth0
                  - name: NCCL_SOCKET_IFNAME
                    value: eth0
                  - name: NCCL_IB_DISABLE
                    value: "0"
                  - name: NCCL_IB_GID_INDEX
                    value: "7"
                  - name: UCX_TLS
                    value: ^gga
                volumeMounts:
                  - name: models
                    mountPath: /models
                    readOnly: true
                  - name: shared-mem
                    mountPath: /dev/shm
                resources:
                  limits:
                    nvidia.com/gpu: 1
                securityContext:
                  capabilities:
                    add:
                      - IPC_LOCK
                readinessProbe:
                  initialDelaySeconds: 5
                  periodSeconds: 5
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
                livenessProbe:
                  initialDelaySeconds: 900
                  periodSeconds: 5
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
      - name: decode
        replicas: 1
        entryTemplate:
          spec:
            initContainers:
              - name: downloader
                imagePullPolicy: IfNotPresent
                image: ghcr.io/volcano-sh/downloader:latest
                args:
                  - --source
                  - Qwen/Qwen3-0.6B
                  - --output-dir
                  - /models/Qwen3-0.6B/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: decode
                image: ghcr.io/volcano-sh/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2
                command: ["sh", "-c"]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-0.6B \
                    --served-model-name Qwen/Qwen3-0.6B \
                    --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_consumer"}'
                env:
                  - name: PYTHONHASHSEED
                    value: "1047"
                  - name: VLLM_NIXL_SIDE_CHANNEL_HOST
                    value: "0.0.0.0"
                  - name: VLLM_NIXL_SIDE_CHANNEL_PORT
                    value: "5558"
                  - name: VLLM_WORKER_MULTIPROC_METHOD
                    value: spawn
                  - name: VLLM_ENABLE_V1_MULTIPROCESSING
                    value: "0"
                  - name: GLOO_SOCKET_IFNAME
                    value: eth0
                  - name: NCCL_SOCKET_IFNAME
                    value: eth0
                  - name: NCCL_IB_DISABLE
                    value: "0"
                  - name: NCCL_IB_GID_INDEX
                    value: "7"
                  - name: UCX_TLS
                    value: ^gga
                volumeMounts:
                  - name: models
                    mountPath: /models
                    readOnly: true
                  - name: shared-mem
                    mountPath: /dev/shm
                resources:
                  limits:
                    nvidia.com/gpu: 1
                securityContext:
                  capabilities:
                    add:
                      - IPC_LOCK
                readinessProbe:
                  initialDelaySeconds: 5
                  periodSeconds: 5
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
                livenessProbe:
                  initialDelaySeconds: 900
                  periodSeconds: 5
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
EOF
```

### 2. ModelServer Configuration

The `ModelServer` resource configures the networking layer. The `pdGroup` field tells Kthena how to identify prefill and decode pods so that it can perform PD-aware routing.

```sh
kubectl apply -f - <<'EOF'
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: vllm-qwen-06b
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      modelserving.volcano.sh/name: vllm-qwen-06b
    pdGroup:
      groupKey: "modelserving.volcano.sh/group-name"
      prefillLabels:
        modelserving.volcano.sh/role: prefill
      decodeLabels:
        modelserving.volcano.sh/role: decode
  workloadPort:
    port: 8000
  model: "Qwen/Qwen3-0.6B"
  inferenceEngine: "vLLM"
  trafficPolicy:
    timeout: 10s
EOF
```

### 3. ModelRoute Configuration

The `ModelRoute` resource routes incoming requests by model name to the `ModelServer` created above.

```sh
kubectl apply -f - <<'EOF'
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: vllm-qwen-06b
  namespace: default
spec:
  modelName: "Qwen/Qwen3-0.6B"
  rules:
    - name: "default"
      targetModels:
        - modelServerName: "vllm-qwen-06b"
EOF
```

## Verification

### Check Pod Status

After applying the resources, verify that both prefill and decode pods are running:

```sh
kubectl get pod -owide -l modelserving.volcano.sh/name=vllm-qwen-06b
```

Expected output:

```
NAME                              READY   STATUS    RESTARTS   AGE   IP         NODE     NOMINATED NODE   READINESS GATES
vllm-qwen-06b-0-decode-0-0        1/1     Running   0          5m    <pod-ip>   <node>   <none>           <none>
vllm-qwen-06b-0-prefill-0-0       1/1     Running   0          5m    <pod-ip>   <node>   <none>           <none>
```

### Send a Test Request

Once the pods are ready, send a chat completion request through the Kthena router:

```sh
curl -v http://<ROUTER_IP>:80/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"Hello"}]}'
```

Expected response:

```json
{
  "id": "chatcmpl-0888d3ba-146b-4b8e-880d-6748e0a3c8dd",
  "object": "chat.completion",
  "created": 1776342232,
  "model": "Qwen/Qwen3-0.6B",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "<think>\nOkay, the user just said \"Hello\". I need to respond appropriately. Since they started the conversation with a greeting, my response should be friendly and welcoming. I should acknowledge their message and maybe add a cheerful line to keep the conversation going. I should check if there's anything else they need help with. Let me make sure the response is simple and positive.\n</think>\n\nHello! How can I assist you today? 😊",
        "refusal": null,
        "annotations": null,
        "audio": null,
        "function_call": null,
        "tool_calls": [],
        "reasoning_content": null
      },
      "logprobs": null,
      "finish_reason": "stop",
      "stop_reason": null
    }
  ],
  "service_tier": null,
  "system_fingerprint": null,
  "usage": {
    "prompt_tokens": 9,
    "total_tokens": 98,
    "completion_tokens": 89,
    "prompt_tokens_details": null
  },
  "prompt_logprobs": null,
  "kv_transfer_params": null
}
```
