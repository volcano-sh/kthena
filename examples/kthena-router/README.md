# Kthena Router Examples

Each directory under `scenarios/` is an independently deployable Kustomize
target. The root target deploys the GPU-free simple-routing scenario:

```bash
kubectl apply -k examples/kthena-router
```

Deploy another scenario by selecting its directory, for example:

```bash
kubectl apply -k examples/kthena-router/scenarios/canary
```

Use the same target for cleanup:

```bash
kubectl delete -k examples/kthena-router/scenarios/canary
```

| Scenario                         | Kustomize target                        | Additional prerequisites                           |
| -------------------------------- | --------------------------------------- | -------------------------------------------------- |
| Simple routing                   | `scenarios/simple`                      | None                                               |
| LoRA-aware routing               | `scenarios/lora`                        | None                                               |
| Weighted canary routing          | `scenarios/canary`                      | None                                               |
| Header-based multi-model routing | `scenarios/multi-model`                 | None                                               |
| Local rate limiting              | `scenarios/local-rate-limit`            | None                                               |
| Global rate limiting             | `scenarios/global-rate-limit`           | Redis is included in `kthena-system`               |
| Mock PD disaggregation           | `scenarios/mock-pd-disaggregation`      | Volcano scheduler                                  |
| Ascend PD disaggregation         | `scenarios/ascend-pd-disaggregation`    | Volcano and Ascend-enabled nodes                   |
| Custom Gateway binding           | `scenarios/custom-gateway`              | Gateway API support and router Service port `8081` |
| Gateway Inference Extension      | `scenarios/gateway-inference-extension` | Gateway API and Inference Extension CRDs           |
| SGLang routing                   | `scenarios/sglang`                      | None                                               |
| Self-contained Llama mock        | `scenarios/llama`                       | None                                               |

Kthena must already be installed. The mock backends do not require GPUs, but
the PD disaggregation examples have the hardware requirements documented in
their corresponding user guides.
