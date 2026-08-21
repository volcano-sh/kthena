# Rollout Strategy

Rolling updates represent a critical operational strategy for online services aiming to achieve zero downtime. In the context of LLM inference services, the implementation of rolling updates is important to reduce the risk of service unavailability.

`ModelServing` supports rolling updates at either the `ServingGroup` or `Role` level. Select exactly one granularity with `spec.rolloutStrategy.type`:

- `ServingGroupRollingUpdate` uses `spec.rolloutStrategy.rollingUpdateConfiguration`. Its `maxUnavailable` limits unavailable `ServingGroups`, `maxSurge` limits temporary additional `ServingGroups`, and `partition` protects stable `ServingGroups` from updates.
- `RoleRollingUpdate` uses the inline `maxUnavailable`, `maxSurge`, and `partition` fields on each entry in `spec.template.roles`. The settings are applied independently to each `Role` in every `ServingGroup`. The ModelServing-level `rollingUpdateConfiguration` must not be set and does not participate in the Role availability budget.

Configure rolling update settings only at the selected granularity. `maxUnavailable` accepts an absolute number or a percentage and defaults to `1`. A percentage is calculated from `spec.replicas` for `ServingGroupRollingUpdate` and from the corresponding Role's `replicas` for `RoleRollingUpdate`.

`maxSurge` accepts a non-negative absolute number or percentage, defaults to `0`, and rounds percentages up. It is calculated from `spec.replicas` for `ServingGroupRollingUpdate` and from the corresponding Role's `replicas` for `RoleRollingUpdate`. `maxUnavailable` and `maxSurge` cannot both resolve to zero when the partition leaves replicas eligible for update.

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

    The controller always enforces these bounds:

    $$
    N_{live} \leq replicas + maxSurge
    $$

    $$
    N_{available} \geq replicas - maxUnavailable
    $$

    An unready additional ServingGroup counts toward the replica ceiling but does not contribute to availability. If it cannot be scheduled, the rollout waits without deleting available capacity. No rollout lifecycle label or status journal is required because the expected count is derived from the observed revisions on every reconciliation.

    Ordinal values do not identify surge capacity. Binpack scale-down can leave sparse ordinals or an ordinal greater than `replicas`, and that ServingGroup remains a normal replica. When the update finishes, the expected count returns to `replicas` and the regular ModelServing binpack scale-down policy selects excess groups using readiness and deletion cost. Changing `replicas` or `maxSurge` during rollout therefore changes only the target count; it does not force deletion of high ordinals. Partition protection remains ordinal-based and independent of the current replica count.

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

Role ordinals do not identify surge capacity. Binpack scale-down may retain sparse or high Role ordinals as normal replicas. When the update finishes, the expected count returns to `replicas`, and the existing Role scale-down policy selects excess replicas using readiness and deletion cost.

If `recoveryPolicy` is `ServingGroupRecreate`, deleting an outdated Role triggers recreation of its entire `ServingGroup`, which removes the resource-saving benefit of `RoleRollingUpdate`. Use `RoleRecreate` when only the outdated Role should be rebuilt.
