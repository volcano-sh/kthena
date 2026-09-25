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
rollback. A `ControllerRevision` stores the versioned workload declaration that
should be restored by rollback. It does not attempt to snapshot every input
that can affect the final rendered Pod.

Rollback restores the versioned workload declaration while preserving current
operational fields and live object metadata such as OwnerReferences. The
proposal does not promise bit-for-bit or field-for-field reproduction of the
Pod that existed when a revision was created.

### Motivation

ModelServing already creates `ControllerRevision` objects, but the revision
hash and stored data are built from different inputs. Operational changes such
as Role scaling can therefore keep the same hash while changing the stored
data. In addition, revision numbers do not form a history and completed
revisions are removed.

These behaviors prevent reliable recovery of an earlier desired workload and
make a small, explicit revision boundary necessary.

#### Goals

- Define revision identity as the versioned user workload declaration.
- Build deterministic revision data and its hash from the same allowlisted
  projection.
- Keep revision data immutable, reuse equivalent revisions, and make revision
  numbers monotonic.
- Retain recent history without deleting revisions used by live workloads.
- Restore a historical workload declaration through the existing rollout
  strategies.
- Preserve current operational values during rollback.

#### Non-Goals

- Bit-for-bit reproduction of a historical Pod.
- Snapshotting every input that can affect final Pod rendering.
- Snapshotting `OwnerReferences` in `ControllerRevision.Data` or revision
  identity.
- A separate rollback state machine.
- Rollback of an individual Role.
- Guaranteed CLI rollback to legacy revisions.

### Proposal

ModelServing records a workload revision before scale and rollout
reconciliation. This also records template changes when `replicas` is zero.

The CLI exposes revision history and rollback:

```bash
kthena rollout history modelserving <name>
kthena rollout undo modelserving <name> --to-revision=<revision>
```

Omitting `--to-revision`, or setting it to zero, selects the previous revision.
The CLI reads the selected `ControllerRevision` and applies its revisioned
fields to `ModelServing.spec`. The controller then handles the resulting change
as a normal `ServingGroupRollingUpdate` or `RoleRollingUpdate`.

Kthena's rollout is revision-gated: `manageRollingUpdate` first identifies
outdated ServingGroups using `ServingGroup.Revision != desiredRevision`, and
Role-level outdated comparison happens after a group enters that path. This
proposal therefore does not add another rendering hash, generation, or
independent semantic-comparison mechanism.

Both rollout strategies must treat a change to any revisioned field applicable
to a workload as an update. Under `RoleRollingUpdate`, the comparison must
include ModelServing-level revisioned fields applicable to the Role, that
Role's revisioned definition, and the applicable plugin chain, using the same
canonical normalization as revision data. The concrete comparison or hashing
mechanism remains an implementation detail.

The CLI retries conflicts by reading the latest ModelServing and reapplying the
revisioned fields, so concurrent changes to operational fields are not
overwritten.

### Design Details

#### Revision Data

`ControllerRevision.Data` contains a deterministic serialized projection of the
revisioned fields. `BuildRevisionData`
constructs this projection from an allowlist; it must not copy the API object
and remove known operational fields. API defaults and nil/empty values are
normalized before serialization.

Revisioned fields are:

```text
- schedulerName
- plugins
- Role identity and membership
- Role entryTemplate
- Role workerTemplate
- workerReplicas
```

The full `PluginSpec` participates in revision identity:

```text
- name
- type
- config
- scope
- plugin list order
```

Plugin configuration is a user-supplied workload declaration, and
`OnPodCreate` may mutate Pods. The current plugin API does not declare which
plugins or configuration fields affect Pod rendering, so this proposal does
not introduce plugin-specific revision metadata.

Canonicalization is required for opaque JSON `config` and unordered
`scope.roles`. Plugin list order is preserved because hook execution order is
semantically significant. Roles are stored in canonical name order, so
reordering otherwise identical Roles does not create a revision.

Under `RoleRollingUpdate`, only plugins applicable to the specific Role
participate in that Role's outdated-workload comparison. A plugin scoped only to
`prefill` must not make `decode` outdated. The applicable plugins retain their
relative order from the ModelServing plugin list.

The serialized patch is used unchanged as `ControllerRevision.Data.Raw` and as
the primary hash input. `ControllerRevision.Data` is immutable after creation.
Changes to any revisioned field, including `schedulerName`, therefore produce
a new desired revision identity or reuse an equivalent historical revision and
enter the normal revision-driven rollout path.

#### Operational Fields

The following fields are operational and are not stored in
`ControllerRevision.Data`:

```text
- ModelServing replicas
- Role replicas
- rolloutStrategy
- partition
- maxUnavailable
- maxSurge
- recoveryPolicy
- restartGracePeriodSeconds
- gangPolicy
- networkTopology
```

They control capacity, rollout progression, recovery, or scheduling policy;
rollback preserves them and they normally affect reconciliation rather than
revision identity.

`networkTopology` remains operational: changing it updates the PodGroup and
SubGroupPolicy but does not relocate existing Pods. The new policy applies to
subsequent scheduling and recovery. In contrast, `schedulerName` is revisioned
and requires the affected Pods to be replaced when it changes.

#### Applying a Revision

Applying revision R restores all revisioned fields and preserves current
operational fields.

Preserve:

```text
- ModelServing replicas
- Role replicas
- rolloutStrategy
- partition
- maxUnavailable
- maxSurge
- recoveryPolicy
- restartGracePeriodSeconds
- gangPolicy
- networkTopology
- live object metadata and OwnerReferences
```

Restore:

```text
- schedulerName
- plugins
- Role membership and identity
- Role entryTemplate and workerTemplate
- workerReplicas
```

For a Role present in both the current spec and revision R, its current
operational replica count and list position are preserved. A restored Role that
is absent from the current spec may use the documented default replica behavior
(currently `1`) and is appended in canonical name order; a current Role absent
from R is removed.

The resulting spec must still satisfy current API validation. In particular,
the current `gangPolicy` can prevent removal of a Role referenced by
`minRoleReplicas`; the CLI rejects such a rollback without changing the
ModelServing.

OwnerReferences are live object-relationship metadata, not part of the
historical workload declaration. They must not be stored in revision data or
included in revision identity, and historical ownership metadata is not
restored. If the `lws-standard-labels` plugin needs ownership information when
creating a Pod, it may use the current ModelServing ownership context.

Partition-protected recovery uses the selected historical revision for the
workload definition, then follows the same rollout and Pod construction path.
Revision references required for this recovery must survive Pod deletion and
controller restart; garbage collection cannot rely only on existing Pod labels
or the controller's in-memory store.

#### Revision Lifecycle

For each desired workload, the controller builds a candidate revision from the
revisioned fields only:

```text
nextRevision = max(history.Revision) + 1
```

It then follows this lifecycle:

1. If the candidate equals the latest revision, reuse the latest revision.
2. If it equals an older revision, reuse that ControllerRevision and advance
   its `Revision` to `nextRevision`.
3. Otherwise, create a new ControllerRevision.

Only the numeric `Revision` is updated when an older revision is reused; its
name and immutable Data remain unchanged. For example:

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

#### Compatibility

New revisions carry the annotation
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
projection and canonicalization, plugin ordering and Role applicability,
revision lifecycle and collision handling, retention and legacy compatibility,
rollback preservation of operational fields, scheduler-driven rollout, and
partition-protected recovery after Pod deletion or controller restart.
