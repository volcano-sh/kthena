---
title: Support maxSurge for ModelServing
authors:
- "@LiZhenCheng9527"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-08-11

---

## Support maxSurge for ModelServing

### Summary

This proposal adds `maxSurge` support to both `ServingGroupRollingUpdate` and `RoleRollingUpdate`. During an update, the ModelServing controller may temporarily create additional ServingGroups or Role replicas before deleting outdated replicas, reducing service disruption and enabling create-before-delete updates when `maxUnavailable` is zero.

The implementation is count-driven and stateless. It does not introduce a dedicated surge pool, a surge lifecycle phase, or a surge identity on individual resources. While an updateable outdated replica exists, the controller temporarily raises the expected replica count from the declared replica count to `replicas + maxSurge`. Existing replica synchronization creates or removes capacity to satisfy that count, and existing rolling-update reconciliation decides which outdated resources may be deleted within the availability budget.

Ordinal values identify resources but do not identify surge capacity. Kthena's binpack scale-down may leave sparse or high ordinals in the final replica set, so rollout progress is derived from observed replica counts, revisions, Role template hashes, deletion state, and readiness rather than from an ordinal boundary or persisted rollout state.

### Motivation

ModelServing currently controls disruption with `maxUnavailable`. Without additional capacity, updating a healthy outdated ServingGroup or Role normally requires deleting old capacity before replacement capacity becomes available. This either reduces inference capacity or prevents an update from progressing when `maxUnavailable` is zero.

Users with spare cluster capacity should be able to create updated capacity first and let that ready capacity contribute to the existing availability calculation before outdated capacity is removed.

#### Goals

- Add absolute and percentage-based `maxSurge` configuration for `ServingGroupRollingUpdate`.
- Add independently calculated per-Role `maxSurge` configuration for `RoleRollingUpdate`.
- Keep live replicas at or below the temporary expected count derived from the latest spec.
- Continue enforcing `maxUnavailable` using actual replica readiness.
- Preserve Kthena's existing binpack scale-down behavior, including sparse and high ordinals.
- Preserve partition protection and historical-template recovery.
- Recover rollout progress from observed resources after controller restart without persisting a rollout phase.
- Keep existing behavior unchanged when `maxSurge` is omitted or resolves to zero.

#### Non-Goals

- Assigning a persistent surge identity to particular ServingGroups or Roles.
- Reserving high ordinals exclusively for temporary replicas.
- Restoring a contiguous ordinal range after rollout.
- Retaining one explicitly identified surge replica for the full rollout.
- Redesigning ControllerRevision or adding user-facing rollback support.
- Defining traffic routing, connection draining, or request-level readiness semantics.
- Changing autoscaling semantics or treating temporary capacity as desired capacity.
- Guaranteeing that additional capacity can be scheduled when quota, accelerator capacity, or gang-scheduling requirements cannot be satisfied.

### API

`RollingUpdateConfiguration` includes `maxSurge`:

```go
type RollingUpdateConfiguration struct {
    // MaxUnavailable limits unavailable resources during rollout.
    // Percentages are rounded down. It defaults to 1.
    MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

    // MaxSurge limits resources created above the desired replica count.
    // Percentages are rounded up. It defaults to 0.
    MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

    // Partition protects replicas from rollout.
    // Percentages are rounded up. It defaults to 0.
    Partition *intstr.IntOrString `json:"partition,omitempty"`
}
```

For `ServingGroupRollingUpdate`, the configuration is set at ModelServing level:

```yaml
spec:
  replicas: 4
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 0
      maxSurge: 1
```

For `RoleRollingUpdate`, each Role has an independent configuration:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
  template:
    roles:
    - name: decode
      replicas: 4
      maxUnavailable: 0
      maxSurge: 1
```

The top-level `rollingUpdateConfiguration` is invalid for `RoleRollingUpdate`. Conversely, Role-level `maxSurge` is valid only for `RoleRollingUpdate`.

`maxSurge` accepts a non-negative integer or percentage string:

- An integer is an absolute replica count.
- A percentage is resolved against the latest desired replica count on every reconciliation and rounded up.
- ServingGroup percentages use `spec.replicas` as their base.
- Role percentages use the corresponding Role's `replicas` as their base.
- The default value is zero.
- Malformed and negative values are rejected.
- When partition leaves replicas eligible for update, `maxUnavailable` and `maxSurge` cannot both resolve to zero.

Changing `maxSurge`, `maxUnavailable`, or `partition` changes rollout policy rather than the workload template. These fields are excluded from ControllerRevision and Role template hash calculations.

### Budget Resolution

For desired replicas $R$, resolved `maxSurge` $S$, and resolved `maxUnavailable` $U$, the temporary live-capacity ceiling is:

$$
N_{live} \le R + S
$$

The minimum available capacity used by rolling-update deletion is:

$$
N_{available} \ge R - U
$$

The temporary expected count is not always $R+S$. It is derived from observed rollout state:

$$
N_{expected} =
\begin{cases}
R + S, & \text{if an updateable outdated replica exists} \\
R, & \text{otherwise}
\end{cases}
$$

Replica synchronization compares the current live count with $N_{expected}$:

- if the current count is lower, it creates missing replicas;
- if the current count is higher, it invokes ordinary binpack scale-down;
- otherwise, no replica-count action is required.

This model means `maxSurge` is an upper bound on temporary additional capacity, not a guarantee that the cluster can schedule that capacity and not an identity assigned to a specific replica.

### Identity and Ordinals

The controller does not classify replicas as stable or surge by ordinal. In particular, it does not apply the rule that ordinals below `replicas` are stable and ordinals at or above `replicas` are surge.

Replica synchronization fills missing ordinals in `[0, expectedCount)` when creating capacity. This normally creates a high ordinal when the existing set is contiguous, but it may fill a lower ordinal hole instead. During scale-down, existing readiness, deletion cost, partition protection, and ordinal tie-breaking select the excess resource to remove.

As a result, after rollout:

- ordinal values may be sparse;
- a high ordinal may remain as a normal replica;
- a lower ordinal may have been removed;
- the controller does not recreate resources solely to restore `[0, replicas)`;
- no resource label or status field identifies which capacity was temporarily added.

This behavior is required to remain compatible with Kthena's binpack scale-down policy.

### ServingGroupRollingUpdate

#### Temporary capacity

ServingGroup replica synchronization starts with:

$$
N_{expected} = spec.replicas
$$

When all of the following are true, it adds the resolved top-level `maxSurge`:

- the rollout strategy is `ServingGroupRollingUpdate` or omitted;
- `maxSurge` resolves above zero;
- an observed non-protected ServingGroup uses a revision different from `UpdateRevision`.

The controller then uses normal ServingGroup scale-up to reach the temporary expected count. Each additional ServingGroup contains all Roles and has its own normal PodGroup and associated resources.

#### Availability and outdated deletion

`manageRollingUpdate` does not maintain a separate maxSurge state machine and does not wait for a configured number of surge replicas. It calculates how many outdated ServingGroups may be deleted from observed state:

$$
B_{delete} = N_{live} - (R-U) - N_{newUnavailable}
$$

where `newUnavailable` counts new-revision ServingGroups that are not Running.

Consequences of this formula are:

- a Running additional ServingGroup naturally increases the deletion budget;
- an unready additional ServingGroup consumes live capacity but contributes no availability and therefore cannot authorize disruption;
- an unhealthy outdated ServingGroup can be removed without further reducing availability;
- `maxUnavailable: 0` progresses only after sufficient new capacity is Running.

There is no additional gate that requires the number of new replicas to equal `maxSurge`. Existing readiness and availability are the source of truth.

#### Convergence

Once no updateable outdated ServingGroup remains, replica synchronization derives `N_expected = R`. If extra ServingGroups still exist, ordinary ServingGroup scale-down selects excess groups using:

1. readiness priority, with non-ready groups removed first;
2. lower deletion cost;
3. higher ordinal as a tie-breaker;
4. partition protection, with non-protected groups removed before protected groups.

The selected group is not necessarily the one that was created while `maxSurge` was active. This is intentional because no surge identity exists.

For `replicas: 4`, `maxUnavailable: 0`, and `maxSurge: 1`, a typical count progression is:

| Stage | Old revision | New revision | Live count | Description |
| --- | ---: | ---: | ---: | --- |
| Initial | 4 | 0 | 4 | All declared capacity is old. |
| Extra capacity creating | 4 | 1 unready | 5 | The ceiling is reached, but old capacity is retained. |
| Extra capacity ready | 4 | 1 ready | 5 | Availability permits deletion of an old group. |
| Replacement in progress | 3 | 1 | 4 | One old group is deleted and its ordinal may be recreated. |
| Subsequent reconciliation | decreases | increases | 4 or 5 | Replica sync and rolling update continue from observed state. |
| Converged | 0 | 4 | 4 | Expected count returns to the declared replicas. |

The exact ordinal sequence and whether a particular temporary replica remains are deliberately unspecified.

### RoleRollingUpdate

Role `maxSurge` is evaluated independently for every Role in every ServingGroup. For Role $r$ with desired replicas $R_r$, the controller starts with:

$$
N_{expected,r} = R_r
$$

If the Role has an outdated, non-deleting replica outside the protected prefix, it temporarily derives:

$$
N_{expected,r} = R_r + S_r
$$

Normal Role scale-up creates missing Role instances with the latest template hash. Other Roles in the same ServingGroup are not duplicated merely because one Role uses `maxSurge`.

Role rolling update computes its deletion budget independently:

$$
B_{delete,r} = N_{live,r} - (R_r-U_r) - N_{newUnavailable,r}
$$

A ready additional Role increases this budget, while a Creating or Deleting new Role contributes to `newUnavailable` and can block further deletion. When a new Role becomes Running but the ServingGroup is still not Ready because the temporary Role count exceeds the declared count, the controller re-enqueues the ModelServing so rolling-update reconciliation can spend the newly available deletion budget.

After no updateable outdated Role remains, its expected count returns to $R_r$. Existing Role scale-down removes excess instances according to readiness, deletion cost, ordinal tie-breaking, and partition protection. Role ordinals do not identify temporary capacity and may remain sparse or high after convergence.

A ServingGroup is not considered Running while a Role's instance count differs from that Role's declared replicas. Therefore final readiness occurs only after Role temporary capacity has contracted to the declared count.

`RoleRollingUpdate` is most useful with `recoveryPolicy: RoleRecreate`. With `ServingGroupRecreate`, deleting an outdated Role may recreate the entire ServingGroup and remove the resource-saving advantage of Role-level updates.

### Partition Interaction

Partition is resolved against the latest desired replica count and rounded up for percentages.

For existing replicas, rollout selection protects the first `partition` replicas in the datastore's ascending ordinal order. With a contiguous ordinal set this is equivalent to protecting `[0, partition)`. The controller excludes protected replicas from outdated-resource deletion.

Creation and recovery also preserve historical templates:

- when a missing ServingGroup ordinal is below partition, it is recreated from `CurrentRevision` and the matching ControllerRevision template;
- when a missing Role belongs to the protected prefix, its recorded revision or `CurrentRevision` is used to recover the historical Role template;
- other missing replicas use `UpdateRevision` and the latest template.

Scale-down prefers non-protected replicas. ServingGroup scale-down determines protection from parsed ordinal values so changing `replicas` does not reclassify a surviving high ordinal merely because it lies above the desired count.

`maxSurge` does not move the partition value and does not grant special protection to newly created capacity.

### Readiness, Status, and Revision Promotion

The implementation reports observed resources rather than a separate stable/surge view:

| Status field | Counting behavior |
| --- | --- |
| `replicas` | All observed live ServingGroups. May exceed `spec.replicas`. |
| `availableReplicas` | All Running ServingGroups. May exceed `spec.replicas`. |
| `updatedReplicas` | All observed ServingGroups using `UpdateRevision`. May temporarily exceed `spec.replicas`. |
| `currentReplicas` | All observed ServingGroups using `CurrentRevision`. |
| `updateRevision` | The latest desired workload revision. |
| `currentRevision` | Promoted only after count, availability, and updated replicas converge to the declared count. |

ServingGroup rollout completion requires:

$$
updated = available = live = spec.replicas
$$

and then `CurrentRevision` is promoted to `UpdateRevision`. Temporary capacity therefore cannot prematurely complete the rollout; it must first be reduced to the declared replica count.

For Role surge, top-level ServingGroup replica status does not increase, but the affected ServingGroup remains progressing until each Role returns to its declared replica count and all required Pods are ready.

No surge-specific conditions or lifecycle events are added. Existing resource status and Role/ServingGroup events continue to describe creation, readiness, deletion, and rollout progress.

### Reconciliation and Recovery

No rollout phase, baseline replica count, or complete rollout journal is stored. The datastore remains a reconstructable in-memory cache.

On every reconciliation the controller:

1. reconstructs or reads current ServingGroup and Role state;
2. resolves budgets from the latest spec;
3. compares observed revisions or Role template hashes with the latest desired values;
4. derives whether updateable outdated replicas remain;
5. derives the temporary expected count;
6. creates or removes capacity through ordinary replica synchronization;
7. deletes outdated resources only within the observed availability budget;
8. updates status from the resulting datastore snapshot.

After restart or leader failover, informer-visible Pods, Services, PodGroups, ownership labels, revisions, Role template hashes, and ControllerRevisions allow the controller to derive the same expected counts again. Existing additional capacity counts toward the current expected count, so the controller does not require a persisted surge phase to continue.

ControllerRevision remains important for partition-protected recovery. The controller must persist a required revision before creating resources that reference it and must not replace historical templates with the latest template.

### Spec Changes During Rollout

Budgets and expected counts are recalculated from the latest spec on every reconcile:

- increasing `replicas` changes the base count and percentage budgets;
- decreasing `replicas` lowers the target count and invokes ordinary scale-down as needed;
- changing `maxSurge` changes the temporary ceiling;
- changing `maxUnavailable` immediately changes the deletion budget;
- changing the template changes `UpdateRevision` or Role template hash and makes observed older resources outdated.

There is no promotion or reclassification operation for high ordinals. Existing replicas simply participate in count reconciliation and rollout selection according to their observed revision, readiness, deletion cost, and partition protection.

### Failure Behavior

- **Additional capacity cannot schedule:** it consumes the temporary count but contributes no availability, so healthy old capacity is retained when the unavailable budget would otherwise be violated.
- **Additional capacity fails:** existing recovery policy applies; deletion does not advance until observed availability permits it.
- **A resource is manually deleted:** normal reconciliation recreates missing required capacity while an update remains active or converges to the declared count after the update.
- **Deletion is in progress:** deleting resources continue to occupy datastore identity until deletion is observed, preventing ordinal reuse and duplicate creation.
- **The template changes again:** the latest revision or Role template hash becomes the target and expected counts are re-derived from current observations.
- **The controller restarts:** rollout progress is reconstructed without a persisted surge identity or phase.

### Backward Compatibility

`maxSurge` defaults to zero. Existing ModelServing resources therefore continue to use delete-before-create behavior governed by `maxUnavailable` unless users opt in to temporary capacity.

The field is optional, so existing API clients remain source compatible. Adding it requires regenerated deepcopy code, clients, CRDs, the Helm-embedded CRD, reference documentation, examples, and validation tests.

### Test Plan

- Unit tests for ServingGroup and Role `maxSurge` resolution, including absolute values, percentages, defaults, and rounding up.
- Webhook tests for malformed values, negative values, rollout granularity, and the `maxUnavailable == 0 && maxSurge == 0` rule.
- Controller tests for temporary ServingGroup and Role expected counts.
- Controller tests proving ready additional capacity increases deletion budget and unready additional capacity blocks deletion.
- Controller tests proving high or sparse ordinals may remain after binpack scale-down.
- Controller tests for status counts and revision promotion after capacity contracts.
- Controller tests for partition-protected historical template recovery.
- E2E for `ServingGroupRollingUpdate` with `maxUnavailable: 0` and `maxSurge: 1`, validating temporary extra capacity, coexistence of old and new revisions, and final contraction.
- E2E for `RoleRollingUpdate` with per-Role `maxSurge`, validating that only the changed Role gains temporary capacity, the unaffected Role retains its Pod UID, and the changed Role contracts after rollout.

A dedicated restart-mid-rollout maxSurge E2E and combined maxSurge/partition E2E remain useful follow-up coverage; restart correctness currently relies on the same observed-state reconstruction used by normal controller startup.

### Alternatives

#### Persist a rollout phase in the datastore

The datastore could record whether the controller is creating, using, or deleting temporary capacity. This state would be lost on restart and would duplicate information already derived from observed resources, so the implementation keeps the datastore as a cache.

#### Persist a complete rollout journal in status

A status journal could store a baseline replica count, temporary replica identities, and every rollout phase. This would add status writes and conflict handling while creating another source of truth. Current counts, revisions, hashes, readiness, and the latest spec are sufficient for the implemented count-driven behavior.

#### Classify high ordinals as surge replicas

The controller could define ordinals at or above the desired count as temporary and always delete them after rollout. That conflicts with binpack scale-down, where high or sparse ordinals are valid normal replicas, and would force unnecessary recreation solely to restore names. The implementation therefore does not infer lifecycle identity from ordinal values.

#### Retain a dedicated surge pool through the full rollout

An explicit pool could guarantee that the same additional replicas remain until all lower ordinals are replaced. It would require stable/surge identity, separate readiness rules, cleanup phases, and more complex recovery semantics. The implemented design instead temporarily changes the expected count and lets ordinary synchronization and deletion policy select resources from current state.

#### Continue using only maxUnavailable

This remains available with `maxSurge: 0`, but it cannot provide create-before-delete updates when no unavailable capacity is acceptable.
