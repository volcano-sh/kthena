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

This proposal adds a drain step to `ModelServing` rolling updates so that an old
Pod is deleted only after its in-flight inference requests finish generating
(or a timeout fallback fires), instead of being SIGKILLed immediately by
`DeleteCollection`. The goal is **best-effort** zero-downtime, zero-interruption
upgrades: on the normal path the service keeps serving throughout the update and
in-flight requests are never killed mid-generation; residual cases the drain
signal cannot fully close fall back to a timeout. This is "as lossless as
possible," not absolute lossless — see the PD Disaggregation section.

The drain is a two-phase annotation exchange between `model-serving-controller`
and `kthena-router` over two Pod annotations, with no Redis and no new CRD field.
Drain granularity follows the existing rollout strategy (ServingGroup or Role
instance), so the mechanism is group-aware — including for PD
disaggregated workloads, which are addressed in a dedicated section below.

### Motivation

Today, updating a `ModelServing` (new image, engine config change) drives
`model-serving-controller` to delete old-revision ServingGroups/Roles via
`DeleteCollection` with empty `DeleteOptions` (in `deleteServingGroup` /
`DeleteRole`). Old Pods are removed immediately, with no wait for in-flight
requests. LLM inference requests — especially streaming — often run for tens of
seconds to minutes, so an immediate delete SIGKILLs in-flight work and clients
see 502 / connection resets.

Lossless upgrade has two halves that are already partially in place:

- **No downtime** (keep enough Pods serving throughout): already guaranteed by
  the existing rolling strategy (`maxUnavailable: 0` + `maxSurge: 1`). This
  proposal does **not** change the rolling strategy.
- **No interruption** (in-flight requests finish before the old Pod is removed):
  the missing half. A drain step before the delete solves it: stop routing new
  traffic to the old Pod, wait for in-flight requests to finish, then delete.

#### Goals

- Add a drain step before deleting an old Pod during a `ModelServing` rolling
  update, so in-flight requests on the old Pod finish before it is removed.
- Keep serving throughout the update (already guaranteed by `maxUnavailable: 0`
  + `maxSurge: 1`; this proposal does not change the rolling strategy).
- Cover both rollout strategies (`ServingGroupRollingUpdate` and
  `RoleRollingUpdate`), plus scale-down and failure-rebuild, since they all
  converge on the same deletion path.
- Be group-aware for PD disaggregated workloads: drain follows the rollout
  strategy (whole ServingGroup under `ServingGroupRollingUpdate`), so prefill and
  decode are not deleted as independent single Pods; and provide a precise drain
  rule on the normal path using only the metrics the router already scrapes
  (drain a prefill from its paired decode's in-flight, not its own), with
  `drainTimeout` as the fallback for residual cases (see the PD section).
- Work without Redis or any new CRD field.
- Work for engines the router already adapts (vLLM including ascend, SGLang).

#### Non-Goals

- Drain for engines the router does not support (MindIE): the router does not
  route MindIE traffic, so there is nothing to drain; the timeout fallback still
  ensures the update completes.
- Changing the rolling strategy itself (`maxUnavailable`/`maxSurge`/`partition`).
- Drain as a separate API toggle: the drain timeout flag doubles as the
  enable/disable switch (`0` disables).
- Absolute / strong lossless upgrade. This proposal makes the **normal path**
  lossless and falls back to `drainTimeout` for residual cases, guaranteeing the
  update always completes. Closing every engine-internal window would require
  disproportionate complexity and code; that is left to follow-ups. See the PD
  Disaggregation section.

### Proposal

#### User Stories

##### Story 1 — image update under live traffic

A user updates the model image of a `ModelServing`. During the update, clients
keep calling the model. Without drain, a Pod serving a long streaming request is
killed mid-response; the client sees a broken connection. With drain, the old
Pod stops receiving new requests, finishes its in-flight streaming response, and
is then removed — the client sees no interruption.

##### Story 2 — scale-down

A user scales down a `ModelServing` replica. The same drain applies: the Pod
about to be removed finishes its in-flight requests first, so no request is
dropped during scale-down.

##### Story 3 — PD disaggregated update

A user updates a PD-disaggregated `ModelServing` (a ServingGroup holds paired
prefill + decode roles, where decode consumes the KV cache produced by prefill).
During the update the old ServingGroup is drained. Because the drain gate is
group-aware and a prefill Pod is judged from its paired decode's in-flight (not
its own metrics), the prefill is deleted only once no paired decode is still
generating — i.e. its KV has already been delivered (push) or pulled. Decode
that already holds the KV continues generating independently. On the normal
path no in-flight PD request is broken by the update.

#### Notes/Constraints/Caveats

True zero-downtime requires `maxUnavailable: 0` (+ `maxSurge: 1`): a new Pod must
be ready before the old one is removed. With `maxUnavailable: 0` the delete
budget is zero until the surge Pod is ready, so drain does not start until then —
this is correct. `maxSurge: 1` needs headroom for one extra Pod (GPU/NPU) during
the update; clusters must reserve it. Drain itself does not change this; it only
makes the "delete old" step graceful once the budget allows deleting.

`maxUnavailable` and `maxSurge` count the **rollout strategy's deletion unit**,
not individual Pods: ServingGroups under `ServingGroupRollingUpdate`, role
instances under `RoleRollingUpdate`. They look like Pod-level budgets only in the
PD + `workerReplicas: 0` case where one role instance happens to be one Pod; in a
tensor-parallel role (`workerReplicas: 6`) one role instance is 7 Pods, so the
budgets are clearly not Pod-granular.

#### Risks and Mitigations

- **Drain gets stuck**: if the router cannot confirm in-flight is zero (engine
  unreachable, metrics scrape failing), drain could hang. Mitigation: a
  `drainTimeout` fallback forces deletion after the timeout, so the update
  always completes; residual in-flight is then bounded by the Pod's
  `terminationGracePeriodSeconds` and preStop hook.
- **Router restart during drain**: the router holds drain state in memory. A
  restart could lose the in-progress drain and let a draining Pod receive new
  traffic. Mitigation: the drain signal is also a Pod annotation, so after a
  restart the router re-applies the draining flag from the annotation.
- **Drain window length under PD**: draining a whole ServingGroup waits for the
  slowest Pod (typically decode) to reach zero in-flight, so a fast prefill
  waits. This is safe (no in-flight is broken) but lengthens the drain window.
  Mitigation: PD users who want prefill and decode to drain independently can
  choose `RoleRollingUpdate` (see [PD Disaggregation](#pd-disaggregation)).

### Design Details

The controller and router cooperate over Pod annotations, without Redis and
without a new CRD field. Two annotations carry the drain handshake:

- `modelserving.volcano.sh/traffic-draining` (written by the controller; value
  is an RFC3339 timestamp used to bound the drain wait)
- `modelserving.volcano.sh/traffic-drained` (written by the router; value `true`,
  signals the Pod is safe to delete)

Deletion has two triggers (either fires the delete):

- **drained channel (precise)**: the router judges in-flight zero and writes
  `traffic-drained=true`; the controller deletes immediately on reading it.
- **drainTimeout fallback (last-resort)**: if `--drain-timeout` elapses since
  `traffic-draining` was set, the controller force-deletes regardless of drain
  state; residual in-flight is bounded by `terminationGracePeriodSeconds` +
  preStop. This covers engines the router does not adapt (MindIE) and stuck
  requests, guaranteeing the update eventually completes.

#### Controller

A drain gate is added to the two deletion convergence points,
`deleteServingGroup` and `DeleteRole` (in
`pkg/model-serving-controller/controller/model_serving_controller.go`). Before
deleting, the gate annotates the Pods with `traffic-draining`, then waits. Drain
completes when the router writes `traffic-drained` onto the Pods, or when
`drainTimeout` elapses as a fallback. Sitting on the two convergence points means
rolling update, scale-down, and failure-rebuild all go through the same drain
path — one implementation covers all scenarios.

`patchTrafficDraining` retries transient errors with the client-go retry backoff
(`retry.OnError`); terminal errors (NotFound/Forbidden/Invalid) are not retried.
Pods that still cannot be annotated are force-deleted, so the K8s
`DeletionTimestamp` still removes them from scheduling.

#### Router

The router observes the `traffic-draining` annotation via the Pod informer
(`syncPodHandler`) and sets an in-memory `draining` flag on the `PodInfo`.
Draining Pods stay in the store (in-flight tracking preserved) but are excluded
from all scheduling-candidate queries (`GetPodsByModelServer`, `GetDecodePods`,
`GetPrefillPods`, `GetPrefillPodsForDecodeGroup`, `GetPodsByInferencePool`).

`store.Run` watches engine metrics per Pod and, once in-flight reaches zero,
patches `traffic-drained=true` back onto the Pod (`markPodDrainedIfZero`). The
patch is retried with client-go backoff and is idempotent. The router does **not**
delete the `PodInfo` of a draining Pod — keeping it preserves in-flight counting
so the request can finish and the counter can reach zero correctly.

#### Drain Signal: Engine Metrics, Not onFlight

Drain completion is judged from engine-reported metrics, not the router's
`onFlight` counter. This is the central design choice. For homogeneous and HTTP
workloads the simple rule (`Running==0 && Waiting==0`) holds; for PD a prefill Pod
is drained from its paired decode's in-flight (not its own unreliable metrics),
with no new engine metrics introduced — see the PD Disaggregation section.

| Dimension | `onFlight` (router counter) | Engine metrics (chosen) |
|-----------|-----------------------------|--------------------------|
| Counts all traffic sources | ❌ only traffic through the router, not direct-to-engine | ✅ engine's real in-flight, including direct |
| Redis dependency | single replica in-memory OK; multi-replica without Redis each sees only its own count → false zero | ✅ HTTP scrape of engine, no Redis |
| Monotonic after drain | new traffic could still land (informer latency) | ✅ once draining, no new traffic reaches the Pod; metric only decreases |

The primary engine metrics are `RequestRunningNum` + `RequestWaitingNum`,
engine-agnostic through the `backend/vllm` and `backend/sglang` adapter layers. No
new engine metrics are introduced: a single-Pod workload drains on its own
`Running`/`Waiting`; a PD prefill drains from its paired decode's
`Running`/`Waiting` (see the PD Disaggregation section). The `onFlight` counter is
used only for least-request scheduling scoring, not for drain.

For homogeneous and HTTP workloads, engine metrics are sufficient: a Pod's
`RequestRunningNum==0 && RequestWaitingNum==0` means it has no in-flight work and
is safe to delete. For PD (SGLang push, NIXL/Mooncake pull) a prefill Pod's own
`Running`/`Waiting` is unreliable — for SGLang `num_running_reqs` is always 0 on a
prefill Pod, for NIXL prefill reports zero before decode finishes pulling — so a
prefill is drained from its paired decode's `Running`/`Waiting` instead. No new
engine metrics are needed; the detail and the residual `drainTimeout` fallback
are in the PD Disaggregation section.

**This proposal targets best-effort (as-lossless-as-possible) upgrade, not
strong/absolute lossless.** It makes the **normal path** lossless and falls back
to `drainTimeout` for residual cases, guaranteeing the update always completes.
This framing applies to the whole proposal, not only PD.

#### Drain Granularity Follows Rollout Strategy

The drain gate sits at `deleteServingGroup` / `DeleteRole`, and the set of Pods
it drains is selected by a label index that matches the strategy's deletion unit:

| Strategy | Drain unit | Index | What gets `traffic-draining` |
|----------|-----------|-------|------------------------------|
| `ServingGroupRollingUpdate` (default) | whole ServingGroup | `GroupName` (`ns/group`) | all Pods of all roles in the group — prefill + decode together |
| `RoleRollingUpdate` | role instance | `RoleID` (`ns/group/role/roleID`) | the entry + worker Pods of one role instance (e.g. `prefill-0`) |

There is **no per-Pod drain path**. The drain unit equals the budget unit, so
budget and drain are never mismatched: `ServingGroupRollingUpdate` budgets and
drains ServingGroups; `RoleRollingUpdate` budgets and drains role instances. In
the PD + `workerReplicas: 0` case one role instance is one Pod, so
`RoleRollingUpdate` behaves like per-Pod drain — but the unit is still the role
instance, not the Pod.

`maxUnavailable` / `maxSurge` therefore count the strategy's deletion unit, not
Pods (as noted in Notes/Constraints/Caveats). `maxUnavailable: 0` under
`ServingGroupRollingUpdate` means "ServingGroup count never drops below
`spec.replicas`"; under `RoleRollingUpdate` it means "each role's instance count
never drops below `role.replicas`".

#### PD Disaggregation

This section addresses the PD-specific concern raised on the issue: draining a
ServingGroup with prefill/decode roles must be group-aware, because a decode
Pod's in-flight request depends on the prefill handoff, and the drain gate
should apply to the pair/group, not each Pod independently.

**Best-effort framing applies here too** (see the Drain Signal section): this PR
makes the PD **normal path** lossless — the large majority of PD upgrades — and
falls back to `drainTimeout` for the residual cases, guaranteeing the update
always completes. The residual cases are no worse than today's behavior.

**Drain granularity is already group-aware, not per-Pod.** Under the default
`ServingGroupRollingUpdate`, the whole ServingGroup drains at once: all prefill
and decode Pods get `traffic-draining` together and are deleted together once
every Pod in the group is drained. Prefill and decode are never drained or
deleted as independent single Pods under the default strategy. (Under
`RoleRollingUpdate`, role instances drain independently — see the trade-off
below.)

**The prefill→decode handoff makes a prefill Pod's own metrics unreliable, on
every PD connector, for different reasons.** A PD request has a prefill phase
(compute KV cache) and a decode phase (generate tokens), connected by a KV
handoff. kthena has three connector implementations; on all of them a prefill
Pod's own `Running`/`Waiting` does **not** mean "safe to delete":

- **HTTP** — transparent passthrough, no KV handoff. A PD request has no
  separate prefill role, so this case does not arise; the simple rule
  (`Running==0 && Waiting==0`) holds on the single Pod.

- **SGLang (push)** — the prefill request lives in a separate inflight queue,
  not the running batch (SGLang `disaggregation/prefill.py`), so
  `sglang:num_running_reqs` is **always 0** on a prefill Pod and does not reflect
  whether the KV push is in flight.

- **NIXL / Mooncake (pull)** — prefill computes the KV and posts blocks for
  pulling, returning `kv_transfer_params` (coordinates the decode engine uses to
  pull the KV), then the decode request is sent and the decode engine pulls
  (`connectors/nixl.go`; `connectors/mooncake.go` reuses the NIXL code;
  `connectors/types.go` `RemoteBlockIDs/RemoteHost/RemotePort`). The prefill
  request leaves the running set once it has produced the KV and posted the
  blocks — **before** decode has finished pulling. So `Running==0` on a prefill
  Pod fires too early: the KV may still be needed by a decode pull in progress.

**The shared fix: drain a prefill Pod from its paired decode's in-flight, not
from the prefill's own metrics.** Decode runs the normal engine loop on every
connector, so `RequestRunningNum`/`RequestWaitingNum` on a decode Pod is
reliable. A prefill Pod is safe to delete only when:

> `prefill.Running==0 && prefill.Waiting==0` **and** every paired decode Pod has
> `decode.Running==0 && decode.Waiting==0`

Rationale: decode is the last stage of the request chain. A decode at zero
means no request is generating, and a decode can only start generating after it
received the KV (push delivered, or pull completed). So once every paired decode
is at zero, no decode will generate again and none needs the prefill's KV — the
prefill KV is no longer needed. Under `traffic-draining` no new request enters
the group, so this is monotonic: once all paired decodes reach zero they stay
zero, and the prefill is safe to delete. Pairing is group-level
(`getPrefillPodsForDecodeGroup` returns all prefill Pods of the group), so the
rule is group-aware, not per-Pod independent — matching the review's ask. For
SGLang this works without the inflight-queue gauges: a paired decode at zero
has already received the KV (it could not have generated otherwise), so the
prefill push finished earlier; for NIXL it works because no decode is left to
pull.

This covers the **normal path** (rolling update, graceful scale-down):
in-flight completes monotonically and prefill + decode both reach zero. Any
residual the precise rule cannot close falls back to `drainTimeout`, which
guarantees the update completes (see the best-effort framing in the Drain
Signal section).

**Scope this round: precise drain on the normal path from existing metrics only,
with `drainTimeout` as the universal fallback.** No new engine metrics are
introduced. Every topology gets a precise rule from the metrics the router
already scrapes:

| Topology | Prefill drain signal | Decode drain signal |
|----------|-----------------------|----------------------|
| Single-pod / HTTP | `Running==0 && Waiting==0` | (no decode role) |
| SGLang push (PD) | `prefill==0` **and** paired `decode==0` | `Running==0 && Waiting==0` |
| NIXL/Mooncake pull (PD) | `prefill==0` **and** paired `decode==0` | `Running==0 && Waiting==0` |

The rule changes the *judge* (paired decode / group instead of the prefill Pod
alone), not the *metric*, so it needs **no new scrape surface**.

Follow-ups (not blocking this proposal):
- cross-request KV reuse / prefix caching (decode momentarily zero while
  prefill KV is still needed).

**Trade-off note: drain window under group drain.** Under
`ServingGroupRollingUpdate` the whole group drains together, so a fast Pod
waits for the slowest in the group. Under `RoleRollingUpdate`, role instances
drain independently. For PD users, `ServingGroupRollingUpdate` is the safer
pairing (prefill and decode are not deleted independently), at the cost of a
longer drain window; `RoleRollingUpdate` gives a shorter window but deletes
prefill and decode independently, so the paired-zero rule must be applied
across the pair, not within one role. Both are covered by the `drainTimeout`
fallback.

#### Configuration

A `--drain-timeout` flag on `kthena-controller-manager` (default `5m`; `0`
disables drain and reverts to immediate deletion). It is intentionally a flag,
not a CRD field: drain is the correct behavior for rolling update, and the
timeout expresses both "how long to wait" and "whether to wait", avoiding a
separate boolean toggle. Helm exposes it as `controllerManager.drainTimeout`.

`drainTimeout: 0` = disable drain (immediate delete, current behavior, useful
for test environments); a positive value = enable lossless drain.

`drainTimeout` and `terminationGracePeriodSeconds` divide the work:

- `drainTimeout` — bound the controller's wait **before** delete (router has
  already stopped new traffic).
- `terminationGracePeriodSeconds` — bound the kubelet's wait **after** SIGTERM
  before SIGKILL (an existing Pod field; the single-node vLLM template defaults
  to 300s).

Both should cover the longest expected generation; e.g. `drainTimeout: 300s` +
`terminationGracePeriodSeconds: 300s` means an old Pod waits at most 300s for
in-flight to drain, then 300s for the engine to finish on SIGTERM.

#### RBAC

Drain writes Pod annotations (K8s `PATCH` requests). The original controller only
deleted/watched Pods and the router only watched Pods — neither had `pods/patch`.
Two ClusterRoles need the `patch` verb added:

- `kthena-controller-manager` ClusterRole: `patchTrafficDraining` writes the
  `traffic-draining` annotation (controller → pod signal).
- `kthena-router` ClusterRole: `markPodDrainedIfZero` writes the `traffic-drained`
  annotation (router → pod signal, reporting in-flight reached zero).

Both Helm chart ClusterRoles get `patch` added.

#### Engine Coverage

Drain works for engines the router already adapts: vLLM (including ascend, which
runs the vLLM engine with `vllm:` metrics) and SGLang. MindIE is not supported —
the router does not support MindIE (`BuildModelServer` rejects non-vLLM backends),
so no MindIE traffic flows through the router and there is nothing to drain. The
`drainTimeout` fallback still ensures an update involving MindIE eventually
completes.

#### Test Plan

- Unit tests for the drain gate (both `deleteServingGroup` and `DeleteRole`),
  `patchTrafficDraining` retry/force-delete, and the router's
  `markPodDrainedIfZero`.
- Unit tests for the ServingGroup-outdated check after a controller restart
  rebuilds `SG.Revision` from the first Pod seen.
- E2E test for a `ModelServing` rolling update under `RoleRollingUpdate` with
  `maxUnavailable: 0` + `maxSurge: 1`, asserting the old Pod is drained and
  deleted after in-flight finishes and ready replicas converge to
  `spec.replicas`.
- E2E test for a PD-disaggregated `ModelServing` rolling update under
  `ServingGroupRollingUpdate`, asserting that a prefill Pod is not deleted while
  a paired decode still has in-flight (KV not yet delivered/pulled), and that
  decode continues generating after its paired prefill is removed.
- Unit test for the router's group-aware drain-zero judgment: a prefill Pod is
  judged drained only when both it and every paired decode reach
  `Running==0 && Waiting==0` (no new engine metrics are added).

### Alternatives

#### `onFlight` counter as the drain signal

The router already maintains an in-flight counter. It was rejected as the drain
signal because it only counts traffic through the router (not direct-to-engine
traffic), and in a multi-replica router without Redis each replica only sees
its own count and could falsely report zero. Engine metrics avoid both
problems. The NIXL handoff gap is *not* a reason to prefer engine metrics over
`onFlight` — both signals have the same gap on the NIXL pull path (prefill
reports zero before decode has pulled the KV); that gap is a PD-specific problem
closed by the paired-decode drain rule on the normal path, not by the choice of
drain signal (see [PD Disaggregation](#pd-disaggregation)).

#### `preStop` hook only

A `preStop` hook can delay Pod termination, but it cannot stop the router from
sending new traffic to the Pod (the router watches Pods directly, not via K8s
Service/Endpoints, so a readiness probe does not stop router dispatch). Drain
needs the router to actively stop new traffic, which a `preStop` hook cannot do.
`preStop` remains as a secondary safety net for the `drainTimeout` fallback path.

#### A separate CRD field to enable drain

Rejected because a `drainTimeout` of `0` already disables drain, so a separate
boolean toggle is redundant.
