# vLLM Ascend (Mooncake)

This page describes the prefill-decode disaggregation capabilities in Kthena, based on verified NPU deployment
examples and configurations using Huawei Ascend Neural Processing Units.

## Overview

### Disaggregation Components

The prefill-decode disaggregation architecture consists of several key components optimized for NPU deployments:

#### Prefill Service

- **Purpose:** Processes input tokens and generates initial key-value (KV) cache
- **Resource Requirements:** High compute throughput, NPU acceleration (Huawei Ascend NPUs)
- **Characteristics:** Batch-friendly, parallel processing optimized for NPU tensor operations
- **Node Affinity:** NPU-enabled nodes with Huawei Ascend processors and high memory bandwidth
- **NPU Optimization:** Leverages NPU's parallel processing capabilities for efficient token processing

#### Decode Service

- **Purpose:** Generates output tokens using KV cache from prefill phase
- **Resource Requirements:** Low latency, memory-intensive, NPU-optimized sequential processing
- **Characteristics:** Sequential processing, latency-sensitive, optimized for NPU memory architecture
- **Node Affinity:** NPU-enabled nodes with large memory capacity and Ascend NPU resources
- **NPU Optimization:** Utilizes NPU's memory hierarchy for efficient KV cache access and token generation

#### Communication Layer

- **Purpose:** Manages data transfer between prefill and decode services on NPU infrastructure
- **Implementation:** High-speed inter-service communication via NPU-optimized shared storage or network (HCCL)
- **NPU Optimization:** Leverages Huawei Collective Communication Library (HCCL) for efficient NPU-to-NPU communication
- **Network Configuration:** Utilizes NPU-specific network interfaces and protocols for optimal data transfer
- **Optimization:** Minimizes latency and maximizes throughput between NPU-enabled nodes

### Data Flow

1. **Input Processing:** Client requests are received by the prefill service
2. **Prefill Phase:** Input tokens are processed in parallel, generating KV cache
3. **Cache Transfer:** KV cache is transferred to decode service
4. **Decode Phase:** Output tokens are generated sequentially
5. **Response Delivery:** Generated text is returned to the client

## Prerequisites

- Kubernetes cluster with Kthena installed
- **NPU Hardware**: Huawei Ascend NPU-enabled nodes (Ascend 910 or compatible NPUs)
- **NPU Drivers**: Proper Ascend NPU drivers and runtime installed on cluster nodes
- **NPU Device Plugin**: Kubernetes device plugin for Huawei Ascend NPUs configured
- Access to the Kthena examples repository
- Basic understanding of ModelServing CRD
- Understanding of LLM inference patterns and NPU resource requirements
- Familiarity with NPU-specific configurations and resource allocation

## Getting Started

Deploy LLM inference engine with prefill-decode disaggregation using either the ModelBooster or ModelServing approach.
Both configurations separate prefill and decode workloads for optimal NPU resource utilization on Huawei Ascend hardware.

### ModelBooster Approach (Recommended)

The ModelBooster CRD provides a streamlined way to deploy disaggregated inference with built-in support for advanced
features like KV cache transfer and specialized NPU hardware configurations optimized for Huawei Ascend processors.

**Important Note:** When using the ModelBooster approach, ModelServer and ModelRoute are automatically created and
managed - users do not need to manually deploy these resources. The configuration is optimized for NPU resource allocation.

For a detailed comparison of the ModelBooster approach's advantages, automatically managed components, and when to use it, see the [ModelBooster Approach](../model-deployment.md#modelbooster-approach) section in the ModelBooster documentation.

Deploy the [ModelBooster configuration](../../assets/examples/model-booster/prefill-decode-disaggregation.yaml) for prefill-decode disaggregated inference:

```sh
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/model-booster/prefill-decode-disaggregation.yaml
```

This configuration includes:
- **Prefill Worker**: Handles input processing with NPU-optimized KV cache production
- **Decode Worker**: Manages output generation with NPU-optimized KV cache consumption
- **Automatic Resource Management**: NPU resource allocation (`huawei.com/ascend-1980: "4"`)
- **HCCL Integration**: Huawei Collective Communication Library for NPU-to-NPU communication
- **Mooncake Connector**: Optimized KV transfer mechanism for NPU deployments

#### Verifying ModelBooster Deployment

You can run the following command to check the ModelBooster status and pod status in the cluster:

```sh
kubectl get modelbooster deepseek-v2-lite -n dev -o yaml | grep status -A 10

status:
  availableReplicas: 1
  conditions:
  - lastTransitionTime: "2025-09-29T10:15:30Z"
    message: All workers are ready
    reason: AllWorkersReady
    status: "True"
    type: Available
  - lastTransitionTime: "2025-09-29T10:15:28Z"
    message: 'Prefill-decode disaggregation is active'
    reason: DisaggregationActive
    status: "True"
    type: Disaggregated
  currentReplicas: 1
  observedGeneration: 2
  replicas: 1
  updatedReplicas: 1

kubectl get pod -owide -l modelserving.volcano.sh/name=deepseek-v2-lite-deepseek-v2-lite -n dev

NAME                                              READY   STATUS     RESTARTS   AGE   IP              NODE           NOMINATED NODE   READINESS GATES
deepseek-v2-lite-deepseek-v2-lite-0-decode-0-0    2/2     Running    0          3s    192.168.0.86    192.168.0.90   <none>           <none>
deepseek-v2-lite-deepseek-v2-lite-0-prefill-0-0   2/2     Running    0          3s    192.168.0.242   192.168.0.90   <none>           <none>

```

**Note:** ModelBooster creates a ModelServing resource named `{modelbooster-name}-{backend-name}`. The pods are labeled with `modelserving.volcano.sh/name={modelserving-name}`.

### ModelServing Approach (Alternative)

For environments that require more granular control over the NPU deployment configuration, you can use the ModelServing
approach with fine-tuned NPU resource specifications and Ascend-specific optimizations.

For a detailed comparison of the ModelServing approach's advantages, manually created components, and when to use it, see the [ModelServing Approach](../model-deployment.md#modelserving-approach) section in the ModelBooster documentation.

**Important Note:** When using the ModelServing approach, you need to manually create the following CRD resources:

1. **ModelServing** - Manages workloads and Pods
2. **ModelServer** - Manages networking layer and inter-service communication
3. **ModelRoute** - Provides request routing functionality

#### 1. ModelServing Configuration

First, create the ModelServing resource to manage prefill and decode workloads using the [ModelServing configuration](../../assets/examples/model-serving/prefill-decode-disaggregation.yaml):

```sh
kubectl apply -f examples/model-serving/prefill-decode-disaggregation.yaml
```

This configuration includes:
- **Prefill Role**: Handles input processing with NPU-optimized containers and KV cache production
- **Decode Role**: Manages output generation with NPU-optimized containers and KV cache consumption
- **NPU Resource Allocation**: Dedicated `huawei.com/ascend-1980: "4"` resources for each role
- **HCCL Network Configuration**: Environment variables for Huawei Collective Communication Library
- **Volume Mounts**: Shared model storage and NPU configuration files

#### 2. ModelServer Configuration

Create the ModelServer resource to manage the networking layer for disaggregated inference, providing load balancing and
traffic management between prefill and decode services using the [ModelServer configuration](../../assets/examples/kthena-router/ModelServer-prefill-decode-disaggregation.yaml):

```sh
kubectl apply -f examples/kthena-router/ModelServer-prefill-decode-disaggregation.yaml
```

This configuration includes:
- **NPU-Aware Workload Selection**: Targets ModelServing workloads with NPU resource specifications
- **Prefill-Decode Group Management**: Manages communication between prefill and decode services
- **KV Connector Integration**: Uses nixl connector for efficient KV cache transfer
- **Traffic Policy**: Optimized timeout settings for NPU workloads

#### 3. ModelRoute Configuration

Create the ModelRoute resource to provide routing functionality, directing requests to the appropriate model server using the [ModelRoute configuration](../../assets/examples/kthena-router/ModelRoute-prefill-decode-disaggregation.yaml):

```sh
kubectl apply -f examples/kthena-router/ModelRoute-prefill-decode-disaggregation.yaml
```

This configuration includes:
- **Model Name Mapping**: Routes requests for "deepseek-ai/DeepSeekV2" model
- **Default Routing Rule**: Directs all requests to the ModelServer managing NPU workloads
- **Target Model Integration**: Connects to the ModelServer with NPU-optimized prefill-decode disaggregation

#### ModelServing Deployment Steps

When using the ModelServing approach, create resources in the following order:

```bash
# 1. Create ModelServing resource
kubectl apply -f examples/model-serving/prefill-decode-disaggregation.yaml

# 2. Create ModelServer resource
kubectl apply -f examples/kthena-router/ModelServer-prefill-decode-disaggregation.yaml

# 3. Create ModelRoute resource
kubectl apply -f examples/kthena-router/ModelRoute-prefill-decode-disaggregation.yaml
```

#### Verifying ModelServing Deployment

```sh
kubectl get modelserving deepseek-v2-lite -n dev -o yaml | grep status -A 10

kubectl get pod -owide -l modelserving.volcano.sh/name=deepseek-v2-lite -n dev

NAME                             READY   STATUS    RESTARTS   AGE    IP              NODE           NOMINATED NODE   READINESS GATES
deepseek-v2-lite-0-decode-0-0    2/2     Running   0          105m   192.168.0.88    192.168.0.90   <none>           <none>
deepseek-v2-lite-0-prefill-0-0   2/2     Running   0          105m   192.168.0.152   192.168.0.90   <none>           <none>
```

## Test

After deploying your prefill-decode disaggregated inference using either approach, you can verify that the deployment is
working correctly by testing the model using the Chat API.

### Testing the Deployed Model

Use the following curl command to send a test request to your deployed model:

```bash
curl --location 'http://${ENDPOINT}/v1/chat/completions' \
--header 'Content-Type: application/json' \
--data '{
    "model": "deepseek-ai/DeepSeekV2",
    "messages": [
        {
            "role": "user",
            "content": "Where is the capital of China?"
        }
    ],
    "stream": false
}'
```

**Important Notes:**

- Replace `${ENDPOINT}` with your actual service endpoint IP address and port
- The model name should match the `served-model-name` configured in your deployment
- A successful response indicates that both prefill and decode services are working correctly and communicating properly
- If you receive a proper response, it confirms that the disaggregated inference pipeline is functioning as expected

### Expected Response

A successful API call should return a JSON response similar to:

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "deepseek-ai/DeepSeekV2",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "The capital of China is Beijing."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 8,
    "total_tokens": 18
  }
}
```

## Clean up

### ModelBooster Cleanup

When using the ModelBooster approach, only delete the ModelBooster resource - the associated ModelServer and ModelRoute
will be automatically cleaned up:

```sh
# Delete ModelBooster resource (automatically cleans up ModelServer and ModelRoute)
kubectl delete modelbooster deepseek-v2-lite -n dev
```

**Note:** ModelBooster automatically manages the lifecycle of ModelServer and ModelRoute, no manual deletion required.

### ModelServing Cleanup

When using the ModelServing approach, manually delete all resources in reverse order:

```sh
# 1. Delete ModelRoute resource
kubectl delete modelroute deepseek-v2 -n dev

# 2. Delete ModelServer resource
kubectl delete modelserver deepseek-v2 -n dev

# 3. Delete ModelServing resource
kubectl delete modelserving deepseek-v2-lite -n dev

# 4. Clean up associated resources
kubectl delete podgroup -l modelserving.volcano.sh/name=deepseek-v2-lite -n dev
```

