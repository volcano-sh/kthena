---
title: "EPD Disaggregation: Route Requests Through Encode, Prefill, and Decode Pods"
authors:
- "Shivansh Sahu"
reviewers:
- TBD
approvers:
- TBD
creation-date: 2026-03-19
---

## Support Configurable Orchestrated Inference Pipeline

### Summary

This proposal introduces a configurable Encode-Prefill-Decode (EPD) inference pipeline framework for the Kthena Router. It transforms the current implicit, hardcoded prefill-decode inference flow into a flexible execution model. By introducing a `PipelineMode` configuration in `ModelServerSpec`, users can explicitly configure the execution semantics to support multi-stage pipelines (e.g., `epd`). This architecture segregates specific roles—like encoding, prefilling, and decoding—into separate logical entities and routes requests through native KV connectors designed for multi-stage inference workflows.

### Motivation

As LLM serving architectures become more complex, standard Prefill-Decode (PD) disaggregation is no longer sufficient for all deployment patterns. Modern inference engines like vLLM and SGLang are adopting advanced techniques such as Encode-Prefill-Decode (EPD) to support multi-modal models (vision-language models, audio-language models), specialized prompt encoders, and multi-stage execution.

Currently, the Kthena Router is tightly coupled to the PD disaggregated model, implicitly inferring roles based purely on `prefillLabels` and `decodeLabels`. This rigid design prevents users from deploying EPD architectures where a dedicated encode stage is required before prefilling and decoding.

#### Goals

- Extend `ModelServerSpec` to support a configurable `PipelineMode` property.
- Support explicit tracking and scheduling of `Encode` pods alongside `Prefill` and `Decode` pods by extending the existing `PDGroup` construct.
- Introduce a generic `ProxyEPD` mechanism inside the `KVConnector` interface to natively orchestrate EPD flows.
- Extend router metrics to cover the new encode phase (`StartEncodePhase`, `FinishEncodePhase`).
- Provide a clear, extensible pathway for supporting future pipeline orchestration models without breaking existing PD flows.

#### Non-Goals

- Deprecating or removing the existing default PD disaggregated flow.
- Implementing complex, generic DAG-based inference graphs beyond EPD.
- Implementing the `ProxyEPD` internals for all inference engines immediately. Connectors that do not yet support EPD natively (like SGLang or NIXL) will initially return a `NotImplemented` error to fail fast and prevent silent misrouting.

### Proposal

We propose extending the ModelServer CRD to explicitly declare its execution pipeline mode and expanding the routing/scheduling data plane to support a three-tier Encode-Prefill-Decode (EPD) model.

#### User Stories

##### Story 1: Running Vision-Language Models (VLMs) with EPD
A user wants to deploy a Vision-Language Model (VLM) using vLLM. They deploy three distinct sets of pods: one set for encoding images into embeddings, one set for prompt prefilling, and one set for autoregressive decoding. They configure the `ModelServer` with `pipelineMode: epd` and define `encodeLabels`, `prefillLabels`, and `decodeLabels`. The Kthena Router seamlessly routes requests through the three stages, maximizing utilization of the specialized encode GPUs.

##### Story 2: Multi-Modal Audio/Text Processing
A user wants to deploy a multi-modal model that can accept both audio and text inputs. The heavy audio processing needs to be performed on dedicated, optimized hardware. The user tags the audio processing nodes with the `encode` role, and standard compute nodes with the `prefill` and `decode` roles, ensuring audio embeddings are passed efficiently down the pipeline.

### Design Details

#### API Changes (`ModelServerSpec`)

We will introduce a `PipelineMode` string enum into `ModelServerSpec`:

```go
type PipelineMode string

const (
    PipelineModeEPD PipelineMode = "epd"
)

type ModelServerSpec struct {
    // ... existing fields ...
    
    // PipelineMode defines the inference execution flow.
    // Defaults to implicit PD disaggregation if unset.
    PipelineMode *PipelineMode `json:"pipelineMode,omitempty"`
}
```

We will also update `PDGroup` to include `EncodeLabels`. (While `PDGroup` historically meant Prefill-Decode, it is effectively acting as our pipeline grouping construct. We are adding `EncodeLabels` here to keep the API surface minimal and backwards compatible, rather than inventing a new `PipelineGroup` abstraction).

```go
type PDGroup struct {
    // ... existing fields ...
    EncodeLabels map[string]string `json:"encodeLabels,omitempty"`
}
```

#### Complete ModelServer Example

To configure an EPD pipeline, a user must specify the `epd` pipeline mode and define all three label sets under the `pdGroup` configuration:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
metadata:
  name: vllm-epd-server
  namespace: default
spec:
  model: vlm-model-name
  inferenceEngine: vLLM
  pipelineMode: epd
  workloadSelector:
    pdGroup:
      groupKey: app
      encodeLabels:
        role: encode
      prefillLabels:
        role: prefill
      decodeLabels:
        role: decode
```

#### Datastore and Scheduling Updates

1. **Role Categorization**: The router datastore will be updated to watch and categorize pods matching `encodeLabels` as encode pods, tracking their endpoints and in-flight requests similarly to prefill and decode pods. To prevent scheduling overlaps, label checking will use exclusive assignment (`if / else if`)—a single pod cannot operate in multiple pipeline stages simultaneously.
2. **Scheduling Matrix**: The `Scheduler` will be expanded to form `(encode, prefill, decode)` triples when `PipelineMode` is an EPD mode.
   - If an EPD mode is configured, the scheduler will strictly require valid `EncodeLabels` and active encode pods. Missing encode pods will cause the scheduling to fail early.

#### KVConnector and Metrics Updates

A new method `ProxyEPD` will be added to the `KVConnector` interface:

```go
ProxyEPD(c *gin.Context, reqBody map[string]interface{}, encodeAddr, prefillAddr, decodeAddr string, hooks *OnFlightHooks) (int, error)
```

- When `pipelineMode` is `epd`, the `router` will dispatch requests to `ProxyEPD` rather than the standard two-stage `Proxy`.
- Connectors that do not yet support EPD will return `0, fmt.Errorf("EPD is not natively implemented for <ConnectorName> yet")`, ensuring explicit failures.
- The `RequestMetricsRecorder` will be updated to capture the encode phase timing, allowing observability platforms to track the latencies introduced by the new pipeline stage.

#### Test Plan

- **Unit Tests**: Add tests verifying that `EncodePods` are correctly parsed and categorized in the datastore, and that multi-role assignments are prevented.
- **Scheduler Tests**: Validate that the scheduler enforces `EncodeLabels` prerequisites when `PipelineMode` is set, and correctly falls back or errors when encode pods are missing.
- **Router E2E Tests**: Introduce End-to-End tests simulating a multi-stage workload, verifying that requests with `epd` are routed to the `proxyToEPDDisaggregated` codepath and hit the `ProxyEPD` connector function successfully.

### Alternatives

1. **Implicit EPD deduction**: We considered implicitly deducing EPD mode if `encodeLabels` were present on a `PDGroup`. This was rejected because it reduces explicit user intent and makes debugging harder when labels are accidentally omitted or mismatched.
2. **Generic DAG Configuration**: We considered allowing users to supply an arbitrary DAG YAML defining custom stages (e.g., `schedule -> execute -> custom -> decode`). This was rejected as overly complex for the immediate requirement of supporting EPD for major engines like vLLM and SGLang.
3. **Renaming PDGroup**: We considered renaming `PDGroup` to `PipelineGroup` to better reflect the three-stage nature of EPD. This was rejected to maintain backwards compatibility with the existing v1alpha1 API, but is recommended for a future API version bump (e.g. v1beta1).
