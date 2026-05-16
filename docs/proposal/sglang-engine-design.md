---
title: First-Class SGLang Support in Kthena
authors:
- "@kube-gopher"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-05-17

---

## First-Class SGLang Support in Kthena

### Summary

Kthena supports the vLLM inference engine end-to-end across five layers: API
types, the ModelBooster → ModelServing converter, embedded launch templates,
the router backend adapter, KV connectors, and the tokenizer adapter. SGLang
previously had only partial, router-side support and was rejected by the
ModelBooster converter.

This proposal lifts SGLang to a first-class backend. The user-visible outcome:
a user declares a `ModelBooster` with `backend.type: SGLang` or
`SGLangDisaggregated` and gets a working aggregated or PD-disaggregated SGLang
deployment with the same scheduler-plugin behavior, drain semantics, gang
scheduling, and PD-disaggregation tooling that vLLM ModelBoosters get — the
router automatically selects the SGLang KV connector and a dedicated SGLang
tokenizer adapter.

### Motivation

SGLang already had partial integration: the router-side metrics scraper
(`pkg/kthena-router/backend/sglang/metrics.go`), the PD bootstrap connector
(`pkg/kthena-router/connectors/sglang.go`), the `InferenceEngine: SGLang` enum,
and example CRs. However, the ModelBooster converter
(`pkg/model-booster-controller/convert/model_server.go`) returned
"please use vLLM backend", the `ModelBackendType` kubebuilder enum did not
permit SGLang, there was no SGLang launch template or tokenizer adapter, and
the webhook blocked disaggregated SGLang topologies. As a result, users could
not declaratively run SGLang through the platform's primary workload CRD.

#### Goals

- **Aggregated SGLang**: `ModelBooster` with `backend.type: SGLang`
  materializes a `ModelServing` running a single-node SGLang server
  (`sglang.launch_server`).
- **Disaggregated SGLang (PD)**: `ModelBooster` with
  `backend.type: SGLangDisaggregated` materializes prefill + decode roles wired
  up for SGLang's `bootstrap_room`/`bootstrap_host` KV-handoff protocol.
- **Full router integration**: SGLang pods participate in metric-aware
  scheduling, prefix/KV-cache-aware scoring, and OpenAI-compatible request
  proxying — selecting the SGLang KV connector automatically.
- **Dedicated tokenizer adapter**: SGLang's `/tokenize` endpoint is used
  directly for prefix-cache scoring rather than reusing the vLLM adapter.

#### Non-Goals

- **Multi-node SGLang (Ray/torchrun)**: out of scope for v1. Both the
  aggregated and disaggregated builders return an explicit error if
  `Pods > 1`.
- **Native SGLang chat-tokenize**: the adapter flattens chat messages to a
  role-prefixed text string. Adding a first-party chat-tokenize call is
  deferred until SGLang ships such an endpoint.
- **SGLang version-portable metric names**: the router-side metric names match
  SGLang ≥ 0.4.x. A config-driven metric-name map for older versions is out of
  scope.

### Proposal

A user creating a `ModelBooster` selects an SGLang backend type and receives a
fully materialized, router-integrated deployment without authoring any
low-level `ModelServing` YAML — exactly mirroring the existing vLLM experience.

#### User Stories

##### Story 1 — Aggregated SGLang

A user submits a `ModelBooster` with `backend.type: SGLang` and a single
`server` worker. The controller materializes a `ModelServing` with the kthena
`runtime` sidecar + an `engine` container running
`python3 -m sglang.launch_server`. The router discovers the pod, scrapes
`sglang:*` metrics, and proxies OpenAI-compatible requests to it.

##### Story 2 — Disaggregated SGLang (PD)

A user submits a `ModelBooster` with `backend.type: SGLangDisaggregated` and
`prefill` + `decode` workers. The controller materializes two roles wired for
SGLang's bootstrap KV-handoff. The router selects `ConnectorTypeSGLang`
automatically and runs the prefill/decode bootstrap protocol.

#### Notes/Constraints/Caveats

- The engine binds `--host 0.0.0.0` (not `$(POD_IP)`). This is required so the
  runtime sidecar and the preStop drain can reach the engine on
  `localhost:30000`.
- `getKvConnectorSpec` returns `nil` for SGLang on purpose: SGLang's KV handoff
  is selected by the router from `InferenceEngine`, not from a user-supplied
  `kv-transfer-config` blob. Producing `KVConnector: nil` is the contract that
  makes end-to-end PD work with no extra code.
- The webhook permits a prefill-only disaggregated spec while the converter
  errors on it — this split deliberately matches the existing
  `vLLMDisaggregated` precedent.
- The preStop drain hook and the `/dev/shm` (memory-medium) volume are emitted
  for **aggregated** SGLang only, not for the disaggregated `prefill`/`decode`
  roles. This matches the established kthena template convention: `vllm.yaml`,
  `vllm-pd.yaml`, and `sglang-pd.yaml` all omit both, so the asymmetry is the
  aggregated path being *enriched*, not the PD path being deprived. PD pods
  have a distinct termination contract (bootstrap-room teardown / KV-transfer
  completion) that a request-count drain does not model; extending graceful
  drain to PD is tracked as a follow-up rather than silently reusing the
  aggregated script.
- The aggregated preStop drain is bounded to ~300s (60 × 5s) and aligned with
  `terminationGracePeriodSeconds`, sums all metric series (multi-model safe),
  uses an integer comparison (immune to Prometheus `0` vs `0.0` formatting),
  and exits immediately if the metrics endpoint is unreachable — so a
  stuck/absent metric can never block termination past the grace period.

#### Risks and Mitigations

- **`--host 0.0.0.0` exposure**: binding all interfaces is broader than
  `$(POD_IP)`. Mitigation: the engine port (30000) is not exposed via a
  Service unless the converter emits one; pod network policy governs reach
  ability, identical to the vLLM path which also binds broadly.
- **Metric-name version drift**: preStop drain relies on
  `sglang:num_running_reqs` / `sglang:num_queue_reqs`. These match the shipped
  `backend/sglang/metrics.go` (SGLang ≥ 0.4.x). Mitigation: documented as a
  Non-Goal; a config-driven name map is the follow-up if older versions must be
  supported.
- **Chat-tokenize approximation**: flattening chat to a role-prefixed string
  can diverge from a model's true chat template, slightly skewing prefix-cache
  scoring (a scheduling hint, not correctness). Mitigation: scoped to scoring
  only; swap for a native endpoint when available.
- **No GPU e2e coverage**: SGLang needs a GPU, so e2e asserts CR
  materialization only. Mitigation: Layer 5 GPU verification steps are
  documented for manual/hardware runs; unit + golden-file tests pin the
  generated manifest shape.

### Design Details

#### API Types

`pkg/apis/workload/v1alpha1/model_booster_types.go` — the `ModelBackendType`
kubebuilder enum is narrowed to exactly the supported set:

```
+kubebuilder:validation:Enum=vLLM;vLLMDisaggregated;SGLang;SGLangDisaggregated
```

New const `ModelBackendTypeSGLangDisaggregated`. `make generate` regenerates
the `modelboosters` CRD and the CRD enum docs.

#### ModelBooster → ModelServing Converter

`pkg/model-booster-controller/convert/`:

| Helper | Role |
|---|---|
| `buildSGLangModelServing` | Aggregated SGLang. Emits a single `leader` role with the kthena `runtime` sidecar + `engine` container running `python3 -m sglang.launch_server --model-path ... --host 0.0.0.0 --port 30000 --enable-metrics`. Binding `0.0.0.0` lets the runtime sidecar and the preStop drain reach the engine on `localhost:30000`. PreStop hook drains via `sglang:num_running_reqs` / `sglang:num_queue_reqs`. |
| `buildSGLangDisaggregatedModelServing` | Two roles — `prefill` and `decode` — with `--disaggregation-mode <role> --disaggregation-transfer-backend mooncake` appended. Gang policy requires both roles. Multi-node (`Pods > 1`) is rejected, matching the aggregated path. |
| `buildSGLangCommands` | Shared command builder; user-supplied worker `Config` is flattened to POSIX flags by the renamed `utils.ConvertEngineArgsFromJson`. |

Engine-aware `WorkloadPort` resolution: `getEnginePort(backend.Type)` returns
8000 for vLLM, 30000 for SGLang. The vLLM `VLLM_USE_V1=1` env var is now only
injected for vLLM-typed backends.

`getKvConnectorSpec` short-circuits for SGLang: it returns `nil` because
SGLang's KV handoff is selected by the router based on `InferenceEngine`, not
by the user-supplied `kv-transfer-config` blob.

#### Launch Templates

Two new embed.FS templates parallel the vLLM ones:

- `templates/sglang.yaml` — aggregated. Single role with runtime sidecar +
  engine container; readiness probe `:30000/health`, preStop drain on
  SGLang-named metrics.
- `templates/sglang-pd.yaml` — disaggregated. Prefill + decode roles, both
  exposing port 30000. Mirrors the vLLM disaggregated topology and gang
  policy.

Multi-node SGLang (Ray/torchrun) is out of scope for v1;
`buildSGLangModelServing` errors out if `Pods > 1`.

#### Router Tokenizer Adapter

`pkg/kthena-router/scheduler/plugins/tokenization/`:

- `sglang.go` — new adapter against SGLang's `POST /tokenize` endpoint.
  Defensive response parsing accepts `token_ids`, `tokens`, or `input_ids`
  field names so the adapter survives SGLang's field-name drift across
  versions. Chat inputs are flattened to a single role-prefixed text prompt
  since SGLang has no native chat tokenize endpoint.
- `tokenizer.go` — `NewRemoteTokenizer` is now engine-aware
  (`vllm`/`sglang`/empty=vLLM-default).
- `tokenizer_manager.go` — per-pod routing:
  `engineEndpointForPod(podIP, podInfo.GetEngine())` resolves the right port
  (8000 for vLLM, 30000 for SGLang) and adapter. Pods with unknown engines are
  skipped, not silently routed to a default.
- `kvcache_aware.go` — `EnableVLLMRemote` config knob renamed to
  `EnableRemoteTokenizer`; the engine and endpoint are resolved per-pod, not
  from a static template string.

#### Webhook

`pkg/model-booster-controller/webhook/model_validator.go` — new rule mirroring
`vLLMDisaggregated`: `SGLangDisaggregated` backends must have all workers typed
`prefill` or `decode`. Aggregated SGLang reuses the existing "exactly one
server worker" branch.

#### KV Connector Selection (no change to existing code)

The router at `pkg/kthena-router/router/router.go:944` already auto-selects
`ConnectorTypeSGLang` whenever `ModelServer.Spec.InferenceEngine == SGLang` and
no explicit `KVConnector` is set. The converter producing `KVConnector: nil`
for SGLang is the contract that makes this work end-to-end.

The SGLang connector (`pkg/kthena-router/connectors/sglang.go`) handles the
bootstrap protocol:

- Generates a unique `bootstrap_room` (random int63) shared by the prefill and
  decode requests.
- Sets `bootstrap_host` to the prefill pod's IP on the decode request only —
  that is where the decode receiver opens the bootstrap HTTP connection to
  fetch ZMQ endpoint metadata.
- Runs prefill and decode concurrently; cancels the prefill context if decode
  fails so the prefill bootstrap server doesn't hang.

#### Test Plan

##### Layer 1 — Static / Generation (no env)

- `make gen-crd` regenerates the modelboosters CRD with the new enum members.
- `./hack/update-codegen.sh` is a no-op for SGLang (no API struct shape
  changed) but is run for CI parity.
- `make gen-docs` refreshes
  `docs/kthena/docs/reference/crd/workload.serving.volcano.sh.md` with the
  widened enum.
- `make gen-check` confirms the above produce no drift beyond the source-code
  changes.
- `make lint` (golangci-lint v1.64.8) — clean.

##### Layer 2 — Unit tests

| Suite | Coverage |
|---|---|
| `convert/model_serving_test.go` `TestCreateModelServingResources` | Golden-file diff: `testdata/input/sglang-{aggregated,disaggregated}-model.yaml` → `testdata/expected/sglang-{aggregated,disaggregated}-model-serving.yaml`. Any converter change not captured in a fixture update fails the suite. |
| `convert/model_serving_test.go` `TestBuildSGLang*` | Structural assertions: command flags, `--host 0.0.0.0`, `--port 30000`, no `POD_IP`/`VLLM_USE_V1` env, disaggregation flags appear only on PD roles. |
| `convert/model_server_test.go` `TestBuildSGLang*Routing` | `WorkloadPort.Port == 30000`, `InferenceEngine == SGLang`, `KVConnector` nil, `PDGroup` only set for disaggregated. |
| `webhook/model_validator_test.go` `TestValidateBackendWorkerTypes_SGLang` | Aggregated requires one server worker; Disaggregated requires prefill+decode-only. |
| `tokenization/tokenization_test.go` `TestSGLangAdapter` | Completion + chat request shapes; response parsing for all three SGLang field-name variants. |
| `tokenization_test.go` `TestEngineEndpointForPod` | Per-engine port and adapter resolution. |
| `tokenization_test.go` `TestTokenizerManager_GetTokenizer_PicksSGLangPort` | End-to-end manager path picks port 30000 for an SGLang pod. |
| `router/router_test.go` `TestRouter_HandlerFunc_SGLangAggregated` | Aggregated SGLang request proxies to the backend with no bootstrap mutation. |
| `router/router_test.go` `TestRouter_HandlerFunc_SGLangDisaggregated` | Both requests fire concurrently, share the same `bootstrap_room`, decode carries `bootstrap_host` = prefill pod IP. |

##### Layer 3 — Controller-manager E2E (Kind, no GPU)

`test/e2e/controller-manager/model_booster_test.go`
`TestSGLangModelMaterialization`:

- Creates an SGLang and an SGLangDisaggregated `ModelBooster` in the test
  namespace.
- Waits for the controller to emit the `ModelServing`.
- Asserts on the structure of the materialized ModelServing — engine command,
  ports, env, disaggregation flags. **Does not** wait for pods to become Ready
  (SGLang needs a GPU).

##### Layer 4 — Router E2E (Kind, no GPU)

`test/e2e/router/` already exercises the router against fake backends per
engine. The unit-level router tests above
(`TestRouter_HandlerFunc_SGLang*`) cover the SGLang bootstrap protocol
semantics; the router E2E suite can be extended with a `ModelServer-sglang.yaml`
testdata fixture if a kind-based check is desired.

##### Layer 5 — GPU verification (requires real hardware)

Aggregated SGLang on a single-GPU node:

1. `kubectl apply -f examples/model-booster/sglang-aggregated.yaml`
2. `kubectl wait` for the `ModelBooster` `Active` condition (mirrors the vLLM
   `TestModelCR` shape).
3. `curl POST http://<router>/v1/chat/completions` against the served model
   name; assert a non-empty completion and sane `usage` token counts.
4. Scrape `/metrics` on the SGLang pod (port 30000); confirm
   `sglang:num_running_reqs` increments under sustained load and the preStop
   drain logic blocks termination while requests are in flight.

Disaggregated SGLang on a two-GPU node:

1. `kubectl apply -f examples/model-booster/sglang-disaggregated.yaml`
2. Verify both `prefill` and `decode` pods become `Ready`.
3. Send an inference request; confirm via `kubectl logs` that:
   - The prefill pod logs a bootstrap-room exchange (the decode receiver
     connecting).
   - The decode pod logs a successful KV-transfer receive on the same
     `bootstrap_room`.
4. Compare the streamed output token-by-token against the aggregated mode for
   the same prompt — catches semantic drift in the disaggregated KV-transfer
   path.

### Alternatives

- **Reuse the vLLM tokenizer adapter for SGLang.** Rejected: SGLang's
  `/tokenize` request/response schema and field names differ from vLLM and
  drift across SGLang versions; reusing the vLLM adapter would silently
  produce wrong tokenizations for prefix-cache scoring. A dedicated adapter
  with defensive response parsing is more robust.
- **Keep rejecting SGLang in the converter; require hand-written
  `ModelServing` YAML.** Rejected: that leaves SGLang a second-class citizen,
  duplicates the rollout/gang/drain logic users would have to hand-author, and
  diverges from the vLLM experience. The hand-written low-level path remains
  available but is no longer the only option.
- **Bind the engine to `--host $(POD_IP)`.** Rejected: the runtime sidecar and
  the preStop drain reach the engine over `localhost:30000`; binding only the
  pod IP breaks both. `--host 0.0.0.0` is required and matches the vLLM path.
- **Model SGLang KV handoff via the user-supplied `kv-transfer-config`
  (vLLM-style).** Rejected: SGLang's handoff is a bootstrap protocol selected
  by the router from `InferenceEngine`. Emitting `KVConnector: nil` and letting
  the router pick `ConnectorTypeSGLang` keeps the contract in one place and
  requires no new connector-config surface.
- **Single shared "engine args" enum / MindIE-style placeholder.** Rejected:
  narrowing the `ModelBackendType` enum to only what is wired
  (`vLLM;vLLMDisaggregated;SGLang;SGLangDisaggregated`) avoids advertising
  unimplemented backends in the CRD schema.
