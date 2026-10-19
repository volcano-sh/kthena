---
title: Pluggable Endpoint Discovery in Kthena Router
authors:
  - "@huangshengh"
reviewers:
  - TBD
approvers:
  - TBD
creation-date: 2026-10-10
---

## Pluggable Endpoint Discovery in Kthena Router

### Summary

For [issue #1864](https://github.com/volcano-sh/kthena/issues/1864), add an
in-process discovery extension point. Providers return existing networking
`Endpoint` objects; a shared controller maintains synthetic Pods in
`datastore.Store`, without adding discovery I/O to inference requests.

### Motivation

Model instances may be registered outside the router's Kubernetes cluster.
Integrations need to discover them without changing scheduling or proxying.

#### Goals

- Preserve native Pod/static discovery when the extension is disabled.
- Share endpoint reconciliation across providers and static endpoints.
- Support both Kubernetes-backed and file-backed ModelServer resources.

#### Non-Goals

- CRD changes, bundled providers, remote RPC or dynamically loaded plugins.
- Mixed Pod/external discovery, multiple providers per router, mapping hot reload,
  or external discovery for Gateway API Inference Extension InferencePools.

### Proposal

Select one compiled-in provider and map ModelServers to its parameters.
External mode disables the Pod informer, not resource configuration loading.

| ModelServer in external mode | Membership owner |
| --- | --- |
| Mapped | Discovery controller; selector/static endpoints are ignored |
| Unmapped, with static endpoints | Shared static endpoint reconciliation |
| Unmapped, selector only | Empty pool with a warning; no Pod fallback |

#### User Stories

- Discover instances across clusters from a registry without watching each cluster.
- Add a registry integration while reusing the existing scheduler, proxy and Store.

#### Risks and Mitigations

- **Startup:** by default, one failing mapped model can prevent a new replica from
  becoming Ready and stall rollout. `requireInitialSnapshot: false` permits
  startup without discovery completion, accepting unavailable model pools.
- **Staleness:** lookup/validation failures retain the entire last valid snapshot,
  possibly indefinitely. Endpoint count and last-success metrics support alerts;
  they do not certify endpoint health.
- **Cold instances:** runtime inspection is asynchronous in external mode. Until
  it completes, LoRA filtering excludes new instances and GPU-cache scoring sees
  zero usage. Readiness does not imply populated metrics or inference capacity.
- **Consistency:** Store updates are serialized per ModelServer, not an atomic
  snapshot swap. A provider ignoring cancellation can delay refresh or shutdown.

### Design Details

#### Integration

Implement `discovery.Source` and register a `Factory` in
`Server.EndpointDiscoveryRegistry` before `Run`:

```go
type Source interface {
    Snapshot(context.Context) ([]networkingv1alpha1.Endpoint, error)
    Close()
}

type Factory func(types.NamespacedName, map[string]interface{}, func()) (Source, error)
type Registry map[string]Factory
```

The factory receives the ModelServer name, read-only parameters and a refresh
hint callback. It must avoid blocking I/O; Source must honor cancellation, return
immutable full snapshots and promptly release its own subscription on Close.
Providers never receive Store access. Unknown provider names fail startup.

```yaml
endpointDiscovery:
  provider: custom
  refreshInterval: 30s
  snapshotTimeout: 5s
  maxConcurrentSnapshots: 4
  requireInitialSnapshot: true
  mappings:
    - modelServer: default/example-model
      parameters:
        service: example-model
```

`custom` is integration-supplied; no providers are bundled. Provider/mapping
changes require restart. ModelServer validation remains unchanged: declare valid
selector/port fields or static endpoints. Operational details are in the
[configuration guide](../kthena/docs/user-guide/config-router.md#external-endpoint-discovery).

#### Reconciliation

```text
Kubernetes / resource files -> ModelServer lifecycle
                                      |
Provider Source -> Endpoint snapshot -> Discovery Controller -> synthetic Pods -> Store
```

- The controller implements `Run(ctx)` / `Close()`. It serializes Snapshot/Close
  per source and Store writes per ModelServer. Deletion cancels the subscription;
  subscription/version checks discard obsolete results.
- Validate the whole snapshot before writing. A successful empty snapshot clears
  membership; any invalid endpoint rejects the snapshot. Names follow Endpoint
  DNS-1123 rules and may remain stable across address/port changes. These changes
  update the synthetic Pod in place, invalidating metrics/models and older scrapes
  while preserving in-flight counters and emitting a Pod update event.
  Providers supply PD role/group labels and identities compatible with runtime
  cache/session integrations.
- Reuse the raw snapshot on engine/workload-port changes, preserving membership
  and in-flight counters. Omitted ports follow configuration; explicit ports stay
  fixed. Store invalidates affected runtime data and discards older scrapes.
- External mode uses `WithDeferredRuntimeInspection()`, including for unmapped
  static models. The existing bounded scrape loop handles runtime I/O; native
  initialization and the public Store interface remain unchanged.
- Bound provider queries by `maxConcurrentSnapshots` (default 4), releasing slots
  before reconciliation. Refresh hints are coalesced/rate-limited; periodic
  refresh repairs missed hints. Scheduled initial retries back off from
  `min(1s, refreshInterval)` to the interval; refreshes use bounded jitter.
  The immediate first lookup can still produce a startup burst.
- After resource/Store initialization, Server waits for each existing mapped
  ModelServer's first valid applied snapshot, including an empty one. Missing or
  deleted resources do not block readiness. Once satisfied, the gate is latched;
  later model changes cannot close it. There is no automatic timeout bypass.

#### Test Plan

Keep native/static regression tests. Cover snapshot validation and failure
retention, lifecycle/obsolete results, cancellation, query limits, retry/jitter,
readiness latching, configuration updates and cold-start scheduling. Exercise
both resource-source integrations with test-only providers.

Run affected packages with `-race`, repository lint and unit tests. Actual
registry/routing smoke tests remain necessary; unit tests are not end-to-end
verification.

### Alternatives

- **External synchronizer:** update static endpoints or resource files without an
  in-process extension; suitable when existing resource publication is enough.
- **Provider writes Store:** duplicates ownership, deletion and failure handling.
- **Mixed discovery:** defer per-ModelServer Pod/external selection to keep initial
  ownership explicit; current mappings already identify individual ModelServers.
