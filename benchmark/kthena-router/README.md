# Benchmark for kthena-router

This tool is based on [`sglang/bench_serving.py`](https://github.com/sgl-project/sglang/blob/main/python/sglang/bench_serving.py). We have packaged it as a Docker image and made it runnable as a Kubernetes Job for easy benchmarking in a cluster environment.

## Router-only benchmarking (no GPU)

To measure **kthena-router** (scheduling, routing, filters) without a real LLM engine, use the existing mock vLLM image and example manifests:

- Image: `ghcr.io/faust-benchou/dynamo-mocker-vllm:latest` (`docker pull ghcr.io/faust-benchou/dynamo-mocker-vllm:latest`)
- Manifests: [`examples/kthena-router/LLM-Mock.yaml`](../../examples/kthena-router/LLM-Mock.yaml) (Deployment + ModelServer + ModelRoute)

### Cluster smoke run

1. Install kthena-router (Helm chart or `hack/local-up-kthena.sh`).
2. Deploy the mock backend and CRDs:

   ```bash
   kubectl apply -f examples/kthena-router/LLM-Mock.yaml
   ```

3. Run a short load test through the router:

   ```bash
   kubectl apply -f benchmark/kthena-router/job-mock-smoke.yaml
   kubectl logs -f job/benchmark-mock-smoke
   ```

   `job-mock-smoke.yaml` uses the `random` dataset with low QPS so it finishes quickly. Adjust `--host`, namespace, or `--model` if your ModelRoute name differs.

The full end-to-end path in `job.yaml` still targets a real inference stack; use the mock path above when you only care about router behaviour.

### Metrics to watch

While traffic goes through the router, scrape or query Prometheus metrics such as:

- `kthena_router_requests_total`
- `kthena_router_request_duration_seconds`
- `kthena_router_request_prefill_duration_seconds` / `kthena_router_request_decode_duration_seconds` (when emitted for your path)
- `kthena_router_scheduler_plugin_duration_seconds`

Definitions live in `pkg/kthena-router/metrics`.

### Usage Notes

- **Hugging Face Downloads:**  
  The tool downloads some model/configuration files from Hugging Face at runtime. If you are running in an environment without public internet access, you must pre-download these files and include them in your Docker image.

- **Tokenizer Parameter:**  
  - On a public network, you can set the `--tokenizer` parameter to the model name (e.g., `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`).
  - In a restricted network, set `--tokenizer` to the path of the pre-downloaded tokenizer.  
    For example, for `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`, use:  
    ```
    /root/.cache/huggingface/hub/models--deepseek-ai--DeepSeek-R1-Distill-Qwen-7B/snapshots/916b56a44061fd5cd7d6a8fb632557ed4f724f60/
    ```
  - How to pre-download tokenizer: Run the container on a public network using the model name as the `--tokenizer` parameter for testing. Once the test runs normally, the container will contain the tokenizer. At this point, commit the container to a new image, so that the image will include the corresponding tokenizer. 

- **HostPath Volume for Caching:**  
  We use a `hostPath` volume to cache data generated during benchmarking, especially for datasets like `"generated-shared-prefix"`. Generating test data based on the tokenizer can be time-consuming, but by caching the results on the host, subsequent runs are much faster. You can use any Kubernetes-supported storage type for caching, not just `hostPath`.
