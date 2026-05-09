# Proposal: ExternalModelProvider for Third-Party LLM API Routing

## Summary

This proposal introduces `ExternalModelProvider`, a new CRD that enables Kthena Router to forward inference requests to external third-party LLM APIs (OpenAI, Azure OpenAI, Anthropic Claude, etc.) while preserving existing in-cluster routing semantics.

## Motivation

Currently, Kthena Router only supports routing to in-cluster model serving endpoints via `ModelRoute` and `HTTPRoute`. Users who want to integrate external LLM APIs (for cost optimization, model availability, or hybrid deployments) must:

1. Deploy custom proxy services in the cluster
2. Manually manage API keys and authentication
3. Implement model name mapping logic
4. Handle streaming responses correctly

This creates operational overhead and duplicates functionality that should be built into the router.

### Goals

- Enable routing to external LLM APIs without deploying additional proxy services
- Support OpenAI-compatible APIs (OpenAI, Azure OpenAI, vLLM, etc.)
- Preserve existing routing precedence: ModelRoute → HTTPRoute → ExternalModelProvider
- Support multiple providers for the same model name with weight-based load balancing
- Namespace isolation for multi-tenant deployments

### Non-Goals

- Support for non-OpenAI-compatible APIs (can be added later)
- Built-in API key rotation (users manage Secrets)
- Request/response transformation beyond model name rewriting
- Cost tracking or billing integration

## Proposal

### API Design

#### ExternalModelProvider CRD

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-gpt4
  namespace: default
spec:
  # Base URL of the external API endpoint
  baseURL: https://api.openai.com/v1
  
  # Authentication configuration
  auth:
    # Reference to Secret containing API key
    secretRef: openai-api-key
    # Optional: custom header name (default: "Authorization")
    headerName: ""
  
  # Model mappings: client model name → upstream model name
  models:
    - name: gpt-4-mini          # Client requests this name
      upstreamModel: gpt-4o-mini # Provider receives this name
    - name: gpt-4
      upstreamModel: gpt-4-turbo
  
  # Optional: request timeout (default: 60s)
  timeout: 60s
  
  # Optional: weight for load balancing (default: 1)
  # When multiple providers serve the same model, traffic is split by weight
  weight: 1

status:
  # Conditions track provider health
  conditions:
    - type: Ready
      status: "True"
      reason: SecretFound
      message: "Provider is ready"
    - type: SecretAvailable
      status: "True"
      reason: SecretFound
      message: "Secret openai-api-key found"
  
  # Model conflicts (multiple providers with same model name)
  modelConflicts:
    - modelName: gpt-4-mini
      providers:
        - default/openai-gpt4
        - default/azure-gpt4
```

#### Secret Format

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

### Routing Behavior

**Precedence Order:**
1. `ModelRoute` - In-cluster model serving endpoints
2. `HTTPRoute` - Custom HTTP routes
3. `ExternalModelProvider` - External APIs (fallback)

**Path Filtering:**
- Only `POST /v1/chat/completions` triggers external routing
- Other paths (embeddings, completions, etc.) can be added later

**Multi-Provider Selection:**
- When multiple providers serve the same model name, traffic is split by weight
- Example: Provider A (weight=2) and Provider B (weight=1) → 2:1 traffic split
- Non-ready providers (missing Secret, conflicts) are excluded

**Namespace Isolation:**
- Router extracts namespace from gateway context or `POD_NAMESPACE` env
- Only providers in the same namespace are considered

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Kthena Router                          │
│                                                             │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐ │
│  │ ModelRoute   │───▶│ HTTPRoute    │───▶│ External     │ │
│  │ Lookup       │    │ Lookup       │    │ Provider     │ │
│  │              │    │              │    │ Lookup       │ │
│  └──────────────┘    └──────────────┘    └──────────────┘ │
│         │                   │                    │         │
│         ▼                   ▼                    ▼         │
│  ┌──────────────────────────────────────────────────────┐ │
│  │           Weighted Provider Selection                │ │
│  │  (Exclude non-ready providers)                       │ │
│  └──────────────────────────────────────────────────────┘ │
│                            │                              │
│                            ▼                              │
│  ┌──────────────────────────────────────────────────────┐ │
│  │              External Proxy                          │ │
│  │  - Model name rewriting                              │ │
│  │  - Secret-based auth injection                       │ │
│  │  - Query parameter preservation                      │ │
│  │  - Streaming support (chunked transfer)              │ │
│  └──────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
                  ┌──────────────────┐
                  │  External API    │
                  │  (OpenAI, Azure) │
                  └──────────────────┘
```

### Controller Responsibilities

**ExternalProviderController:**
1. Watch `ExternalModelProvider` and `Secret` resources
2. Validate Secret existence and format
3. Detect model name conflicts across providers
4. Update provider status (Ready, SecretAvailable conditions)
5. Register ready providers in routing datastore
6. Exclude non-ready providers from routing

**Status Conditions:**
- `Ready`: Provider is ready for routing (Secret found, no conflicts)
- `SecretAvailable`: Referenced Secret exists and contains `apiKey`

### Use Cases

#### 1. OpenAI Integration

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai
  namespace: default
spec:
  baseURL: https://api.openai.com/v1
  auth:
    secretRef: openai-key
  models:
    - name: gpt-4-mini
      upstreamModel: gpt-4o-mini
```

#### 2. Azure OpenAI with Custom Headers

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: azure-openai
  namespace: default
spec:
  baseURL: https://my-resource.openai.azure.com/openai/deployments/gpt-4
  auth:
    secretRef: azure-key
    headerName: api-key  # Azure uses custom header
  models:
    - name: gpt-4
      upstreamModel: gpt-4  # Azure deployment name
  timeout: 120s
```

#### 3. Multi-Provider Load Balancing

```yaml
# Provider A: 70% traffic
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-primary
  namespace: default
spec:
  baseURL: https://api.openai.com/v1
  auth:
    secretRef: openai-key
  models:
    - name: gpt-4-mini
      upstreamModel: gpt-4o-mini
  weight: 7
---
# Provider B: 30% traffic
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: azure-backup
  namespace: default
spec:
  baseURL: https://backup.openai.azure.com/openai/deployments/gpt-4
  auth:
    secretRef: azure-key
    headerName: api-key
  models:
    - name: gpt-4-mini
      upstreamModel: gpt-4
  weight: 3
```

## Implementation Plan

### Phase 1: Core API and Controller (This Proposal)
- Define `ExternalModelProvider` CRD
- Implement controller with Secret validation and status reporting
- Add RBAC permissions for controller

### Phase 2: Routing Integration (After Approval)
- Implement external proxy with OpenAI-compatible forwarding
- Integrate into router with correct precedence
- Add weighted multi-provider selection
- Implement namespace isolation

### Phase 3: Testing and Documentation
- Unit tests for controller, datastore, proxy, router
- E2E tests with real external providers
- User guide with examples

## Alternatives Considered

### 1. Extend HTTPRoute with Auth
**Rejected:** HTTPRoute is generic and shouldn't have LLM-specific features like model name rewriting.

### 2. Use Gateway API ExternalName Service
**Rejected:** Doesn't support Secret-based auth injection or model name rewriting.

### 3. Deploy Separate Proxy Service
**Rejected:** Adds operational overhead and duplicates router functionality.

## Open Questions

1. **Should we support non-OpenAI-compatible APIs?**
   - Proposal: Start with OpenAI-compatible, add others based on demand

2. **How to handle API rate limits?**
   - Proposal: Let external providers handle rate limiting, add metrics for visibility

3. **Should we support request/response transformation?**
   - Proposal: Only model name rewriting for now, add transformations if needed

## References

- [OpenAI API Reference](https://platform.openai.com/docs/api-reference)
- [Azure OpenAI Service](https://learn.microsoft.com/en-us/azure/ai-services/openai/)
- [Gateway API](https://gateway-api.sigs.k8s.io/)
