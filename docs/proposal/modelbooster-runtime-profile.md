---
title: ClusterModelRuntime Template Override for ModelBooster
authors:
- "@xrwang8"
reviewers:
- TBD
approvers:
- TBD
creation-date: 2026-05-06
---

## ClusterModelRuntime Template Override for ModelBooster
Related issue: Refs #953

### Summary

Phase 1 focuses on template externalization only. It adds a small
cluster-scoped `ClusterModelRuntime` API that lets cluster operators provide an
optional `ModelServing` YAML template override for an existing `ModelBooster`
backend type.

This proposal does not introduce a runtime catalog, runtime selection on
`ModelBooster`, or new backend types. The controller still owns runtime
selection, placeholder values, `ModelServer` generation, and `ModelRoute`
generation. Existing embedded templates remain the controller's default template
source. A `ClusterModelRuntime` is only an explicit override for one supported
backend type.

### Current Behavior

`ModelBooster` currently owns one `ModelBackend` in `spec.backend`. Its current
fields are `name`, `type`, `modelURI`, `cacheURI`, `envFrom`, `env`,
`minReplicas`, `maxReplicas`, `workers`, `schedulerName`, and
`runtimeClassName`.

There is no runtime selection field on `ModelBackend` today, and phase 1 does
not add one.

`ModelBackendType` currently has CRD validation for only:

- `vLLM`
- `vLLMDisaggregated`

Go constants for `SGLang`, `MindIE`, and `MindIEDisaggregated` exist, but they
are not accepted by the current CRD validation marker. `BuildModelServing` also
switches only `vLLM` and `vLLMDisaggregated`.

The controller currently reads embedded template files from
`pkg/model-booster-controller/convert/templates/`, unmarshals them, and applies
the existing `${VAR}` placeholder replacement through
`utils.ReplacePlaceholders`.

### Motivation

Operators sometimes need small cluster-specific changes to the existing
generated `ModelServing` template: private images, pod scheduling constraints,
probe tuning, image pull settings, or environment wiring. Today those changes
require editing an embedded template and rolling a new controller image.

Phase 1 moves only the template override surface into Kubernetes. It does not
change the `ModelBooster` user API, generated placeholder values, or the
controller-owned `ModelRoute` behavior.

### Goals

1. Add a cluster-scoped `ClusterModelRuntime` CRD.
2. Allow one optional `ModelServing` YAML template override per existing
   supported `ModelBackendType`.
3. Keep embedded templates as the default behavior and source of default
   templates.
4. Continue using the current `${VAR}` placeholder replacement mechanism.
5. Reject overrides whose declared backend type does not match the backend being
   converted.
6. Preserve current `ModelRoute` generation behavior.

### Non-Goals

- Adding a runtime selector field to `ModelBackend`.
- Adding SGLang, MindIE, or other new backend type support.
- Moving embedded templates into installed runtime resources.
- Adding a runtime catalog, automatic selection, ranking, or version negotiation.
- Supporting multiple selectable runtime templates for the same backend type.
- Replacing `${VAR}` placeholder replacement with another template engine.
- Templating `ModelServer`, `ModelRoute`, or route rules.
- Adding namespace-scoped runtime resources.

### Proposal

Introduce `ClusterModelRuntime` as an optional cluster-scoped external template
override for one supported `ModelBackendType`.

For phase 1, the controller resolves the template source by backend type only.
It looks for a well-known `ClusterModelRuntime` name for that type. If the
object exists and matches the backend type, the controller uses `spec.template`.
If the object is missing, the controller falls back to the existing embedded
template.

Embedded templates remain the normal default path. Installing or upgrading the
controller does not require installing `ClusterModelRuntime` resources.

### Go API

`ClusterModelRuntime` is added to the existing workload API package:
`pkg/apis/workload/v1alpha1`, group `workload.serving.volcano.sh`, version
`v1alpha1`.

```go
// ClusterModelRuntimeSpec defines a cluster-level ModelServing template override
// for one ModelBooster backend type.
type ClusterModelRuntimeSpec struct {
	// BackendType specifies which ModelBooster backend type this runtime applies to.
	// Phase 1 supports only vLLM and vLLMDisaggregated.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=vLLM;vLLMDisaggregated
	BackendType ModelBackendType `json:"backendType"`

	// Template is a complete ModelServing YAML template.
	// It uses the existing ${VAR} placeholder replacement mechanism.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cmrt
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backendType`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterModelRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterModelRuntimeSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ClusterModelRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterModelRuntime `json:"items"`
}
```

### Field Reference

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `spec.backendType` | `ModelBackendType` | Yes | The existing ModelBooster backend type this override applies to. Phase 1 accepts only `vLLM` and `vLLMDisaggregated`. |
| `spec.template` | `string` | Yes | A complete `ModelServing` YAML template using the current `${VAR}` placeholder format. |

### Runtime Resolution

Phase 1 uses well-known names derived from backend type:

```go
const (
	DefaultClusterModelRuntimeVLLM              = "kthena-vllm-single"
	DefaultClusterModelRuntimeVLLMDisaggregated = "kthena-vllm-pd"
)

func DefaultClusterModelRuntimeName(t ModelBackendType) string {
	switch t {
	case ModelBackendTypeVLLM:
		return DefaultClusterModelRuntimeVLLM
	case ModelBackendTypeVLLMDisaggregated:
		return DefaultClusterModelRuntimeVLLMDisaggregated
	default:
		return ""
	}
}
```

Resolution order:

1. Read the backend type from `ModelBooster.spec.backend.type`.
2. Resolve the well-known runtime name for that backend type.
3. If no well-known name exists, return the current unsupported backend error.
4. Get the cluster-scoped `ClusterModelRuntime` with that name.
5. If it is not found, use the current embedded template for that backend type.
6. If it is found, require `spec.backendType` to equal the backend type being
   converted.
7. Parse, replace placeholders, and decode `spec.template`.

### Template Format

Phase 1 keeps the current placeholder mechanism. Templates continue to use
`${VAR}` placeholders and `utils.ReplacePlaceholders`.

An example abbreviated from the current `vllm.yaml` template:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata: ${MODEL_SERVING_TEMPLATE_METADATA}
spec:
  schedulerName: ${SCHEDULER_NAME}
  replicas: ${BACKEND_REPLICAS}
  template:
    roles:
      - name: leader
        replicas: ${SERVER_REPLICAS}
        entryTemplate:
          spec:
            runtimeClassName: ${RUNTIME_CLASS_NAME}
            volumes: ${VOLUMES}
            containers:
              - name: runtime
                image: ${MODEL_SERVING_RUNTIME_IMAGE}
                args:
                  - --engine
                  - ${MODEL_SERVING_RUNTIME_ENGINE}
                  - --model
                  - ${MODEL_NAME}
              - name: engine
                image: ${ENGINE_SERVER_IMAGE}
                command: ${ENGINE_SERVER_COMMAND}
                env: ${ENGINE_ENV}
                resources: ${ENGINE_SERVER_RESOURCES}
```

This example is intentionally incomplete. Full default templates remain embedded
in the controller image.

### Supported Placeholders

`ClusterModelRuntime.spec.template` uses the same placeholder contract as the
current embedded template for the selected backend type. The valid placeholders
are the keys produced by `BuildModelServing` for that backend.

For phase 1, placeholder names and their typed values are part of the
controller/template compatibility contract. Removing or renaming a placeholder
that can appear in an override is a breaking change for operators who installed
that override.

The current placeholder sets are:

- `vLLM`: `MODEL_SERVING_TEMPLATE_METADATA`, `MODEL_NAME`, `BACKEND_NAME`,
  `BACKEND_REPLICAS`, `BACKEND_TYPE`, `ENGINE_ENV`, `WORKER_ENV`,
  `SERVER_REPLICAS`, `SERVER_ENTRY_TEMPLATE_METADATA`,
  `SERVER_WORKER_TEMPLATE_METADATA`, `VOLUMES`, `VOLUME_MOUNTS`,
  `INIT_CONTAINERS`, `MODEL_DOWNLOAD_ENVFROM`, `MODEL_SERVING_RUNTIME_IMAGE`,
  `MODEL_SERVING_RUNTIME_PORT`, `MODEL_SERVING_RUNTIME_URL`,
  `MODEL_SERVING_RUNTIME_METRICS_PATH`, `MODEL_SERVING_RUNTIME_ENGINE`,
  `MODEL_SERVING_RUNTIME_POD`, `ENGINE_SERVER_RESOURCES`,
  `ENGINE_SERVER_IMAGE`, `ENGINE_SERVER_COMMAND`, `WORKER_REPLICAS`,
  `SCHEDULER_NAME`, and `RUNTIME_CLASS_NAME`.
- `vLLMDisaggregated`: `MODEL_SERVING_TEMPLATE_METADATA`, `VOLUME_MOUNTS`,
  `VOLUMES`, `MODEL_NAME`, `BACKEND_REPLICAS`, `INIT_CONTAINERS`,
  `MODEL_DOWNLOAD_ENVFROM`, `ENGINE_PREFILL_COMMAND`, `ENGINE_DECODE_COMMAND`,
  `SERVER_ENTRY_TEMPLATE_METADATA`, `MODEL_SERVING_RUNTIME_IMAGE`,
  `MODEL_SERVING_RUNTIME_PORT`, `MODEL_SERVING_RUNTIME_URL`,
  `MODEL_SERVING_RUNTIME_METRICS_PATH`, `ENGINE_PREFILL_ENV`,
  `ENGINE_DECODE_ENV`, `MODEL_SERVING_RUNTIME_ENGINE`,
  `MODEL_SERVING_RUNTIME_POD`, `PREFILL_REPLICAS`, `DECODE_REPLICAS`,
  `ENGINE_DECODE_RESOURCES`, `ENGINE_DECODE_IMAGE`,
  `ENGINE_PREFILL_RESOURCES`, `ENGINE_PREFILL_IMAGE`, `SCHEDULER_NAME`, and
  `RUNTIME_CLASS_NAME`.

The existing renderer preserves structured values when the whole YAML scalar is
a placeholder such as `${ENGINE_ENV}` or `${VOLUMES}`. A placeholder embedded
inside a larger string is rendered as text.

The proposed override semantics should validate the rendered result before it is
used. The rendered template must decode to exactly one
`workload.serving.volcano.sh/v1alpha1` `ModelServing`, and the controller should
validate the rendered apiVersion/kind.

Generated identity and ownership remain controller-owned. After rendering and
decoding, the controller should overwrite generated metadata such as resource
name, namespace, labels, and owner references from the owning `ModelBooster`.
Object-level metadata from the override is ignored or overwritten. Pod-template
metadata under `spec.template.roles[*].entryTemplate` or `workerTemplate` remains
customizable. Overrides are for the `ModelServing` spec/template shape, not for
changing ownership.

### Upgrade Semantics

Normal controller image upgrades continue to upgrade embedded default templates
when no `ClusterModelRuntime` override exists.

If an operator creates a `ClusterModelRuntime`, that object becomes an explicit
cluster-owned override. The controller should use it until the operator changes
or deletes it. The operator owns the lifecycle of that override, including
keeping it compatible with newer controller versions.

### ModelServer and ModelRoute Scope

`ModelServer` and `ModelRoute` remain controller-owned in phase 1.

`BuildModelRoute` currently uses `ModelBooster.spec.modelMatch` for route
matching and targets the generated `ModelServer` name returned by
`utils.GetBackendResourceName(model.Name, backend.Name)`. This proposal does
not change that behavior.

Route-rule templating is deferred. A `ClusterModelRuntime` only overrides the
`ModelServing` template used by `BuildModelServing`.

### Error Handling

- Missing `ClusterModelRuntime`: fall back to the embedded template for the
  backend type.
- Backend type mismatch: if the well-known runtime object exists but
  `spec.backendType` does not match the backend being converted, return an error
  and do not generate a `ModelServing`.
- Invalid template: return an error if YAML parsing, placeholder replacement
  fails because a required key is missing, or decoding into `ModelServing` fails.

### Risks and Mitigations

- Stale override templates can drift from newer controller placeholder data.
  Mitigation: embedded templates remain the default path, and operators own
  override upgrades.
- Invalid overrides can block `ModelServing` generation for that backend type.
  Mitigation: surface reconcile errors, while missing overrides fall back to
  embedded templates.
- Cluster-scoped template edits can affect many workloads. Mitigation: protect
  `ClusterModelRuntime` writes with cluster-admin/operator RBAC.

### Test Plan

- Unit test default runtime name resolution for `vLLM` and
  `vLLMDisaggregated`.
- Generation check that CRD, client, lister, and informer artifacts include the
  cluster-scoped `ClusterModelRuntime`.
- Unit test missing override fallback to the embedded template.
- Unit test matching override use for each supported backend type.
- Unit test backend mismatch error when a runtime object exists with the wrong
  `spec.backendType`.
- Unit test invalid template failures for YAML parse, missing placeholder key,
  wrong apiVersion/kind, empty or multi-document input, and decode errors.
- Unit test unchanged `BuildModelRoute` output.
- Controller/envtest coverage that `ModelBooster` reconcile reads a
  cluster-scoped override and falls back when absent.
- Controller RBAC grants `get`, `list`, and `watch` for `ClusterModelRuntime`.

### Alternatives

- Add `spec.backend.runtimeProfile`: rejected for phase 1 to avoid changing the
  `ModelBooster` user API.
- Use ConfigMaps for templates: rejected because a CRD gives typed backend
  ownership and clearer RBAC.
- Install default `ClusterModelRuntime` resources: rejected for phase 1 because
  embedded templates should remain the upgradeable default path.
