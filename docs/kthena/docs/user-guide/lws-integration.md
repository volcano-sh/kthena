# LeaderWorkerSet (LWS) Integration

Kthena ModelServing provides native support for the **LeaderWorkerSet (LWS)** API. This capability allows users to define inference workloads using the standard LWS Custom Resource Definition (CRD) while leveraging Kthena's powerful orchestration, routing, and autoscaling features underneath.

This guide explains how to use LeaderWorkerSet resources with Kthena.

## Overview

LeaderWorkerSet (LWS) is a widely adopted API for describing multi-host inference workloads (e.g., LLM inference). Kthena integrates LWS support by directly watching and handling `LeaderWorkerSet` Custom Resources.

**Key Features:**

- **LWS API Compatibility**: Users can submit standard LeaderWorkerSet CRs directly.
- **Zero Extra Infrastructure**: No need to deploy the native LWS Controller; Kthena's ModelServing Controller handles the logic.
- **Kthena Powered**: Automatically inherits Kthena's capabilities like ModelRoute and Autoscaling.
- **Seamless Migration**: Ideal for users already using LWS who want to migrate to Kthena without rewriting their manifests.

## How It Works

When Kthena's LWS integration is enabled:

1.  **Direct Processing**: The Model Serving Controller listens for `LeaderWorkerSet` resources.
2.  **One-Way Conversion**: It automatically converts the LWS specification into Kthena's internal `ModelServing` resources.
3.  **Status Sync**: The status of the underlying pods is aggregated and written back to the `LeaderWorkerSet` status, allowing you to use standard `kubectl get lws` commands to monitor progress.

> **Note**: This is a one-way synchronization. Changes should be made to the `LeaderWorkerSet` resource, which will propagate to the underlying Kthena resources.

## Configuration Mapping

### Spec Mapping (LeaderWorkerSet -> ModelServing)

<div class="table-wrapper">

| LeaderWorkerSet Field | ModelServing Internal Semantics | Description |
|-----------------------|---------------------------------|-------------|
| `metadata.name` | `metadata.name` | Mapped to the ModelServing identifier. |
| `spec.replicas` | `spec.replicas` | Defines the number of independent serving groups. |
| `spec.leaderWorkerTemplate.leaderTemplate` | `spec.template.roles.EntryTemplate` | Parsed as Leader role definition. If nil, `workerTemplate` is used as Entry Pod. |
| `spec.leaderWorkerTemplate.workerTemplate` | `spec.template.roles.WorkerTemplate` | Parsed as Worker role definition. |
| `spec.leaderWorkerTemplate.size` | Worker Role Replicas | Used to calculate replica count for Worker Role: `Replicas = Size - 1` (Assuming Leader is 1). |
| `spec.startupPolicy` | Startup Policy | Maps startup order policy (e.g., `LeaderFirst`). |

</div>

### Status Mapping (ModelServing -> LeaderWorkerSet)

| ModelServing Internal Status | LeaderWorkerSet Status | Description |
|------------------------------|------------------------|-------------|
| ServingGroup Ready Count | `status.readyReplicas` | Number of ready groups. |
| ServingGroup Total Count | `status.replicas` | Number of currently existing groups. |
| Conditions | `status.conditions` | Aggregated health status (Available, Progressing). |

## Deployment Example

Below is an example of deploying an inference workload using `LeaderWorkerSet`.

### Prerequisites

Ensure the `LeaderWorkerSet` CRD is installed in your cluster. You do **not** need to install the LWS controller or operator.

```bash
# Example: Install LWS CRD only
kubectl apply -f https://github.com/kubernetes-sigs/lws/releases/download/v0.3.0/crd.yaml
```

### LWS Configuration

This example defines a deployment with 1 replica group. Each group consists of 1 Leader and 1 Worker (Size = 2).

<details>
<summary>
<b>lws-inference-example.yaml</b>
</summary>

```yaml
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: llama-multinode
  namespace: default
spec:
  # Number of independent model replicas (Serving Groups)
  replicas: 1
  
  leaderWorkerTemplate:
    # Total size of the group (1 Leader + 1 Worker)
    size: 2
    
    # Leader Pod Configuration
    leaderTemplate:
      metadata:
        labels:
          role: leader
          model: llama-405b
      spec:
        containers:
        - name: leader
          image: vllm/vllm-openai:latest
          env:
          - name: HUGGING_FACE_HUB_TOKEN
            value: $HUGGING_FACE_HUB_TOKEN
          command:
            - sh
            - -c
            - "bash /vllm-workspace/examples/online_serving/multi-node-serving.sh leader --ray_cluster_size=2; 
              python3 -m vllm.entrypoints.openai.api_server --port 8080 --model meta-llama/Llama-3.1-405B-Instruct --tensor-parallel-size 8 --pipeline_parallel_size 2"
          resources:
            limits:
              nvidia.com/gpu: "8"
              memory: 1124Gi
              ephemeral-storage: 800Gi
            requests:
              ephemeral-storage: 800Gi
              cpu: 125
          ports:
          - containerPort: 8080
            name: http
          readinessProbe:
            tcpSocket:
              port: 8080
            initialDelaySeconds: 15
            periodSeconds: 10
          volumeMounts:
          - mountPath: /dev/shm
            name: dshm
        volumes:
        - name: dshm
          emptyDir:
            medium: Memory
            sizeLimit: 15Gi

    # Worker Pod Configuration
    workerTemplate:
      metadata:
        labels:
          role: worker
          model: llama-405b
      spec:
        containers:
        - name: worker
          image: vllm/vllm-openai:latest
          command:
            - sh
            - -c
            - "bash /vllm-workspace/examples/online_serving/multi-node-serving.sh worker --ray_address=$(ENTRY_ADDRESS)"
          resources:
            limits:
              nvidia.com/gpu: "8"
              memory: 1124Gi
              ephemeral-storage: 800Gi
            requests:
              ephemeral-storage: 800Gi
              cpu: 125
          env:
          - name: HUGGING_FACE_HUB_TOKEN
            value: $HUGGING_FACE_HUB_TOKEN
          volumeMounts:
          - mountPath: /dev/shm
            name: dshm
        volumes:
        - name: dshm
          emptyDir:
            medium: Memory
            sizeLimit: 15Gi
```

</details>

### Verifying Deployment

After applying the YAML, you can check the status using standard kubectl commands:

```bash
# Check the LeaderWorkerSet status
kubectl get lws qwen-72b-inference

# Check the underlying Pods created by Kthena
kubectl get pods -l leaderworkerset.x-k8s.io/name=qwen-72b-inference
```

## Status & Troubleshooting

The `LeaderWorkerSet` status is automatically updated by Kthena:

- **ReadyReplicas**: Indicates how many serving groups are fully ready.
- **Conditions**: Provides details on the health and state of the deployment.

If the `LeaderWorkerSet` is not progressing:
1. Check if the CRD is installed correctly.
2. Inspect the Kthena Controller logs for any validation errors regarding the LWS spec.
3. Verify that the resource requests (GPUs, CPU) can be satisfied by the cluster.
