# Kthena Model Downloader

A universal LLM downloader tool that supports retrieving model files from multiple sources.

## Features

- Support for multiple model sources:
  - Hugging Face repositories (`<namespace>/<repo_name>` or `hf://<namespace>/<repo_name>`)
  - ModelScope (`ms://<namespace>/<repo_name>`)
  - S3 buckets (`s3://bucket/path`)
  - Object Storage (`obs://bucket/path`)
  - PVC storage (`pvc://path`)
- Concurrent downloads for improved performance
- Thread-safe operations with file-based locking mechanism
- Flexible configuration options (environment variables or JSON)
- Detailed logging

## Installation

Build the Docker image:

```bash
cd python
docker build -t kthena-downloader:latest -f Dockerfile --target downloader .
```

## Usage

### Docker Command Options

```bash
docker run --rm -v /path/to/local/output:/output kthena-downloader:latest --source [MODEL_SOURCE] --output-dir /output
```

Parameters:

- `-s, --source`: Model source URI or identifier (required)
- `-o, --output-dir`: Local directory where model files will be saved (default: ~/downloads)
- `-w, --max-workers`: Maximum number of concurrent workers for downloading files (default: 8)
- `-c, --config`: JSON-formatted configuration string with provider-specific settings

### Examples

Download a model from Hugging Face:
```bash
docker run --rm -v ./models:/output kthena-downloader:latest --source "microsoft/phi-2" --output-dir /output
```

Download a model from S3:
```bash
docker run --rm -v ./models:/output kthena-downloader:latest --source "s3://my-bucket/models/llama3" --output-dir /output --config '{"access_key":"YOUR_KEY", "secret_key":"YOUR_SECRET"}'
```

Download a model from PVC:
```bash
docker run --rm -v ./local-models:/output kthena-downloader:latest --source "pvc://models" --output-dir /output
```

Download a model from ModelScope:
```bash
docker run --rm -v ./models:/output kthena-downloader:latest --source "ms://Qwen/Qwen2.5-7B-Instruct" --output-dir /output
```

## Configuration

Configuration can be provided through environment variables (using Docker's `-e` flag) or the `--config` parameter:

### Environment Variables

```bash
docker run --rm \
  -e HF_AUTH_TOKEN="your_huggingface_token" \
  -e HF_ENDPOINT="custom_endpoint" \
  -e HF_REVISION="main" \
  -e MS_TOKEN="your_modelscope_token" \
  -e MS_REVISION="master" \
  -e ACCESS_KEY="your_access_key" \
  -e SECRET_KEY="your_secret_key" \
  -e ENDPOINT="your_endpoint_url" \
  -v ./models:/output \
  kthena-downloader:latest \
  --source "<namespace>/<repo_name>" \
  --output-dir /output
```

### Configuration JSON Example

```json
{
  "hf_token": "your_huggingface_token",
  "hf_endpoint": "custom_endpoint",
  "hf_revision": "main",
  "ms_token": "your_modelscope_token",
  "ms_revision": "master",
  "access_key": "your_access_key",
  "secret_key": "your_secret_key",
  "endpoint": "your_endpoint_url"
}
```

## License

This project is open-sourced under the Apache License 2.0. See the [LICENSE](LICENSE) file for details.