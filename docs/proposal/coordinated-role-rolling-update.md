---
title: Coordinated Proportional Role Rolling Update
authors:
- "@haoeeeee"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-08-17

---

## Coordinated Proportional Role Rolling Update

### Summary

Kthena supports `RoleRollingUpdate` for replacing changed Role replicas inside a ServingGroup. The existing controller calculates each Role's rolling candidates independently. For a ServingGroup whose Roles cooperate in one request path, independent replacement can create a large difference between their target-version capacities.

This proposal adds optional coordination to `RoleRollingUpdate`. Users configure:

- the Roles participating in one coordination domain;
- `maxSkew`, which limits the percentage-point difference in normalized rollout progress;
- a Role dependency DAG, which describes the version call topology.

For every reconcile, the controller builds one snapshot for all participating Roles in a ServingGroup and calculates the boundary passed to the existing Role rolling-update path:

```text
effectivePartition = boundary required by userPartition, maxSkew, and dependency rules
```

`maxSkew` limits the replacement progress of replicas that existed before the update. The dependency rules delay a rollout root until its target-version dependency path is Ready and retain the old-version dependency path while old callers remain. The existing Role rolling-update path then uses `effectivePartition` and `maxUnavailable` to perform deletion and replacement.

For a dependency chain `A -> B -> C`, A is the rollout root. B and C first establish target-version `RoleRunning` capacity, while their replacement progress remains governed by `maxSkew`. A then starts. At rollout completion, an old A keeps at least one old B, and an old B keeps at least one old C.

### Motivation

Consider one ServingGroup with:

```text
A: 100 replicas
B:  50 replicas
C:  10 replicas

A depends on B
B depends on C
```

The target revision changes all three Roles. The application routes new-version requests through the new-version dependency chain.

For example, a new Predict Role may require the vector schema produced by a new Embedding Role. Starting the new Predict Role before the new Embedding Role is Ready can make requests fail. This is rollout-time service compatibility rather than model sharding.

The coordinated rollout addresses two requirements:

1. Ready progress produced by stable old-replica replacement remains proportional across Roles with different replica counts.
2. A target-version upstream Role starts after its target-version dependency has Ready capacity.

The coordinated rollout maintains proportional stable-replacement progress and establishes the new dependency chain before exposing its root.

#### Goals

- Keep the old-replica replacement progress of selected Roles within a configured proportional bound, including Roles with different replica counts and partitions.
- Establish the target-version dependency path before starting its rollout root, and preserve the old-version dependency path until old callers have exited.
- Keep coordination optional and compatible with existing `RoleRollingUpdate` behavior.
- Expose the current coordination constraint and waiting state.

### Proposal

#### Applicability

Coordination is enabled under `RoleRollingUpdate`:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
    roleCoordination:
      maxSkew: 10%
```

Users continue to submit Role templates through `ModelServing.spec.template.roles`. With `RoleRollingUpdate`, the controller compares Role template hashes and replaces changed Role replicas inside each existing ServingGroup. Coordination is calculated separately for every ServingGroup.

#### API Design

```go
type RolloutStrategy struct {
    Type RolloutStrategyType `json:"type"`

    RollingUpdateConfiguration *RollingUpdateConfiguration
        `json:"rollingUpdateConfiguration,omitempty"`

    RoleCoordination *RoleCoordination
        `json:"roleCoordination,omitempty"`
}

type RoleCoordination struct {
    // Empty selects every Role in spec.template.roles.
    Roles []string `json:"roles,omitempty"`

    // Maximum percentage-point difference in normalized rollout progress.
    MaxSkew *intstr.IntOrString `json:"maxSkew"`

    // Version call topology used for root startup and old-chain retention.
    Dependencies []RoleRolloutDependency `json:"dependencies,omitempty"`
}

type RoleRolloutDependency struct {
    Role      string   `json:"role"`
    DependsOn []string `json:"dependsOn"`
}
```

Example:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: example
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
    roleCoordination:
      roles: [a, b, c]
      maxSkew: 10%
      dependencies:
      - role: a
        dependsOn: [b]
      - role: b
        dependsOn: [c]

  template:
    roles:
    - name: a
      replicas: 100
      maxUnavailable: 1
      partition: 0
    - name: b
      replicas: 50
      maxUnavailable: 1
      maxSurge: 1
      partition: 0
    - name: c
      replicas: 10
      maxUnavailable: 1
      maxSurge: 1
      partition: 0
```

This proposal integrates with the Role-level `maxSurge` field proposed in #1626. Its default value is zero, and positive values provide temporary dependency bootstrap capacity. The dependency integration is implemented after that Role-level capability is available; without it, admission rejects a changed dependency that has no replacement or ordinary scale-up capacity for bootstrap.

The `roles` list defines one coordination domain:

- an empty list selects all Roles in `spec.template.roles`;
- a non-empty list selects the named Roles;
- selected Roles share one `maxSkew` calculation;
- each selected Role keeps its own `partition`, `maxUnavailable`, and `maxSurge`.

Roles outside the list retain the existing independent Role rolling behavior, allowing one request path to be coordinated within a ServingGroup that also contains unrelated Roles.

The webhook validates:

- `roleCoordination` is used with `RoleRollingUpdate`;
- `maxSkew` is a percentage in the range `(0%, 100%]`;
- at least two Roles participate;
- all Role names exist;
- Role names and dependency edges are unique;
- dependency endpoints belong to the selected Role set;
- the dependency graph is a DAG;
- on UPDATE, every changed target-version dependency has `partition < replicas` and provides target-version bootstrap capacity through replacement, ordinary scale-up, or positive `maxSurge` while required old-version capacity is retained.

UPDATE validation reports the dependency Role and the capacity setting to adjust.

### Design Details

#### Overall Flow

For each non-deleting ServingGroup, the controller completes one cross-Role coordination decision before changing any Role. The following six steps are also the order used by the detailed design below:

1. **Build the snapshot.** Resolve the participating Roles, the pre-update and target desired counts, partitions, template hashes, existing Role replicas, and readiness.
2. **Calculate proportional allowance.** Use normalized Ready progress and `maxSkew` to calculate how many replacements each Role may have started.
3. **Apply dependencies.** Decide whether target-version work may start and whether a final old dependency replica must be retained.
4. **Derive final limits.** Convert the proportional and dependency results into `effectivePartition` and `targetStartAllowed` for every Role.
5. **Execute existing paths.** Apply those limits to Role scale synchronization and the existing rolling-delete path.
6. **Observe and continue.** A later reconcile rebuilds the snapshot from the resulting deletion, creation, and Ready state and repeats the calculation.

#### Step 1: Build the ServingGroup Snapshot

For every selected Role, the state builder reads three inputs:

- `previousDesired` from the pre-update ControllerRevision;
- `desired` from the new Role spec; and
- the resolved `userPartition`.

They define the stable population covered by proportional replacement:

```text
stableEnd = min(previousDesired, desired)
stableReplacementRange = [userPartition, stableEnd)
T = max(stableEnd - userPartition, 0)
```

`T` is the number of pre-update stable Role ordinal slots covered by proportional replacement. It uses the smaller of the old and new desired counts, so ordinary scale-up slots at ordinals greater than or equal to `stableEnd` and scale-down excess stay outside the proportional denominator. The controller obtains the ordinal from the Role ID, and the existing scale path fills missing ordinals inside its expected range. If Role scale synchronization fills a missing ordinal inside `stableReplacementRange`, that capacity occupies a pre-update stable slot and is therefore replacement capacity, regardless of which execution path created it.

The proportional calculation keeps only three per-Role values. They are reconstructed on every reconcile and are not persisted in the API or status:

| State | Meaning | Where it is used |
| --- | --- | --- |
| `T` | total pre-update stable slots to replace | Step 2 uses it as the progress denominator and replacement target |
| `S` | slots whose replacement has started | Step 2 subtracts it from the allowed total so in-flight work is not admitted again |
| `R` | started slots in `stableReplacementRange` backed by target-version `RoleRunning` capacity at the same ordinal | Step 2 uses `R/T` as normalized Ready progress |

`S` prevents already admitted deletion and recreation work from consuming the allowance again. `R` advances only when stable target-version capacity reaches `RoleRunning`. Their reconstruction from Role and Pod observations is described later in [State Reconstruction and Controller Restart](#state-reconstruction-and-controller-restart).

#### Step 2: Calculate the Proportional Allowance

A Role participates in the active `maxSkew` baseline while its template changed, `T > 0`, and `R < T`. An unchanged selected Role is already complete for proportional coordination, although its target-version Ready capacity may still satisfy a dependency. The normalized Ready progress of an active Role is:

```text
progress = R / T
```

For each reconcile:

```text
if activeRoleCount <= 1:
    ratioLimit = 1
else:
    readyBaseline = minimum progress across active Roles
    ratioLimit = min(1, readyBaseline + maxSkew)

allowedStarted = min(T, ceil(ratioLimit * T))
```

`allowedStarted` is the total number of replacements currently allowed to have started. This snapshot may admit at most `max(allowedStarted-S, 0)` additional replacements. The per-Role ceiling converts the percentage allowance to a replica count. Because rollout progress is discrete, the observed ratio can exceed the percentage boundary by at most one replica step for that Role.

When zero or one Role remains active, there is no cross-Role difference to constrain. Dependency rules and the Role's `maxUnavailable` continue to control execution.

With A=100, B=50, C=10, and `maxSkew: 10%`:

| State | Ready progress A/B/C | `allowedStarted` for A/B/C |
| --- | --- | --- |
| Initial | 0% / 0% / 0% | 10 / 5 / 1 |
| B=5 and C=1 Ready | 0% / 10% / 10% | 10 / 5 / 1 |
| A=10 Ready | 10% / 10% / 10% | 20 / 10 / 2 |
| A=20, B=10, C=2 Ready | 20% / 20% / 20% | 30 / 15 / 3 |

The table shows the percentage relationship across unequal replica counts. In the first row, A waits for dependency startup while B and C receive the initial target-version work. Each Role's `maxUnavailable` can further reduce the number acted on in one reconcile.

The implementation compares ratios with integer cross-multiplication and converts the configured percentage to fixed-point integer units.

#### Step 3: Apply the Dependency Rules

Dependency coordination uses two additional per-Role states, separate from the proportional values above:

| State | Meaning | Where it is used |
| --- | --- | --- |
| `targetState` | `NotStarted`, `Started`, or `Ready` | gates the first target-version start of a rollout root |
| `hasOldVersion` | whether old-version capacity still exists | retains the final old replica on a dependency path |

Replacement, ordinary scale-up, and surge capacity may update `targetState`, but only target-version capacity occupying `stableReplacementRange` contributes to `T`, `S`, and `R`.

An edge:

```yaml
- role: a
  dependsOn: [b]
```

describes a version call from A to B.

`dependsOn` describes target-version traffic compatibility, not an unconditional Pod-creation order for every edge. A Role may prestart while no target-version traffic can reach it. A rollout root is the first Role that can receive such traffic, so the controller holds that root until its complete dependency path is Ready.

The graph provides two coordination rules:

1. rollout-root startup;
2. old-version dependency retention.

##### Rollout roots

A rollout root is a selected Role with zero incoming dependency edges.

For:

```text
A -> B -> C
```

A is the rollout root. Its transitive dependency closure is `{B, C}`. B and C are internal Roles.

Before the first target-version A start is admitted, B and C each reach `targetState=Ready`. A target-hash `RoleRunning` replica created by replacement, ordinary scale-up, or surge provides this state. B and C can prestart together, and their old-replica replacements advance proportional progress.

For the graph:

```text
B -> C
```

B is the rollout root and waits for target-version C Ready capacity.

The root startup rule uses `targetState`. A root in `NotStarted` receives `targetStartAllowed=false` until every Role in its transitive dependency closure is `Ready`. Internal dependency Roles receive `targetStartAllowed=true` and can prestart together. After target-version replacement, ordinary scale-up, or surge work is admitted, the root enters `Started`; later reconciles continue its rollout through `maxSkew`.

##### Old-version dependency retention

Every direct edge also protects the old-version request path.

For `A -> B -> C`:

- while an old A replica exists, B retains at least one old replica;
- while an old B replica exists, C retains at least one old replica.

With zero user partitions, the old-version completion order is:

```text
A completes
    -> B can remove its final old replica
        -> C can remove its final old replica
```

The retention rule uses `hasOldVersion`. A terminating old replica keeps this value true until its Pods have disappeared.

If a Role's `userPartition` already preserves one or more old replicas, that boundary satisfies the retention requirement. A partition-protected old caller also keeps its direct old dependency for the same lifetime.

Retention protects old-replica deletion, while target-version creation remains available. A dependency may create ordinary scale-up or surge capacity while its final old replica remains protected. This lets a one-replica dependency establish new-version capacity while preserving the old-version request path.

#### Step 4: Derive the Final Role Limits

The proportional allowance from Step 2 becomes the initial final boundary:

```text
effectivePartition = userPartition + T - allowedStarted
```

This protects the part of the pre-update stable population that has not yet been admitted by `maxSkew`.

The dependency rules from Step 3 then adjust the same value. A rollout root with `targetState=NotStarted` and `targetStartAllowed=false` uses `effectivePartition=userPartition+T`, which admits no old-replica replacement. Once its dependency closure is Ready, it uses the proportional boundary.

Old-version retention applies the following boundary:

```text
if hasOldVersion and a direct caller hasOldVersion:
    effectivePartition = max(effectivePartition, 1)
```

`userPartition` is already included once in the initial formula, so no later `max(userPartition, ...)` is required. The existing path keeps its current Role ordering, selects outdated replicas outside the first `effectivePartition` partition-protected Role replicas, and applies the Role's `maxUnavailable`. Existing descending ordinal ordering remains in use. `effectivePartition` therefore controls old-replica replacement, while `targetStartAllowed` controls target-version creation by ordinary scale-up and `maxSurge`.

#### Step 5: Execute Through Existing Role Paths

The controller builds the Step 1 snapshot and coordination decision once for the ServingGroup before running Role scale synchronization and rolling deletion. The same decision is passed to both paths:

```text
ServingGroup snapshot
    -> calculate effectivePartition and targetStartAllowed
    -> syncRoleReplicas creates admitted target capacity
    -> rolesToDeleteForRoleRollingUpdate applies effectivePartition
    -> existing maxUnavailable selects the local delete count
    -> DeleteRole
    -> later syncRoleReplicas fills the missing stable capacity
```

During a template update, target-version Role capacity can come from three paths:

| Capacity source | How it is created | Proportional coordination | Dependency coordination |
| --- | --- | --- | --- |
| Rolling replacement | an old stable replica is deleted and recreated with the target template | contributes to `T`, `S`, and `R` | becomes dependency Ready at `RoleRunning` |
| Ordinary scale-up | `desired` increases beyond `previousDesired` | stays outside `T`, `S`, and `R` | becomes dependency Ready at `RoleRunning` |
| Temporary `maxSurge` | capacity is created above `desired`, up to `desired+maxSurge` | stays outside `T`, `S`, and `R` | becomes dependency Ready at `RoleRunning` and is reclaimed after rollout |

`rolesToDeleteForRoleRollingUpdate` remains responsible for outdated detection, descending Role ordering, and the Role's `maxUnavailable`. Coordination only narrows its candidate set through `effectivePartition`.

For a simultaneous template update and ordinary scale-up, `syncRoleReplicas` creates target capacity toward `desired` after `targetStartAllowed` becomes true. Target capacity at ordinals in `[stableEnd, desired)` stays outside proportional progress, while a resulting target-hash `RoleRunning` replica can make `targetState=Ready` for dependency startup. Target capacity that fills an ordinal in `stableReplacementRange` is counted as replacement progress.

For example, `previousDesired=10`, `desired=12`, and `userPartition=0` produce `stableReplacementRange=[0,10)` and `T=10`. Ordinals 10 and 11 are ordinary scale-up slots outside `T`. The ten pre-update stable ordinal slots form the proportional denominator.

For a simultaneous scale-down, `min(previousDesired, desired)` excludes the removed excess from the proportional population. Existing scale-down policy reconciles that excess, while `T` remains the replacement denominator.

`maxUnavailable` remains the per-Role availability budget after all coordination limits have been applied.

##### Role maxSurge execution

Role-level `maxSurge` uses count-based behavior. The coordinator first determines whether temporary capacity is still required:

```text
surgeRequired = updateableOldExists or dependencyRetentionIsBinding

if targetStartAllowed and surgeRequired:
    temporaryExpected = desired + maxSurge
else:
    temporaryExpected = desired
```

`dependencyRetentionIsBinding` is true only when dependency coordination, rather than `userPartition`, is retaining the final old replica. Normal Role scale synchronization creates capacity up to `temporaryExpected`. The controller does not persist a permanent replacement, scale-up, or surge origin on an individual Role. Each reconcile classifies its current capacity by ordinal range: `stableReplacementRange` contributes to proportional progress, `[stableEnd, desired)` contributes ordinary scale-up capacity, and `[desired, temporaryExpected)` contributes admitted surge capacity. Eligible target-hash capacity in these admitted ranges that becomes `RoleRunning` can make `targetState=Ready` and unlock a dependent root.

Old-replica deletion still follows `effectivePartition` and `maxUnavailable`. `maxSkew` still follows `R/T`; temporary capacity above `desired` is outside the stable target capacity used to derive `R`.

While dependency retention is binding, the temporary expected count does not contract and the target-version capacity used to unlock the root remains available. After the caller's final old replica disappears, retention releases; the dependency's final old replica becomes updateable and is replaced. Only after updateable old work and the coordination retention are both gone does `temporaryExpected` return to `desired`. Existing Role scale-down scoring then selects any remaining target-version excess; ordinal does not determine which Role is removed.

For one-replica Roles in `A -> B -> C`, `maxSurge: 1` on B and C permits the following sequence:

1. Keep old B and old C for the old request path.
2. Create target B and target C as admitted surge capacity.
3. After target B and target C are `RoleRunning`, update A.
4. After the final old A disappears, remove the final old B.
5. After the final old B disappears, remove the final old C.
6. Reclaim any capacity above the declared Role replica counts.

For root A, delete-first replacement provides the target-version start. For a changed dependency, replacement or ordinary scale-up provides the target-version start when old capacity can be retained; `maxSurge` provides bootstrap capacity for the remaining single-capacity case.

#### Step 6: Observe the Result and Continue

The controller completes one bounded amount of work and returns from reconcile. Later resource state changes enqueue the ModelServing again.

A participating Role replica becomes `RoleRunning` after its entry Pod and all worker Pods are Running and Ready. That transition enqueues the ModelServing so the next reconcile can:

- unlock a rollout root;
- advance the Ready baseline;
- admit the next proportional work;
- release an old-version retention boundary.

#### Worked Example: A, B, and C

Assume each Role has ten replicas, `maxUnavailable: 1`, `maxSurge: 0`, `partition: 0`, and `maxSkew: 10%`. Initially, every Role has `T=10` and `S=R=0`.

The first proportional calculation gives `allowedStarted=1` and `effectivePartition=9` for every Role. Dependency startup then changes only A's result:

| Role | Proportional boundary | Dependency result | `effectivePartition` |
| --- | ---: | --- | ---: |
| A | 9 | A is a `NotStarted` root, so protect all ten old slots | 10 |
| B | 9 | Internal Role; this boundary already retains old B | 9 |
| C | 9 | Internal Role; this boundary already retains old C | 9 |

The existing rolling path can therefore replace one B and one C, while A has no delete candidate. Their in-flight work makes `S=1` for B and C. When both target replicas become `RoleRunning`, their `R` becomes 1 and their `targetState` becomes `Ready`. A's dependency closure is now Ready, so the next reconcile restores A's proportional boundary to 9 and admits one A replacement.

After A is started, all three Roles use the same Ready baseline. When every Role has `R=1`, `allowedStarted` becomes 2; when every Role has `R=2`, it becomes 3, and so on.

Near completion, `maxSkew` can produce:

```text
effectivePartition(A) = 0
effectivePartition(B) = 1    # old A still keeps one old B
effectivePartition(C) = 1    # old B still keeps one old C
```

After old A disappears, `hasOldVersion(A)=false` and B may use `effectivePartition=0`. After old B disappears, C may also use `effectivePartition=0`.

#### State Reconstruction and Controller Restart

No coordination state is persisted. Every reconcile derives it from the ModelServing spec, the pre-update and target ControllerRevisions, and the observed Roles and Pods.

##### Proportional state

- `T` is the size of `stableReplacementRange=[userPartition,min(previousDesired,desired))`.
- `S` is `T` minus the non-deleting old replicas that still occupy this ordinal range. A slot remains started while its old replica is deleting, its stable capacity is missing, or its replacement is Creating.
- `R` is the number of started ordinals in this range occupied by a target-hash `RoleRunning` replica.

Here, `remainingOldToUpdate` is the number of non-deleting old replicas still occupying `stableReplacementRange`, and `stableTargetReady` is the number of target-hash `RoleRunning` replicas whose Role ordinal is in that range. The reconstruction is:

```text
stableEnd              = min(previousDesired, desired)
stableReplacementRange = [userPartition, stableEnd)
T                      = max(stableEnd - userPartition, 0)
S                      = T - min(T, remainingOldToUpdate)
R                      = min(S, stableTargetReady)
```

This range-based reconstruction is independent of Ready ordering. If a replacement becomes Ready before an ordinary scale-up replica, it advances `R` immediately. If the scale-up replica becomes Ready first, it may satisfy dependency startup but does not advance `R`. Capacity above `stableEnd` cannot inflate proportional progress.

For example, `previousDesired=2`, `desired=3`, and `userPartition=0` produce `stableReplacementRange=[0,2)`. If target ordinal 0 is `RoleRunning` while scale-up ordinal 2 is not Ready, then `R=1`. If ordinal 2 is Ready while the target replacement at ordinal 0 is not Ready, then `R=0`; the scale-up capacity affects only dependency state.

##### Dependency state

- `targetState` becomes `Started` when target-version replacement, ordinary scale-up, or admitted surge work exists in the currently admitted ordinal range. It becomes `Ready` when target-hash `RoleRunning` capacity exists in that range.
- `hasOldVersion` remains true while an old-hash Role or terminating old Pod still exists, or while a partition-protected old slot is temporarily missing and awaiting recovery. Terminating Pods are read from the Pod informer cache so the final-old-replica protection survives datastore reconstruction.

```text
dependencyExpected = desired + admittedSurge
dependencyRange    = [0, dependencyExpected)
dependencyReady    = target-hash RoleRunning capacity in dependencyRange
```

`admittedSurge` is the resolved `maxSurge` only while target startup is allowed and `surgeRequired` from Step 5 is true; otherwise it is zero. `dependencyReady > 0` sets `targetState=Ready`. This allows ordinary scale-up and admitted surge capacity to unlock a root, while Role ordinals outside `dependencyRange` that are awaiting scale-down are ignored.

Ordinary scale-up occupies the admitted desired range above `stableEnd`. Capacity in `[desired, dependencyExpected)` is admitted surge capacity while `surgeRequired` is true. Revision and template-hash labels distinguish old and target versions, and the ordinal boundaries classify their current coordination semantics without persisting an origin label.

After a controller restart, the startup path rebuilds the datastore and enqueues existing ModelServings. The first reconcile observes the same revisions, hashes, deletion state, missing capacity, and Ready conditions, reconstructs the values above, and resumes from the resulting limits.

### Status and Observability

The proposal adds a ModelServing condition through the existing `status.conditions` field:

```go
const ModelServingCoordinatedRoleRolloutBlocked ModelServingConditionType =
    "CoordinatedRoleRolloutBlocked"
```

The condition is `True` when a participating Role still has rollout work and is waiting on a coordination constraint.

Supported reasons:

- `DependencyNotReady`;
- `MaxSkewLimitReached`;
- `OldVersionDependencyPresent`.

Example:

```yaml
status:
  conditions:
  - type: CoordinatedRoleRolloutBlocked
    status: "True"
    reason: DependencyNotReady
    message: "ServingGroup example-0: root role a is waiting for target-version RoleRunning capacity from roles b and c"
    observedGeneration: 12
```

Current blockers are ordered by ServingGroup ordinal, Role declaration order, and fixed reason priority: `DependencyNotReady`, `OldVersionDependencyPresent`, and `MaxSkewLimitReached`. The first blocker supplies the condition reason, and the message summarizes the affected Roles.

The controller sets the condition to `False` with `ProgressAvailable` or `RolloutComplete` when the corresponding state is observed. Kubernetes Events record blocker changes and rollout resumption.

### Blocking and Admission Behavior

An admitted replacement in deletion, creation, scheduling, or readiness stages contributes to `S`, reserving its proportional allowance. Stable target Ready capacity advances `R`. Existing Role and Pod status expose the readiness of admitted replicas; the coordination condition identifies the constraint that prevents additional work.

| Situation | Controller behavior | Condition reason |
| --- | --- | --- |
| A root has `targetState=NotStarted` and its dependency closure is not target Ready | Keep its target start gated and identify the dependency Roles | `DependencyNotReady` |
| A Role has `S >= allowedStarted` while `allowedStarted < T` | Preserve admitted work and wait for the shared Ready baseline | `MaxSkewLimitReached` |
| Dependency retention raises `effectivePartition` | Retain the dependency's final old replica | `OldVersionDependencyPresent` |

Update admission verifies that each changed dependency can provide target-version capacity while retaining required old-version capacity. For a one-replica dependency, ordinary scale-up or positive `maxSurge` supplies the concurrent capacity. Validation identifies the dependency Role and directs the user to adjust `maxSurge`, `replicas`, or the rollout staging.

### Implementation

The implementation adds a per-ServingGroup coordinator with a pure calculation interface:

```go
type coordinatedRoleState struct {
    roleName       string
    userPartition  int
    totalToUpdate  int // T
    startedCount   int // S
    readyCount     int // R
    targetState    targetVersionState
    hasOldVersion  bool
    active         bool
}

type targetVersionState string

const (
    targetNotStarted targetVersionState = "NotStarted"
    targetStarted    targetVersionState = "Started"
    targetReady      targetVersionState = "Ready"
)

type coordinatedRoleDecision struct {
    effectivePartitions map[string]int
    targetStartAllowed   map[string]bool
    temporaryExpected    map[string]int
    blockers             []coordinatedRoleBlocker
}

func coordinateRoleRollout(
    states []coordinatedRoleState,
    coordination *RoleCoordination,
) (coordinatedRoleDecision, error)
```

The state builder derives `totalToUpdate` from `previousDesired`, `desired`, and `userPartition`; the coordinator does not carry those extra intermediate values. `active` is true only for a changed Role with unfinished stable replacement work. `targetStartAllowed` is true for internal dependency Roles and for rollout roots whose dependency closure is Ready or whose target-version work has already started. `effectivePartitions` controls old-replica replacement, while `temporaryExpected` keeps admitted surge capacity until both updateable old work and dependency retention have cleared.

Main code changes:

- API types, generated clients, deepcopy code, and CRDs for `roleCoordination`;
- admission parsing of the old and new ModelServing objects for update-time rollout feasibility validation;
- webhook validation for Role selection, `maxSkew`, dependency DAGs, partition-limited target capacity, and insufficient bootstrap capacity on update;
- flat Role-state construction for `T`, `S`, `R`, `targetState`, `hasOldVersion`, and `active` from the ControllerRevision, Role spec, datastore Roles, Role ordinals, Pods, hashes, readiness, and admitted capacity ranges;
- `syncModelServing` construction of one per-ServingGroup decision map before Role replica synchronization, passed to both `syncRoleReplicas` and `manageRollingUpdate`;
- `rolesToDeleteForRoleRollingUpdate` integration for `effectivePartition`;
- `scaleUpRoles` integration with dependency startup and `targetState` updates during a coordinated Role rollout;
- `manageRoleReplicasPerGroup` integration with dependency-gated Role-level `maxSurge` expected counts and `targetState` updates;
- immediate ModelServing enqueue on participating Role `RoleRunning` transitions;
- ModelServing condition and Event updates for coordination blockers.

Existing `DeleteRole`, `CreatePodsByRole`, ControllerRevision, Role template hash, and per-Role `maxUnavailable` execution remain the rollout mechanisms.

### Risks and Mitigations

#### Integer progress granularity

Small Roles have coarse progress steps. Per-Role ceiling conversion permits one local step while retaining the configured percentage baseline for every other Role.

#### Repeated reconciliation

`S` reconstructs admitted delete-first replacement across deleting, missing, Creating, and Ready states. `targetState` reconstructs ordinary scale-up and surge work for dependency startup. Repeated reconciles calculate the same or a stricter admission boundary from the observed snapshot.

#### Ordinary Scale-up and Surge Capacity

Ordinary scale-up and temporary capacity contribute through `targetState`. After reaching `RoleRunning` in an admitted ordinal range, both provide dependency Ready capacity. Only target Ready capacity inside `stableReplacementRange` contributes to `R`; Ready ordering between replacement and expansion capacity therefore cannot change the measured proportional progress. Ordinary scale-up persists inside `desired`, while the expected count contracts to `desired` after eligible old replicas are gone.

#### Informer convergence

Terminating Pods and missing ordinals are classified conservatively. Reconciliation resumes as informer state converges.

#### Long-running blockers

The ModelServing condition records the blocking ServingGroup, Role, reason, and relevant progress data. Events record transitions between blocker states.

### Test Plan

#### Unit tests

- Role selection, `maxSkew`, and dependency DAG validation.
- UPDATE capacity validation for changed dependencies, including `partition < replicas` and single-replica bootstrap through ordinary scale-up or positive `maxSurge`.
- Rollout-root inference for chains, branches, multiple roots, and isolated Roles.
- `T=max(min(previousDesired,desired)-userPartition,0)` for equal, scale-up, and scale-down updates.
- `S` and `R` construction from Running, Creating, Deleting, terminating, and missing states inside `stableReplacementRange`.
- Ordinal-range distinction among stable replacement, ordinary scale-up, admitted surge, and scale-down excess capacity.
- A replacement Ready before ordinary scale-up advances `R` immediately, while ordinary scale-up Ready before replacement leaves `R` unchanged and only advances `targetState`.
- Ordinary scale-up and surge transitions through `targetState=Started` and `targetState=Ready`.
- Equal and unequal Role replica counts.
- Zero or one active replacement Role releases the cross-Role ratio limit.
- Per-Role ceiling conversion and small-Role quantization.
- `A -> B -> C` internal prestart and root startup behavior.
- `B -> C` root startup behavior.
- Direct-edge old-version retention.
- Derivation of `effectivePartition` from `userPartition`, `T`, `allowedStarted`, and dependency adjustments.
- Ordinary scale-up and surge creation while the final old dependency replica is retained.
- Root ordinary scale-up and surge creation after the dependency closure reaches `Ready`.
- Surge creation up to the Role's `maxSurge`.
- Surge capacity is reclaimed after eligible old replicas complete.
- Descending ordinal selection and `maxUnavailable`.
- Deterministic blocker aggregation and condition transitions.
- Restart reconstruction during deletion, missing replacement, creation, and readiness.

#### Property and fuzz tests

For generated replica counts, partitions, Role states, and acyclic graphs:

- `effectivePartition >= userPartition`;
- new replacement admission is zero when `S >= allowedStarted`, and otherwise projected `S` stays within `allowedStarted`;
- a rollout root starts after every Role in its closure reaches `targetState=Ready`;
- target-hash RoleRunning capacity inside `dependencyRange` sets `targetState=Ready`; only capacity inside `stableReplacementRange` contributes to `R`, while `R <= S <= T`;
- `T` remains derived solely from `previousDesired`, `desired`, and `userPartition` when ordinary scale-up or temporary excess exists;
- scale-down capacity is reconciled through the desired range;
- an observed old caller retains its direct old dependency;
- a retained final old dependency allows admitted target scale-up and surge creation;
- identical snapshots produce identical decisions;
- graph validation covers generated DAGs and structural errors.

#### Integration tests

- A ModelServing spec update enters coordinated Role rolling update.
- Deletion and recreation gaps preserve in-flight reservation.
- Participating Role `RoleRunning` transitions trigger the next decision.
- Controller restart reconstructs deleting and Creating work.
- Controller restart reconstructs the temporary expected count without creating capacity above `desired+maxSurge`.
- Terminating old Pods retain the old-version dependency boundary.
- Status updates preserve existing conditions and update `CoordinatedRoleRolloutBlocked`.
- Concurrent template update and scale-up uses stable-range `R/T` for `maxSkew` and the wider admitted dependency range for scale-up startup, including both Ready-order permutations.
- Surge creation reaches the configured `maxSurge` through dependency startup.
- A rollout root creates replacement, ordinary scale-up, or surge target capacity after its dependency closure reaches `Ready`.

#### End-to-end tests

- Equal-replica A/B/C rollout with delayed C readiness.
- A/B/C rollout completion in old-version order A, B, C.
- Unequal A/B/C replica counts with `maxSkew: 10%`.
- Different user partitions across Roles.
- Concurrent update and ordinary scale-up with replacement Ready before expansion, and with expansion Ready before replacement.
- One-replica B and C establish target Ready capacity through `maxSurge: 1` while old B and C remain available.
- Admission reports the capacity requirement for a changed fully partitioned dependency.
- Admission reports the bootstrap requirement for a one-replica dependency.
- An unschedulable dependency produces a visible blocker condition.
- Explicit Role subset coordination.
- Controller restart during delete, create, and readiness stages.

### Rollout Plan

Configuring `roleCoordination` under `RoleRollingUpdate` enables the feature. The existing independent Role rolling-update path remains the default behavior.

Implementation proceeds in four stages:

1. add the API, generated artifacts, and webhook validation;
2. add per-ServingGroup state construction and the pure coordination calculation;
3. integrate effective partitions, dependency-gated ordinary scale-up and `maxSurge`, Ready-triggered continuation, and status reporting;
4. add unit, integration, and end-to-end coverage.

### References

- [Kthena Role rolling update proposal](https://github.com/volcano-sh/kthena/blob/main/docs/proposal/role-rollingupdate.md)
- [Kthena Role-level maxSurge implementation](https://github.com/volcano-sh/kthena/pull/1626)
