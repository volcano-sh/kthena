# Model Deployment

## ModelBooster vs ModelServing Deployment Approaches

Kthena provides two approaches for deploying LLM inference workloads: the **ModelBooster approach** and the **ModelServing approach**. This section compares both approaches to help you choose the right one for your use case.

### Deployment Approach Comparison

| Deployment Method | Manually Created CRDs                 | Automatically Managed Components      | Use Case                                     |
|-------------------|---------------------------------------|---------------------------------------|----------------------------------------------|
| **ModelBooster**  | ModelBooster                          | ModelServing, ModelServer, ModelRoute | Simplified deployment, automated management  |
| **ModelServing**  | ModelServing, ModelServer, ModelRoute | Pod Management                        | Fine-grained control, complex configurations |

### ModelBooster Approach

**Advantages:**

- Simplified configuration with built-in disaggregation support optimized for AI accelerators
- Automatic KV cache transfer configuration using accelerator-optimized protocols
- Integrated support for major accelerators (e.g., NVIDIA GPUs, Huawei Ascend NPUs) with automatic resource allocation
- Streamlined deployment process with hardware-specific optimizations
- Built-in communication backend configuration (e.g., NCCL, HCCL)

**Automatically Managed Components:**

- ModelServing (automatically created and managed for workload orchestration)
- ModelServer (automatically created and managed with hardware awareness)
- ModelRoute (automatically created and managed)
- AutoscalingPolicy and Binding (automatically created when `autoscalingPolicy` is defined in ModelBooster spec)
- Inter-service communication configuration (backend-optimized)
- Load balancing and routing for AI workloads
- Accelerator resource scheduling and allocation

**User Only Needs to Create:**

- ModelBooster CRD with accelerator resource specifications

### ModelServing Approach

**Advantages:**

- Fine-grained control over accelerator container configuration
- Support for init containers and complex volume mounts for device drivers
- Detailed environment variable configuration for hardware-specific settings
- Flexible accelerator resource allocation (e.g., `nvidia.com/gpu`, `huawei.com/ascend-1980`)
- Custom network interface configuration for communication backends

**Manually Created Components:**

- ModelServing CRD with accelerator resource specifications
- ModelServer CRD with hardware-aware workload selection
- ModelRoute CRD for AI service routing
- AutoscalingPolicy and Binding CRDs (if autoscaling is required)
- Manual inter-service communication configuration (e.g., NCCL/HCCL settings)

**Networking Components:**

- **ModelServer** - Manages inter-service communication and load balancing for AI workloads
- **ModelRoute** - Provides request routing and traffic distribution to AI services
- **Supported KV Connector Types** - nixl, mooncake, lmcache (optimized for accelerator communication)
- **Communication Backend Integration** - Libraries (e.g., NCCL, HCCL) for accelerator-to-accelerator communication

### Selection Guidance

- **Recommended: Use ModelBooster Approach** - Suitable for most deployment scenarios, providing simple deployment and high automation with hardware optimization
- **Use ModelServing Approach** - Only when fine-grained control or special hardware-specific configurations are required

## Model Booster Examples

Below are examples of ModelBooster configurations for different deployment scenarios.

### Aggregated Deployment

This example shows a standard aggregated deployment where prefill and decode phases run on the same instance.

<details>
<summary>
<b>Qwen2.5-Coder-32B-Instruct.yaml</b>
</summary>

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelBooster
metadata:
  annotations:
    api.kubernetes.io/name: example
  name: qwen25
spec:
  name: qwen25-coder-32b
  owner: example
  backend:
    name: "qwen25-coder-32b-server"
    type: "vLLM"
    modelURI: s3://kthena/Qwen/Qwen2.5-Coder-32B-Instruct
    cacheURI: hostpath://cache/
    envFrom:
      - secretRef:
          name: your-secrets
    env:
      - name: "RUNTIME_PORT"  # default 8100
        value: "8200"
      - name: "RUNTIME_URL"   # default http://localhost:8000/metrics
        value: "http://localhost:8100/metrics"
    minReplicas: 1
    maxReplicas: 2
    workers:
      - type: server
        image: openeuler/vllm-ascend:latest
        replicas: 1
        pods: 1
        resources:
          limits:
            cpu: "8"
            memory: 96Gi
            huawei.com/ascend-1980: "2"
          requests:
            cpu: "1"
            memory: 96Gi
            huawei.com/ascend-1980: "2"
```

</details>

### Disaggregated Deployment

This example demonstrates a disaggregated deployment where prefill and decode phases are separated into different worker pools, optimized for performance.

<details>
<summary>
<b>prefill-decode-disaggregation.yaml</b>
</summary>

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelBooster
metadata:
  name: deepseek-v2-lite
  namespace: dev
spec:
  name: deepseek-v2-lite
  owner: example
  backend:
    name: deepseek-v2-lite
    type: vLLMDisaggregated
    modelURI: hf://deepseek-ai/DeepSeek-V2-Lite
    cacheURI: hostpath://mnt/cache/
    minReplicas: 1
    maxReplicas: 1
    workers:
      - type: prefill
        image: ghcr.io/volcano-sh/kthena-engine:vllm-ascend_v0.10.1rc1_mooncake_v0.3.5
        replicas: 1
        pods: 1
        resources:
          limits:
            cpu: "8"
            memory: 64Gi
            huawei.com/ascend-1980: "4"
          requests:
            cpu: "8"
            memory: 64Gi
            huawei.com/ascend-1980: "4"
        config:
          served-model-name: "deepseek-ai/DeepSeekV2"
          tensor-parallel-size: 2
          max-model-len: 8192
          gpu-memory-utilization: 0.8
          max-num-batched-tokens: 8192
          trust-remote-code: ""
          enforce-eager: ""
          kv-transfer-config: |
            {"kv_connector": "MooncakeConnector",
              "kv_buffer_device": "npu",
              "kv_role": "kv_producer",
              "kv_parallel_size": 1,
              "kv_port": "20001",
              "engine_id": "0",
              "kv_rank": 0,
              "kv_connector_module_path": "vllm_ascend.distributed.mooncake_connector",
              "kv_connector_extra_config": {
                "prefill": {
                  "dp_size": 2,
                  "tp_size": 2
                },
                "decode": {
                  "dp_size": 2,
                  "tp_size": 2
                }
              }
            }
      - type: decode
        image: ghcr.io/volcano-sh/kthena-engine:vllm-ascend_v0.10.1rc1_mooncake_v0.3.5
        replicas: 1
        pods: 1
        resources:
          limits:
            cpu: "8"
            memory: 64Gi
            huawei.com/ascend-1980: "4"
          requests:
            cpu: "8"
            memory: 64Gi
            huawei.com/ascend-1980: "4"
        config:
          served-model-name: "deepseek-ai/DeepSeekV2"
          tensor-parallel-size: 2
          max-model-len: 8192
          gpu-memory-utilization: 0.8
          max-num-batched-tokens: 16384
          trust-remote-code: ""
          enforce-eager: ""
          kv-transfer-config: |
            {"kv_connector": "MooncakeConnector",
              "kv_buffer_device": "npu",
              "kv_role": "kv_consumer",
              "kv_parallel_size": 1,
              "kv_port": "20002",
              "engine_id": "1",
              "kv_rank": 1,
              "kv_connector_module_path": "vllm_ascend.distributed.mooncake_connector",
              "kv_connector_extra_config": {
                "prefill": {
                  "dp_size": 2,
                  "tp_size": 2
                },
                "decode": {
                  "dp_size": 2,
                  "tp_size": 2
                }
              }
            }
```

</details>

## Model Serving Examples

Below are examples of ModelServing configurations for different deployment scenarios.

### GPU PD Disaggregation

This example demonstrates a disaggregated deployment using NVIDIA GPUs with prefill and decode roles.

<details>
<summary>
<b>gpu-pd-disaggregation.yaml</b>
</summary>

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: PD-sample
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
                  - Qwen/Qwen3-8B
                  - --output-dir
                  - /models/Qwen3-8B/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: prefill
                image: kvcache-container-image-hb2-cn-beijing.cr.volces.com/aibrix/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2
                command: [ "sh", "-c" ]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-8B \
                    --served-model-name qwen3-8B \
                    --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_both"}'
                env:
                  - name: PYTHONHASHSEED
                    value: "1047"
                  - name: VLLM_SERVER_DEV_MODE
                    value: "1"
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
                  - name: NCCL_DEBUG
                    value: "INFO"
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
                emptyDir: { }
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
                  - Qwen/Qwen3-8B
                  - --output-dir
                  - /models/Qwen3-8B/
                volumeMounts:
                  - name: models
                    mountPath: /models
            containers:
              - name: decode
                image: kvcache-container-image-hb2-cn-beijing.cr.volces.com/aibrix/vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2
                command: [ "sh", "-c" ]
                args:
                  - |
                    python3 -m vllm.entrypoints.openai.api_server \
                    --host "0.0.0.0" \
                    --port "8000" \
                    --uvicorn-log-level warning \
                    --model /models/Qwen3-8B \
                    --served-model-name qwen3-8B \
                    --kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_both"}'
                env:
                  - name: PYTHONHASHSEED
                    value: "1047"
                  - name: VLLM_SERVER_DEV_MODE
                    value: "1"
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
                  - name: NCCL_DEBUG
                    value: "INFO"
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
                emptyDir: { }
              - name: shared-mem
                emptyDir:
                  sizeLimit: 256Mi
                  medium: Memory
        workerReplicas: 0
```

</details>

You can find more examples of model booster CR [here](https://github.com/volcano-sh/kthena/tree/main/examples/model-booster), and model serving CR [here](https://github.com/volcano-sh/kthena/tree/main/examples/model-serving).

## Advanced features

### Gang Scheduling

`GangPolicy` is enabled by default, we may make it optional in future release.
