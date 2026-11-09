# ModelServing rollout, scaling, and recovery

A `ModelServing` contains ServingGroups. Each group contains Role instances, and each instance has one entry Pod and its configured worker Pods. Use this guide to choose update settings and understand a waiting rollout.

For complete field definitions, see the [workload API reference](../reference/crd/workload.serving.volcano.sh.md). Start a deployment with the [ModelServing quick start](../getting-started/quick-start.md#modelserving).

## Defaults and configuration

| Setting | Default | Purpose |
| --- | --- | --- |
| `spec.replicas` | `1` | Number of ServingGroups |
| `spec.template.roles[].replicas` | `1` | Role instances in each group |
| `spec.schedulerName` | `volcano` | Scheduler for managed Pods |
| `spec.rolloutStrategy.type` | `ServingGroupRollingUpdate` | Unit updated together |
| `spec.rolloutStrategy.updateMethod` | `Recreate` | Replace Pods; eligible image changes can request `InPlace` |
| Active `maxUnavailable` / `maxSurge` / `partition` | `1` / `0` / `0` | Unavailable allowance, extra capacity, and protected instance numbers |
| `spec.recoveryPolicy` | `RoleRecreate` | Unit recreated after a fault |
| `spec.template.restartGracePeriodSeconds` | `0` | Wait before fault recovery |

Counts, budgets, partition, and recovery settings can change when the new configuration passes validation. Role names can be reordered, but cannot be added, removed, or renamed after creation. Plugin configuration and `roleCoordination` are also immutable; create another ModelServing to change them.

The following YAML examples are `spec` fragments. A complete resource also needs the required entry and worker templates.

## Choose the rollout unit

### ServingGroup rolling updates

```yaml
spec:
  replicas: 3
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 1
      maxSurge: 0
      partition: 0
```

The budget counts complete ServingGroups. Changing only prefill can still replace decode in the selected group. Role-level budgets are inactive in this mode. `roleCoordination` is not allowed.

### Role rolling updates

```yaml
spec:
  replicas: 2
  rolloutStrategy:
    type: RoleRollingUpdate
  template:
    roles:
      - name: prefill
        replicas: 3
        maxUnavailable: 1
        maxSurge: 0
        partition: 0
      - name: decode
        replicas: 2
        maxUnavailable: 1
        maxSurge: 0
        partition: 0
```

Each Role has its own budget within each group. Updating only prefill preserves decode Pods. Both groups can replace one prefill instance concurrently. Top-level rollout budgets are inactive. Use recovery policy `RoleRecreate` or `None`; `ServingGroupRecreate` is not allowed in Role mode.

A Role instance is Ready only when its entry and all required workers exist and are Ready. A group is Ready only when every expected instance is Ready. One Ready entry Pod cannot release the allowance for an incomplete instance.

### In-place image updates

`updateMethod: InPlace` requests patches for eligible regular-container image changes. Partition and availability limits still apply. All required members must finish updating before the unit completes. Ineligible changes use recreation and report `InPlaceUpdateFallback`; plugins can also require recreation. For example, do not rely on image updates preserving Pods when using `headless-service`.

`progressDeadlineSeconds` defaults to 1800 and must be at least 60 when set. An unfinished update follows the configured recovery policy after this deadline; restart grace starts at the deadline. `None` or grace `-1` prevents timeout-driven deletion. Expected update restarts do not trigger fault recovery.

## Availability budgets and update order

`N` means the latest desired number of groups, or instances of one Role within one group.

| Setting | Accepted values | Percentage rounding |
| --- | --- | --- |
| `maxUnavailable` | Nonnegative integer or integer percentage from `0%` to `100%` | Down |
| `maxSurge` | Nonnegative integer or integer percentage, including above `100%` | Up |
| `partition` | Nonnegative integer or integer percentage from `0%` to `100%` | Up |

Use integers such as `1` or percentage strings such as `"25%"`. Numeric strings such as `"1"`, fractional percentages, negatives, and overflow are rejected. Active `maxUnavailable` and `partition` cannot exceed `N`, and `N + maxSurge` must fit an int32.

After rounding, unavailable and surge allowances cannot both be zero, even with a fully protected partition. For three replicas, `maxUnavailable: "25%"` rounds to zero and therefore needs positive surge. Inactive budgets, and every budget when `N` is zero, still require valid syntax but are not checked against the replica count.

Choose fixed limits for the smallest size an autoscaler may request. For example, `maxUnavailable: 2` is valid at four replicas but prevents a scale-down request to one replica. Use a compatible limit, or change the budget with the replica count.

Before deleting healthy capacity, the controller needs at least `max(0, N - maxUnavailable)` complete Ready units to remain. Protected and old-version Ready units count. Pending or NotReady surge does not. If an outside failure has already reduced availability, the controller can replace eligible old unavailable units, but cannot spend that failure as permission to delete more healthy units.

For ordinary ServingGroup and independent Role updates, eligible old NotReady units are selected before old Ready units, with higher instance numbers first within each class. Complete replacements release allowance one at a time. A failed instance already using the latest target is not repeatedly recreated by the template rollout; submitting a corrected template makes it eligible again. Coordinated Roles also follow their configured ordering and dependency constraints.

With `maxUnavailable: 0`, provide enough schedulable resources for extra capacity. A surge instance stuck Pending cannot allow the next healthy deletion. An eviction strategy separately controls `pods/eviction` requests; it does not change these rollout limits or protect direct Pod deletes.

## Partition, sparse ordinals, and temporary capacity

Partition protects instance numbers below its value. It uses the number in each instance's name, rather than its position in the current list. With existing instances `{0, 3}` and `partition: 1`, instance 0 is protected and outdated instance 3 can update.

| Situation | Result |
| --- | --- |
| Existing instance below partition | Keep it when the template changes |
| Missing unit below partition | Recreate using the established historical template |
| Missing unit at or above partition | Use the latest target template |
| A target instance becomes protected after raising partition | Keep it; do not roll it back |
| Another template arrives during an unfinished rollout | Use it for eligible work; the unfinished version does not become the historical baseline |

If required history cannot be resolved, affected updates wait and report the problem.

Explicit scale-down can remove protected instances. It selects NotReady instances first, then lower deletion cost, then higher instance numbers. A stable result can have gaps. Scaling `{0, 3}` from two to three adds instance 1 and retains 3; it does not rebuild healthy instances just to make their numbers consecutive.

Temporary extra replicas are removed after sufficient regular capacity is Ready. Scaling up can reuse an eligible temporary instance without recreating its Pods. A high instance number alone does not mean an existing healthy instance is temporary. Older instances without enough recorded information are preserved rather than guessed to be disposable.

Changing `rolloutStrategy.type` during an unfinished update is not supported. The controller keeps reconciling toward the latest configuration, but instance preservation, ordering, availability bounds, and removal of the previous mode's temporary capacity are not guaranteed. Let the update finish before changing the type.

## Scaling and template updates

| Change | Behavior |
| --- | --- |
| ServingGroup or Role replica count | Add or remove instances; retained instances keep their applicable templates |
| `workerReplicas` | Update the instance layout through the rollout budget |
| Entry or effective worker template | Update the selected complete unit |
| Budget, partition, recovery policy, or grace | Change controls without creating a template revision |

Increasing a Role from two instances to three adds capacity. Increasing its workers from one to two changes the layout: old instances retain their old worker count until selected for update.

Role count changes apply to every group without waiting for earlier groups to become Ready. Each affected group is `Scaling` until its new instances are Ready and is reported as progressing, so several groups can become unavailable while the added capacity starts. Protected groups use their historical templates for new instances. When counts and templates change together, the controller adds requested regular capacity before deleting healthy old capacity, then follows the rollout budget.

Setting every Role to zero keeps the ServingGroups without Pods. The groups become Ready under the regular rules, so a pending rollout completes once the old Pods are gone. Later expansion uses the current counts and partition.

## Coordinated Role updates

Configure `roleCoordination` at creation to add progress and dependency constraints within each group. Per-Role budgets still apply independently in each group.

Progress is the fraction of desired regular instances that are complete, Ready, and using the target template. Protected instances stay in the total; temporary surge does not count toward progress. Two target Ready instances out of four desired instances mean 50% progress, even if the other two are protected.

`maxSkew` compares progress percentages. It does not pair instance numbers or promise an atomic change across Roles. Dependencies can require target-version readiness while retaining old capacity needed by old callers. A blocked highest-numbered regular candidate is not skipped merely to update a lower one.

Whole-instance updates and the bounded final-step dependency rule can temporarily exceed the nominal skew. See [rolling update details](../developer-guide/model-serving-rolling-update.md) for coordination constraints and examples.

## Fault recovery and grace

Fault recovery responds to an unhealthy Pod with a restarted container, or a terminal Failed Pod. A readiness probe failure alone reduces available capacity but does not authorize recovery deletion. A Ready Pod's old restart count is not a new fault; unexpected crashes during an in-place update still count.

| `recoveryPolicy` | Unhealthy restart or Failed Pod | Pod already deleted |
| --- | --- | --- |
| `ServingGroupRecreate` | Recreate the complete group after grace | Recover the group without restart grace |
| `RoleRecreate` | Recreate the affected entry and its workers after grace | Recover that instance without restart grace |
| `None` | Do not proactively delete | Refill the missing Pod and keep the others |

`None` permits normal kubelet restarts, missing-Pod refill, scaling, and template updates. It does not make a terminal Failed Pod recover by itself or count failed capacity as available.

| `restartGracePeriodSeconds` | Recovery behavior |
| --- | --- |
| `0` or omitted | Act on a qualifying fault immediately |
| Positive | Wait from the first observation of the continuing fault; keep the Pod if it recovers |
| `-1` | Disable proactive deletion for that fault |

Values below `-1` are rejected. `None` disables proactive deletion for every valid grace value.

A waiting recovery uses the latest policy, grace, and health. Changing to `None` or `-1` cancels a pending action; changing a finite grace changes its deadline. Becoming Ready ends the observation. Re-enabling recovery or observing a new replacement starts a fresh wait. After a controller restart, the wait also starts again; downtime does not consume the grace period.

Refilling one Pod retains its instance's applied template and worker layout. Recreating a Role within a ServingGroup rollout retains that group's template. Recreating the complete rollout unit instead selects the version using the current partition.

Once a group or Role-instance deletion has started, a controller restart finishes deleting the originally selected Pods before replacing them. For example, scaling down a Role under `ServingGroupRecreate` deletes only the selected instance, even if the controller restarts mid-deletion; the rest of the group is not restarted. Other Pods and new replacements with reused names are preserved. Changing recovery policy cannot withdraw an already accepted delete; later replacements use the latest counts and applicable historical template.

## Revisions and rollout status

| Status field | Meaning |
| --- | --- |
| `observedGeneration` | Latest configuration generation observed; does not prove readiness |
| `replicas` | Reported ServingGroup count, including capacity still converging |
| `availableReplicas` | Complete Ready groups, including older versions |
| `updatedReplicas` | Groups matching the target; check readiness separately |
| `currentReplicas` | Groups associated with the established revision |
| `currentRevision` | Established historical baseline |
| `updateRevision` | Target template revision |

The baseline advances after the target capacity is fully Ready, counts have settled, and temporary capacity is resolved. A replacement gap, an unavailable target, or `spec.replicas: 0` cannot establish successful completion. A healthy partition stop can retain old versions, so it differs from a complete rollout to the target.

Condition messages can change without changing `lastTransitionTime`; that time changes when the condition's status changes. See [revision details](../architecture/model-serving-controller.mdx#revisions-and-conservative-rolling-updates) for template comparison and history.

Inspect a waiting rollout with:

```sh
kubectl get modelserving NAME -n NAMESPACE -o yaml
kubectl describe modelserving NAME -n NAMESPACE
kubectl get pods -n NAMESPACE
```

Check entry and worker readiness, scheduling, terminating Pods, partition, required history, and coordination conditions. Resolve the reported cause; an ordinary rollout that keeps the same mode does not bypass its configured budget.
