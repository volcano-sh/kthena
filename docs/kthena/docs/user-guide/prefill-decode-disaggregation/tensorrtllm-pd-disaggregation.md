# TensorRT-LLM Prefill-Decode Disaggregation (GPU)

This page describes how to deploy prefill-decode disaggregated inference using [TensorRT-LLM](https://github.com/NVIDIA/TensorRT-LLM) with Kthena on a GPU cluster.

## Prerequisites

- Kubernetes cluster with Kthena installed
- NVIDIA GPU-enabled nodes with the appropriate device plugin configured (at least 2 GPUs for one prefill + one decode)
- Access to the NGC TensorRT-LLM image (`nvcr.io/nvidia/tensorrt-llm/release:1.0.0`)
- Access to pull the model weights used below (e.g. `TinyLlama/TinyLlama-1.1B-Chat-v1.0`)

## Deployment

When using the ModelServing approach, you need to manually create the following resources in order:

1. **ModelServing** — Manages Pods for prefill and decode roles
2. **ModelServer** — Manages the networking layer and PD-group routing
3. **ModelRoute** — Provides request routing to the ModelServer

### 1. ModelServing Configuration

The `ModelServing` resource defines two roles: `prefill` and `decode`. Each role runs `trtllm-serve` with `cache_transceiver_config.backend: NIXL`. Prefill also sets `disable_overlap_scheduler: true`, which TensorRT-LLM requires for disaggregated context.

:::note
Omit `kvConnector` on the ModelServer. With `inferenceEngine: TensorRTLLM` and a `pdGroup`, the router selects the native TensorRT-LLM PD connector automatically. Do not set `kvConnector.type: nixl` (that path is for vLLM).
:::

```sh
kubectl apply -f - <<'EOF'
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: trtllm-tinyllama-pd
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
                  - TinyLlama/TinyLlama-1.1B-Chat-v1.0
                  - --output-dir
                  - /models/TinyLlama-1.1B-Chat-v1.0/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: prefill
                image: nvcr.io/nvidia/tensorrt-llm/release:1.0.0
                command: ["sh", "-c"]
                args:
                  - |
                    cat >/tmp/extra_llm_api_options.yaml <<'YAML'
                    disable_overlap_scheduler: true
                    enable_iter_perf_stats: true
                    cache_transceiver_config:
                      backend: NIXL
                    YAML
                    trtllm-serve serve /models/TinyLlama-1.1B-Chat-v1.0 \
                      --host 0.0.0.0 \
                      --port 8000 \
                      --backend pytorch \
                      --extra_llm_api_options /tmp/extra_llm_api_options.yaml
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
                  initialDelaySeconds: 30
                  periodSeconds: 10
                  timeoutSeconds: 10
                  failureThreshold: 60
                  httpGet:
                    path: /health
                    port: 8000
                livenessProbe:
                  initialDelaySeconds: 900
                  periodSeconds: 10
                  timeoutSeconds: 10
                  failureThreshold: 10
                  httpGet:
                    path: /health
                    port: 8000
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  medium: Memory
                  sizeLimit: 8Gi
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
                  - TinyLlama/TinyLlama-1.1B-Chat-v1.0
                  - --output-dir
                  - /models/TinyLlama-1.1B-Chat-v1.0/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: decode
                image: nvcr.io/nvidia/tensorrt-llm/release:1.0.0
                command: ["sh", "-c"]
                args:
                  - |
                    cat >/tmp/extra_llm_api_options.yaml <<'YAML'
                    enable_iter_perf_stats: true
                    cache_transceiver_config:
                      backend: NIXL
                    YAML
                    trtllm-serve serve /models/TinyLlama-1.1B-Chat-v1.0 \
                      --host 0.0.0.0 \
                      --port 8000 \
                      --backend pytorch \
                      --extra_llm_api_options /tmp/extra_llm_api_options.yaml
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
                  initialDelaySeconds: 30
                  periodSeconds: 10
                  timeoutSeconds: 10
                  failureThreshold: 60
                  httpGet:
                    path: /health
                    port: 8000
                livenessProbe:
                  initialDelaySeconds: 900
                  periodSeconds: 10
                  timeoutSeconds: 10
                  failureThreshold: 10
                  httpGet:
                    path: /health
                    port: 8000
            volumes:
              - name: models
                emptyDir: {}
              - name: shared-mem
                emptyDir:
                  medium: Memory
                  sizeLimit: 8Gi
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
  name: trtllm-tinyllama-pd
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      modelserving.volcano.sh/name: trtllm-tinyllama-pd
    pdGroup:
      groupKey: "modelserving.volcano.sh/group-name"
      prefillLabels:
        modelserving.volcano.sh/role: prefill
      decodeLabels:
        modelserving.volcano.sh/role: decode
  workloadPort:
    port: 8000
  model: "TinyLlama-1.1B-Chat-v1.0"
  inferenceEngine: "TensorRTLLM"
  trafficPolicy:
    timeout: 120s
EOF
```

### 3. ModelRoute Configuration

The `ModelRoute` resource routes incoming requests by model name to the `ModelServer` created above.

```sh
kubectl apply -f - <<'EOF'
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: trtllm-tinyllama-pd
  namespace: default
spec:
  modelName: "TinyLlama-1.1B-Chat-v1.0"
  rules:
    - name: "default"
      targetModels:
        - modelServerName: "trtllm-tinyllama-pd"
EOF
```

## Verification

### Check Pod Status

After applying the resources, verify that both prefill and decode pods are running:

```sh
kubectl get pod -owide -l modelserving.volcano.sh/name=trtllm-tinyllama-pd
```

Expected output:

```
NAME                                 READY   STATUS    RESTARTS   AGE   IP         NODE     NOMINATED NODE   READINESS GATES
trtllm-tinyllama-pd-0-decode-0-0     1/1     Running   0          5m    <pod-ip>   <node>   <none>           <none>
trtllm-tinyllama-pd-0-prefill-0-0    1/1     Running   0          5m    <pod-ip>   <node>   <none>           <none>
```

### Send a Test Request

Once the pods are ready, send a chat completion request through the Kthena router:

```sh
curl -v http://<ROUTER_IP>:80/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"TinyLlama-1.1B-Chat-v1.0","messages":[{"role":"user","content":"Hello"}],"max_tokens":32}'
```

Expected response:

```json
{
  "id": "...",
  "object": "chat.completion",
  "created": 0,
  "model": "TinyLlama-1.1B-Chat-v1.0",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 20,
    "completion_tokens": 10,
    "total_tokens": 30
  }
}
```
