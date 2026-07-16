# External Model Providers

Kthena Router can route requests to third-party model APIs through `ExternalModelProvider`. This is useful when you want one Router entry point for both in-cluster `ModelServer` backends and external HTTPS providers.

The external provider path reuses the normal Router entry flow: request parsing, authentication, rate limiting, `ModelRoute` matching, access logs, metrics, and weighted routing. After a route selects an `ExternalModelProvider`, the Router calls the provider API directly instead of scheduling a Pod.

## Supported Providers

The Router supports these provider protocols:

- OpenAI-compatible APIs: `/v1/chat/completions`, `/v1/completions`, and `/v1/responses`
- Anthropic Messages API: `/v1/messages`

Request bodies are forwarded using the selected provider's native format. Tool definitions, tool calls, tool results, images, audio, files, and provider extensions are not removed or rejected by Kthena. Whether they work depends on the selected model, provider API, and backend configuration.

External providers must use HTTPS. The Router does not use Pod scheduling, KV-cache-aware routing, prefix-aware routing, LoRA affinity, or PD disaggregation for external providers.

## Configure Credentials

Create a Kubernetes `Secret` in the same namespace as the `ExternalModelProvider`. The provider references one key from that Secret.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: openai-api-key
  namespace: default
type: Opaque
stringData:
  api-key: "replace-with-your-openai-api-key"
```

The `secretRef.key` value controls which key in the Secret is used. If the Secret or the key is missing, the provider is not ready and Router requests return `503`.

### Access control

Treat `ExternalModelProvider` as an administrator API. Anyone who can create or update one can use any Secret in the same namespace and choose where the Router sends that credential. For example, a user could set `baseURL` to an HTTPS server they control and reference another Secret in the namespace.

Do not grant tenant roles permission to create or update `ExternalModelProvider`. Limit that permission to cluster administrators. Router egress should also be restricted with NetworkPolicy or an egress proxy. Same-namespace Secret references prevent cross-namespace selection, but they do not remove this credential-forwarding risk.

## Configure an OpenAI-Compatible Provider

Use `providerType: OpenAI` for OpenAI-compatible APIs. Compatible providers can use their own endpoint in `baseURL`.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: openai-provider
  namespace: default
spec:
  providerType: OpenAI
  baseURL: "https://api.openai.com"
  model: "gpt-4o-mini"
  auth:
    secretRef:
      name: openai-api-key
      key: api-key
```

The Router sends the Secret value as `Authorization: Bearer <token>`. If `spec.model` is set, the Router rewrites the upstream request model to that value. If `spec.model` is omitted, the original request model is preserved.

For streaming Chat/Completions requests, the Router may add `stream_options.include_usage=true` upstream so it can read the provider's final usage event. Existing fields in `stream_options` are preserved. The Router does not inject `include_usage` or `stream_options` into non-streaming requests.

Create a `ModelRoute` that targets the provider:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: openai-external-route
  namespace: default
spec:
  modelName: "openai-chat"
  rules:
  - name: "default"
    targetModels:
    - externalModelProviderName: "openai-provider"
```

Apply the complete example:

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/ExternalModelProvider-openai.yaml
```

Send a request:

```bash
curl http://$ROUTER_IP/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai-chat",
    "messages": [
      {"role": "user", "content": "Hello from Kthena Router"}
    ]
  }'
```

The same provider and route can serve OpenAI Responses API requests:

```bash
curl http://$ROUTER_IP/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai-chat",
    "input": "Hello from Kthena Router"
  }'
```

Responses requests are forwarded in their native format. Set `stream: true` to pass through Responses API SSE events. The Router parses native `usage.input_tokens`, `usage.output_tokens`, and `usage.total_tokens` without adding chat/completions-specific `include_usage` or `stream_options` fields. See [Observability and Token Accounting](#observability-and-token-accounting) for how Kthena uses these values.

## Configure an Anthropic Provider

Use `providerType: Anthropic` for the Anthropic Messages API.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ExternalModelProvider
metadata:
  name: anthropic-provider
  namespace: default
spec:
  providerType: Anthropic
  baseURL: "https://api.anthropic.com"
  model: "claude-3-5-sonnet-latest"
  auth:
    secretRef:
      name: anthropic-api-key
      key: api-key
  headers:
    anthropic-version: "2023-06-01"
```

The Router sends the Secret value as `x-api-key: <token>`. Anthropic also requires an API version header, which can be configured through `spec.headers`.

`baseURL` can be the API host, such as `https://api.anthropic.com`, or a URL ending in `/v1`. The Router avoids adding a second `/v1` when the version is already present.

Create a `ModelRoute`:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: anthropic-external-route
  namespace: default
spec:
  modelName: "anthropic-chat"
  rules:
  - name: "default"
    targetModels:
    - externalModelProviderName: "anthropic-provider"
```

Apply the complete example:

```bash
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/kthena/main/examples/kthena-router/ExternalModelProvider-anthropic.yaml
```

Send a request:

```bash
curl http://$ROUTER_IP/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic-chat",
    "max_tokens": 128,
    "messages": [
      {"role": "user", "content": "Hello from Kthena Router"}
    ]
  }'
```

## Request Content and Accounting

Internal `ModelServer` targets and external providers use the same Router-level content contract: text and tool-call payloads pass through, and multimodal payloads can be forwarded to a backend that supports them. Kthena does not execute tools or translate request formats between OpenAI and Anthropic protocols.

Kthena's local prompt extraction currently includes only string prompts and text-bearing content blocks. Image, audio, file, tool-definition, and other opaque content may therefore be absent from input-token estimates, input-token rate limiting, access logs, metrics, and internal prefix/KV-cache scheduling. Request-count limiting still applies, and upstream response usage continues to feed the existing output and accounting paths.

## Observability and Token Accounting

Kthena records two token counts for different purposes:

- Before dispatch, the Router tokenizes the text it can extract from the request. This local estimate is used for input-token rate limiting and for the input count in access logs and `kthena_router_tokens_total`.
- After a successful response, the Router reads usage reported by the provider. The provider's input and output counts update per-user usage history. The reported output count also updates output-token rate limiting, access logs, and `kthena_router_tokens_total`.

Provider-reported input usage does not replace the local input estimate in access logs or Prometheus metrics. The two values can differ because providers may use another tokenizer or count cached and non-text input differently. If the provider omits usage, Kthena cannot record provider-derived output tokens or update the per-user token history for that response.

Metrics for a selected external destination use `backend_type="external_provider"`. The `backend_name` label contains the namespaced `ExternalModelProvider` name, and `upstream_model` contains the configured provider model or the route model when `spec.model` is unset. The same destination fields appear in structured access logs.

Provider HTTP errors keep the upstream status code. Metrics classify them as `error_type="upstream_response"`, while access logs set `error_origin="upstream"`. Connection, TLS, and timeout failures originate in the Router and are classified as `upstream_transport`.

## Weighted Routing

`ModelRoute` can split traffic between internal `ModelServer` targets and external provider targets. Each target still uses exactly one of `modelServerName` or `externalModelProviderName`.

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelRoute
metadata:
  name: mixed-route
  namespace: default
spec:
  modelName: "chat"
  rules:
  - name: "default"
    targetModels:
    - modelServerName: "local-model-server"
      weight: 80
    - externalModelProviderName: "openai-provider"
      weight: 20
```

Do not use `externalModelProviderName` in a route with `loraAdapters`. LoRA routing is an in-cluster `ModelServer` feature in this version.

## Static Headers

Use `spec.headers` for non-sensitive provider headers, such as provider API version headers. Do not put API keys in `spec.headers`; use `spec.auth.secretRef` instead. Credential headers such as `Authorization` and `x-api-key` are reserved and are set by the provider adapter.

## Check Provider Status

Check whether credentials are resolved:

```bash
kubectl get externalmodelprovider -n default
kubectl describe externalmodelprovider openai-provider -n default
```

`Ready=True` means the provider can be selected by Router. `CredentialsResolved=False` usually means the referenced Secret or key is missing.

## Limitations

- Tool and multimodal payloads require support from the selected model and provider API. Kthena forwards these fields but does not validate provider capabilities or fully account for non-text input.
- OpenAI-compatible embeddings are not part of the MVP.
- External providers are not scheduled through Pods and do not use KV-cache-aware routing, prefix-aware routing, LoRA affinity, or PD disaggregation.
- Router passes provider HTTP errors such as `401`, `429`, and `5xx` back to the client.

See the [Networking CRD reference](../reference/crd/networking.serving.volcano.sh.md) for the full field list.
