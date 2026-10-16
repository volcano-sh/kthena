---
title: ExternalModelProvider for Third-Party LLM API Routing
creation-date: 2026-05-09

---

## ExternalModelProvider for Third-Party LLM API Routing

### Summary

This proposal introduces `ExternalModelProvider`, a new CRD that enables Kthena Router to forward inference requests to external OpenAI-compatible LLM APIs (OpenAI, vLLM, and other OpenAI-compatible endpoints) through `ModelRoute`, preserving existing routing semantics and traffic management capabilities.

### Motivation

Currently, Kthena Router only supports routing to in-cluster model serving endpoints via `ModelRoute` and `HTTPRoute`. Users who want to integrate external LLM APIs must:

1. Deploy custom proxy services in the cluster
2. Manually manage API keys and authentication
3. Implement model name mapping logic
4. Handle streaming responses correctly

This creates operational overhead and duplicates functionality that should be built into the router.

#### Goals

- Enable routing to external LLM APIs without deploying additional proxy services
- Support OpenAI-compatible APIs (OpenAI, vLLM, and other OpenAI-compatible endpoints)
- Reuse `ModelRoute` for unified traffic management (weight-based splitting, rate limiting, etc.)
- Support mixed routing: in-cluster + external backends for the same model name
- Namespace isolation for multi-tenant deployments
- Keep the initial API small while leaving room for a future cost-aware routing proposal

#### Non-Goals

- Support for non-OpenAI-compatible APIs (can be added later)
- Built-in API key rotation (users manage Secrets)
- Request/response transformation beyond model name rewriting
- Cost / billing data modeling on `ExternalModelProvider` in v1alpha1
- Adding a concrete `ModelRoute.spec.selectionPolicy` API in v1alpha1; cost-aware and latency-aware selection policies are deferred to a follow-up proposal

### Proposal

#### API Design

#### ExternalModelProvider CRD

`ExternalModelProvider` defines an external API backend. It is referenced by `ModelRoute.rules[].targetModels[]`, not queried independently.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-gpt4
  namespace: default
spec:
  # Base URL of the external API endpoint (do not include /v1 suffix)
  baseURL: https://api.openai.com

  # Authentication configuration
  auth:
    # Authentication type (default: Bearer)
    # Options: Bearer | APIKeyHeader | RawToken
    type: Bearer
    
    # Reference to Secret containing the API key.
    # Mirrors corev1.SecretKeySelector while defaulting key to "apiKey".
    secretRef:
      name: openai-api-key
      key: apiKey              # Optional, defaults to "apiKey"

    # headerName is omitted for Bearer and RawToken.
    # For type=APIKeyHeader, set the custom header name, e.g. "api-key".

  # Models provided by this external API
  # These are the upstream model names that the provider actually serves
  models:
    - name: gpt-4o-mini
    - name: gpt-4-turbo

  # Optional: request timeout (default: 60s)
  timeout: 60s

status:
  conditions:
    - type: Ready
      status: "True"
      reason: ProviderReady
      message: "Provider is ready"
    - type: SecretAvailable
      status: "True"
      reason: SecretFound
      message: "Secret openai-api-key found"
```

#### Secret Format

`secretRef` intentionally uses a small local struct instead of embedding
`corev1.SecretKeySelector` directly, because this API wants `key` to be optional
and default to `apiKey`.

```go
type ExternalSecretKeyRef struct {
    // Name of the Secret in the same namespace as the ExternalModelProvider.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`

    // Key in the Secret data map. Defaults to "apiKey".
    // +optional
    // +kubebuilder:default=apiKey
    // +kubebuilder:validation:MinLength=1
    Key string `json:"key,omitempty"`
}

// AuthType defines the authentication scheme used when forwarding requests.
// +kubebuilder:validation:Enum=Bearer;APIKeyHeader;RawToken
type AuthType string

const (
    // AuthTypeBearer injects "Authorization: Bearer <token>".
    AuthTypeBearer AuthType = "Bearer"
    // AuthTypeAPIKeyHeader injects "<headerName>: <token>".
    AuthTypeAPIKeyHeader AuthType = "APIKeyHeader"
    // AuthTypeRawToken injects "Authorization: <token>" with no scheme prefix.
    AuthTypeRawToken AuthType = "RawToken"
)

// +kubebuilder:validation:XValidation:rule="(self.type == 'APIKeyHeader') == has(self.headerName)",message="headerName is required when type=APIKeyHeader and must be omitted otherwise"
type ExternalModelProviderAuth struct {
    // Type of authentication scheme. Defaults to Bearer.
    // +optional
    // +kubebuilder:default=Bearer
    Type AuthType `json:"type,omitempty"`

    // SecretRef references the Secret containing the API key.
    // +kubebuilder:validation:Required
    SecretRef ExternalSecretKeyRef `json:"secretRef"`

    // HeaderName is the HTTP header used to carry the token.
    // Required when type=APIKeyHeader; rejected otherwise.
    // +optional
    // +kubebuilder:validation:MinLength=1
    HeaderName string `json:"headerName,omitempty"`
}
```

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: openai-api-key
  namespace: default
type: Opaque
stringData:
  apiKey: sk-...
```

The full Go type for `ExternalModelProvider` (spec and status):

```go
type ExternalModelProviderSpec struct {
    // BaseURL is the HTTPS root of the external API (no trailing /v1).
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=9
    // +kubebuilder:validation:MaxLength=2048
    // +kubebuilder:validation:XValidation:rule="self.lowerAscii().startsWith('https://')",message="baseURL must use HTTPS"
    // +kubebuilder:validation:XValidation:rule="!self.contains('@')",message="baseURL must not contain userinfo"
    // +kubebuilder:validation:XValidation:rule="!self.contains('?') && !self.contains('#')",message="baseURL must not contain query or fragment"
    // +kubebuilder:validation:XValidation:rule="!self.matches('^.*/v\\\\d+/?$')",message="baseURL must not end with a version path segment"
    BaseURL string `json:"baseURL"`

    // Auth configures how the router authenticates to the external API.
    // +kubebuilder:validation:Required
    Auth ExternalModelProviderAuth `json:"auth"`

    // Models lists the upstream model names this provider serves.
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:MaxItems=64
    Models []ExternalModel `json:"models"`

    // Timeout for requests to this provider. Defaults to 60s.
    // Applies as an idle timeout for streaming responses.
    // +optional
    Timeout *metav1.Duration `json:"timeout,omitempty"`
}

type ExternalModel struct {
    // Name is the upstream model identifier (e.g. "gpt-4o-mini").
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    Name string `json:"name"`
}

// ExternalModelProviderStatus defines the observed state of ExternalModelProvider.
type ExternalModelProviderStatus struct {
    // ObservedGeneration is the .metadata.generation the controller last reconciled.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Conditions summarize the provider's readiness.
    // Known types: Ready, SecretAvailable.
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```



`ExternalModelProvider` is referenced by `ModelRoute.rules[].targetModels`, enabling unified traffic management:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: gpt-4-mini-route
  namespace: default
spec:
  # Client-facing model name
  modelName: gpt-4-mini
  rules:
    - name: default
      # Empty modelMatch matches requests selected by spec.modelName.
      # Traffic targets: mix in-cluster and external backends
      targetModels:
        # 80% traffic to in-cluster ModelServer
        - modelServerName: local-llama
          weight: 80

        # 20% traffic to external OpenAI API
        - externalProviderRef:
            name: openai-gpt4
            modelName: gpt-4o-mini    # Which model in the provider
          weight: 20
```

#### TargetModel API Changes

`TargetModel` is extended to support external providers via a discriminated union:

```go
// +kubebuilder:validation:XValidation:rule="[has(self.modelServerName), has(self.externalProviderRef)].filter(x, x).size() == 1",message="exactly one of modelServerName or externalProviderRef must be set"
type TargetModel struct {
    // Exactly one of the following must be set:
    
    // ModelServerName references an in-cluster ModelServer
    // +optional
    // +kubebuilder:validation:MinLength=1
    ModelServerName string `json:"modelServerName,omitempty"`
    
    // ExternalProviderRef references an ExternalModelProvider
    // +optional
    ExternalProviderRef *ExternalProviderRef `json:"externalProviderRef,omitempty"`
    
    // Weight for traffic splitting (0-100)
    // +optional
    // +kubebuilder:default=100
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=100
    Weight *uint32 `json:"weight,omitempty"`
}

type ExternalProviderRef struct {
    // Name of the ExternalModelProvider in the same namespace
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`
    
    // ModelName specifies which model in the provider to use
    // Must match one of ExternalModelProvider.spec.models[].name
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    ModelName string `json:"modelName"`
}
```

**CRD Validation**: The CEL rule uses `filter` form rather than `!=` to ensure it extends cleanly when a third backend type is added: `[has(self.modelServerName), has(self.externalProviderRef)].filter(x, x).size() == 1`. Adding a third field (e.g. `inferencePoolRef`) only requires appending it to the list — no rule replacement needed.

#### Versioning and Compatibility

The existing v1alpha1 `TargetModel` schema requires `modelServerName`. This
proposal changes `TargetModel` into a union by removing that field from the CRD
`required` list and adding a CEL rule that requires exactly one backend field.

Existing `ModelRoute` objects that set `modelServerName` remain valid under the
new schema. No stored-object migration is required for normal routes.

There is still a rollout constraint: an old router binary or old typed client
does not understand `externalProviderRef`. A future implementation PR must
upgrade the API types, generated clients, Helm CRDs, router informers, and data
plane together. The ExternalModelProvider CRD should not be installed on its own
against routers that only know `MatchModelServer`; otherwise an admitted
external target could be treated as an empty in-cluster backend.

This proposal keeps the change in networking v1alpha1 because the API is already
alpha and the old `modelServerName` form remains valid.

One edge case: the current `modelroute_types.go:110` marks `ModelServerName`
as `+kubebuilder:validation:required`, which enforces key presence but not
non-empty value. A stored object with `modelServerName: ""` would have
`has(self.modelServerName) == false` under the new schema (because the field
becomes `omitempty`), causing the CEL rule to reject it on the next update.
Mitigation: add `+kubebuilder:validation:MinLength=1` to `ModelServerName`
in the same PR that removes `required`. This was always the correct constraint;
the migration is safe because an empty `modelServerName` was never a valid
reference to an in-cluster `ModelServer`.

#### Validation Rules

**ExternalModelProvider**:
- `spec.baseURL`: required, must be an **HTTPS** API root. HTTP is rejected because auth headers (Bearer token / API key) are always injected; allowing HTTP would leak credentials to any network observer on the path. CRD validation provides conservative string-level guards:
  1. `self.lowerAscii().startsWith('https://')` — rejects HTTP and other schemes.
  2. `!self.contains('@')` — rejects userinfo-form URLs (e.g. `https://token@evil.example/`) that could redirect auth headers to an attacker-controlled endpoint (SSRF via credential forwarding).
  3. `!self.contains('?') && !self.contains('#')` — rejects query strings and fragments in the root URL.
- `spec.baseURL`: must **not** end with a version path segment (e.g. `/v1`, `/v2`). The router appends the full inbound path as-is, so a trailing version segment would produce a doubled prefix like `/v1/v1/chat/completions`. CRD validation enforces this with: `!self.matches('^.*/v\\d+/?$')`.
- A validating webhook additionally parses `baseURL` as a URL and rejects empty hosts, malformed URLs, or unsupported URL shapes that cannot be expressed cleanly in CEL.
- `spec.models`: required, must contain at least one unique entry; represented as a map-list keyed by `name`.
- `spec.models[].name`: required, must be non-empty.
- `spec.auth.secretRef`: required; uses `ExternalSecretKeyRef`, a `corev1.SecretKeySelector`-shaped struct with `name` required and `key` optional/defaulted to `apiKey`. Both fields must be non-empty when present.
- `spec.auth.type`: optional, default `Bearer`; valid values: `Bearer`, `APIKeyHeader`, `RawToken`
- `spec.auth.headerName`: **required** and non-empty when `type=APIKeyHeader`; **rejected** otherwise. Enforced by CEL plus `MinLength=1`.
- `spec.timeout`: optional, defaults to 60s. The validating webhook rejects non-positive durations if CRD duration validation cannot express the constraint portably.

**TargetModel**:
- Exactly one of `modelServerName` or `externalProviderRef` must be set (CEL filter rule on `TargetModel`)
- `modelServerName`: when set, must be non-empty (`+kubebuilder:validation:MinLength=1`). Replaces the prior `+kubebuilder:validation:required` on `modelroute_types.go:110` — see the edge-case paragraph above.
- `externalProviderRef.name`: required and non-empty when `externalProviderRef` is set
- `externalProviderRef.modelName`: required and non-empty when `externalProviderRef` is set
- **Cross-resource consistency** (`externalProviderRef.modelName` must match a name in `ExternalModelProvider.spec.models[]`) is **not** enforced by CRD validation, since CEL cannot read other objects. The `ExternalModelProvider` controller emits a Warning event on referencing `ModelRoute` resources when it detects an invalid model reference; the router data plane returns a clear error if a request hits an invalid reference.

#### baseURL Path Construction

`baseURL` is treated as the upstream API root and is concatenated directly with the inbound request path. The router does **not** strip or rewrite any path segments.

| baseURL | Inbound path | Upstream URL |
|---------|-------------|--------------|
| `https://api.openai.com/v1` | `/v1/chat/completions` | `https://api.openai.com/v1/v1/chat/completions` ❌ |
| `https://api.openai.com` | `/v1/chat/completions` | `https://api.openai.com/v1/chat/completions` ✅ |

**Rule:** set `baseURL` to the API root without a trailing path version segment. The router appends the full inbound path (`/v1/chat/completions`) as-is.

#### Routing Behavior

**Path Filtering:**
- Only `POST /v1/chat/completions` triggers external routing in the initial delivery

**Namespace Isolation:**
- Router resolves `ExternalModelProvider` in the matched `ModelRoute` namespace
- The referenced Secret must be in the same namespace as the `ExternalModelProvider`
- Cross-namespace references are not supported in v1alpha1

**Model Name Mapping:**
- Client sends `model: gpt-4-mini` (in request body)
- Router looks up `ModelRoute` with `modelName: gpt-4-mini`
- Router selects a `TargetModel` using the existing weight semantics
- If selected target is `externalProviderRef`, router rewrites request body to `model: <externalProviderRef.modelName>`
- Router forwards to `ExternalModelProvider.spec.baseURL`

#### Implementation Impact

The current router data plane is named around in-cluster `ModelServer` targets:
`MatchModelServer` returns a `types.NamespacedName`, and the router immediately
enters pod discovery and scheduling. Mixed in-cluster and external routing
requires a typed destination result instead of a bare ModelServer name.

A future implementation should evolve the internal contract along these lines:

```go
type RouteTargetKind string

const (
    RouteTargetKindModelServer      RouteTargetKind = "ModelServer"
    RouteTargetKindExternalProvider RouteTargetKind = "ExternalProvider"
)

type RouteTarget struct {
    Kind RouteTargetKind

    // Set when Kind=ModelServer.
    ModelServerName types.NamespacedName

    // Set when Kind=ExternalProvider.
    ExternalProviderName types.NamespacedName
    UpstreamModelName    string

    ModelRoute *networkingv1alpha1.ModelRoute
    IsLora     bool
}
```

The store still performs rule matching and weighted target selection once. The
router then branches by `RouteTarget.Kind`:

- `ModelServer`: existing pod discovery, scheduler, request rewrite to
  `ModelServer.spec.model`, and in-cluster proxy path.
- `ExternalProvider`: skip pod scheduling, resolve the provider and Secret from
  informer-backed caches, rewrite the request body to the upstream model name,
  inject auth, and proxy to `ExternalModelProvider.spec.baseURL`.

The implementation PR must add informer/cache and RBAC coverage for
`ExternalModelProvider` and referenced `Secret` objects. Secret rotation is
observed through the Secret informer cache; there is no controller-mediated
copying of credentials.

External proxying must reuse the parsed model request that the router already
has, not re-read the original request body. The proxy path must rebuild the JSON
body after model rewrite, set the correct `ContentLength`, avoid forwarding the
original `Host` / `Content-Length`, overwrite inbound `Authorization`, and pass
through upstream status codes and error bodies without writing a second error
response after streaming has started.

Streaming support follows the same high-level contract as the existing
in-cluster path: respect client cancellation, flush incrementally, avoid
unbounded buffering, and treat `spec.timeout` as an idle timeout for streaming
responses. Non-streaming responses may be inspected for usage accounting, but
large responses must not require unbounded memory growth.

Metrics and access logs need backend attribution in addition to the existing
client-facing model and route labels. The implementation should expose at least
the backend kind, provider name, upstream model name, upstream status, and error
class so operators can distinguish local ModelServer traffic from external API
traffic.

Fairness scheduling continues to use the client-facing `ModelRoute.spec.modelName`
as the quota and accounting key. External traffic does not enter pod scheduling,
but its token usage still updates the same per-model accounting after the
response is observed.

#### Controller Ownership & Status

The initial API adds status only to `ExternalModelProvider`. A dedicated
`ExternalModelProvider` controller owns that status subresource. Its
responsibilities:

- **Secret watch**: watches the referenced `Secret` in the same namespace and
  updates `status.conditions[type=SecretAvailable]` when the Secret appears,
  disappears, or is missing the referenced key.
- **Reference validation**: watches `ModelRoute` resources that point at this
  provider and emits a Warning event on those routes when
  `externalProviderRef.modelName` does not match any entry in
  `spec.models[].name`. This is the cross-resource check that CRD CEL validation
  cannot express.
- **Ready aggregation**: `status.conditions[type=Ready]` is `True` iff the
  provider spec is valid and the referenced Secret is available. Invalid
  `ModelRoute` references do not make the provider itself NotReady.

This proposal does not add `ModelRoute.status.conditions` in v1alpha1. If route
reference status is added later, it should be owned by the ModelRoute controller
and use positive condition types such as `ResolvedRefs=False` with reasons like
`ModelNotFoundInProvider`. The router data plane does **not** write status.

#### Security Considerations

**Transport security**

`spec.baseURL` is restricted to HTTPS. Because the router unconditionally injects
the auth header (Bearer token or API key from the referenced Secret) into every
forwarded request, allowing plaintext HTTP would expose credentials to any
network observer or intermediary. There is no opt-in for HTTP in v1alpha1; if a
trusted in-cluster plaintext endpoint becomes necessary, a future iteration may
add an explicit, audit-friendly opt-in field.

**Same-namespace references only (v1alpha1)**

In v1alpha1, `ModelRoute`, `ExternalModelProvider`, and the referenced auth
`Secret` MUST reside in the same namespace. Cross-namespace references are not
supported. This matches the existing `ModelRoute` ↔ `ModelServer` relationship
and prevents a "confused deputy" scenario where a `ModelRoute` author could
indirectly consume a `Secret` they cannot read by referencing an
`ExternalModelProvider` owned by another tenant.

**Residual in-namespace risk (acknowledged)**

Same-namespace isolation does not eliminate confused-deputy entirely. If RBAC
within a namespace is fine-grained — for example, User A has `create` on
`ModelRoute` but no `get` on `Secret` — User A can still consume a Secret
indirectly by referencing an `ExternalModelProvider` they did not author. This
is the intended trade-off in v1alpha1; tightening it is the goal of the
authorization model below.

**Authorization model: future work**

A multi-tenant deployment in which `ExternalModelProvider` / `Secret` ownership
is separated from `ModelRoute` authorship requires explicit binding
authorization. Candidate mechanisms to be evaluated in a follow-up proposal:

- A `ReferenceGrant`-style cross-namespace permission resource (Gateway API
  pattern), allowing the provider's namespace to opt in to specific consumer
  namespaces or routes.
- An `allowedRoutes` selector on `ExternalModelProvider.spec`, listing which
  routes (by namespace/label) may reference it.
- An admission webhook that verifies the `ModelRoute` author has `get`
  permission on both the referenced `ExternalModelProvider` and its `Secret`.

This is deferred until a concrete multi-tenant use case emerges, to avoid
locking in a model before requirements are understood.

#### Architecture

```
Client Request (model: gpt-4-mini)
      │
      ▼
┌─────────────────────────────────────────┐
│             Kthena Router               │
│                                         │
│  1. Lookup ModelRoute (modelName)      │
│  2. Select TargetModel (by weight)     │
│     ├─ modelServerName?                │
│     │  └─ Forward to in-cluster        │
│     └─ externalProviderRef?            │
│        ├─ Resolve ExternalModelProvider│
│        ├─ Rewrite model field          │
│        ├─ Inject auth header           │
│        ├─ Forward to provider baseURL  │
│        └─ Stream response back         │
└─────────────────────────────────────────┘
      │ externalProviderRef
      ▼
External OpenAI-compatible API
```

The router performs auth injection, model field rewrite, and streaming proxy
in-process. There is no separate "external proxy" component to deploy.

### Design Rationale

#### 1. Cost-Aware Routing Is Deferred

This proposal intentionally does **not** add concrete cost fields or a
`selectionPolicy` field in v1alpha1.

The API still leaves room for cost-aware routing because `TargetModel` becomes a
single abstraction over in-cluster `modelServerName` targets and external
`externalProviderRef` targets. A future selector can evaluate both target kinds
without introducing a parallel routing mechanism for external providers.

Cost data is deferred because pricing and in-cluster cost are different
domains:

- External MaaS providers usually price per input/output token.
- In-cluster cost is derived from GPU time, instance price, utilization, and
  amortization.
- Comparing those two requires a normalization model and a data source; locking
  either into `ExternalModelProvider` now would make the alpha API harder to
  correct later.

A follow-up proposal can add `ModelRoute.spec.selectionPolicy` (for example,
`Weighted`, `LeastCost`, or `LowestLatency`) once the cost data source,
normalization rules, and fallback semantics are defined. Adding an optional
policy field later is backward-compatible, so v1alpha1 stays focused on the
external backend reference itself.

#### 2. Secret Keys Are Customizable

`secretRef` is a structured reference instead of a plain Secret name:

```yaml
auth:
  secretRef:
    name: openai-api-key
    key: apiKey          # Optional, defaults to "apiKey"
```

This follows standard Kubernetes patterns and lets users reuse existing Secrets
without creating new ones just to satisfy a fixed key name.

#### 3. Bearer Token Is the Default

Most OpenAI-compatible MaaS providers use bearer-token authentication, so
`auth.type` defaults to `Bearer`:

```yaml
auth:
  type: Bearer           # Default; other options: APIKeyHeader, RawToken
  secretRef: { name, key }
```

**Behavior**:
- `type: Bearer` (default) → `Authorization: Bearer <token>`
- `type: APIKeyHeader` → `<headerName>: <token>`
- `type: RawToken` → `Authorization: <token>` (no "Bearer " prefix)

This makes the common MaaS behavior the zero-configuration path.

#### 4. ExternalModelProvider Is a Backend Reference

`ExternalModelProvider` is a **backend reference**, not an independent routing
layer.

**How it works**:
1. `ModelRoute.spec.modelName` defines the client-facing model name
2. `ModelRoute.spec.rules[].targetModels[]` defines where traffic goes (in-cluster or external)
3. `TargetModel.externalProviderRef.modelName` specifies which upstream model to use

**Example**:
```yaml
kind: ModelRoute
spec:
  modelName: gpt-4-mini              # Client sees this
  rules:
    - name: default
      targetModels:
        - externalProviderRef:
            name: openai-gpt4
            modelName: gpt-4o-mini   # Provider receives this
          weight: 100
```

**No ambiguity**: There is no "matching" between `ModelRoute` and `ExternalModelProvider`. The relationship is explicit via `externalProviderRef`.

**Mixed routing**: Naturally supported by adding multiple `TargetModel` entries:
```yaml
rules:
  - name: default
    targetModels:
      - modelServerName: local-llama # 80% in-cluster
        weight: 80
      - externalProviderRef:         # 20% external
          name: openai-gpt4
          modelName: gpt-4o-mini
        weight: 20
```

### Use Cases

#### 1. OpenAI Integration

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai
  namespace: default
spec:
  baseURL: https://api.openai.com
  auth:
    type: Bearer
    secretRef:
      name: openai-key
  models:
    - name: gpt-4o-mini
    - name: gpt-4-turbo
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: gpt-4-mini-route
  namespace: default
spec:
  modelName: gpt-4-mini
  rules:
    - name: default
      targetModels:
        - externalProviderRef:
            name: openai
            modelName: gpt-4o-mini
          weight: 100
```

#### 2. vLLM OpenAI-Compatible Endpoint

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: vllm-external
  namespace: default
spec:
  baseURL: https://my-vllm-service.example.com
  auth:
    type: Bearer
    secretRef:
      name: vllm-key
  models:
    - name: meta-llama/Meta-Llama-3-70B-Instruct
  timeout: 120s
---
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: llama-3-70b-route
  namespace: default
spec:
  modelName: llama-3-70b
  rules:
    - name: default
      targetModels:
        - externalProviderRef:
            name: vllm-external
            modelName: meta-llama/Meta-Llama-3-70B-Instruct
          weight: 100
```

#### 3. Multi-Provider Load Balancing

```yaml
# Primary OpenAI provider (70% traffic)
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-primary
  namespace: default
spec:
  baseURL: https://api.openai.com
  auth:
    type: Bearer
    secretRef:
      name: openai-key
  models:
    - name: gpt-4o-mini
---
# Backup provider (30% traffic)
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-backup
  namespace: default
spec:
  baseURL: https://backup-openai-proxy.example.com
  auth:
    type: Bearer
    secretRef:
      name: backup-key
  models:
    - name: gpt-4o-mini
---
# ModelRoute with traffic splitting
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: gpt-4-mini-route
  namespace: default
spec:
  modelName: gpt-4-mini
  rules:
    - name: default
      targetModels:
        - externalProviderRef:
            name: openai-primary
            modelName: gpt-4o-mini
          weight: 70
        - externalProviderRef:
            name: openai-backup
            modelName: gpt-4o-mini
          weight: 30
```

#### 4. Hybrid Routing (In-Cluster + External)

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: gpt-4-mini-hybrid
  namespace: default
spec:
  modelName: gpt-4-mini
  rules:
    - name: default
      targetModels:
        # 80% to in-cluster ModelServer
        - modelServerName: local-llama-server
          weight: 80
        # 20% to external OpenAI (overflow/canary)
        - externalProviderRef:
            name: openai
            modelName: gpt-4o-mini
          weight: 20
```

> **Note on Azure OpenAI:** Azure OpenAI requires a deployment-specific URL path and a mandatory `api-version` query parameter. Supporting Azure requires adding a `queryParams` field to the CRD, which is deferred to a future iteration.

### Extension Points

This proposal implements only weight-based target selection through the existing
`TargetModel.weight` field. The following items are intentionally future work:

#### 1. Cost-Aware Routing

A follow-up proposal may add `ModelRoute.spec.selectionPolicy` and a `LeastCost`
policy after defining cost data sources and normalization across external APIs
and in-cluster ModelServers.

#### 2. Latency-Aware Routing

A future proposal may add latency-aware target selection. That proposal should
define where observed latency is stored, which controller owns it, and how it is
compared across in-cluster and external backends.

#### 3. Quota and Rate-Limit Aware Routing

External 429 handling, temporary backend exclusion, and automatic failover are
left to a later routing-policy proposal.

#### 4. Regional Failover

Multiple `baseURL` entries, regional priorities, and provider-level failover are
out of scope for v1alpha1.

### Alternatives Considered

#### 1. Independent Fallback Layer (Original Design)

**Rejected**: The original design had `ExternalModelProvider` as an independent routing layer with its own weight-based selection, separate from `ModelRoute`. This created:
- Two separate weight mechanisms (ModelRoute vs ExternalProvider)
- Confusion about the relationship between ModelRoute and ExternalProvider
- No path for mixed routing (in-cluster + external)
- Duplicate selection logic for future cost-aware routing

The new design unifies all routing through `ModelRoute`, making `ExternalModelProvider` a backend reference like `ModelServer`.

#### 2. Extend HTTPRoute with Auth

**Rejected**: HTTPRoute is generic and should not have LLM-specific features like model name rewriting and streaming response handling.

#### 3. Use Gateway API ExternalName Service

**Rejected**: Does not support Secret-based auth injection or model name rewriting.

#### 4. Deploy Separate Proxy Service

**Rejected**: Adds operational overhead and duplicates router functionality.

#### 5. Extend Gateway API `HTTPRoute.backendRefs` / Inference Extension

**Deferred for v1alpha1**: `ExternalModelProvider` is consumed by
`ModelRoute.rules[].targetModels[]`, which already carries kthena-specific LLM
routing semantics such as model-name matching and model rewrite. Routing through
generic `HTTPRoute.backendRefs` would add another indirection and still would
not solve Secret-backed per-backend auth injection. Kthena should revisit this
once Gateway API or the Inference Extension has a stable external-backend auth
pattern.
