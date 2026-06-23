---
title: Support Configurable Orchestrated Inference Pipeline
authors:
- "@PadawanZH"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-03-19

---

## Support Configurable Orchestrated Inference Pipeline

### Summary

This proposal introduces a configurable and orchestratable inference pipeline framework for Kthena Router. It transforms the current hardcoded prefill-decode inference flow processing into a flexible execution model. By introducing a `PipelineMode` configuration in `ModelServerSpec`, users can explicitly configure the execution semantics for different LLM inference engines (e.g., `vllm-epd`, `sglang-epd`, or standard `pd-disaggregated`). This architecture segregates specific roles—like encoding, prefilling, and decoding—into separate logical entities and routes requests through native KV connectors designed for multi-stage inference workflows.

### Motivation

As LLM serving architectures become more complex, standard Prefill-Decode (PD) disaggregation is no longer sufficient for all deployment patterns. Modern inference engines like vLLM and SGLang are adopting advanced techniques such as Encode-Prefill-Decode (EPD) to support modalities like vision-language models, specialized prompt encoders, and multi-stage execution. 

Currently, the Kthena Router is tightly coupled to the PD disaggregated model, implicitly inferring roles based purely on `prefillLabels` and `decodeLabels`. This rigid design prevents users from deploying EPD architectures where a dedicated encode stage is required before prefilling and decoding.

#### Goals

- Extend `ModelServerSpec` to support a configurable `PipelineMode` property.
- Support explicit tracking and scheduling of `Encode` pods alongside `Prefill` and `Decode` pods.
- Introduce a generic `ProxyEPD` mechanism inside the `KVConnector` interface to natively orchestrate EPD flows.
- Provide a clear, extensible pathway for supporting future pipeline orchestration models without breaking existing PD flows.

#### Non-Goals

- Deprecating or removing the existing `pd-disaggregated` default flow.
- Implementing complex, generic DAG-based inference graphs beyond EPD.
- Implementing the `ProxyEPD` internals for all inference engines immediately (some engines will initially return `NotImplemented`).

### Proposal

We propose extending the ModelServer CRD to explicitly declare its execution pipeline mode and expanding the routing/scheduling data plane to support a three-tier Encode-Prefill-Decode (EPD) model.

#### User Stories

##### Story 1: Running Vision-Language Models with EPD
A user wants to deploy a Vision-Language Model (VLM) using vLLM. They deploy three distinct sets of pods: one set for encoding images into embeddings, one set for prompt prefilling, and one set for autoregressive decoding. They configure the `ModelServer` with `pipelineMode: vllm-epd` and define `encodeLabels`, `prefillLabels`, and `decodeLabels`. The Kthena Router seamlessly routes requests through the three stages, maximizing utilization of the specialized encode GPUs.

##### Story 2: Explicit PD Disaggregation Configuration
A user deploying standard text-generation models prefers explicit configuration over implicit defaults. They set `pipelineMode: pd-disaggregated`, clearly signaling to operators and internal tooling that the deployment uses a strict two-stage pipeline.

### Design Details

#### API Changes (`ModelServerSpec`)

We will introduce a `PipelineMode` string enum into `ModelServerSpec`:

```go
type PipelineMode string

const (
    PipelineModeEPD PipelineMode = "vllm-epd"
    PipelineModeSGLangEPD PipelineMode = "sglang-epd"
    PipelineModePDDisaggregated PipelineMode = "pd-disaggregated"
)

type ModelServerSpec struct {
    // ... existing fields ...
    
    // PipelineMode defines the inference execution flow.
    // Defaults to implicit PD disaggregation if unset.
    PipelineMode *PipelineMode `json:"pipelineMode,omitempty"`
}
```

We will also update `PDGroup` to include `EncodeLabels`:
```go
type PDGroup struct {
    // ... existing fields ...
    EncodeLabels map[string]string `json:"encodeLabels,omitempty"`
}
```

#### Datastore and Scheduling Updates

1. **Role Categorization**: The router datastore will be updated to watch and categorize pods matching `encodeLabels` as encode pods, tracking their endpoints and in-flight requests similarly to prefill and decode pods.
2. **Scheduling Matrix**: The `Scheduler` will be expanded to form `(encode, prefill, decode)` triples when `PipelineMode` is an EPD mode.
   - If an EPD mode is configured, the scheduler will strictly require valid `EncodeLabels` and active encode pods. Missing encode pods will cause the scheduling to fail early.

#### KVConnector Updates

A new method `ProxyEPD` will be added to the `KVConnector` interface:
```go
ProxyEPD(c *gin.Context, reqBody map[string]interface{}, encodeAddr, prefillAddr, decodeAddr string, hooks *OnFlightHooks) (int, error)
```

- When `pipelineMode` is `vllm-epd` or `sglang-epd`, the `router` will dispatch requests to `ProxyEPD` rather than the standard two-stage `Proxy`.
- Connectors that do not yet support EPD will return `0, fmt.Errorf("EPD is not natively implemented...")`, ensuring explicit failures rather than silent misrouting.

#### Test Plan

- **Unit Tests**: Add tests verifying that `EncodePods` are correctly parsed and categorized in the datastore.
- **Scheduler Tests**: Validate that the scheduler enforces `EncodeLabels` prerequisites when `PipelineMode` is set, and correctly falls back or errors when encode pods are missing.
- **Router E2E Tests**: Verify that requests with `vllm-epd` are routed to the `proxyToEPDDisaggregated` codepath and hit the `ProxyEPD` connector function. 

### Alternatives

1. **Implicit EPD deduction**: We considered implicitly deducing EPD mode if `encodeLabels` were present on a `PDGroup`. This was rejected because it reduces explicit user intent and makes debugging harder when labels are accidentally omitted or mismatched.
2. **Generic DAG Configuration**: We considered allowing users to supply an arbitrary DAG YAML defining custom stages (e.g., `schedule -> execute -> custom -> decode`). This was rejected as overly complex for the immediate requirement of supporting EPD for major engines like vLLM and SGLang.
