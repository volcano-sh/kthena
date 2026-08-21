---
title: Coordinated Proportional Role Rolling Update
authors:
- haoeeeee
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-08-17

---

## Coordinated Proportional Role Rolling Update

### Summary

Kthena supports `RoleRollingUpdate`, but the current controller calculates `partition` and `maxUnavailable` independently for every Role in every ServingGroup. If several cooperating Roles change in one ModelServing revision, a fast Role can advance much further than a slow Role. The controller also has no Role dependency graph, so an upstream Role may be replaced and become ready before the new revision of its downstream dependency is ready.

This proposal adds optional, per-ServingGroup coordination to `RoleRollingUpdate`. Users select the participating Roles, define a maximum percentage-point skew between their normalized ready progress, and optionally define a dependency DAG. The controller continues to use each Role's existing `partition` and `maxUnavailable` as local safety limits, then applies a second, cross-Role filter before deleting or creating a rollout candidate.

Dependencies are a startup gate, not a permanent progress order. For `A -> B -> C`, the internal B and C stages may prestart together while A remains on the old revision. A starts only after both B and C have target-revision `RoleRunning` capacity. After a Role has a target-revision Ready replica, `maxSkew` coordinates later progress without maintaining `progress_A <= progress_B <= progress_C`. Every reconcile derives progress and in-flight reservations from Role state, Pod labels, and readiness.

### Applicability Prerequisite

This proposal applies only when the user explicitly selects:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
```

Users still submit the target A/B/C configuration through `ModelServing.spec.template.roles`, because Roles are embedded in the ServingGroup template. The configuration location does not determine the execution unit; `rolloutStrategy.type` does:

- With `RoleRollingUpdate`, the controller enters each ServingGroup, compares per-Role `RoleTemplateHash` values, and replaces only changed Role replicas. This proposal coordinates those Role-level candidates.
- With `ServingGroupRollingUpdate`, or with the default strategy when `rolloutStrategy` is omitted, the controller deletes and recreates an outdated ServingGroup as a whole. It does not calculate independent A/B/C progress and this proposal does not apply.

Updating A, B, and C in one ModelServing update therefore gives the controller one shared target Spec; they become three coordinated rollout objects only under explicit `RoleRollingUpdate`. This proposal neither changes strategy selection nor converts a ServingGroup rollout into a Role rollout automatically.

### Motivation

Consider one ServingGroup containing three Roles:

```text
A depends on B
B depends on C
```

The application guarantees version-aware routing: new A calls new B, and new B calls new C. That guarantee alone does not make an uncoordinated rollout safe. In the first reconcile, the current Role rolling update can independently select one replica from A, B, and C. If new A and B become Ready before new C, traffic can reach new A and then fail because the required new C endpoint is not Ready.

Updating only C until completion is also undesirable. New C receives no new-version traffic before B advances, and a large excess of new C replicas may be idle. The required behavior is:

1. keep normalized new-version Ready progress approximately proportional across selected Roles;
2. keep an upstream entry Role on the old revision until its dependency closure has target-revision Ready capacity;
3. retain existing per-Role availability and partition semantics;
4. allow slow or failed dependencies to stop upstream progress safely.

The current implementation cannot provide these properties because `rolesToDeleteForRoleRollingUpdate` loops over Role specs and calculates each Role's `maxScaleDown` independently. `RoleRunning` is already a useful readiness signal: a Role replica becomes Running only after its entry Pod and all worker Pods are Running and Ready. This proposal builds coordination on that existing signal instead of introducing a separate readiness definition.

#### Goals

- Coordinate Role rolling updates independently inside each ServingGroup.
- Define progress as the percentage of update-eligible Role replicas that are both on the target Role template hash and `RoleRunning`.
- Limit the progress difference among selected active Roles with a user-supplied percentage such as `10%`.
- Allow users to define an acyclic Role dependency graph for rollout ordering.
- Keep an upstream entry Role from starting until every Role in its dependency closure has at least one target-version `RoleRunning` replica, while allowing internal dependency stages to prestart together.
- Preserve each Role's current `maxUnavailable` and `partition` behavior.
- Reserve in-flight work so asynchronous deletion, creation, scheduling, and readiness cannot oversubscribe the skew budget.
- Remain compatible with the current informer, workqueue, and repeated-reconcile architecture.
- Reconstruct coordination state from current objects after controller restarts.
- Define validation, failure behavior, and a complete test plan.

#### Non-Goals

- Implement business request routing or verify that new A actually calls only new B.
- Change initial ModelServing creation ordering. Dependencies in this proposal apply only to coordinated Role rolling updates.
- Coordinate rollout progress across different ServingGroups. Each ServingGroup has an independent coordinator.
- Replace the existing Role `maxUnavailable` or `partition` fields.
- Coordinate ordinary scaling, failure recovery, or Role addition/removal as proportional rolling-update work.
- Automatically roll back a revision when rollout progress stalls.
- Turn Pod readiness on or off, mutate readiness gates, or change Service endpoint selection.
- Guarantee runtime dependency health after an upstream replica has already completed rollout. Ordinary recovery policy handles later failures.

### Proposal

Coordination is opt-in and only valid with `rolloutStrategy.type: RoleRollingUpdate`.

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: example
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
    roleRollingUpdateConfiguration:
      coordination:
        # Optional. Omission selects all Roles in spec.template.roles.
        roles: [a, b, c]

        # Percentage points of normalized rollout progress. Percent only.
        maxSkew: 10%

        # Rollout-only startup dependency graph. "a dependsOn b" means b
        # must have one target-revision RoleRunning replica before a starts.
        dependencies:
        - role: a
          dependsOn: [b]
        - role: b
          dependsOn: [c]

  template:
    roles:
    - name: a
      replicas: 4
      maxUnavailable: 1
      partition: 0
      # templates omitted
    - name: b
      replicas: 4
      maxUnavailable: 1
      partition: 0
    - name: c
      replicas: 4
      maxUnavailable: 1
      partition: 0
```

The `roles` list scopes one coordination domain:

- omitted or empty: all Roles in `spec.template.roles`;
- specified: exactly those Roles participate in progress skew and dependency ordering;
- an unchanged selected Role is treated as complete for the current revision and does not block changed Roles;
- a non-selected Role retains the existing independent Role rollout behavior;
- a dependency endpoint must be in the selected set.

#### User Stories

##### Proportional PD rollout

A ServingGroup has 10 prefill and 20 decode Role replicas. Both templates change. With `maxSkew: 10%`, the controller prevents one Role from accumulating substantially more target-hash Ready capacity than the other while both still have rollout work.

##### Dependency-safe three-stage rollout

A, B, and C each have four replicas, `maxUnavailable: 1`, `maxSkew: 10%`, and dependencies A -> B -> C. Each Role's first integer step is 25%. Startup proceeds as follows:

```text
B and C each start one replacement; A remains old
    -> B and C reach target hash and RoleRunning
    -> A starts one replacement
```

After these first Ready replicas exist, all three Roles are unlocked. Later selection is based on `maxSkew` and deterministic candidate ordering; it does not have to repeat C/B/A order. New A cannot start before compatible new B and C capacity exists.

##### Different Role replica counts

A has 2 replicas, B has 4, and C has 10. With `maxSkew: 10%` and a zero Ready baseline, their per-Role integer start limits are `ceil(0.1*2)=1`, `ceil(0.1*4)=1`, and `ceil(0.1*10)=1`. A's first replica is an unavoidable local 50% step, but that does not inflate B or C to a global 50% bound.

##### Different partitions

A has four replicas with `partition: 2`; B and C have four replicas with no partition. A's rollout target contains only A-2 and A-3. Its denominator is therefore two, not four. Once those two replicas are updated and Ready, A's coordinated progress is 100%, while A-0 and A-1 intentionally remain old. B and C may still reach 100% of their own four-replica targets; A's partition does not implicitly partition B or C.

### API Design

```go
type RolloutStrategy struct {
    Type RolloutStrategyType `json:"type"`

    // Used only by ServingGroupRollingUpdate.
    RollingUpdateConfiguration *RollingUpdateConfiguration `json:"rollingUpdateConfiguration,omitempty"`

    // Used only by RoleRollingUpdate.
    RoleRollingUpdateConfiguration *RoleRollingUpdateConfiguration `json:"roleRollingUpdateConfiguration,omitempty"`
}

type RoleRollingUpdateConfiguration struct {
    Coordination *RoleRollingUpdateCoordination `json:"coordination,omitempty"`
}

type RoleRollingUpdateCoordination struct {
    // Empty means all Roles.
    Roles []string `json:"roles,omitempty"`

    // Required. Percentage strings only, for example "10%".
    MaxSkew *intstr.IntOrString `json:"maxSkew"`

    Dependencies []RoleRolloutDependency `json:"dependencies,omitempty"`
}

type RoleRolloutDependency struct {
    Role      string   `json:"role"`
    DependsOn []string `json:"dependsOn"`
}

```

Dependency ordering has one fixed meaning: an upstream entry Role waits for target-revision `RoleRunning` capacity across its dependency closure. Internal stages may prestart together while all of their upstream dependents remain unstarted. Once a Role has a target-revision Ready replica, dependency edges do not impose a continuing progress order. The API intentionally does not expose a scheduled-only readiness mode.

#### Validation and defaulting

The webhook must enforce:

- When `roleRollingUpdateConfiguration.coordination` is present, `rolloutStrategy.type` must explicitly equal `RoleRollingUpdate`; the default `ServingGroupRollingUpdate` selected when the strategy is omitted is not valid.
- `coordination.maxSkew` is required and must be a percentage string in `(0%, 100%]`; integer values are rejected.
- Role names in `roles`, `role`, and `dependsOn` exist in `spec.template.roles`.
- Names are unique; self-dependencies and duplicate dependency edges are rejected.
- The dependency graph is acyclic.
- Every dependency endpoint belongs to the effective selected Role set.
- At least two Roles are selected; a single Role provides no coordination benefit.
- Existing Role-level `partition` and `maxUnavailable` syntax validation remains unchanged.

Coordination does not introduce a `maxUnavailable` default. If it is omitted, the existing Role rolling logic supplies all outdated replicas as local candidates, and the coordinator may still reduce that set through skew and dependency constraints. Operators should configure `maxUnavailable` when they require an explicit per-Role availability bound.

### Progress Model

All calculations are performed independently for each ServingGroup and target ModelServing revision.

For participating Role `r`:

```text
D_r = desired Role replica count
P_r = resolved partition count
T_r = max(D_r - P_r, 0)              update-eligible target replicas
R_r = eligible replicas with target RoleTemplateHash and RoleRunning
I_r = eligible rollout work already reserved but not Ready

readyProgress_r    = R_r / T_r
reservedProgress_r = min(R_r + I_r, T_r) / T_r
```

`I_r` includes:

- Role replicas in `RoleDeleting` because of this rollout;
- target-hash Role replicas that are not `RoleRunning`;
- stable replacement slots missing between deletion completion and successful recreation;

Counting in-flight work is essential. If the controller selected A, B, and C using only Ready progress, A and B could become Ready faster than C and exceed a decision made when all progress values were zero. Reservation prevents repeated reconciles from admitting more work than the coordination budget already committed.

Only ordinals greater than or equal to the resolved Role partition contribute to `T_r`, `R_r`, or `I_r`. Protected replicas remain available but are outside the rollout target.

If a selected Role's template did not change, it is excluded from the active skew minimum, while its current target-hash Ready replicas may still satisfy a dependency. If `T_r` is zero for a changed dependency Role, it cannot provide target-revision Ready capacity and its dependents remain blocked.

#### Percentage semantics

`maxSkew: 10%` means a target difference of ten percentage points before conversion to integer replica limits:

```text
progress_r <= minimum Ready progress + 0.10
```

It does not mean:

- ten replica instances;
- ten percent of the larger Role's raw replica count;
- a relative error such as `progress_a / progress_b`.

The only permitted overshoot is the local integer quantization described below. The implementation should use integer cross-multiplication or fixed-point basis points rather than floating-point comparison.

#### Per-Role integer limits

A Role with `T_r` eligible replicas advances in increments of `1/T_r`. The controller must not increase one global effective skew to the coarsest Role's step, because that would grant unrelated large Roles extra work.

For every reconcile, define:

```text
readyBaseline = min(readyProgress_r) across active participating Roles
ratioLimit    = min(1, readyBaseline + configuredSkew)
allowedStarted_r = min(T_r, ceil(ratioLimit * T_r))
```

A candidate for Role `r` is skew-eligible only when:

```text
R_r + I_r + 1 <= allowedStarted_r
```

For A/B/C with 8, 4, and 10 eligible replicas, a `10%` limit at a zero Ready baseline permits 1, 1, and 1 started replica respectively: 12.5%, 25%, and 10%. B's unavoidable 25% integer step does not raise A or C to a 25% global limit. Implementations use integer cross-multiplication rather than floating point.

### Dependency Semantics

An edge:

```yaml
- role: a
  dependsOn: [b]
```

means that B is a rollout prerequisite of A.

Before A's first replacement is admitted, every dependency B must have at least one eligible target-revision `RoleRunning` replica. A dependency that is merely created or scheduled does not unlock A.

After A is unlocked, the edge no longer compares A and B progress. A may advance ahead of B when A's own candidate remains inside the shared `maxSkew` limit. No ordinal pairing such as A-3 to B-3 is required.

The dependency gate is checked when rollout work is admitted, before deleting the old upstream Role replica. It does not change Kubernetes Pod readiness. Consequently, the required downstream new-version capacity already exists before the upstream replacement can be created and become reachable.

Among currently eligible candidates, the Role with the lowest started progress is preferred, followed by Role order in the ModelServing spec and descending Role ordinal. Dependency order affects eligibility only while a Role is locked; it does not remain a priority after startup unlock.

### Reconcile Algorithm

The existing `syncModelServing` order remains conceptually unchanged:

```text
sync ServingGroup count
sync Role count and missing Pods
manage rolling update
sync Services
update status
```

Coordination changes only the Role rolling-update decision inside `manageRollingUpdate`.

#### Phase 1: build a snapshot

For each non-deleting ServingGroup:

1. Resolve selected Roles and determine which Role templates changed in the current revision.
2. Calculate the target Role template hash for every Role.
3. Resolve per-Role partition and eligible ordinals.
4. Classify eligible replicas as old Ready, old unavailable, deleting, target Creating, or target Running.
5. Calculate `T`, `R`, `I`, Ready progress, started progress, the shared Ready baseline, and each Role's `allowedStarted` integer limit.
6. Generate local outdated candidates using the existing `maxUnavailable` logic.

Ordinary scaling remains owned by the existing `syncRoleReplicas` path and is not treated as coordinated rollout progress. The coordinator only filters outdated-role candidates produced by the existing rolling-update logic. Missing replacement slots for an active changed Role remain reserved as in-flight work.

#### Phase 2: filter and select candidates

The coordinator repeatedly evaluates local candidates using a simulated reservation snapshot:

```text
selected = []

while candidates remain:
    eligible = candidates that satisfy all of:
      1. Role partition allows the ordinal
      2. Role maxUnavailable/local budget allows the candidate
      3. projected R + I does not exceed this Role's allowedStarted limit
      4. dependency Ready gate is satisfied

    if eligible is empty:
        break

    choose the candidate with:
      1. lowest projected/reserved Role progress
      2. Role order in spec
      3. highest replica ordinal

    append candidate to selected
    increment the simulated in-flight reservation for its Role
```

The simulated increment prevents one reconcile from selecting an unlimited number of replicas before informer events report `RoleDeleting`.

After selection, the existing `DeleteRole` path performs the actual delete. Deletion-completion events remove the logical Role from the datastore and enqueue the ModelServing. The next reconcile recreates the missing Role with the target hash. A target-hash non-Running Role consumes both the local `maxUnavailable` budget and the cross-Role reservation until it becomes `RoleRunning`.

#### Phase 3: event-driven continuation

The Pod handler already changes a Role to `RoleRunning` only after its entry Pod and all worker Pods are Running and Ready. The current `syncModelServing` function itself has no whole-ServingGroup Ready gate: whenever a ModelServing is enqueued, it runs the normal reconcile. The subtle issue is the current Ready-event path. `handleReadyPod` updates an individual Role to `RoleRunning`, but that path normally calls `enqueueModelServing` only after `checkServingGroupReady` reports that the whole ServingGroup is Ready. Delete completion, spec changes, retries, or other events may still enqueue earlier, so this is not a hard semantic barrier; it is a missing immediate enqueue for an intermediate Role Ready transition.

Coordinated rollout therefore additionally enqueues the ModelServing whenever a participating Role replica transitions to `RoleRunning`, while retaining the existing ServingGroup status update. This lets the first Ready dependency unlock its dependent and lets later Ready progress release new skew capacity promptly.

The controller still does not wait inside reconcile:

```text
reconcile -> select/delete -> return
Pod/Service delete events -> enqueue
reconcile -> recreate -> return
Pod Ready events -> RoleRunning -> enqueue
reconcile -> select the next allowed work
```

After restart, Pod revision/hash labels, Role status reconstruction, desired counts, and missing slots reproduce the same constraints.

### Interaction with Existing Features

#### maxUnavailable

`maxUnavailable` remains a local availability constraint for one Role. Coordination is an additional global filter:

```text
final candidates = local maxUnavailable candidates
                 intersect partition-eligible candidates
                 intersect skew-allowed candidates
                 intersect dependency-allowed candidates
```

Coordination may reduce the number selected by the existing logic but never increases it.

#### partition

Partition is resolved separately for every Role and affects only that Role's denominator. A partition on A never stops B or C from completing their own rollout. Protected old replicas remain old after coordinated rollout completes.

#### recovery policy

Intentional deletion continues to be identified by `RoleDeleting`, so it must not be treated as a failure-recovery event. A later failure after rollout admission follows the configured RecoveryPolicy. The coordinator does not retract already Ready upstream replicas.

#### new template changes during rollout

The latest ModelServing spec remains authoritative. A new target hash starts a new reconciliation target. Previously created replicas that no longer match become outdated again.

### Failure and Blocking Behavior

#### Slow or failed dependency

If new C remains Creating:

- C consumes its local and reserved budget;
- B may consume its initial skew budget while all upstream Roles remain unstarted;
- A cannot start until both B and C have target-revision `RoleRunning` replicas;
- B and C cannot consume more than their per-Role integer limits derived from `maxSkew`;
- old A replicas remain serving.

The rollout is intentionally stalled rather than exposing an incompatible upstream chain.

#### Unschedulable replicas

Unschedulable target replicas remain in-flight and do not release budget. Existing Kubernetes Pod events explain scheduling failure, and the rollout remains waiting for Role readiness.

#### Dependency cycle or missing Role

These are admission errors and never reach the controller.

#### No eligible dependency replica

If a changed dependency is fully protected by partition, it cannot provide target-revision Ready capacity and dependent rollout remains blocked. The controller does not bypass the dependency.

#### Inconsistent or legacy hash data

The existing ControllerRevision fallback is used to infer missing Role template hashes. If a hash cannot be resolved, the controller conservatively does not count the replica as target Ready and logs the unresolved comparison.

### Status and Observability

This change does not add a new public status or metric API. Operators can inspect existing Pod Ready conditions, Role template-hash labels, Role lifecycle events, and controller logs. `status.updatedReplicas` remains ServingGroup-level and is not Role rollout progress.

### Controller Integration

The implementation introduces a compact per-Role snapshot and a pure selection function so algorithm tests do not require Kubernetes clients:

```go
type coordinatedRoleState struct {
    roleName  string
    specIndex int
    target    int
    ready     int
    inFlight int
    active    bool
    candidates []roleToDelete
}

func selectCoordinatedRoleCandidates(
    states []coordinatedRoleState,
    coordination *RoleRollingUpdateCoordination,
) ([]roleToDelete, error)
```

Expected code touch points:

- API types and generated clients/deepcopy/CRDs.
- ModelServing webhook validation/defaulting.
- `rolesToDeleteForRoleRollingUpdate`: build all Role candidates first instead of immediately concatenating independent selections.
- `buildCoordinatedRoleState`: classify target Ready and in-flight replicas.
- `handleReadyPod`: enqueue on participating Role transition to Running.

The existing `DeleteRole`, role recreation, ControllerRevision, Pod hash labels, and workqueue retry paths remain in use.

### Risks and Mitigations

#### Rollout throughput reduction

Ready-based startup gating delays an upstream entry Role until its dependency closure is Ready and can be slower than independent rollout. Internal dependency stages may prestart together. After all Roles are unlocked, `maxSkew` and each Role's `maxUnavailable` determine throughput.

#### Deadlock from discrete replica counts

Strict percentage inequalities can be impossible for small Roles. Per-Role ceiling conversion provides one-replica liveness without inflating every Role's budget. Coordinator tests cover mixed replica counts.

#### Stale informer observations

Candidate admission uses conservative in-flight reservations and simulated increments. Operations are idempotent, and RoleDeleting/target-Creating/missing replacement slots remain reserved across reconciles.

#### Misconfigured dependency direction

API documentation states that `A dependsOn B` makes B a startup prerequisite of A. Webhook DAG validation and examples reduce ambiguity.

#### Feature interaction complexity

The coordinator only filters candidates that already passed existing Role update rules. It does not own deletion, creation, readiness, or recovery.

### Test Plan

#### Unit tests

- Percent parsing and rejection of absolute `maxSkew`.
- Role selection defaulting and explicit subset behavior.
- Dependency existence, duplicate, self-edge, and cycle validation.
- Progress calculation with target hash, `RoleRunning`, Creating, Deleting, and missing replacement slots.
- Per-Role partition denominator and different partitions across Roles.
- Unchanged selected Roles and fully partitioned changed dependencies.
- Per-Role integer limits for equal and unequal replica counts.
- Quantization of a small Role does not inflate other Roles' limits.
- Ready-based startup dependency gating at equal and unequal replica counts.
- Coordination preserves the existing behavior when `maxUnavailable` is omitted.
- Candidate simulation prevents over-selection in one reconcile.
- Deterministic selection order and descending ordinal behavior.
- Existing `maxUnavailable` always remains an upper bound.
- Controller restart snapshot produces the same decision.

#### Property and fuzz tests

For random DAGs, replica counts, partitions, Ready states, and local budgets:

- selected candidates never violate local availability or partition;
- projected reservations never exceed that Role's quantized integer limit;
- dependency gating never admits a dependent before every dependency has a target-revision Ready replica;
- selection is deterministic for the same snapshot;
- repeated Ready transitions eventually complete when all creations succeed;
- invalid graphs are always rejected.

#### Integration tests

- Spec update event enqueues a ModelServing and selects only dependency-allowed work.
- Delete events preserve reservation through deletion/recreation gaps.
- each participating RoleRunning transition enqueues reconciliation.
- rate-limited retries do not duplicate deletions.
- controller restart during Deleting and Creating reconstructs reservations.
- a second template update during rollout switches target hashes safely.

#### End-to-end tests

- A/B/C equal replicas, delayed C readiness: B and C may prestart, while A waits until both dependencies have a target-revision Ready replica.
- Different replica counts with configured skew below one Role's single-replica step.
- Mixed partitions: A partially remains old while B/C complete.
- One Role unschedulable: upstream stops and old capacity remains.
- Explicit Role subset: selected Roles coordinate; unselected Role preserves legacy behavior.

### Rollout Plan

1. Add the opt-in alpha API and webhook validation.
2. Add per-ServingGroup state construction and pure candidate selection.
3. Integrate candidate filtering into `RoleRollingUpdate` and enqueue intermediate participating RoleRunning transitions.
4. Add unit, controller, and end-to-end coverage with injected readiness delays.

When `coordination` is absent, the existing independent Role rolling update behavior remains unchanged.

### Alternatives

#### Rely only on business-layer version routing

Rejected as the controller can still make a new upstream Role reachable before any compatible downstream replica is Ready. Routing cannot select a nonexistent endpoint.

#### Use only dependency order

The invariant `progress_A <= progress_B <= progress_C` prevents upstream from leading, but by itself allows C to reach 100% while A remains at 0%. `maxSkew` is still needed to bound idle downstream progress.

#### Use only maxSkew with simultaneous candidate admission

Rejected because all Roles can be at zero when A, B, and C are admitted simultaneously. Different readiness latency can then expose A before C. The dependency Ready gate must constrain admission.

#### Define skew as a raw replica-count difference

Rejected because Roles commonly have different desired counts. A one-replica difference means 50% for a two-replica Role but 1% for a hundred-replica Role.

#### Mutate Pod readiness until dependencies are Ready

Rejected for the initial design. It couples rollout control to kubelet readiness and Service endpoint semantics, can hide otherwise healthy Pods, and still needs a policy for already Ready Pods when a dependency later fails. Candidate admission prevents the unsafe state earlier.

### References

- [Kthena Role rolling update proposal](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/role-rollingupdate.md)
