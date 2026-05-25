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

This proposal lifts SGLang to a first-class backend in the control plane
(API, converter, templates, webhook). The user-visible outcome: a user
declares a `ModelBooster` with `backend.type: SGLang` or
`SGLangDisaggregated` and gets a working aggregated or PD-disaggregated SGLang
deployment with the same drain semantics, gang scheduling, and PD bootstrap
hand-off that vLLM ModelBoosters get; the router auto-selects the existing
SGLang KV connector based on `InferenceEngine`.

Router-side SGLang work (tokenizer adapter, per-engine endpoint resolution,
KVCacheAware knob rename) is intentionally out of scope here and is tracked in
a separate PR.

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
- **Router KV-connector selection**: the existing router auto-selects
  `ConnectorTypeSGLang` whenever `ModelServer.Spec.InferenceEngine == SGLang`
  and `KVConnector` is nil. The converter cooperating with this contract
  (emitting `KVConnector: nil` for SGLang) is in scope here; no new router
  code is added.

#### Non-Goals

- **Multi-node SGLang (Ray/torchrun)**: out of scope for v1. Both the
  aggregated and disaggregated builders return an explicit error if
  `Pods > 1`.
- **Router-side SGLang tokenizer adapter, per-engine endpoint resolution,
  KVCacheAware knob rename**: tracked in a separate PR and not landed here.
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
- The preStop drain hook and the memory-medium `/dev/shm` volume are emitted
  on **all** SGLang engine containers — aggregated `leader`, and both
  disaggregated `prefill` and `decode`. The drain is bounded to ~300s
  (60 × 5s) and aligned with `terminationGracePeriodSeconds`, sums all
  `sglang:num_running_reqs` / `sglang:num_queue_reqs` series (multi-model
  safe), uses an integer comparison (immune to Prometheus `0` vs `0.0`
  formatting), and exits immediately if the metrics endpoint is unreachable —
  so a stuck/absent metric can never block termination past the grace period.
  The shared `dshm` volume is required by the mooncake transfer engine and
  NCCL in the disaggregated path and is harmless in the aggregated path.
- The kthena runtime sidecar receives `--engine sglang` for both aggregated
  and disaggregated SGLang. The Python runtime's `_get_real_engine_type`
  collapses `vllm`/`vllmdisaggregated` → `EngineType.VLLM` but only recognizes
  the literal string `sglang`; emitting `sglangdisaggregated` would crash the
  sidecar on startup with `UnsupportedEngineError`. The converter centralizes
  this in a `runtimeEngineName(backendType)` helper.

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

#### Router Tokenizer Adapter (out of scope here)

A dedicated SGLang tokenizer adapter (`pkg/kthena-router/scheduler/plugins/
tokenization/sglang.go`), engine-aware `NewRemoteTokenizer`, per-engine
endpoint resolution in `tokenizer_manager.go`, and the `EnableVLLMRemote` →
`EnableRemoteTokenizer` rename in `kvcache_aware.go` are tracked in a separate
PR. Until that lands, the router's KVCacheAware plugin continues using its
existing vLLM-only tokenizer path for SGLang pods. This PR ships only the
control-plane slice (API + converter + templates + webhook).

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
| `convert/model_serving_test.go` `TestBuildSGLangModelServing` / `TestBuildSGLangDisaggregatedModelServing` | Structural assertions: engine container named `engine`, command flags, `--host 0.0.0.0`, `--port 30000`, no `POD_IP`/`VLLM_USE_V1` env, preStop drain on `sglang:num_running_reqs`, memory-backed `/dev/shm`, `terminationGracePeriodSeconds`. Disaggregated test also asserts the runtime sidecar receives `--engine sglang` (never `sglangdisaggregated`). |
| `convert/model_serving_test.go` `TestBuildSGLangModelServerRouting` / `TestBuildSGLangDisaggregatedModelServerRouting` | `WorkloadPort.Port == 30000`, `InferenceEngine == SGLang`, `KVConnector` nil, `PDGroup` only set for disaggregated. |
| `convert/model_serving_test.go` `TestBuildSGLangCommandsTransferBackendDedup` | `buildSGLangCommands` does not duplicate `--disaggregation-transfer-backend` when the user supplies it in `worker.Config` (in either dash or underscore key form); default `mooncake` is still applied when unset. |
| `webhook/model_validator_test.go` `TestValidateBackendWorkerTypes_SGLang` | Aggregated requires one server worker; Disaggregated requires prefill+decode-only. |
| `webhook/model_validator_test.go` `TestValidateSGLangReservedFlags` | Rejects worker.config keys the converter hard-codes (port/host/disaggregation-mode/...); allows non-reserved keys such as `disaggregation-transfer-backend`. |

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

Out of scope for this PR. The router-side SGLang tokenizer adapter and
per-engine endpoint resolution land in a separate PR; that PR will own the
matching router unit and E2E coverage. The pre-existing `pkg/kthena-router/
backend/sglang/metrics.go`, `connectors/sglang.go`, and router KV-connector
auto-selection on `InferenceEngine == SGLang` are unchanged here.

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

### Known Limitations / Follow-ups

These are intentionally out of scope for the first PR but tracked here so they
are not lost:

- **Readiness probe uses `/health`, not `/health_generate`.** SGLang's
  `/health` returns 200 once the HTTP process is up — *before* model weights
  finish loading — so the router can briefly route to a pod that 503s. SGLang
  also exposes `/health_generate`, which runs an actual generation and only
  succeeds once the model is serving. A follow-up should switch the *readiness*
  probe to `/health_generate` (keeping `/health` for liveness) so traffic is
  gated on true model-readiness. Deferred because the longer warmup cost of
  `/health_generate` needs measuring against the `initialDelaySeconds` budget.
- **`--served-model-name` is not auto-defaulted.** If the user omits
  `served-model-name` in `worker.config`, SGLang advertises the model under its
  on-disk path on `/v1/models`, so the auto-created `ModelRoute` (whose
  `modelName` is the ModelBooster name) will not match and routing fails. The
  examples set it explicitly. A follow-up should inject
  `--served-model-name <model.Name>` when the user did not supply one, matching
  what the router expects.
- **Disaggregated bootstrap wiring is minimal.** `buildSGLangCommands` emits
  only `--disaggregation-mode` and `--disaggregation-transfer-backend`. The
  SGLang bootstrap server port (default 8998) is not exposed as a
  `containerPort`, and the engine binds `--host 0.0.0.0` rather than the pod
  IP. The router's `ConnectorTypeSGLang` injects `bootstrap_host`/
  `bootstrap_room` per request, so aggregated and router-orchestrated PD work,
  but a fuller PD design (explicit bootstrap port exposure, host advertisement)
  is needed before disaggregated SGLang is GPU-validated. Disaggregated is not
  part of the tested path in the first PR.
- **`spec.topologySpreadConstraints` in the launch templates is dead config.**
  `ModelServing.Spec` has no `topologySpreadConstraints` field, so the block
  present in `vllm.yaml`/`sglang.yaml` is silently dropped by
  `loadModelServingTemplate`. It was deliberately *not* carried into
  `sglang-pd.yaml` to avoid misleading no-op YAML. Real anti-affinity/spread
  for engine pods should be expressed at the pod template
  (`entryTemplate.spec.topologySpreadConstraints`, a real `PodSpec` field) for
  both vLLM and SGLang in a separate change.
- **Multi-node (Pods > 1).** Explicitly rejected at convert time with a clear
  error; no Ray/torchrun bootstrap template is provided for SGLang in v1.
