# Runtime

Kthena Runtime is a lightweight sidecar service designed to standardize Prometheus metrics from inference engines, provides LoRA adapter download/load/unload capabilities, and supports model downloading.

## Overview

- Metrics standardization: fetch native metrics from the engine's /metrics endpoint, rename them to unified Kthena metrics according to rules.
- LoRA lifecycle management: simple HTTP APIs to download+load and unload LoRA adapters for dynamic enable/disable.
- Model downloading: supports downloading models from S3/OBS/PVC/HuggingFace to a local path.

Notes:

1. If you want to download from S3/OBS, you first need to upload the model to the bucket.

## Installation

- Runtime does not support separate installation.  it will be automatically deployed alongside the inference container as a sidecar when you are using `ModelBooster` to deploy llm.
- When deploying via the ModelBooster CR (one-stop deployment), no additional configuration is needed; ModelServing will automatically enable the runtime feature.
- For standalone deployment using ModelServing YAML, you can add the following configuration to start Runtime as sidecar container:

  ```
  - name: runtime
    ports:
      - containerPort: 8900
    image: kthena/runtime:latest
    args:
      - --port
      - "8900"
      - --engine
      - vllm
      - --engine-base-url
      - http://localhost:8000
      - --engine-metrics-path
      - /metrics
      - --pod
      - $(POD_NAME).$(NAMESPACE)
      - --model
      - test-model
    env:
      - name: ENDPOINT
        value: https://obs.test.com
      - name: RUNTIME_PORT
        value: "8900"
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
    envFrom:
      - secretRef:
          name: "test-secret"
    readinessProbe:
      httpGet:
        path: /health
        port: 8900
      initialDelaySeconds: 5
      periodSeconds: 10
    resources: { }
  ```

Startup arguments:

- `-E, --engine` (required): engine name, supports `vllm`, `sglang`
- `-H, --host` (default `0.0.0.0`): listen address for Runtime
- `-P, --port` (default `9000`): listen port for Runtime
- `-B, --engine-base-url` (default `http://localhost:8000`): engine base URL
- `-M, --engine-metrics-path` (default `/metrics`): engine metrics path
- `-I, --pod` (required): current instance/Pod identifier, used for events and Redis keys
- `-N, --model` (required): model name

In the ModelBooster YAML, you can control Runtime startup values via `spec.backend.env`:

```
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
    type: "vLLM" # --engine
    modelURI: s3://kthena/Qwen/Qwen2.5-Coder-32B-Instruct
    cacheURI: hostpath:///cache/
    envFrom:
      - secretRef:
          name: your-secrets
    env:
      - name: "RUNTIME_PORT"  # default 8100
        value: "8200"
      - name: "RUNTIME_URL"   # default http://localhost:8000/metrics
        value: "http://localhost:8100"
      - name: "RUNTIME_METRICS_PATH" # default /metrics
        value: "/metrics"
    minReplicas: 1
    maxReplicas: 1
    workers:
      - type: server
        image: openeuler/vllm-ascend:latest
        replicase: 1
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

## Metric Standardization

Runtime renames key metrics from different engines to unified names prefixed with `kthena:*` for consistent observability (Prometheus/Grafana):

- `kthena:generation_tokens_total`
- `kthena:num_requests_waiting`
- `kthena:time_to_first_token_seconds`
- `kthena:time_per_output_token_seconds`
- `kthena:e2e_request_latency_seconds`

Notes:

1. When `engine=vllm` or `engine=sglang`, key metrics from vLLM/SGLang are renamed to the standard names above.
2. Only metrics covered by built-in mappings are standardized, and the original metrics are preserved. You can obtain all raw engine metrics plus the standardized metrics.

## Dynamic Lora configuration

You can use ModelBooster YAML to configure LoRA adapters for automatic download and loading during the model startup.
If you only change loraAdapters in ModelBooster YAML, Runtime will dynamically download and load/unload the adapters without restarting the Pod.

```
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelBooster
metadata:
  annotations:
    api.kubernetes.io/name: example
  name: deepseek-r1-distill-llama-8b
spec:
  name: deepseek-r1-distill-llama-8b
  owner: example
  backend:
    name: "deepseek-r1-distill-llama-8b-vllm"
    type: "vLLM"
    modelURI: "s3://model-bucket/deepseek-r1-distill-llama-8b"
    cacheURI: hostpath:///cache/
    envFrom:
      - secretRef:
          name: your-secrets  # AccessKey/SecretKey for S3/OBS or HF_AUTH_TOKEN for HuggingFace
    env:
      - name: "ENDPOINT"
        value: "https://obs.test.com"
      - name: "VLLM_ALLOW_RUNTIME_LORA_UPDATING"
        value: "True"  # Enable dynamic LoRA load/unload
    minReplicas: 1
    maxReplicas: 1
    workers:
      - type: server
        image: openeuler/vllm-ascend:latest
        replicase: 1
        pods: 1
```

Notes:

1. To enable dynamic LoRA configuration, ensure that the environment variable `VLLM_ALLOW_RUNTIME_LORA_UPDATING` is set to `True`.
2. `loraAdapters.artifactURL` supports the same sources and formats as modelURI in the ModelBooster CR, including:
   - Hugging Face: `<namespace>/<repo_name>`, e.g., `microsoft/phi-2`
   - S3: `s3://bucket/path`
   - OBS: `obs://bucket/path`
   - PVC: `pvc://path`
3. You can configure the following environment variables for Runtime to access private models or object storage services:
   - Hugging Face:
     - `HF_AUTH_TOKEN` (optional): token for accessing private models
     - `HF_ENDPOINT` (optional): custom HF API endpoint
     - `HF_REVISION` (optional): model branch/revision (e.g., `main`)
   - S3/OBS:
     - `ACCESS_KEY`, `SECRET_KEY`: access credentials (recommended to store in a Secret and load via `envFrom.secretRef.name`)
     - `ENDPOINT`: object storage service endpoint (e.g., `https://s3.us-east-1.amazonaws.com` or `https://obs.test.com`)

