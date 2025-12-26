# Model Deployment

## ModelBooster vs ModelServing Deployment Approaches

Kthena provides two approaches for deploying LLM inference workloads: the **ModelBooster approach** and the **ModelServing approach**. This section compares both approaches to help you choose the right one for your use case.

### Deployment Approach Comparison

| Deployment Method | Manually Created CRDs                 | Automatically Managed Components        | Use Case                                     |
|-------------------|---------------------------------------|-----------------------------------------|----------------------------------------------|
| **ModelBooster**  | ModelBooster                          | ModelServer, ModelRoute, Pod Management | Simplified deployment, automated management  |
| **ModelServing**  | ModelServing, ModelServer, ModelRoute | Pod Management                          | Fine-grained control, complex configurations |

### ModelBooster Approach

**Advantages:**

- Simplified configuration with built-in disaggregation support optimized for NPUs
- Automatic KV cache transfer configuration using NPU-optimized protocols
- Integrated support for Huawei Ascend NPUs with automatic resource allocation
- Streamlined deployment process with NPU-specific optimizations
- Built-in HCCL (Huawei Collective Communication Library) configuration

**Automatically Managed Components:**

- ✅ ModelServer (automatically created and managed with NPU awareness)
- ✅ ModelRoute (automatically created and managed)
- ✅ Inter-service communication configuration (HCCL-optimized)
- ✅ Load balancing and routing for NPU workloads
- ✅ NPU resource scheduling and allocation

**User Only Needs to Create:**

- ModelBooster CRD with NPU resource specifications

### ModelServing Approach

**Advantages:**

- Fine-grained control over NPU container configuration
- Support for init containers and complex volume mounts for NPU drivers
- Detailed environment variable configuration for Ascend NPU settings
- Flexible NPU resource allocation (`huawei.com/ascend-1980`)
- Custom HCCL network interface configuration

**Manually Created Components:**

- ❌ ModelServing CRD with NPU resource specifications
- ❌ ModelServer CRD with NPU-aware workload selection
- ❌ ModelRoute CRD for NPU service routing
- ❌ Manual inter-service communication configuration (HCCL settings)

**NPU-Specific Networking Components:**

- **ModelServer** - Manages inter-service communication and load balancing for NPU workloads
- **ModelRoute** - Provides request routing and traffic distribution to NPU services
- **Supported KV Connector Types** - nixl, mooncake (optimized for NPU communication)
- **HCCL Integration** - Huawei Collective Communication Library for NPU-to-NPU communication

### Selection Guidance

- **Recommended: Use ModelBooster Approach** - Suitable for most deployment scenarios, providing simple deployment and high automation with hardware optimization
- **Use ModelServing Approach** - Only when fine-grained control or special hardware-specific configurations are required

## Examples

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
            {"kv_connector": "MooncakeConnectorV1",
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
            {"kv_connector": "MooncakeConnectorV1",
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

You can find more examples of model booster CR [here](https://github.com/volcano-sh/kthena/tree/main/examples/model-booster), and model serving CR [here](https://github.com/volcano-sh/kthena/tree/main/examples/model-serving).

## Advanced features

### Gang Scheduling

`GangPolicy` is enabled by default, we may make it optional in future release.
