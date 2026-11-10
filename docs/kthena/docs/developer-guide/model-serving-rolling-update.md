# Rollout Strategy

Rolling updates represent a critical operational strategy for online services aiming to achieve zero downtime. In the context of LLM inference services, the implementation of rolling updates is important to reduce the risk of service unavailability.

`ModelServing` supports rolling updates at either the `ServingGroup` or `Role` level. Select exactly one granularity with `spec.rolloutStrategy.type`:

See [ModelServing behavior](../user-guide/modelserving-behavior.md) for the complete configuration, scaling, recovery and status rules.

- `ServingGroupRollingUpdate` uses `spec.rolloutStrategy.rollingUpdateConfiguration`. Its `maxUnavailable` limits unavailable `ServingGroups`, `maxSurge` limits temporary additional `ServingGroups`, and `partition` protects stable `ServingGroups` from updates.
- `RoleRollingUpdate` uses the inline `maxUnavailable`, `maxSurge`, and `partition` fields on each entry in `spec.template.roles`. The settings are applied independently to each `Role` in every `ServingGroup`. The ModelServing-level `rollingUpdateConfiguration` is allowed but ignored in this mode.

Configure the active granularity's budget; inactive settings still undergo basic syntax validation. `maxUnavailable` accepts an absolute number or a percentage and defaults to `1`; percentages round down. A percentage is calculated from `spec.replicas` for `ServingGroupRollingUpdate` and from the corresponding Role's `replicas` for `RoleRollingUpdate`.

`maxSurge` accepts a non-negative absolute number or integer percentage, including percentages above 100%, defaults to `0`, and rounds percentages up. It uses the same count axis as `maxUnavailable`, and replicas plus resolved surge must fit an int32. Active `maxUnavailable` and `maxSurge` cannot both resolve to zero, including with a full partition. Checks that depend on the replica count are skipped at zero replicas.

`partition` protects ServingGroups whose ordinals are in `[0, partition)`. The remaining ServingGroups are eligible for rolling update. This definition remains deterministic when binpack scale-down leaves a sparse ordinal set.

## ServingGroup rolling update

When a protected replica is missing and must be recreated, the ordinal itself selects the template: a missing ordinal below `partition` uses `CurrentRevision` and its historical template, while other missing ordinals use `UpdateRevision` and the current template. Before creating a `ServingGroup` that references a new revision, the controller must successfully persist its `ControllerRevision`; otherwise reconciliation stops and retries without creating a partial `ServingGroup`.

Here's a ModelServing configured with rollout strategy:

```yaml
spec:
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 0
      maxSurge: 1
      partition: 0
```

With `replicas: 4` and `maxSurge: 1`, the controller temporarily changes the expected ServingGroup count from four to five while an updateable outdated ServingGroup exists. Normal replica synchronization creates or removes the required capacity; rolling-update reconciliation only selects outdated groups within the availability budget.

New creates respect the physical capacity ceiling. Healthy rollout deletions respect the availability floor; external failures can already have reduced availability below it. A replica decrease can also leave existing terminating capacity temporarily above the new ceiling:

$$
N_{live} \leq replicas + maxSurge
$$

$$
N_{available} \geq replicas - maxUnavailable
$$

An unready additional ServingGroup counts toward the replica ceiling but does not contribute to availability. If it cannot be scheduled, it cannot fund deletion of healthy capacity. Deletion and replacement reservations, including protected historical replacements not yet Ready, cannot spend an allowance twice.

Ordinal values alone do not identify surge capacity. Binpack scale-down can retain sparse or high stable ordinals. The controller tracks temporary origin separately, fills required formal capacity first, and removes temporary capacity once replacements are Ready and the budget permits it. Temporary capacity is adopted without changing UID only when its ordinal enters the new formal range; merely reaching the desired total count does not adopt it. Partition remains ordinal-based.

In the following we'll show how rolling update processes for a `ModelServing` with four replicas. Three Replica status are simulated here:

- ✅ Replica has been updated
- ❎ Replica hasn't been updated
- ⏳ Replica is in rolling update

| | R-0 | R-1 | R-2 | R-3 | Note |
| --- | --- | --- | --- | --- | --- |
| Stage1 | ✅ | ✅ | ✅ | ✅ | Before rolling update |
| Stage2 | ❎ | ❎ | ❎ | ⏳ | Rolling update started; R-3 is selected first in this example |
| Stage3 | ❎ | ❎ | ⏳ | ✅ | R-3 is updated. The next replica (R-2) is now being updated |
| Stage4 | ❎ | ⏳ | ✅ | ✅ | R-2 is updated. The next replica (R-1) is now being updated |
| Stage5 | ⏳ | ✅ | ✅ | ✅ | R-1 is updated. The last replica (R-0) is now being updated |
| Stage6 | ✅ | ✅ | ✅ | ✅ | Update completed. All replicas are on the new version |

During a rolling upgrade, the controller selects an eligible outdated replica while respecting partition and availability constraints, then deletes and rebuilds it. Unhealthy outdated replicas are prioritized; ordinal order is used within the applicable candidate ordering. The controller does not proceed beyond the availability budget until replacement capacity is ready.

## Role rolling update

Use `RoleRollingUpdate` when only the changed Roles should be recreated instead of rebuilding an entire `ServingGroup`. Configure the availability budget and partition directly on each Role:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
  template:
    roles:
      - name: prefill
        replicas: 4
        maxUnavailable: 0
        maxSurge: 1
        partition: 0
        # entryTemplate and other Role fields are omitted
      - name: decode
        replicas: 2
        maxUnavailable: 1
        maxSurge: 1
        partition: 0
        # entryTemplate and other Role fields are omitted
```

Kthena evaluates Role updates across all `ServingGroups`. Because each `ServingGroup` applies the per-Role availability budget independently, `RoleRollingUpdate` is recommended for a ModelServing with a single `ServingGroup`.

While an updateable outdated Role replica exists, the controller temporarily changes that Role's expected replica count from `replicas` to `replicas + maxSurge`. Normal Role replica synchronization creates and later removes the additional capacity, while rolling-update reconciliation only selects outdated replicas within the `maxUnavailable` budget. An unready new replica consumes availability budget and can naturally block further deletion.

Role ordinals alone do not identify surge capacity. Binpack scale-down may retain sparse or high Role ordinals as stable replicas. Temporary Role capacity follows the same origin, formal-refill and Ready-before-retirement rules as temporary ServingGroups.

`RoleRollingUpdate` rejects `recoveryPolicy: ServingGroupRecreate`. Use `RoleRecreate` or `None`; recovery behavior is separate from template rollout.
