# Data Parallel Deployment

Data parallelism is a technique for scaling LLM serving by deploying multiple replicas of the same model. Unlike model
parallelism (which splits a single model across multiple GPUs to fit large models), data parallelism focuses on
increasing throughput by distributing incoming requests across multiple independent model instances.

This guide describes how to deploy the **Qwen3-0.6B** model using data parallelism with **ModelBooster**. The deployment
leverages **vLLM** as the inference backend and demonstrates two different load balancing strategies to suit different
infrastructure needs:

1. **Internal Load Balancing**: Distribute requests across workers.
2. **External Load Balancing**: Relies on external components (like Kubernetes Services or Ingress) to route traffic to
   independent replicas.

## Internal Load Balancing

Internal load balancing serves as the default mode where the coordination implementation (e.g., Ray) manages the
distribution of requests to the available workers. This is suitable for scenarios where you want a unified endpoint that
internally manages its worker pool.

### For Single Node

For example, deploy on a single 2-GPU machine.

```yaml
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

Here is an example to deploy on 2 nodes with 2 GPUs each. The `pods: 2` configuration ensures we have distributed
workers, and `data-parallel-backend: "ray"` enables the coordination.

```yaml
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
load balancing is the preferred approach. For now, it does not support deploy by `ModelBooster`, you should use
`ModelServing` directly. Here is an example deployment of 2 pods (each with 1 GPU) for the **Qwen3-0.6B** model:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: my-model
  namespace: kthena-system
spec:
  schedulerName: volcano
  replicas: 1
  template:
    gangPolicy:
      minRoleReplicas:
        leader: 1
    roles:
      - name: leader0
        replicas: 1
        entryTemplate:
          spec:
            containers:
              - name: engine
                image: vllm/vllm-openai:v0.13.0
                command:
                  - python3
                  - -m
                  - vllm.entrypoints.openai.api_server
                  - --model
                  - /tmp/cache/Qwen3
                  - --data-parallel-address
                  - my-model-0-leader0-0-0.kthena-system.svc.cluster.local # Use the internal address of pod to communicate. Can not use Pod IP here. Because Pod IP will change. Also, can not use node IP, because node IP is invisible inside pod.
                  - --data-parallel-rank
                  - "0" # rank 0
                  - --data-parallel-rpc-port
                  - "13345"
                  - --data-parallel-size
                  - "2" # 2 pods total, each with 1 GPU, so size is 2
                  - --enforce-eager
                  - --gpu-memory-utilization
                  - "0.9"
                  - --max-model-len
                  - "2048"
                  - --max-num-seqs
                  - "16"
                  - --served-model-name
                  - my-model
                env:
                  - name: POD_NAME
                    valueFrom:
                      fieldRef:
                        fieldPath: metadata.name
                  - name: NAMESPACE
                    valueFrom:
                      fieldRef:
                        fieldPath: metadata.namespace
                  - name: VLLM_USE_V1
                    value: "1"
                lifecycle:
                  preStop:
                    exec:
                      command:
                        - /bin/sh
                        - -c
                        - |
                          while true; do
                            RUNNING=$(curl -s http://localhost:8000/metrics | grep 'vllm:num_requests_running' | grep -v '#' | awk '{print $2}')
                            WAITING=$(curl -s http://localhost:8000/metrics | grep 'vllm:num_requests_waiting' | grep -v '#' | awk '{print $2}')
                            if [ "$RUNNING" = "0.0" ] && [ "$WAITING" = "0.0" ]; then
                              echo "Terminating: No active or waiting requests, safe to terminate" >> /proc/1/fd/1
                              exit 0
                            else
                              echo "Terminating: Running: $RUNNING, Waiting: $WAITING" >> /proc/1/fd/1
                              sleep 5
                            fi
                          done
                readinessProbe:
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
                    scheme: HTTP
                  initialDelaySeconds: 180
                  periodSeconds: 5
                  successThreshold: 1
                  timeoutSeconds: 1
                resources:
                  limits:
                    nvidia.com/gpu: "1"
                volumeMounts:
                  - mountPath: /tmp/cache
                    name: example-weights
                  - mountPath: /dev/shm
                    name: dshm
            initContainers:
              - args:
                  - --source
                  - hf://Qwen/Qwen3-0.6B
                  - --output-dir
                  - /tmp/cache/Qwen3
                image: ghcr.io/volcano-sh/downloader:v0.2.0
                name: downloader
                resources: { }
                volumeMounts:
                  - mountPath: /tmp/cache
                    name: models
            terminationGracePeriodSeconds: 300
            volumes:
              - hostPath:
                  path: /tmp/cache
                  type: DirectoryOrCreate
                name: example-weights
              - emptyDir:
                  medium: Memory
                name: dshm
        workerReplicas: 0
      - name: leader1
        replicas: 1
        entryTemplate:
          spec:
            containers:
              - name: engine
                image: vllm/vllm-openai:v0.13.0
                command:
                  - python3
                  - -m
                  - vllm.entrypoints.openai.api_server
                  - --model
                  - /tmp/cache/Qwen3
                  - --data-parallel-address
                  - my-model-0-leader0-0-0.kthena-system.svc.cluster.local
                  - --data-parallel-rank
                  - "1" # rank 1
                  - --data-parallel-rpc-port
                  - "13345"
                  - --data-parallel-size
                  - "2"
                  - --enforce-eager
                  - --gpu-memory-utilization
                  - "0.9"
                  - --max-model-len
                  - "2048"
                  - --max-num-seqs
                  - "16"
                  - --served-model-name
                  - my-model
                env:
                  - name: POD_NAME
                    valueFrom:
                      fieldRef:
                        fieldPath: metadata.name
                  - name: NAMESPACE
                    valueFrom:
                      fieldRef:
                        fieldPath: metadata.namespace
                  - name: VLLM_USE_V1
                    value: "1"
                lifecycle:
                  preStop:
                    exec:
                      command:
                        - /bin/sh
                        - -c
                        - |
                          while true; do
                            RUNNING=$(curl -s http://localhost:8000/metrics | grep 'vllm:num_requests_running' | grep -v '#' | awk '{print $2}')
                            WAITING=$(curl -s http://localhost:8000/metrics | grep 'vllm:num_requests_waiting' | grep -v '#' | awk '{print $2}')
                            if [ "$RUNNING" = "0.0" ] && [ "$WAITING" = "0.0" ]; then
                              echo "Terminating: No active or waiting requests, safe to terminate" >> /proc/1/fd/1
                              exit 0
                            else
                              echo "Terminating: Running: $RUNNING, Waiting: $WAITING" >> /proc/1/fd/1
                              sleep 5
                            fi
                          done
                readinessProbe:
                  failureThreshold: 3
                  httpGet:
                    path: /health
                    port: 8000
                    scheme: HTTP
                  initialDelaySeconds: 180
                  periodSeconds: 5
                  successThreshold: 1
                  timeoutSeconds: 1
                resources:
                  limits:
                    nvidia.com/gpu: "1"
                volumeMounts:
                  - mountPath: /tmp/cache
                    name: example-weights
                  - mountPath: /dev/shm
                    name: dshm
            initContainers:
              - args:
                  - --source
                  - hf://Qwen/Qwen3-0.6B
                  - --output-dir
                  - /tmp/cache/Qwen3
                image: ghcr.io/volcano-sh/downloader:v0.2.0
                name: my-model-model-downloader
                resources: { }
                volumeMounts:
                  - mountPath: /tmp/cache
                    name: example-weights
            terminationGracePeriodSeconds: 300
            volumes:
              - hostPath:
                  path: /tmp/cache
                  type: DirectoryOrCreate
                name: example-weights
              - emptyDir:
                  medium: Memory
                name: dshm
        workerReplicas: 0
```

The key point is the configuration `--data-parallel-address`, pod IP or node IP cannot be used here because pod IP is
not stable and node IP is invisible inside pod. You should use the internal DNS address of the pod to ensure proper
communication between replicas.