---
title: Report Unschedulable ServingGroups in ModelServing Status
authors:
- "@hzxuzhonghu"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-11-20

---

## Report Unschedulable ServingGroups in ModelServing Status

### Summary

Add two fields to `ModelServing.status`: `unschedulableReplicas`, the number of ServingGroups the scheduler cannot place, and `unschedulableSince`, the time since which the oldest of them has been unschedulable. A multi-cluster rescheduler, such as the Karmada descheduler, reads them to move stuck replicas to another cluster. Scale-down also removes unschedulable ServingGroups first.

### Motivation

When Karmada spreads a ModelServing over several clusters, it divides `spec.replicas` from an estimate of the free resources in each cluster. The estimate does not know that a ServingGroup is several Pods that must be scheduled together, and it does not see network topology constraints or queue quota. So a cluster can be given more ServingGroups than it can host. They stay unschedulable while another cluster has idle GPUs.

The Karmada [descheduler](https://karmada.io/docs/userguide/scheduling/descheduler) handles this for Deployments. It lowers the replicas of a cluster by the number that have been unschedulable for longer than a threshold, and the scheduler places them somewhere else. It gets that number by listing the Deployment's Pods in the member cluster, which it cannot do for ModelServing. The status it collects does not help either: a ServingGroup that is loading a checkpoint and one that fits on no node are both just "not available". Moving the first kind throws away minutes of model loading, so `replicas - availableReplicas` cannot be used.

#### Goals

- Status tells how many ServingGroups wait for a node and since when.
- Lowering `spec.replicas` by `unschedulableReplicas` removes the unschedulable groups, not the ones that are starting or running.
- Works with Volcano and the default scheduler, with no new configuration.

#### Non-Goals

- The rescheduler itself and the Karmada resource interpreter for ModelServing.
- Groups that are not ready for other reasons, such as image pull errors, crash loops or slow model loading. Another cluster does not fix those.
- A waiting threshold in kthena. The rescheduler owns it.

### Proposal

```yaml
status:
  replicas: 5
  availableReplicas: 3
  unschedulableReplicas: 2
  unschedulableSince: "2026-11-20T06:40:00Z"
```

Two ServingGroups cannot be scheduled, and the one that has waited longest has been waiting since 06:40. A rescheduler with a 5 minute threshold lowers this cluster from 5 to 3 replicas after 06:45. The controller deletes the two unschedulable groups, and Karmada places 2 replicas in a cluster that has room.

#### Notes/Constraints/Caveats (Optional)

- `unschedulableReplicas` reflects `PodScheduled=False` with reason `Unschedulable`, matching Kubernetes scheduling conditions and Karmada's unschedulable replica terminology. It intentionally does not count scheduled Pods that are still in Pod phase `Pending` (for example pulling an image or running init containers).
- `unschedulableSince` belongs to the oldest unschedulable group. A group that became unschedulable later can be moved before it has waited for the full threshold. Groups of one ModelServing are identical, so if the oldest one found no room for that long, a newer one will not either.
- Kubernetes `UpdatePodCondition` only updates `lastTransitionTime` when the condition `status` changes. If a Pod transitions from `PodScheduled=False` with reason `SchedulingGated` (or `SchedulerError`) directly to `PodScheduled=False` with reason `Unschedulable`, `lastTransitionTime` is preserved from when `PodScheduled` first became `False` rather than when the reason changed. This matches Karmada's built-in estimator behavior.

#### Risks and Mitigations

- Among groups that are not ready, an unschedulable group is now deleted before one that is starting, whatever their pod deletion cost. Running groups still go last.
- Status is written only when the number of unschedulable groups or the oldest time changes. A scheduler that rewrites the message of a Pod condition does not cause a write.

### Design Details

```go
// UnschedulableReplicas is the number of ServingGroups that wait for a node: the
// scheduler reported at least one of their Pods as unschedulable. They are
// never counted in AvailableReplicas.
// +optional
UnschedulableReplicas int32 `json:"unschedulableReplicas,omitempty"`

// UnschedulableSince is the time since which the oldest ServingGroup counted in
// UnschedulableReplicas has been unschedulable. It is recomputed on every sync
// and is unset when UnschedulableReplicas is 0.
// +optional
UnschedulableSince *metav1.Time `json:"unschedulableSince,omitempty"`
```

How the values are computed:

- A Pod is unschedulable when its `PodScheduled` condition is `False` with reason `Unschedulable`. kube-scheduler and Volcano both report it this way, and the Karmada estimator uses the same test for Deployment Pods. A Pod held by a scheduling gate or hit by a scheduler error is not unschedulable. If `lastTransitionTime` is unset (zero), the controller falls back to a non-zero `pod.CreationTimestamp` and avoids writing a zero timestamp into `unschedulableSince`.
- A ServingGroup is unschedulable when it is not being deleted and at least one of its owned Pods that is not terminating is unschedulable. One Pod is enough: when Volcano cannot satisfy the gang, it marks the Pods that would fit as `Schedulable` and keeps them waiting.
- A group has been unschedulable since the earliest non-zero timestamp of those conditions. `unschedulableSince` is the earliest such time over all unschedulable groups, so it moves to the next oldest group when the oldest one is scheduled or deleted.

Controller changes:

- `updateModelServingStatusInRound` already visits every ServingGroup. For a group that is not available it looks at the group's owned Pods in the informer cache. Nothing new is stored, so the values survive a controller restart.
- `updatePod` enqueues the ModelServing when a Pod becomes unschedulable, stops being unschedulable, its timestamp changes, or an unschedulable Pod starts terminating. Today a Pending Pod does not trigger a sync.
- `scaleDownServingGroups` sorts unschedulable groups (evaluated using owned Pods) in front of the other groups that are not ready.

Karmada copies the whole `.status` of a CRD without a custom interpreter into `ResourceBinding.status.aggregatedStatus`, so the fields reach the control plane as they are. A rescheduler uses `availableReplicas` as the ready count, and `unschedulableReplicas` as the unschedulable count once `now - unschedulableSince` reaches its threshold.

#### Test Plan

Table-driven unit tests for the Pod test (including zero `lastTransitionTime` fallback), for which groups are unschedulable and the value of `unschedulableSince` (gang with mixed Pods, deleting groups, terminating Pods, scheduling gates, the oldest group being scheduled), for the Pod update that triggers a sync (including an unschedulable Pod starting termination), and for the scale-down order (including ignoring foreign-owned Pods).

### Alternatives

- Teach the Karmada estimator to list ModelServing Pods. It would copy kthena's Pod grouping into Karmada.
- Report only groups that have been unschedulable for longer than a threshold set in kthena. The threshold is a rescheduling policy and would have to be configured in every member cluster.
- Report the time of the newest unschedulable group. No group would be moved too early, but the wait starts again whenever one more group becomes unschedulable.
- Read the Volcano PodGroup phase. It only works with Volcano.
- Name the fields `pendingReplicas` / `pendingSince`. Rejected because `Pending` in Kubernetes Pod phase also includes scheduled Pods pulling images or running init containers, which would confuse users and diverge from `PodReasonUnschedulable` and Karmada's `UnschedulableReplicas`.
