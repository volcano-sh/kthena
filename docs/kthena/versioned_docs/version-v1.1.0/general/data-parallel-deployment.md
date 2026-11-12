import DataParallelDeployment from '../assets/examples/model-serving/data-parallel-deployment.yaml?raw';
import CodeBlock from '@theme/CodeBlock';

# Data Parallel Deployment

Data parallelism is a technique for scaling LLM serving by deploying multiple replicas of the same model. Unlike model
parallelism (which splits a single model across multiple GPUs to fit large models), data parallelism focuses on
increasing throughput by distributing incoming requests across multiple independent model instances.

:::warning ModelBooster examples are deprecated
ModelBooster is deprecated in v1.1; use ModelServing, ModelServer, and ModelRoute. Removal is no earlier than v1.5. For new deployments, follow the [ModelServing quick start](../getting-started/quick-start.md#modelserving). The internal-load-balancing examples below are retained for existing users; see the [deprecation details](../user-guide/model-deployment.md#modelbooster-deprecation).
:::

This guide describes data parallelism for the **Qwen3-0.6B** model with **vLLM** as the inference backend and demonstrates two different load balancing strategies:

1. **Internal Load Balancing**: Distribute requests across workers.
2. **External Load Balancing**: Relies on external components (like Kubernetes Services or Ingress) to route traffic to
   independent replicas.

## Internal Load Balancing

Internal load balancing serves as the default mode where the coordination implementation (e.g., Ray) manages the
distribution of requests to the available workers. This is suitable for scenarios where you want a unified endpoint that
internally manages its worker pool.

The following legacy ModelBooster examples are retained for existing deployments. For new deployments, configure
the workload through ModelServing and manage ModelServer and ModelRoute directly.

### For Single Node

Legacy ModelBooster example for a single 2-GPU machine:

```yaml
# ModelBooster is deprecated in v1.1; use ModelServing, ModelServer, and ModelRoute.
# Removal is no earlier than v1.5.
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelBooster
metadata:
  annotations:
    api.kubernetes.io/name: "example"
  name: "my-model"
spec:
  name: "my-model"
  owner: "example"
  backend:
    name: "example"
    type: "vLLM"
    modelURI: "hf://Qwen/Qwen3-0.6B"
    cacheURI: "hostpath://tmp/cache"
    minReplicas: 1
    maxReplicas: 1
    workers:
      - type: "server"
        image: "vllm/vllm-openai:v0.13.0"
        replicas: 1
        pods: 1
        config:
          served-model-name: "my-model"
          tensor-parallel-size: 1   # TP=1
          data-parallel-size: 2     # DP=2
          enforce-eager: ""
          kv-cache-dtype: auto
          gpu-memory-utilization: 0.95
          max-num-seqs: 32
          max-model-len: 2048
        resources:
          limits:
            nvidia.com/gpu: "2"
```

### For Multiple Nodes

When deploying across multiple nodes, we typically rely on a distributed framework like Ray. This allows the model
serving engine to scale horizontally beyond a single machine's capacity.

This legacy ModelBooster example deploys on 2 nodes with 2 GPUs each. The `pods: 2` configuration ensures we have distributed
workers, and `data-parallel-backend: "ray"` enables the coordination.

```yaml
# ModelBooster is deprecated in v1.1; use ModelServing, ModelServer, and ModelRoute.
# Removal is no earlier than v1.5.
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelBooster
metadata:
  annotations:
    api.kubernetes.io/name: "example"
  name: "my-model"
spec:
  name: "my-model"
  owner: "example"
  backend:
    name: "example"
    type: "vLLM"
    modelURI: "hf://Qwen/Qwen3-0.6B"
    cacheURI: "hostpath://tmp/cache"
    minReplicas: 1
    maxReplicas: 1
    workers:
      - type: "server"
        image: "vllm/vllm-openai:v0.13.0"
        replicas: 1
        pods: 2 # every node would have 1 pod, so total 2 pods
        config:
          served-model-name: "my-model"
          data-parallel-size: 4 # 4 GPUs in total
          data-parallel-size-local: 2 # 2 GPUs per node
          data-parallel-backend: "ray"  # we use ray
          enforce-eager: ""
          gpu-memory-utilization: 0.9
          max-num-seqs: 16
          max-model-len: 2048
          api-server-count: 2 # 2 ranks per node
        resources:
          limits:
            nvidia.com/gpu: "2"
```

## External Load Balancing

In scenarios where you want to deploy multiple independent replicas of the model, each with its own endpoint, external
load balancing is the preferred approach. Use `ModelServing` directly for new deployments; the deprecated ModelBooster
API does not support this mode. The following ModelServing example deploys 2 pods (each with 1 GPU) for the
**Qwen3-0.6B** model. Configure `ModelServer` and `ModelRoute` separately when routing through Kthena Router, as
shown in the [quick start](../getting-started/quick-start.md#modelserving).

<CodeBlock language="yaml" showLineNumbers>
    {DataParallelDeployment}
</CodeBlock>

The key point is the configuration `--data-parallel-address`, pod IP or node IP cannot be used here because pod IP is
not stable and node IP is invisible inside pod. You should use the internal DNS address of the pod to ensure proper
communication between replicas.
