# Downloader

Kthena Downloader prepares model files before an inference engine starts. It supports Hugging Face, ModelScope, S3-compatible object storage, OBS, and files already stored on a mounted PersistentVolumeClaim (PVC).

For new deployments, add the downloader as an init container in your `ModelServing` Pod template. The init container writes model files to a shared volume and exits when the download completes. The inference container then loads the model from that volume. You can also run the downloader independently with Docker.

## Prerequisites

- For ModelServing deployments, a Kubernetes cluster with Kthena installed and `kubectl` configured. See [Installation](../getting-started/installation.md).
- Network access from the downloader to the model repository or object storage endpoint, unless copying from a mounted PVC.
- Credentials with read access when downloading private or gated models.
- Enough writable storage for the model files. The inference container must be able to read the output directory.
- For the inference example below, a node with an NVIDIA GPU and the NVIDIA device plugin installed.

## Supported sources

| Source | URI example | Behavior |
| --- | --- | --- |
| Hugging Face | `hf://Qwen/Qwen3-0.6B` or `Qwen/Qwen3-0.6B` | Downloads a repository snapshot with the Hugging Face SDK. |
| ModelScope | `ms://Qwen/Qwen3-0.6B` | Downloads a repository snapshot with the ModelScope SDK. |
| S3 | `s3://model-bucket/models/qwen3` | Synchronizes objects under the specified bucket prefix using AWS CLI. |
| OBS | `obs://model-bucket/models/qwen3` | Uses the same S3-compatible synchronization path with a configured endpoint. |
| PVC | `pvc:///source/models/qwen3` | Copies a directory already visible inside the container using `rsync`. |

For S3 and OBS, upload the complete model directory to the bucket before starting the downloader. Include the weights, model configuration, tokenizer, and any other files required by your inference engine.

## Deploy with ModelServing

The following example downloads a public Hugging Face model to an `emptyDir` volume and starts vLLM after the download succeeds. Save it as `modelserving-qwen.yaml`:

```yaml showLineNumbers
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: qwen3-downloader
  namespace: default
spec:
  replicas: 1
  template:
    roles:
      - name: server
        replicas: 1
        workerReplicas: 0
        entryTemplate:
          spec:
            initContainers:
              - name: downloader
                image: ghcr.io/volcano-sh/downloader:latest
                args:
                  - --source
                  - hf://Qwen/Qwen3-0.6B
                  - --output-dir
                  - /models/qwen3
                  - --max-workers
                  - "8"
                volumeMounts:
                  - name: model-storage
                    mountPath: /models
            containers:
              - name: vllm
                image: vllm/vllm-openai:v0.11.0
                args:
                  - --model
                  - /models/qwen3
                  - --served-model-name
                  - Qwen/Qwen3-0.6B
                ports:
                  - containerPort: 8000
                resources:
                  limits:
                    nvidia.com/gpu: "1"
                volumeMounts:
                  - name: model-storage
                    mountPath: /models
            volumes:
              - name: model-storage
                emptyDir: {}
```

Apply the resource:

```bash
kubectl apply -f modelserving-qwen.yaml
```

Notes:

1. The downloader image already defines its entrypoint; set `args` without overriding `command`.
2. The downloader's `--output-dir` and vLLM's `--model` refer to the same directory on the shared volume.
3. An `emptyDir` survives container restarts within a Pod, but is deleted when the Pod is removed. For persistent storage, replace it with a PVC as described in [Storage configuration](#storage-configuration).
4. Pin the downloader image to the version used by your Kthena release for production deployments instead of using `latest`.
5. When using additional roles or worker Pods, configure the downloader and volume mounts in each Pod template that needs the model, including `workerTemplate` when applicable.

This example only prepares and starts the model workload. Configure `ModelServer` and `ModelRoute` separately to expose it through Kthena Router; see the [ModelServing quick start](../getting-started/quick-start.md#modelserving).

## Downloader configuration

### Command-line arguments

| Argument | Default | Description |
| --- | --- | --- |
| `-s`, `--source` | Required | Model repository identifier or source URI. |
| `-o`, `--output-dir` | `~/downloads` | Local destination directory. The CLI expands `~` and resolves the path to an absolute path. Set this explicitly to a mounted path in containers. |
| `-w`, `--max-workers` | `8` | Concurrent download workers for Hugging Face and ModelScope. This option does not control AWS CLI or `rsync` concurrency. |
| `-c`, `--config` | Not set | JSON object containing provider-specific configuration. Pass the JSON string itself, not a file path. |

### Environment variables and JSON configuration

The downloader reads the following environment variables. Each has a corresponding key in `--config`:

| Environment variable | JSON key | Description |
| --- | --- | --- |
| `HF_AUTH_TOKEN` | `hf_token` | Hugging Face access token for private or gated repositories. |
| `HF_ENDPOINT` | `hf_endpoint` | Custom Hugging Face API endpoint or mirror. |
| `HF_REVISION` | `hf_revision` | Hugging Face branch, tag, or commit to download. |
| `MS_TOKEN` | `ms_token` | ModelScope access token. |
| `MS_REVISION` | `ms_revision` | ModelScope revision to download. |
| `ACCESS_KEY` | `access_key` | Access key for S3-compatible storage or OBS. |
| `SECRET_KEY` | `secret_key` | Secret key for S3-compatible storage or OBS. |
| `ENDPOINT` | `endpoint` | Object storage endpoint URL. Set this for OBS and custom S3-compatible services. |

Values explicitly present in the JSON object take precedence over environment variables. Non-empty environment variables fill only missing JSON keys. If a revision is not configured, the provider SDK selects its default revision.

For example, add these arguments to the downloader init container to select a Hugging Face revision:

```yaml
args:
  - --source
  - hf://Qwen/Qwen3-0.6B
  - --output-dir
  - /models/qwen3
  - --config
  - '{"hf_revision":"main"}'
```

:::tip Credentials
Keep credentials in Kubernetes Secrets and pass them to the downloader through `envFrom` or `env[].valueFrom.secretKeyRef`. Avoid placing tokens or access keys in container arguments or checked-in manifests. Secrets must exist in the same namespace as the workload.
:::

### Hugging Face

For private or gated repositories, create a Secret containing `HF_AUTH_TOKEN`. Replace the placeholder before applying it, and ensure the token's account has access to the model:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: model-download-credentials
  namespace: default
type: Opaque
stringData:
  HF_AUTH_TOKEN: "<your-hugging-face-token>"
```

Add the following fields to the downloader init container:

```yaml
envFrom:
  - secretRef:
      name: model-download-credentials
env:
  - name: HF_REVISION
    value: main
  # Optional: use a reachable Hugging Face-compatible mirror.
  # - name: HF_ENDPOINT
  #   value: https://hf-mirror.example.com
```

Public repositories generally do not require a token. Set `HF_ENDPOINT`, not `ENDPOINT`, when configuring a Hugging Face mirror.

### ModelScope

Change `--source` to `ms://Qwen/Qwen3-0.6B`. To choose a revision, add the following environment variable to the downloader init container:

```yaml
env:
  - name: MS_REVISION
    value: master
```

If authentication is required, store `MS_TOKEN` in a Secret and reference it from the downloader using `envFrom`, as in the Hugging Face example. Use a revision that exists in the selected ModelScope repository.

### S3 and OBS

Change `--source` to the model's bucket prefix, such as `s3://model-bucket/models/qwen3` or `obs://model-bucket/models/qwen3`.

Create a Secret with the object storage credentials:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: object-storage-credentials
  namespace: default
type: Opaque
stringData:
  ACCESS_KEY: "<your-access-key>"
  SECRET_KEY: "<your-secret-key>"
```

Reference the Secret and set the endpoint on the downloader init container:

```yaml
envFrom:
  - secretRef:
      name: object-storage-credentials
env:
  - name: ENDPOINT
    value: https://obs.example.com
```

Replace the endpoint with your service's S3-compatible endpoint. For standard AWS S3, a custom endpoint is not required. The downloader passes credentials and the endpoint to AWS CLI as `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_ENDPOINT_URL`.

If you explicitly configure `ACCESS_KEY` and `SECRET_KEY` (or their JSON equivalents), provide both. When neither is provided, AWS CLI uses its normal credential resolution, such as credentials supplied by the workload's IAM role. Configure the AWS region through the usual AWS CLI environment or configuration when required.

## Storage configuration

### Persist downloaded models on a PVC

Create a PVC with enough capacity for your model, or use an existing claim. Replace the `volumes` entry in the ModelServing example with:

```yaml
volumes:
  - name: model-storage
    persistentVolumeClaim:
      claimName: model-cache
```

Keep the `/models` mount in both the downloader and inference container. The `model-cache` claim must exist in the workload namespace. Choose an access mode and storage backend compatible with your Pod placement; sharing the volume across nodes typically requires `ReadWriteMany` support.

Use separate output directories for different models or revisions. The downloader takes a lock at `<output-dir>/.lock` while preparing files. This serializes cooperating downloaders targeting that directory; it is not a guarantee that future runs skip synchronization, nor does it protect a running inference engine from changes to its model files.

### Copy models from an existing PVC

A `pvc://` source describes a path **inside the downloader container**, not a Kubernetes claim lookup. Mount the source PVC explicitly. For example, replace the downloader init container in the ModelServing example with:

```yaml
initContainers:
  - name: downloader
    image: ghcr.io/volcano-sh/downloader:latest
    args:
      - --source
      - pvc:///source/models/qwen3
      - --output-dir
      - /models/qwen3
    volumeMounts:
      - name: source-models
        mountPath: /source
        readOnly: true
      - name: model-storage
        mountPath: /models
```

In the same Pod spec, configure both volumes:

```yaml
volumes:
  - name: source-models
    persistentVolumeClaim:
      claimName: preloaded-models
  - name: model-storage
    emptyDir: {}
```

The `preloaded-models` PVC must already contain a `models/qwen3` directory. The downloader copies that directory's contents to `/models/qwen3`; keep the inference container's original `model-storage` mount. Keep the destination separate from the source and outside the source directory tree.

If the inference engine can read the preloaded model directly, you can instead mount the PVC in the inference container and omit the downloader.

## Verification and troubleshooting

Find the Pods for the ModelServing example and inspect the downloader logs, replacing `<pod-name>` with a returned name:

```bash
kubectl get pods -n default -l modelserving.volcano.sh/name=qwen3-downloader
kubectl logs -n default <pod-name> -c downloader
kubectl describe pod -n default <pod-name>
```

A successful download leaves the init container in the `Terminated` state with reason `Completed` and exit code `0`. Kubernetes then starts the inference container. Downloader failures exit with a non-zero status and block inference startup.

| Symptom | What to check |
| --- | --- |
| Pod remains in `Init:0/1` | Check download progress in the init container logs. Large models may take time. If it is waiting for a lock, check other downloaders using the same output directory. |
| Authentication or access denied errors | Verify the source URI, Secret keys, namespace, and read permissions. For gated Hugging Face models, confirm the token's account has been granted access. |
| `access_key and secret_key must be provided together` | Supply both object storage credential fields, or omit both and configure AWS CLI credentials through another supported mechanism. |
| Connection or endpoint errors | Check Pod DNS, network access, and the provider-specific endpoint variable: `HF_ENDPOINT` for Hugging Face, `ENDPOINT` for S3/OBS. |
| PVC source path is not visible | Ensure the source PVC is mounted in the downloader and the `pvc://` URI matches the in-container directory, not just the claim name. |
| Permission denied or no space left | Check volume permissions and capacity. The downloader must be able to create files and its lock in the output directory. |
| Download succeeds but the engine cannot load the model | Check that the engine reads the same shared directory and that the source contains all required model files. |
