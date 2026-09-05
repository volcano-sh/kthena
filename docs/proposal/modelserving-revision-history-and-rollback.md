---
title: ModelServing Revision History and Rollback
authors:
- "@acsoto"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-08-13

---

## ModelServing Revision History and Rollback

### Summary

This proposal makes ModelServing revisions stable, retained, and usable for
rollback. It aligns revision lifecycle and rollback behavior with Kubernetes
StatefulSet: revision data is immutable, equivalent historical revisions are
reused, and rollback updates the desired spec before following the existing
rolling update flow.

### Motivation

ModelServing already creates `ControllerRevision` objects, but the revision
hash and stored data are built from different inputs. Operational changes such
as Role scaling can therefore keep the same hash while changing the stored
data. In addition, revision numbers do not form a history and completed
revisions are removed.

These behaviors prevent reliable recovery of an earlier desired workload.

#### Goals

- Build the workload hash and `ControllerRevision.Data` from the same data.
- Keep revision data immutable and revision numbers monotonic.
- Retain recent history without deleting revisions used by live workloads.
- Restore a historical ModelServing workload through the existing rollout
  strategies.
- Preserve current capacity and rollout policies during rollback.

#### Non-Goals

- A separate rollback state machine.
- Rollback of an individual Role.
- Guaranteed CLI rollback to legacy revisions.

### Proposal

ModelServing will record the desired workload revision before scale and rollout
reconciliation. This also records template changes when `replicas` is zero.

The CLI will expose revision history and rollback:

```bash
kthena rollout history modelserving <name>
kthena rollout undo modelserving <name> --to-revision=<revision>
```

Omitting `--to-revision`, or setting it to zero, selects the previous revision.
The CLI applies the selected revision to `ModelServing.spec`. The controller
then handles the change as a normal `ServingGroupRollingUpdate` or
`RoleRollingUpdate`.

Both rollout strategies must treat a change to any revisioned field applicable
to a workload as an update. The mechanism used to detect outdated workloads is
an implementation detail of each strategy.

The CLI retries conflicts by reading the latest ModelServing and reapplying the
revision, so concurrent changes to operational fields are not overwritten.

### Design Details

#### Revision Data

`ControllerRevision.Data` will contain a deterministic strategic merge patch
with only explicitly selected workload-defining fields. `BuildRevisionData`
constructs this projection from an allowlist; it must not copy the API object
and remove known operational fields. API defaults and nil/empty values are
normalized before serialization.

| Revisioned fields | Operational fields |
| --- | --- |
| `schedulerName` | ModelServing and Role `replicas` |
| `plugins` | `rolloutStrategy` |
| Role identity | `maxUnavailable` and `partition` |
| Role entry and worker templates | `recoveryPolicy` |
| `workerReplicas` | `restartGracePeriodSeconds` |
| | `gangPolicy` and `networkTopology` |

Roles are identified by name and stored in canonical name order, so reordering
otherwise identical Roles does not create a revision. Plugin order is preserved
because plugin hooks execute in spec order and may mutate the same Pod. All
plugins are revisioned because the current plugin API does not declare whether
an `OnPodCreate` hook mutates the Pod. Role-name lists within a plugin scope are
also canonicalized because their order is not significant.

Revision membership uses replace semantics so applying a revision can remove
Roles and plugins added after that revision. `ApplyRevision` preserves the
operational Role properties described below while restoring the plugin chain
exactly. The serialized patch is used unchanged as
`ControllerRevision.Data.Raw` and as the primary hash input.

`networkTopology` remains operational: changing it updates the PodGroup and
SubGroupPolicy but does not relocate existing Pods. The new policy applies to
subsequent scheduling and recovery. In contrast, `schedulerName` is written to
`PodSpec.schedulerName` and requires Pod replacement to converge.

`ControllerRevision.Data` is never modified after creation. The revision hash
continues to identify workload content and is used in the ControllerRevision
name, ModelServing status, and workload labels.

#### Applying a Revision

Applying a revision restores revisioned fields and preserves operational
fields from the current ModelServing.

- ModelServing replicas remain unchanged.
- A Role present in both specs keeps its current replicas and list position.
- A Role restored from history uses the API default replicas value of `1` and
  is appended in canonical name order after Roles retained from the current
  spec.
- A current Role absent from the target revision is removed.

The resulting spec must still satisfy current API validation. In particular,
an operational `gangPolicy` can prevent removal of a Role referenced by
`minRoleReplicas`; the CLI rejects such a rollback without changing the
ModelServing.

When a workload is created or recovered for revision R, all revisioned fields,
including `schedulerName`, `plugins`, and Role templates, come from R, while
operational fields come from the current ModelServing. This also applies to
partition-protected recovery. Role replicas therefore come from the current
spec, replacing the current behavior that restores them from revision data.

#### Revision Lifecycle

For each desired workload, the controller builds a candidate revision with:

```text
nextRevision = max(history.Revision) + 1
```

It then follows the StatefulSet revision lifecycle:

1. If the candidate equals the latest revision, reuse the latest revision.
2. If it equals an older revision, reuse that ControllerRevision and advance
   its `Revision` to `nextRevision`.
3. Otherwise, create a new ControllerRevision.

Only the numeric `Revision` is updated when an older revision is reused; its
name and Data remain unchanged. For example:

```text
A(revision=1) -> B(revision=2) -> C(revision=3)
rollback to A
A's ControllerRevision is reused with revision=4
```

Hash collisions are handled with a collision count stored in
`ModelServingStatus`:

```go
CollisionCount *int32 `json:"collisionCount,omitempty"`
```

A name collision with different Data increments the collision count and salts
the hash calculation. Existing Data is never overwritten.

#### History Retention

`ModelServingSpec` will add:

```go
// +kubebuilder:default=10
// +kubebuilder:validation:Minimum=0
RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
```

The default is `10`. Zero retains no non-live history, and negative values are
rejected.

The limit applies to non-live history. Revisions referenced by
`CurrentRevision`, `UpdateRevision`, or existing child workloads are live and
do not count toward the limit. Non-live revisions are deleted from oldest to
newest until at most `revisionHistoryLimit` remain.

Revision references required for partition-protected recovery must survive Pod
deletion and controller restart, and remain live until the replacement is
created. GC therefore cannot rely only on existing Pod labels or the
controller's in-memory store.

#### Compatibility

New revisions will carry the annotation
`modelserving.volcano.sh/revision-data-version: "v1"` so they can be
distinguished from the legacy wrapped Role list.

- Legacy revisions remain readable for active rollout and recovery.
- The first reconciliation with the v1 format builds revision data and a hash
  through the normal revision lifecycle. Because this identity differs from a
  legacy revision, upgrading the controller initiates a one-time rollout of an
  otherwise unchanged ModelServing.
- The migration does not persist a compatibility baseline or maintain an alias
  between legacy and v1 revisions. The one-time rollout follows the configured
  rollout strategy, `maxUnavailable`, and `partition`.
- CLI history and rollback only guarantee revisions in the new format.
- Legacy revision Data is not rewritten or renumbered and is removed through
  normal history retention once it is no longer live.

#### Test Plan

The implementation will add unit and end-to-end coverage for revision
lifecycle, rollback, retention, and compatibility behavior.

### Alternatives

#### Append-only rollback history

Creating another ControllerRevision whenever old content becomes desired would
preserve every transition as a separate object. This was rejected because it
duplicates revision data and diverges from the StatefulSet lifecycle. Reusing
an equivalent revision also preserves the existing `<name>-<hash>` identity.

#### Controller-managed rollback state

Adding a rollback request to the API would require separate rollback state and
completion handling. Updating the desired spec through the CLI keeps rollback
declarative and reuses the existing rollout implementation.

#### Zero-rollout legacy migration

Keeping the legacy revision active while recording equivalent v1 data would
require a persistent compatibility baseline or alias and special reconciliation
and GC rules. This was rejected in favor of a one-time rollout through the
normal revision lifecycle.
