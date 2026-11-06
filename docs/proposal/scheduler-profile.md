---
title: "Scheduler Profile: Per-ModelServer Plugin Combinations"
authors:
- "@0xjasoncao"
reviewers:
- "@YaoZengzeng"
- TBD
approvers:
- TBD

creation-date: 2026-10-30

---

## Scheduler Profile: Per-ModelServer Plugin Combinations

## Summary

The kthena router builds a single global scheduler plugin chain from its
ConfigMap, and every model served by the router is scheduled by that one
chain. Different models have different serving characteristics — a small
model tolerates deep engine queues, a latency-sensitive model needs tight
thresholds and aggressive cache affinity — so a single global chain cannot
fit all of them.

This proposal introduces `SchedulerProfile`, a CRD that holds a complete
scheduler plugin combination (filter and score plugins with weights and
args). ModelServers that need custom scheduling reference a profile by
name; unreferenced ModelServers fall back to the global scheduler
configuration. Multiple ModelServers can share one profile.

## Motivation

The starting point was
[#1899](https://github.com/volcano-sh/kthena/issues/1899): the
`least-request` filter uses a single global `maxWaitingRequests` for all
ModelServers, which is too strict for some models and too loose for
others. The field-by-field fix ([PR
#1912](https://github.com/volcano-sh/kthena/pull/1912)) addressed one
knob, but each additional per-ModelServer scheduling requirement would
add another field to the ModelServer API, and over time the spec would
grow into an unstructured mix of scheduling concerns (raised in the
community meeting of 2026-10-27).

Two further weaknesses of keeping scheduling configuration in the router
ConfigMap surfaced during the discussion:

- **Late validation.** A bad plugin combination in a ConfigMap is only
  caught when the router reloads the config — today a bad global config
  even crashes the router (`Fatalf` on load). With an API object, a
  validating webhook rejects invalid combinations at `kubectl apply`.
- **No sharing or isolation.** A ConfigMap is one global text blob
  edited by cluster administrators; it cannot be shared selectively,
  namespaced, or referenced.

### Goals

- A `SchedulerProfile` CRD whose spec reuses the existing scheduler
  configuration structure (pluginConfig + plugins with weights and args),
  so users keep writing the same format they know.
- ModelServer references a profile by name (same namespace); unset means
  global fallback.
- Validation at apply time: CRD schema for mechanical mistakes, a
  validating webhook for semantic mistakes (unknown/duplicate plugins,
  invalid plugin args, plugin role capability, plugin combinations).
- The router builds and caches one plugin chain per profile; the chain
  for a request is resolved once in `doLoadbalance` and carried on the
  request context.
- Zero change for existing users: no profile and no reference → today's
  behavior.

## Proposal

### User Stories

- Story 1: An operator runs a 7B model (fast, queue-tolerant) and a 70B
  model (slow, queue-sensitive). Each references a profile tuned to its
  capacity; traffic to each is filtered and scored by its own chain.
- Story 2: Three ModelServers share one `cache-heavy-profile` with
  prefix-cache weight 3. Updating the profile updates all three at once —
  the policy is written once and updated in one place.
- Story 3: A user applies a profile with a typo'd plugin name, a plugin
  used outside its capable role, or a negative weight. The apiserver
  schema and the validating webhook reject it at apply time — the mistake
  never reaches the router.

## Design Details

### API

`SchedulerProfile` lives in the `networking.serving.volcano.sh` group
(next to ModelServer and ModelRoute, whose scheduling behavior it
shapes). Its spec reuses the structure of the router's global scheduler
configuration:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: SchedulerProfile
metadata:
  name: cache-heavy-profile
  namespace: default
spec:
  pluginConfig:
  - name: least-request
    args:
      maxWaitingRequests: 50
  plugins:
    filter:
      enabled:
      - least-request
    score:
      enabled:
      - name: prefix-cache
        weight: 3
```

ModelServer gains one optional reference field:

```yaml
apiVersion: networking.serving.volcano.sh/v1alpha1
kind: ModelServer
spec:
  trafficPolicy:
    loadBalancer:
      schedulerProfile: cache-heavy-profile   # unset → global scheduler config
```

### How a profile relates to the global ConfigMap

A SchedulerProfile is a complete plugin combination. For a ModelServer
that references a profile, the global chain does not run at all for that
ModelServer's requests:

- Plugins not listed in the profile do not run — even if the global
  ConfigMap enables them.
- Plugins listed without a `pluginConfig` entry run with their built-in
  defaults, not the args from the global ConfigMap.

Two cases decide which chain serves a ModelServer's requests:

- **The ModelServer has no profile reference**: it uses the global chain.
  This is the only case that falls back to the global chain.
- **The ModelServer has a profile reference that cannot be resolved**
  (build failure or dangling reference): its requests fail directly
  (503 with a reason).

Users migrating from the global config must copy every plugin and arg
they rely on into the profile — nothing carries over.

Each profile chain builds its own plugin instances from its own args
(see [Plugin lifecycle](#plugin-lifecycle)), so every plugin's args —
including the stateful prefix-cache and kvcache-aware — take effect per
profile.

### Why a referenced CRD instead of extending ModelServer

- **Sharing**: one profile can be referenced by many ModelServers — the
  policy is written once and updated in one place, instead of being
  duplicated (and drifting) across every ModelServer that embeds it.
- **Lifecycle decoupling**: scheduling tweaks happen far more often than
  model deployments. With a separate CRD, tuning a profile never touches
  the ModelServer object.
- **Separation of concerns**: ModelServer describes what is running; the
  profile describes how its requests are scheduled.

### Validation layers

1. **Schema (apiserver)**: field types, `weight > 0`, name lengths —
   from kubebuilder markers on the CRD.
2. **Validating webhook (router)**: two kinds of checks.
    - *Structural* (webhook-local): duplicate plugin entries, score
      weight ranges.
    - *Plugin-dependent*: plugin names must be registered, args must pass
      each plugin's validator, and plugins must support their declared
      role (filter vs score). These checks are delegated to the plugins
      package — the single source of truth for the plugin set — so the
      webhook does not depend on the scheduling engine.
3. **Chain construction (runtime)**: the last line of defense when the
   ChainManager materializes a profile into a plugin chain.

Plugin extensibility note: plugin args are intentionally free-form
(`RawExtension`) so adding a plugin never changes the profile API. CRD
schemas cannot express per-discriminant sub-structures (a field's shape
cannot be selected by another field's value), which is exactly why the
ecosystem pairs such blobs with validating webhooks — the trade-off is
documented here.

### Plugin registration (single source of truth)

The plugins package owns a single registration map
(`plugins.SchedulerPlugins`): one entry per plugin holding its args
validator and its builders for the roles it serves (score and/or
filter). All consumers derive from this map — nothing is registered or
validated anywhere else:

- the scheduler factory, when registering default plugins and building
  chains;
- the ChainManager, when materializing a profile into a chain;
- the admission webhook, when validating profile plugin entries.

Adding a plugin means adding the plugin implementation plus one entry
here; registration, validation, role capability and the known-plugin set
all follow automatically.

### Plugin-set-derived state lives on the chain

Two pieces of scheduler state are derived from the plugin set rather than
configured directly; with per-profile chains they must be derived per
chain, not globally:

- `postScheduleHooks` — the score plugins that also implement
  `PostScheduleHook`; already carried on the chain.
- `syncOnFlight` — whether scheduling must pull on-flight counts from
  Redis first (required by least-request scoring under multiple router
  replicas); derived at chain-build time from whether this chain's plugin
  set includes least-request, and carried on the chain. A profile that
  enables least-request while the global config does not now syncs
  correctly. Redis absence degrades gracefully: `SyncOnFlightCounts` is a
  no-op without a Redis-backed counter, and the chain falls back to
  local-only on-flight counts.

### Plugin lifecycle

#### Stateful plugins hold long-lived resources

Two score plugins are stateful: their instances hold resources that
outlive a single request and must be stopped when the instance goes away.

`prefix-cache` — each instance owns a `ModelPrefixStore` (per-pod hash
LRUs for prefix matching) and registers two process-wide attachments:

- a `"Pod"` store callback that purges a pod's hash entries on deletion
- a provider that feeds the `kthena_router_prefix_cache_entries` gauge

`kvcache-aware` — memory mode (the default) owns a `KVBlockMemoryIndex`
plus two background workers: a KV event subscriber that dials runtime
sidecars and streams block events into the index, and an index GC loop.
It registers a `"Pod"` store callback that feeds pod lifecycle into the
subscriber. Redis mode runs a ticker-based GC loop over stale fields.

Since every profile chain builds its own plugin instances and every
profile update rebuilds the chain, one update without cleanup leaks one
set of goroutines, callbacks and metric providers per instance.

#### Store: callbacks are now unregisterable

`datastore.Store.RegisterCallback` used to append to a slice with no way
to remove an entry. It now returns an unregister function: callbacks are
stored as `{id, fn}` entries (Go function values are not comparable, so
removal is by assigned id), and the idempotent unregister closure deletes
its own entry. Existing callers are unaffected — the return value is
optional to use. `triggerCallbacks` snapshots the callback list under the
read lock, so concurrent unregister during event dispatch is safe.

#### The two plugins now release their resources

Both plugins keep the unregister function from registration and expose an
idempotent `Close`:

- `prefix-cache`: `Close` unregisters the pod-deletion callback and the
  gauge provider. The hash data stays readable for in-flight requests; it
  only stops being maintained.
- `kvcache-aware`: `Close` cancels the subscriber and GC goroutines and
  unregisters the pod callback (memory mode), or stops the GC ticker
  (redis mode).

#### Close hook and its timing

Plugins holding resources implement `framework.PluginCloser`
(`Close() error`, must be idempotent). The ChainManager closes the
plugins of replaced and deleted chains. The global chain is never closed:
it is built once at startup, never rebuilt, and not owned by the
ChainManager — its lifetime is the router process itself.

Close does not run at swap time. In-flight requests carry the old chain
on their context and keep using its instances across their whole
lifetime — scoring during `Schedule`, bookkeeping during `RunPostHooks` —
and a `Close` racing those uses could free a resource a request is still
using. Instead, a chain
tracks its in-flight requests: `Acquire` when a request resolves it,
`Release` when `doLoadbalance` returns (post hooks run inside that call
stack). A replaced or deleted chain is retired and closed only when its
in-flight count drains to zero. The gate applies to every chain — for
chains whose plugins hold nothing, the wait simply ends in a no-op
`Close`. The counter is router-local (no
cross-instance coordination) and a crashing router releases everything
with the process.

Plugin lifecycle timeline:

```mermaid
sequenceDiagram
    participant E as store event (profile add/update/delete)
    participant CM as ChainManager
    participant NC as new plugin instances
    participant OC as old plugin instances
    participant R as in-flight requests

    E->>CM: onProfileEvent
    CM->>NC: build plugin instances
    NC->>NC: start background workers, register store callbacks and metrics providers
    CM->>CM: swap chain cache to the new chain
    CM->>OC: retire (no new request resolves it)
    R->>OC: Acquire'd earlier: Schedule + RunPostHooks keep using old instances
    R-->>OC: request done, Release
    OC->>OC: in-flight == 0 → Close
    OC->>OC: stop goroutines, unregister Pod callbacks and metrics provider
```

Tuning a profile's args discards the learned cache data of its instances;
the cache warms up again as traffic flows.

### Chain management and resolution

A dedicated `ChainManager` (in the scheduler package, used by the
router) owns the per-profile chains. The `SchedulerImpl` itself stays
chain-agnostic — it consumes whatever chain the request context carries.

1. Router startup: the global chain is built from the ConfigMap by
   `NewScheduler` (unchanged); the ChainManager registers a
   SchedulerProfile store callback.
2. A SchedulerProfile informer keeps the router's store in sync; the
   store callback triggers the ChainManager to build a chain per profile
   and atomically swap it into an internal cache.
3. `doLoadbalance` resolves the chain once per request — the
   ModelServer's profile reference is looked up in the ChainManager —
   and carries it on the request context (`ctx.ScheduleChain`). A
   ModelServer without a profile reference uses the global chain; a
   ModelServer whose reference cannot be resolved fails its requests
   directly.
4. `Schedule` consumes the context chain for filtering and scoring;
   `RunPostHooks` (after dispatch) reuses the same chain for post-schedule
   bookkeeping (e.g. prefix-cache recording). When no chain is on the
   context (no profile reference), both fall back to the global chain.

The full flow:

Profile chain lifecycle (control plane):

```mermaid
flowchart TD
    U["kubectl apply SchedulerProfile"] --> W{"validating webhook"}
    W -- "reject: unknown plugin / bad args / role or combination rules" --> U
    W -- "allow" --> S["etcd"]
    S -- "watch" --> I["SchedulerProfile informer → router store"]
    I --> CM["ChainManager: build one plugin chain per profile"]
    CM --> CACHE["profile chain cache"]
```

Per-request resolution (data plane) — `doLoadbalance` resolves once against
the profile chain cache and carries the result on the request context;
`Schedule` falls back to the global chain only when the ModelServer has no
profile reference. A broken reference fails the request instead of
falling back. The request `Acquire`s its chain on resolve and `Release`s
it when `doLoadbalance` returns (post hooks run inside that call stack) —
a retired chain closes only after its last in-flight request releases:

```mermaid
flowchart TD
    R["doLoadbalance"] --> RES{"resolve: ModelServer → schedulerProfile"}
    RES -- "no reference" --> G["global chain (router ConfigMap, never closed)"]
    RES -- "hit: profile chain built" --> ACQ["chain.Acquire + ctx.ScheduleChain = chain"]
    RES -- "broken reference: build failed / profile deleted" --> FAIL["request fails: 503 + reason"]
    ACQ --> SCH["Schedule: filters → scores (chain via ctx)"]
    G --> SCH
    SCH --> H["RunPostHooks: post-schedule bookkeeping (same chain via ctx)"]
    H --> REL["doLoadbalance returns → chain.Release"]
    REL --> DRAIN{"chain retired and in-flight == 0?"}
    DRAIN -- "yes" --> CLOSE["close old chain's plugins"]
    DRAIN -- "no" --> KEEP["chain stays open for remaining requests"]
```

### Update and deletion semantics

- Profile update: rebuild the chain, swap it into the cache atomically;
  requests already scheduled finish on the chain they were admitted
  with (the resolved chain travels on the request context).
- Profile deletion: the chain is removed from the cache; referencing
  ModelServers cannot resolve their reference anymore, so their requests
  fail directly (503 with a reason) until the reference is updated or
  removed; the condition on ModelServerStatus records the cause. Profile
  deletion itself is not blocked.

### Failure handling when building plugin chains

Validation lives in each plugin's args validation method (e.g.
`ValidateKVCacheAwareArgs`), which returns an error for invalid
configuration. Plugin constructors must return an error for invalid
configuration — destructive aborts such as `klog.Fatalf` are not
allowed. Chain building runs the full validation before constructing any
plugin (the validating webhook already checked the same rules) to guard
against version skew between the webhook and this binary, or objects
written while the webhook was unavailable. The global chain keeps its
existing ConfigMap handling unchanged.

Chain building is triggered when a SchedulerProfile is created or
updated, and its failures fall into two cases:

- Chain build failure: the profile's args fail validation. On create,
  the chain is not cached and requests to referencing ModelServers fail
  directly (503 with the reason). On update, the previously built chain
  keeps serving, and the failed rebuild is recorded on the condition.
- Dangling reference: a referenced profile is deleted (or not yet
  created), so the chain cannot be resolved, and requests to
  referencing ModelServers fail directly.

Both failures surface as a `SchedulerProfileResolved` condition on the
ModelServer's status:

```yaml
status:
  conditions:
  - type: SchedulerProfileResolved
    status: "False"
    reason: ProfileResolutionFailed     # or ProfileNotFound for a dangling reference
    message: 'invalid args for plugin "least-request": maxWaitingRequests must be positive'
    lastTransitionTime: "2026-10-31T08:12:00Z"
```

## Alternatives

- **Inline scheduling configuration in ModelServer.spec**: rejected —
  the configuration would be duplicated across ModelServers instead of
  shared, and every scheduling tweak would trigger ModelServer spec
  changes.
- **Per-ModelServer sections in the router ConfigMap**: rejected — a
  ConfigMap is free-form text with no schema, so a bad plugin
  combination is hard to validate and only surfaces when the router
  reloads the config instead of at `kubectl apply`.
