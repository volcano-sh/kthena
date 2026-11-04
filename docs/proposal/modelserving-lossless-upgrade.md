---
title: Lossless Upgrade for ModelServing
authors:
- "@chenhuiluo"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-10-20

---

## Lossless Upgrade for ModelServing

### Summary

This proposal adds a drain step to `ModelServing` rolling updates: an old Pod is
deleted only after its in-flight inference requests finish, instead of being
SIGKILLed immediately by `DeleteCollection`. The drain lives at the Pod
boundary, so no component's core logic grows:

- The **controller issues the delete unchanged** — its delete path grows by
  nothing (only two optional env vars injected at Pod creation, for PD
  deployments).
- The **router drains reactively**: when it sees `deletionTimestamp` it stops
  dispatching new requests to that Pod while keeping the Pod tracked, so
  in-flight requests finish against real traffic.
- A **`preStop` hook inside the Pod** polls the engine's own metrics and exits
  only once in-flight reaches zero; SIGTERM then reaches an idle engine.
- `terminationGracePeriodSeconds` (plus an in-hook timeout) bounds the wait, so
  the update always completes.

This is best-effort lossless: the normal path is lossless for engines whose
metrics the hook can read (vLLM including ascend, SGLang), and residual cases
fall back to the grace period — never worse than today.

### Motivation

Updating a `ModelServing` (new image, engine config change) deletes
old-revision ServingGroups/Roles via `DeleteCollection` with empty
`DeleteOptions` — old Pods are removed immediately, with no wait for in-flight
requests. LLM inference requests, especially streaming, often run for tens of
seconds to minutes, so an immediate delete SIGKILLs in-flight work and clients
see 502 / connection resets.

The existing rolling strategy (`maxUnavailable: 0` + `maxSurge: 1`) already
keeps serving capacity intact during an update — what is missing is the
transition on the "delete old" step: nothing stops the router from dispatching
new requests to the Pod being deleted, and nothing waits for its in-flight
requests to finish.

#### Goals

- Drain an old Pod before its containers are terminated: no new dispatch, then
  in-flight finishes, then SIGTERM.
- Keep the **controller's delete path unchanged** — the drain must not add
  controller complexity.
- No Redis dependency, no new annotations or finalizers, no K8s API access from
  inference Pods, and no new engine metrics.
- Cover PD disaggregated workloads: a prefill Pod waits for its paired decodes
  to drain before exiting (pairing resolved via an injected env + headless
  service DNS — no API access needed).
- Be robust to multi-replica router deployments by construction (see
  [Multi-replica routers](#multi-replica-routers)).

#### Non-Goals

- Engines whose metrics the hook cannot parse: those Pods fall back to the
  grace period (same behavior as today, bounded by `preStop` +
  `terminationGracePeriodSeconds`).
- Changing the rolling strategy itself.
- Absolute lossless: normal path is lossless, residual cases fall back to the
  grace period.

### Proposal

Three components, each with one small job:

| Component | Job |
|-----------|-----|
| controller | issues the delete as today; at Pod creation, injects env vars (paired decode discovery for PD prefill Pods, and `GRACE_SECONDS` for the hook's auto-correction — see [Configuration](#configuration)) |
| router | on `deletionTimestamp`: keep the Pod tracked and exclude it from all scheduling-candidate queries (no new dispatch, in-flight still served) |
| Pod (`preStop` hook) | poll the engine's own metrics; exit only when in-flight reaches zero; kubelet then sends SIGTERM to an idle engine |

```mermaid
sequenceDiagram
    autonumber
    participant C as controller
    participant A as K8s API
    participant R as kthena-router
    participant P as old Pod

    C->>A: delete Pod (unchanged path)
    A-->>R: Pod event (deletionTimestamp)
    R->>R: keep tracking, exclude from candidate pool<br/>(no new dispatch, in-flight still served)
    Note over P: kubelet runs preStop → SIGTERM is held back
    loop preStop hook (inside Pod)
        P->>P: poll localhost engine metrics
    end
    alt in-flight reaches zero
        P->>P: preStop exits
        A->>P: SIGTERM → engine exits fast
    else in-hook timeout elapses
        P->>P: preStop exits (logs a warning)
        A->>P: SIGTERM → residual in-flight finishes during grace
    end
    Note over P: grace period ends → SIGKILL (last resort)
```

Because the hook runs before SIGTERM, the container keeps serving its in-flight
requests at full speed while the drain completes — the drain and the engine
teardown never overlap.

#### User Stories

- **Image update under live traffic**: without drain, a Pod serving a long
  streaming request is killed mid-response. With drain, the old Pod finishes its
  in-flight response before SIGTERM — no client-visible interruption.
- **Scale-down**: the Pod about to be removed drains first, so no request is
  dropped.
- **PD disaggregated update**: under the default ServingGroup-level drain, the
  whole group drains together; the prefill Pod's hook waits until every paired
  decode Pod reaches zero in-flight — its KV has been delivered (push) or
  pulled, so no PD request is broken.

#### Risks and Mitigations

- **preStop fails silently** (tooling missing in the image, script bug, RBAC):
  the hook exits early and the Pod degrades to today's behavior. Mitigation:
  the hook logs every step, kubelet emits a `FailedPreStopHook` event on
  failure, and the grace period still bounds termination — degraded, not stuck.
- **Drain and engine shutdown share the grace budget**: preStop execution time
  counts against `terminationGracePeriodSeconds`. Mitigation: the hook's
  effective wait is auto-clipped below the grace deadline (see
  [Configuration](#configuration)), and the grace period is documented to cover
  the drain timeout plus the engine's SIGTERM shutdown.
- **Drain window length under PD group drain**: waits for the slowest Pod
  (typically decode). Safe, but lengthens the window; the in-hook timeout
  bounds it.

### Design Details

#### Router: drain on deletionTimestamp

On `deletionTimestamp` the router keeps the Pod in its store — in-flight
metrics keep being scraped — while the scheduling-candidate queries exclude
Pods carrying a `deletionTimestamp` (via the existing `IsDraining` checks,
driven by the Pod's own deletionTimestamp). The exclusion points cover
`GetPodsByModelServer`, `GetDecodePods`, `GetPrefillPods`,
`GetPrefillPodsForDecodeGroup`, and `GetPodsByInferencePool`. New requests
therefore go only to non-draining Pods, while the in-flight requests on the
draining Pod keep running to completion. The signal delivery is guaranteed by
the standard client-go informer behavior: setting `deletionTimestamp` is a Pod
update (bumping resourceVersion), and the router's Pod informer registers
unfiltered handlers — every replica receives the transition event. The Pod
leaves the store on the object-delete event.

#### preStop: judge in-flight from engine metrics

The hook (part of the Pod template, engine-agnostic) polls
`localhost:<port>/metrics` and reads the engine-agnostic counters
(`RequestRunningNum` / `RequestWaitingNum`, mapped per engine through the
`backend/vllm` / `backend/sglang` adapters' same metric names). Exit condition:
the counters stay at zero for N consecutive polls (with a short settle delay) —
the repetition closes the dispatch-to-admission window where a request has been
forwarded by the router but not yet admitted by the engine. The hook also runs
with an internal timeout — the effective wait is `min(DRAIN_TIMEOUT,
70% × GRACE_SECONDS)` (see
[Configuration](#configuration)): if zero is not reached in time, it logs a
warning and exits, letting SIGTERM + grace handle the residual.

Judging from engine metrics has a visibility advantage: the counters reflect
direct-to-engine traffic as well, and they are local to the Pod — no K8s API
access, no ServiceAccount/RBAC changes, no cross-component signaling.

#### PD disaggregation: pairing without API access

A PD request has a prefill phase (compute KV) and a decode phase (generate),
connected by a KV handoff (SGLang push, NIXL/Mooncake pull). On the push/pull
connectors a prefill Pod's own metrics fire too early — SGLang's prefill
`num_running_reqs` is always 0 (the request lives in a separate inflight
queue), and NIXL's prefill reports zero before decode finishes pulling — so a
prefill cannot judge itself.

The rule: a prefill is safe to exit only when every paired decode Pod is at
zero in-flight. The pairing is resolved without any controller/router
signaling:

- At Pod creation the controller injects two env vars into PD prefill Pods:
  the paired decode role's headless service DNS name and its metrics port.
- The hook resolves the headless service DNS each poll, so the decode list is
  always current (recreated or scaled decodes are picked up automatically; a
  decode that no longer resolves is treated as drained — it cannot pull KV
  anymore).
- The hook polls each decode's metrics endpoint directly. PD deployments
  already require prefill↔decode network reachability for KV transfer, and the
  metrics port is the serving port, so the endpoints are reachable.

| Topology | Hook exit condition |
|----------|--------------------|
| Single-pod / aggregated | own `Running==0 && Waiting==0` (N consecutive polls) |
| PD decode | own `Running==0 && Waiting==0` (reliable on all connectors) |
| PD prefill | every paired decode at zero (own counters ignored where unreliable) |

Under the default `ServingGroupRollingUpdate` the whole group drains together
and the budget unit equals the drain unit, so prefill and decode are never
deleted independently. Residual cases (e.g. cross-request KV reuse) fall back
to the in-hook timeout and the grace period.

#### Multi-replica routers

The drain signal (`deletionTimestamp`) is persisted on the Pod object, and
every router replica watches Pods independently, so every replica drains the
Pod — there is no "replica A saw it, replica B did not" state to lose. The
replicas' informer latencies differ by milliseconds; requests dispatched in
that window become the Pod's in-flight work, and the hook waits for exactly
that — a longer drain, never a lost request. Because completion is judged from
engine metrics inside the Pod, no replica's local in-flight counter is trusted
for the drain decision, so multi-replica deployments without Redis work
unchanged.

#### Configuration

**Enablement**: lossless drain is on by default — the built-in engine templates
ship with the preStop hook and a default `DRAIN_TIMEOUT`. The switch lives at
the Pod template layer: `DRAIN_TIMEOUT=0` (or removing the hook) disables the
wait for that workload — the hook exits immediately and the Pod degrades to
today's delete behavior. An explicit one-off fast delete remains available via
the platform escape hatch (`kubectl delete pod --force --grace-period=0`, which
skips preStop entirely). The router drains unconditionally either way; for Pods
without a hook this is harmless (SIGTERM follows within seconds) and strictly
better than today (in-flight tracking is kept instead of dropped).

**Two values, one split rule**: the hook reads `DRAIN_TIMEOUT` (how long to
wait for in-flight to drain; user intent, e.g. 300s) and the controller injects
`GRACE_SECONDS` (the Pod's `terminationGracePeriodSeconds`, e.g. 600) at Pod
creation. The effective wait is `min(DRAIN_TIMEOUT, 70% × GRACE_SECONDS)`: the
drain never takes more than 70% of the grace budget, so at least 30% always
remains for the engine's SIGTERM shutdown — a hook that waits until the grace
deadline would starve the SIGTERM window entirely and end in a bare SIGKILL.
With the 70% split the two settings cannot conflict: a modest `DRAIN_TIMEOUT`
is honored exactly, an oversized one is clipped at the 70% line. Example:
`GRACE_SECONDS=600` caps the drain at 420s and reserves 180s for shutdown.

**Deployment practice**: keeping `maxUnavailable: 0` + `maxSurge: 1` remains
the recommended configuration so serving capacity never dips during an update,
but it is not an enablement condition — the drain activates on deletion
regardless, and with `maxUnavailable > 0` the drain still protects whatever
in-flight the deleted Pod holds (the temporary capacity dip is the existing
semantics of that setting, not something the drain adds).

### Test Plan

- Unit tests: preStop hook parsing and exit conditions (N-consecutive-zero,
  in-hook timeout, DNS resolution edge cases — decode gone treated as drained);
  router drain-on-deletionTimestamp (Pod kept, excluded from candidates).
- E2E: rolling update of a single-pod ModelServing under load — in-flight
  requests complete, no client-visible interruption, converge to
  `spec.replicas`; PD disaggregated update under `ServingGroupRollingUpdate` —
  the prefill Pod's hook does not exit while a paired decode still has
  in-flight.

### Alternatives

Earlier iterations of this proposal (and the community discussion that shaped
it) considered: a **controller-side gate with an annotation handshake** —
rejected because it grows the controller's delete path and relies on the
router's local `onFlight` counter, which splits per-replica in multi-router
deployments without Redis; **finalizer-based gating** — rejected because a
finalizer holds back the deletion of the API object, not the container teardown
(the kubelet kills the container as soon as `deletionTimestamp` is set);
**preStop waiting for a router-written confirmation** — rejected because it
requires API read access on every inference Pod; and a **fixed sleep in
preStop** — rejected as non-adaptive. The engine-metric zero-check and the PD
paired-decode rule carried over across all of these.
