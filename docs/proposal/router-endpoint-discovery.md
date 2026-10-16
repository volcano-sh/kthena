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

Draft implementation for [issue #1864](https://github.com/volcano-sh/kthena/issues/1864).
Introduce an in-process discovery extension point that reuses the networking API's
`Endpoint`, synthetic Pods, and `datastore.Store`. Providers supply full snapshots;
a shared controller handles Store synchronization. Interface and configuration
choices remain subject to community discussion.

### Motivation

Serving instances can be registered outside the router's Kubernetes cluster.
Integrations should not need to modify the scheduler or proxy to discover them.

#### Goals

- Preserve default Pod discovery and static endpoint behavior when not enabled.
- Share reconciliation between static endpoints and external discovery.
- Support Kubernetes-backed and file-backed ModelServer configuration.
- Distinguish lookup failure from successful empty membership.

#### Non-Goals

- New CRD fields, another Endpoint type, or changes to the inference request path.
- Bundled discovery backends, vendor-specific registries, dynamically loaded Go
  plugins, or a remote plugin RPC.
- Multiple discovery providers in one router, dynamic mapping reload, or discovery
  for Gateway API Inference Extension InferencePools.

### Proposal

Select one compiled-in provider through `endpointDiscovery.provider`. Explicit
`namespace/name` mappings provide its parameters for each ModelServer. Factories
are registered by name in a constructor-supplied `Registry`. Integrations inject
their factories through `Server.EndpointDiscoveryRegistry` before starting the
server. No external providers are bundled; an unregistered provider name fails
startup. The default native discovery path does not require a registry.

```text
Kubernetes / resource files -> ModelServer lifecycle
                                      |
Provider Source -> Endpoint snapshot -> Discovery controller -> synthetic Pods -> Store
```

External mode disables the Pod informer, not the ModelServer resource source.
Mapped ModelServers have a single owner for membership: the discovery controller.
Their selector and static endpoints cannot overwrite discovered instances.
Unmapped static ModelServers remain supported; unmapped selector-based models
have empty pools and emit warnings, rather than silently falling back to Pods.

The ModelServer schema and validation stay unchanged. A mapped ModelServer must
still declare a valid selector and workload port, or valid static endpoints. This
is a compatibility constraint of the draft, not a claim that a selector-only
resource shape is ideal for external discovery. A future API change would need
separate agreement and generated schema/client updates.

### Design Details

`Source` provides `Snapshot(context.Context) ([]networkingv1alpha1.Endpoint, error)`
and `Close()`. A factory receives a ModelServer name, read-only parameters, and a
nonblocking refresh callback. It must not perform blocking discovery I/O.

`Controller` receives the Store and implements `Run(ctx)` / `Close()`. ModelServer
controllers and file sources delegate updates and deletions to it. The existing
ModelServer controller continues managing transport configuration. Provider code
does not access Store internals.

- Refresh hints are coalesced; periodic snapshots repair missed hints. At most
  four snapshot/reconciliation operations run concurrently.
- Snapshot and Close are serialized per source. Providers must honor cancellation
  and release only their own subscriptions on Close.
- Writes are serialized per ModelServer. Deletion cancels its subscription;
  generation checks discard results from replaced or updated ModelServers.
- Successful snapshots are validated in full before Store writes. Errors retain
  the last successful membership; a successful empty snapshot clears it.
- Endpoint names are scoped to the ModelServer. Providers must change names on
  instance replacement, including address or port changes. Role/group labels must
  be supplied explicitly for PD routing; identity-dependent runtime integrations
  still require compatible instance identities.
- Default refresh interval is 30 seconds, with a 5-second snapshot timeout.
  Mappings and provider selection are startup configuration; changes need restart.
- Controller readiness is resource initialization, not proof of healthy discovery.
  Initial discovery failures leave mapped pools empty and are retried. Provider
  factory failures are also logged and retried in the background.

#### Risks and Mitigations

Retaining the last successful snapshot can leave stale endpoints indefinitely
during a registry outage. This draft does not introduce a maximum stale age or
claim routing health from registry membership. A stale-snapshot eviction policy
remains an explicit follow-up design question.

Store reconciliation consists of multiple operations, not an atomic snapshot
swap. The controller provides single-writer ownership and retries failures, but
does not add transactional visibility to the existing Store.

Providers are trusted, compiled-in code. Snapshot cancellation cannot forcibly
stop a provider that ignores context. Implementations must bound discovery I/O,
honor cancellation, and release subscriptions promptly on Close.

#### Test Plan

Unit tests cover configuration/registry validation, add/update/remove, successful
empty snapshots versus lookup errors, invalid snapshots, per-endpoint ports and
labels, stale results after deletion or metadata change, cancellation, refresh
notifications, periodic repair, and independent targets. Integration-style unit
tests verify ownership delegation through both the ModelServer controller and
file resource source. Test-only sources exercise factory injection and lifecycle
without introducing a production backend. Existing native and static endpoint
tests remain in place.

Run scoped tests with the race detector, then repository lint/tests before opening
a PR. Cluster and standalone smoke tests should validate actual routing and
membership replacement; unit coverage alone is not an end-to-end verification.

### Alternatives

- An external synchronizer can continue updating `ModelServer.spec.endpoints` or
  resource files. That remains appropriate when no in-process extension is needed.
- Providers could write directly to Store. Shared helpers would still be needed
  to keep deletion, identity and failed-snapshot behavior consistent.
- Per-ModelServer provider selection and mixed Pod/external discovery could be
  added later; this draft uses a process-wide mode to keep ownership explicit.
