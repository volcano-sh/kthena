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
| least-request | maxWaitingRequests                                                                      | Sets the maximum number of waiting requests                                                                                                                                                               |
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

Place `endpointDiscovery` alongside `scheduler` and `auth` in the Router
configuration file selected by `--router-config` (default:
`/etc/config/routerConfiguration.yaml`, populated from the ConfigMap's
`routerConfiguration` key). It is not a ModelServer field.
All discovery settings are read at startup; changing them requires restarting
the Router replicas. Endpoint changes from the provider are applied automatically
without a restart.

Each Router process selects one compiled-in provider for all mappings. Omit
`endpointDiscovery` (or use `provider: kubernetes`) to keep native Pod/static
discovery. External mode disables Pod discovery for the entire router and
supports either Kubernetes resources or `--config-source=file` resource manifests.

No external provider is registered in the default binary. Implement
`discovery.Source` and supply a factory through `Server.EndpointDiscoveryRegistry`
before `Run`:

```go
server.EndpointDiscoveryRegistry = discovery.Registry{
    "custom": customdiscovery.NewSource,
}
```

```yaml
endpointDiscovery:
  provider: custom
  refreshInterval: 5s
  snapshotTimeout: 2s
  maxConcurrentSnapshots: 4
  requireInitialSnapshot: true
  mappings:
    - modelServer: default/example-model
      parameters:
        service: example-model
```

`customdiscovery.NewSource`, `custom` and `service` are illustrative.
Providers define and validate their parameters. YAML cannot load implementations;
an unregistered provider fails startup. Selecting different providers per
ModelServer and using Gateway API Inference Extension with external discovery
are not supported.

For a complete implementation, build commands and a local routing smoke test,
see [Build an endpoint discovery provider](./endpoint-discovery-provider.md).

### Configuration and ownership

| Setting | Default | Scope and behavior | Constraints |
| --- | --- | --- | --- |
| `provider` | Empty, equivalent to `kubernetes` | Process-wide discovery mode; an external provider's factory creates a separate Source for each mapped ModelServer | External provider must be registered before `Run`; no native Pod discovery fallback |
| `refreshInterval` | `30s` | Periodic delay after a refresh completes, per Source; approximately +/-10% jitter, with earlier notification-driven refreshes | Positive duration; not an endpoint-freshness guarantee |
| `snapshotTimeout` | `5s` | Context timeout for each `Source.Snapshot` call | Positive duration; excludes slot waiting, factory construction and Store updates; Source must honor cancellation |
| `maxConcurrentSnapshots` | `4` | Shared provider-query concurrency across mappings in one process; each replica has its own limit | Omitted/0 uses the default; negative values are invalid; not an inference-request or endpoint-count limit |
| `requireInitialSnapshot` | `true` | Startup readiness waits for a valid applied snapshot of each existing mapped ModelServer; empty snapshots count as success | Gate latches once satisfied; no timeout bypass; `false` skips only this discovery gate |
| `mappings` | Empty | Binds ModelServers to independent Sources using the selected provider | Required in external mode; must be empty in native mode; no duplicate targets |
| `mappings[].modelServer` | None | Resource key in `namespace/name` format, not the request's model name; discovery starts when the resource is observed | Does not create the ModelServer; missing resources warn but do not block readiness |
| `mappings[].parameters` | `nil` | Provider-specific parameters passed unchanged to its factory | Provider defines required fields, defaults and validation |

Mapped resources use only provider membership; their selectors/static endpoints
are ignored. Unmapped static models work; unmapped selector-based models stay
empty, with warnings and no Pod fallback. ModelServer validation is unchanged:
declare valid `workloadSelector.matchLabels` and `workloadPort`, or static
endpoints. File-backed resource loading is not an endpoint discovery provider.

### Refresh and reconciliation

Each replica creates Sources independently as mapped ModelServers are observed.
Discovery runs in the background; request routing reads Store without querying
the provider. Deleting a ModelServer cancels its subscription and removes its
Store membership.

The first lookup starts immediately, subject to available concurrency slots.
Subsequent lookups run periodically or on provider notifications and ModelServer
spec changes. Notifications are coalesced; refreshes wait at least
`min(1s, refreshInterval)` after the previous refresh completes. Before the first
successful snapshot, scheduled retries back off exponentially from that minimum
up to `refreshInterval`, with jitter within these bounds. Jitter spreads periodic
queries but does not eliminate startup bursts. Slots are released before Store
updates.

Sources return complete snapshots and honor cancellation. Names are DNS-1123
subdomains (at most 253 characters), unique within a ModelServer; they may remain
stable across address/port changes. Supply PD labels explicitly.
The controller validates snapshots and manages synthetic Pods/Store updates:
successful empty results clear membership; lookup/validation failures retain the
entire last successful snapshot without an expiry limit.

### Readiness and runtime data

After resource/Store initialization, readiness waits for the first valid applied
snapshot of each existing mapped ModelServer, including successful empty results.
Missing/deleted resources do not block it. Server latches this gate once
satisfied; later discovery changes do not close it.

There is no automatic timeout bypass: a failing initial lookup can block new
replicas and stall rollout. Set `requireInitialSnapshot: false` to allow startup
without discovery completion, accepting temporarily unavailable model pools.

External mode constructs Store with `WithDeferredRuntimeInspection()`, including
for unmapped static models. Metrics/models are fetched by the bounded background
loop, not discovery; native initialization stays synchronous. Until inspection
succeeds, LoRA affinity excludes new instances and GPU-cache scoring sees zero
usage. The scrape interval does not bound this window; Ready does not certify
endpoint health, fresh metrics or capacity.

An endpoint's address/port can change without renaming it. Store updates the
synthetic Pod in place and emits a Pod update event, invalidating metrics/models
and discarding obsolete scrapes while preserving in-flight counters.

Names also identify affinity state within a ModelServer: an address/port update
does not invalidate name-keyed `prefix-cache` history or session-sticky bindings.
Normal cache eviction, binding TTL and scheduling filters still apply. Providers
must use a new name when a replacement must not inherit this affinity state,
for example after a restart that loses its cache. A replacement with an unchanged
name, address and port cannot be inferred from the snapshot.

Engine/workload-port edits reapply the raw snapshot without clearing membership
or in-flight counters. Omitted ports follow configuration, explicit ports stay
fixed, and obsolete scrapes are discarded. Store reconciliation remains a
multi-operation update, not an atomic swap.

### Observability

Metrics use `model_server` (`namespace/name`); addresses/parameters are not labels.

| Metric | Meaning |
| --- | --- |
| `kthena_router_discovery_refresh_total` | Count by `result`: `success`, `error`, or `discarded` (obsolete/cancelled work) |
| `kthena_router_discovery_refresh_duration_seconds` | Duration including slot waiting and reconciliation |
| `kthena_router_discovery_last_success_timestamp_seconds` | Last applied snapshot timestamp; 0 before initial success |
| `kthena_router_discovery_endpoints` | Last successfully applied endpoint count |

Failures leave snapshot gauges unchanged; deletion removes them and replacement
resets them. Configuration-only reapplication does not advance the last-success
timestamp. Alert on missing initial success/stale snapshots; these metrics are
not health checks. Missing mapped resources and ignored static endpoints emit
warnings.
