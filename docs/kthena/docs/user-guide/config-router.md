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
| prefix-cache  | blockSizeToHash<br />maxBlocksToMatch<br />maxHashCacheSize                             | Configures prefix cache parameters                                                                                                                                                                        |
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

## External Endpoint Discovery

The optional `endpointDiscovery` section selects a compiled-in discovery provider.
When omitted (or `provider: kubernetes`), native Pod and static endpoint behavior
is unchanged. External mode disables Pod discovery for the whole router.

No external providers are included by default. An integration implements
`discovery.Source` and a `discovery.Factory`, then supplies its registry through
`Server.EndpointDiscoveryRegistry` before calling `Run`. For example, with an
integration-provided `customdiscovery.NewSource` factory:

```go
server.EndpointDiscoveryRegistry = discovery.Registry{
    "custom": customdiscovery.NewSource,
}
```

The matching router configuration could be:

```yaml
endpointDiscovery:
  provider: custom
  refreshInterval: 5s
  snapshotTimeout: 2s
  mappings:
    - modelServer: default/example-model
      parameters:
        service: example-model
```

`custom` and `service` are illustrative, not a bundled provider or a built-in
parameter. Providers define and validate their own parameters. Selecting a name
that was not registered fails startup; YAML cannot load a provider implementation.

Sources return complete routable snapshots using the existing Endpoint type. A
successful empty snapshot removes all instances. Lookup errors and invalid
snapshots retain the last successful membership and are retried.

Each mapping exclusively owns the ModelServer's endpoint membership. Its selector
or static endpoints do not override discovery results. ModelServer validation is
unchanged: keep a valid `workloadSelector.matchLabels` plus `workloadPort`, or valid
static endpoints. Unmapped static models still work; unmapped selector-based
models remain empty, with no fallback to Kubernetes Pods.

Both Kubernetes resources and `--config-source=file` are supported. The latter
loads ModelServer/ModelRoute resource manifests; it is independent of endpoint
discovery and does not provide an endpoint discovery backend.

Endpoint names are instance identities within a ModelServer. Change the name on
replacement or address/port changes. Optional `labels` supply scheduling metadata
such as PD role/group labels; discovery does not infer them from an IP address.

Provider names and mappings require a router restart to change. This draft does
not support multiple providers per router or Gateway API Inference Extension
with external discovery. Registry failures can retain stale endpoints without an
expiry limit; readiness does not prove registry health or serving capacity.

Providers must honor cancellation and release their subscriptions on Close. The
shared controller owns validation, synthetic Pods and Store synchronization.
