# TensorRT-LLM Prefill-Decode Disaggregation (GPU)

Deploy TensorRT-LLM with **native NIXL KV transfer** under Kthena PD routing.

This path mirrors NVIDIA `trtllm-serve` disaggregated serving: workers enable
`cache_transceiver_config.backend: NIXL`, and the Kthena router (when
`inferenceEngine: TensorRTLLM` and `pdGroup` is set) runs context-first
orchestration via `disaggregated_params` (`context_only` → `generation_only`).
It is **not** the generic HTTP connector that only sends `max_tokens=1` and
recomputes KV on decode.

## Examples

- ModelServing + ModelServer + ModelRoute: [`examples/model-serving/gpu-pd-tensorrtllm.yaml`](../../../../../examples/model-serving/gpu-pd-tensorrtllm.yaml)
- Standalone ModelServer: [`examples/kthena-router/ModelServer-tensorrtllm-pd.yaml`](../../../../../examples/kthena-router/ModelServer-tensorrtllm-pd.yaml)

## Worker requirements

Prefill `extra_llm_api_options` must include:

```yaml
disable_overlap_scheduler: true
cache_transceiver_config:
  backend: NIXL
```

Decode must include the same `cache_transceiver_config`. Confirm workers log
`Using KvCacheTransceiverV2` (or equivalent) after start.

Single-node GPUs typically use UCX/CUDA IPC; multi-node needs RDMA-capable
networking. Grant `IPC_LOCK` on the containers when required by NIXL/UCX.

## Router behavior

Omit `kvConnector` on the ModelServer. With `inferenceEngine: TensorRTLLM`,
the router selects the internal TensorRT-LLM connector automatically (same
pattern as SGLang). Do **not** set `kvConnector.type: nixl` — that path is
vLLM `kv_transfer_params`, not TensorRT-LLM `disaggregated_params`.
