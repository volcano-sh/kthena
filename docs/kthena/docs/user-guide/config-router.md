# Inference Router Customization

## Overview

ConfigMap is a Kubernetes API object used to store configuration data. Kthena Router uses ConfigMap to configure scheduler plugins and authentication settings, allowing users to customize router behavior without recompiling the code.

**NOTICE:** The ConfigMap must be prepared before launching the router pod, otherwise it will not take effect. Because we do not support hot reload.

## Configuration options

### Scheduler Configuration

The scheduler configuration includes plugin configurations and lists of enabled/disabled plugins.

Plugin Configuration (PluginConfig):

| Plugin Name   | Parameters                                                                              | Description                                                                                                                                                                                               |
| ------------- | --------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| least-request | maxWaitingRequests                                                                      | Sets the maximum number of waiting requests. Must not be negative; 0 or omitted uses the default of 10.                                                                                                                                                             |
| least-latency | TTFTTPOTWeightFactor                                                                    | Sets the TTFT weight factor. Must be between `0.0` and `1.0`.                                                                                                                                             |
| prefix-cache  | blockSizeToHash<br />maxBlocksToMatch<br />maxHashCacheSize<br />topKMatches            | Configures prefix cache parameters. See the [Prefix Cache Plugin](./prefix-cache) guide for their meaning and tuning.                                                                                     |
| kvcache-aware | blockSizeToHash<br />maxBlocksToMatch<br />indexMode<br />runtimePort                   | Configures KV cache aware token-block matching parameters. Requires the Kthena Runtime sidecar; uses Redis (`indexMode: redis`, default) or an in-memory index streamed from the runtimes (`indexMode: memory`). |

`TTFTTPOTWeightFactor` controls how the least-latency plugin balances Time To First Token (TTFT) and Time Per Output Token (TPOT):

- `0.0` prioritizes TPOT.
- `0.5` gives equal weight to TTFT and TPOT.
- `1.0` prioritizes TTFT.

The router exits with a configuration error when this value is outside the `0.0` to `1.0` range, or is non-finite.

Filter Plugins (Filter):

| Configuration Name | Description                     |
| ------------------ | ------------------------------- |
| enabled            | List of enabled filter plugins  |
| disabled           | List of disabled filter plugins |

Score Plugins (Score):

| Configuration Item | Description                                  |
| ------------------ | -------------------------------------------- |
| enabled            | List of enabled score plugins. Each entry must set a `weight` greater than 0, which scales that plugin's scores when they are combined. |
| disabled           | List of disabled score plugins               |

### Authentication Configuration

Authentication configuration is used to enable and configure JWT authentication.

| Parameter | Type     | Description        |
| --------- | -------- | ------------------ |
| issuer    | string   | JWT issuer         |
| audiences | []string | JWT audiences list |
| jwksUri   | string   | Jwks Provider  URI |

### Access Log Configuration

The `accessLog` section holds access-log options that are not set through the
`ACCESS_LOG_*` environment variables. Enablement, format and output stay on
those variables; see [Router Observability](./router-observability.md#access-logs).

| Parameter            | Type | Default | Description |
| -------------------- | ---- | ------- | ----------- |
| includeUpstreamUsage | bool | `false` | Record the input and cached-input token counts the upstream reports in its usage as `upstream_input_tokens` and `cached_input_tokens` |
| claimFields | map[string]string | none | Map access log field names to top-level JWT claim names; each request with a validated JWT logs the mapped claims |

```yaml
accessLog:
  includeUpstreamUsage: true
  claimFields:
    user_id: sub
    tenant: tid
```

`claimFields` is checked at startup, and the router fails to start with the
reason when it is invalid:

- At most 8 fields.
- Field names must match `^[a-z][a-z0-9_]*$`, are at most 64 bytes, and cannot
  be a name the access log already writes, such as `model_name`, `tokens`,
  `timings`, `upstream_input_tokens` or `cached_input_tokens`.
- Claim names must be non-empty, at most 64 bytes, and without leading or
  trailing whitespace. They name top-level claims exactly; nested paths are not
  supported.

Claims are read only from a JWT validated by the `auth` section, so the mapping
has no effect while JWT authentication or access logging is disabled; the
router logs this at startup. See
[JWT claim fields](./router-observability.md#jwt-claim-fields) for the value rules.

When installing with Helm, set `networking.kthenaRouter.accessLog.includeUpstreamUsage`
and `networking.kthenaRouter.accessLog.claimFields` instead of editing the
ConfigMap by hand.

<!-- Add routing rules here -->

## Examples

<!-- Add examples here -->
### Basic Scheduler Configuration

Here's a complete ConfigMap example showing how to configure the scheduler:

```yaml showLineNumbers
apiVersion: v1
kind: ConfigMap
metadata:
  name: kthena-router-config
  namespace: <namespace>
data:
  routerConfiguration: |-
    scheduler:
      pluginConfig:
      - name: least-request
        args: 
          maxWaitingRequests: 10
      - name: least-latency
        args:
          TTFTTPOTWeightFactor: 0.5
      - name: prefix-cache
        args:
          blockSizeToHash: 64
          maxBlocksToMatch: 128
          maxHashCacheSize: 50000
          topKMatches: 5
      - name: kvcache-aware
        args:
          blockSizeToHash: 16
          maxBlocksToMatch: 128
      plugins:
        Filter:
          enabled:
            - least-request
          disabled:
            - lora-affinity
        Score:
          enabled:
            - name: least-request
              weight: 1
            - name: kvcache-aware
              weight: 1
            - name: least-latency
              weight: 1
            - name: prefix-cache
              weight: 1
```

If you want to use Authentication feature of router. Here is an example:

```yaml showLineNumbers
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "kthena.name" . }}-router-config
  namespace: {{ .Release.Namespace }}
data:
  routerConfiguration: |-
    scheduler:
      pluginConfig:
      - name: least-request
        args: 
          maxWaitingRequests: 10
      - name: least-latency
        args:
          TTFTTPOTWeightFactor: 0.5
      - name: prefix-cache
        args:
          blockSizeToHash: 64
          maxBlocksToMatch: 128
          maxHashCacheSize: 50000
          topKMatches: 5
      - name: kvcache-aware
        args:
          # The default block size of vllm is 16, keep in line with it.
          # Changes as needed.
          blockSizeToHash: 16
          maxBlocksToMatch: 128
      plugins:
        Filter:
          enabled:
            - least-request
          disabled:
            - lora-affinity
        Score:
          enabled:
            - name: least-request
              weight: 1
            - name: kvcache-aware
              weight: 1
            - name: least-latency
              weight: 1
            - name: prefix-cache
              weight: 1
    auth:
      issuer: "testing@secure.istio.io"
      audiences: ["kthena.io"]
      jwksUri: "https://raw.githubusercontent.com/istio/istio/release-1.27/security/tools/jwt/samples/jwks.json"
```

After creating or updating the ConfigMap, you need to restart the Router Pod for the configuration to take effect:

```bash
# Create ConfigMap
kubectl apply -f configmap.yaml

# Restart Router Pod
kubectl rollout restart deployment/kthena-router
```
