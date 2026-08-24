# SGLang Prefill-Decode Disaggregation (GPU)

This page describes how to deploy prefill-decode disaggregated inference using [SGLang](https://github.com/sgl-project/sglang) with Kthena on a GPU cluster.

## Prerequisites

- Kubernetes cluster with Kthena installed
- NVIDIA GPU-enabled nodes with the appropriate device plugin configured
- Access to the SGLang container image (`docker.io/lmsysorg/sglang:latest`)
- A Hugging Face token with access to the target model (e.g. `Qwen/Qwen3-0.6B`)

## Deployment

When using the ModelServing approach, you need to manually create the following resources in order:

1. **ModelServing** — Manages Pods for prefill and decode roles
2. **ModelServer** — Manages the networking layer and PD-group routing
3. **ModelRoute** — Provides request routing to the ModelServer

### 1. ModelServing Configuration

The `ModelServing` resource defines two roles: `prefill` and `decode`. Each role runs a separate SGLang server with the corresponding `--disaggregation-mode` flag, using Mooncake as the KV-transfer backend.

:::note
Replace `<YOUR_HF_TOKEN>` with your actual Hugging Face token before applying.
:::

```sh
kubectl apply -f - <<'EOF'
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: sglang-qwen-06b
  namespace: default
spec:
  schedulerName: volcano
  replicas: 1
  template:
    roles:
      - name: prefill
        replicas: 1
        entryTemplate:
          spec:
            containers:
              - name: sglang-prefill
                image: docker.io/lmsysorg/sglang:latest
                imagePullPolicy: IfNotPresent
                command: ["python3", "-m", "sglang.launch_server"]
                args:
                  - "--model-path"
                  - "Qwen/Qwen3-0.6B"
                  - "--host"
                  - "$(POD_IP)"
                  - "--port"
                  - "30000"
                  - "--disaggregation-mode"
                  - "prefill"
                  - "--disaggregation-transfer-backend"
                  - "mooncake"
                  - "--enable-metrics"
                env:
                  - name: POD_IP
                    valueFrom:
                      fieldRef:
                        fieldPath: status.podIP
                  - name: HF_TOKEN
                    value: <YOUR_HF_TOKEN>
                ports:
                  - containerPort: 30000
                    name: http
                resources:
                  limits:
                    nvidia.com/gpu: 1
                    cpu: "4"
                    memory: 12Gi
                  requests:
                    nvidia.com/gpu: 1
                    cpu: "2"
                    memory: 12Gi
                volumeMounts:
                  - name: shm
                    mountPath: /dev/shm
                  - name: hf-cache
                    mountPath: /root/.cache/huggingface
                readinessProbe:
                  httpGet:
                    path: /health
                    port: 30000
                  initialDelaySeconds: 120
                  periodSeconds: 15
                  timeoutSeconds: 10
                  failureThreshold: 5
            volumes:
              - name: shm
                emptyDir:
                  medium: Memory
                  sizeLimit: 8Gi
              - name: hf-cache
                hostPath:
                  path: /root/.cache/huggingface
                  type: DirectoryOrCreate
        workerReplicas: 0
      - name: decode
        replicas: 1
        entryTemplate:
          spec:
            containers:
              - name: sglang-decode
                image: docker.io/lmsysorg/sglang:latest
                imagePullPolicy: IfNotPresent
                command: ["python3", "-m", "sglang.launch_server"]
                args:
                  - "--model-path"
                  - "Qwen/Qwen3-0.6B"
                  - "--host"
                  - "$(POD_IP)"
                  - "--port"
                  - "30000"
                  - "--disaggregation-mode"
                  - "decode"
                  - "--disaggregation-transfer-backend"
                  - "mooncake"
                  - "--enable-metrics"
                env:
                  - name: POD_IP
                    valueFrom:
                      fieldRef:
                        fieldPath: status.podIP
                  - name: HF_TOKEN
                    value: <YOUR_HF_TOKEN>
                ports:
                  - containerPort: 30000
                    name: http
                resources:
                  limits:
                    nvidia.com/gpu: 1
                    cpu: "4"
                    memory: 12Gi
                  requests:
                    nvidia.com/gpu: 1
                    cpu: "2"
                    memory: 12Gi
                volumeMounts:
                  - name: shm
                    mountPath: /dev/shm
                  - name: hf-cache
                    mountPath: /root/.cache/huggingface
                readinessProbe:
                  httpGet:
                    path: /health
                    port: 30000
                  initialDelaySeconds: 120
                  periodSeconds: 15
                  timeoutSeconds: 10
                  failureThreshold: 5
            volumes:
              - name: shm
                emptyDir:
                  medium: Memory
                  sizeLimit: 8Gi
              - name: hf-cache
                hostPath:
                  path: /root/.cache/huggingface
                  type: DirectoryOrCreate
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
  name: sglang-qwen-06b
  namespace: default
spec:
  workloadSelector:
    matchLabels:
      modelserving.volcano.sh/name: sglang-qwen-06b
    pdGroup:
      groupKey: "modelserving.volcano.sh/group-name"
      prefillLabels:
        modelserving.volcano.sh/role: prefill
      decodeLabels:
        modelserving.volcano.sh/role: decode
  workloadPort:
    port: 30000
  model: "Qwen/Qwen3-0.6B"
  inferenceEngine: "SGLang"
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
  name: sglang-qwen-06b
  namespace: default
spec:
  modelName: "sglang-qwen-06b"
  rules:
    - name: "default"
      targetModels:
        - modelServerName: "sglang-qwen-06b"
EOF
```

## Verification

### Check Pod Status

After applying the resources, verify that both prefill and decode pods are running:

```sh
kubectl get pod -owide -l modelserving.volcano.sh/name=sglang-qwen-06b
```

Expected output:

```
NAME                              READY   STATUS    RESTARTS   AGE   IP         NODE     NOMINATED NODE   READINESS GATES
sglang-qwen-06b-0-decode-0-0      1/1     Running   0          5m    <pod-ip>   <node>   <none>           <none>
sglang-qwen-06b-0-prefill-0-0     1/1     Running   0          5m    <pod-ip>   <node>   <none>           <none>
```

### Send a Test Request

Once the pods are ready, send a chat completion request through the Kthena router:

```sh
curl -v http://<ROUTER_IP>:80/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"sglang-qwen-06b","messages":[{"role":"user","content":"Hello"}]}'
```

Expected response:

```json
{
  "id": "2db2ac09a603498283f61badd8ff0ba3",
  "object": "chat.completion",
  "created": 1776149893,
  "model": "Qwen/Qwen3-0.6B",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "<think>\nOkay, the user just said \"Hello\". I need to respond appropriately. Let me think. They might be testing if I can handle a greeting. Since I'm a language model, I should acknowledge their message. I can say \"Hello! How can I assist you today?\" That way, I'm polite and open to helping. I should make sure to keep the tone friendly and helpful. Let me check for any possible errors or issues. Everything looks good. Alright, time to send a friendly response.\n</think>\n\nHello! How can I assist you today?",
        "reasoning_content": null,
        "tool_calls": null
      },
      "logprobs": null,
      "finish_reason": "stop",
      "matched_stop": 151645
    }
  ],
  "usage": {
    "prompt_tokens": 9,
    "total_tokens": 125,
    "completion_tokens": 116,
    "prompt_tokens_details": null,
    "reasoning_tokens": 0
  },
  "metadata": {
    "weight_version": "default"
  }
}
```
