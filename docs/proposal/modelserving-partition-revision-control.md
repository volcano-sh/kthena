---
title: ModelServing Partition Revision Control
authors:
- "@FAUST-BENCHOU" # Authors' GitHub accounts here.
reviewers:
- TBD
approvers:
- TBD

creation-date: 2025-01-16

---

## ModelServing Partition Revision Control

<!--
This is the title of your proposal. Keep it short, simple, and descriptive. A good
title can help communicate what the proposal is and should be considered as part of
any review.
-->

### Summary

<!--
This section is incredibly important for producing high-quality, user-focused
documentation such as release notes or a development roadmap.

A good summary is probably at least a paragraph in length.
-->

This proposal introduces partition-aware revision control for `ModelServing` scaling and rolling update operations. When a `partition` is configured in the rollout strategy, the system will intelligently fill missing ordinals within the partition range during scale-up operations and respect partition boundaries during rolling updates. This ensures that partition-protected replicas maintain their current revision while allowing controlled updates to replicas outside the partition range.

Key improvements:
- **Scale-up with partition**: When partition is set, missing ordinals in `[0, partition)` are filled using `CurrentRevision` instead of always creating new replicas at the end.
- **Rolling update with partition**: When partition is set, only replicas with `ordinal >= partition` are updated, protecting lower-ordinal replicas from updates.
- **Revision management**: Proper creation of `ControllerRevision` and status updates (`UpdateRevision`) when scaling up with new revisions.

### Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this proposal.  Describe why the change is important and the benefits to users.
-->

In production environments, users often need to maintain a subset of replicas at a stable version while gradually rolling out updates to other replicas. The current implementation has limitations:

1. **Scale-up gap issue**: When scaling up with partition set, if some replicas in `[0, partition)` are missing (e.g., due to deletion or failure), the system would create new replicas at the end (maxOrdinal + 1) instead of filling the gaps, breaking the partition semantics.

2. **Rolling update boundary**: The rolling update logic needs to respect partition boundaries to ensure that replicas below the partition threshold are not updated.

3. **Revision tracking**: When scaling up with new revisions, proper `ControllerRevision` creation and status updates are needed to track the update state correctly.

#### Goals

<!--
List the specific goals of the proposal. What is it trying to achieve? How will we
know that this has succeeded?
-->

- Support partition-aware scale-up that fills missing ordinals in `[0, partition)` using `CurrentRevision`.
- Ensure rolling updates respect partition boundaries by only updating replicas with `ordinal >= partition`.
- Properly create `ControllerRevision` and update `status.UpdateRevision` when scaling up with new revisions.
- Maintain backward compatibility: when partition is not set, fall back to existing behavior.

#### Non-Goals

<!--
What is out of scope for this proposal? Listing non-goals helps to focus discussion
and make progress.
-->

- This proposal does not change the partition semantics itself, only how scaling and rolling updates interact with partitions.

### Proposal

<!--
This is where we get down to the specifics of what the proposal actually is.
This should have enough detail that reviewers can understand exactly what
you're proposing, but should not include things like API designs or
implementation. What is the desired outcome and how do we measure success?.
The "Design Details" section below is for the real
nitty-gritty.
-->

#### User Stories (Optional)

<!--
Detail the things that people will be able to do if this proposal is implemented.
Include as much detail as possible so that people can understand the "how" of
the system. The goal here is to make this feel real for users without getting
bogged down.
-->

##### Story 1: Scale-up with Partition Gap Filling

A user has a `ModelServing` with `partition=3` and `replicas=5`. Initially, replicas R-0, R-1, R-2 exist. After R-1 is deleted (e.g., due to node failure), the user scales up to `replicas=5`. With this proposal, the system will:
1. Detect that R-1 is missing in `[0, 3)`
2. Recreate R-1 using `CurrentRevision` (not the new revision)
3. Then create R-3 and R-4 using the new revision

This ensures partition semantics are maintained: R-0, R-1, R-2 remain on the old revision, while R-3, R-4 use the new revision.

##### Story 2: Rolling Update with Partition Protection

A user has a `ModelServing` with `partition=2` and 5 replicas (R-0 to R-4). When a rolling update is triggered:
- R-0 and R-1 (ordinal < partition) are protected and remain on `CurrentRevision`
- R-2, R-3, R-4 (ordinal >= partition) are updated to the new revision

This allows gradual rollout while keeping critical low-ordinal replicas stable.

#### Notes/Constraints/Caveats (Optional)

<!--
What are the caveats to the proposal?
What are some important details that didn't come across above?
Go in to as much detail as necessary here.
This might be a good place to talk about core concepts and how they relate.
-->


#### Risks and Mitigations

<!--
What are the risks of this proposal, and how do we mitigate?

How will security be reviewed, and by whom?

How will UX be reviewed, and by whom?

Consider including folks who also work outside the SIG or subproject.
-->

### Design Details

<!--
This section should contain enough information that the specifics of your
change are understandable. This may include API specs (though not always
required) or even code snippets. If there's any ambiguity about HOW your
proposal will be implemented, this is the place to discuss them.
-->

#### Scale-up Logic with Partition Support

In `scaleUpServingGroups`, the logic is enhanced as follows:

1. **Detect partition value**: Read from `ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition`.

2. **Fill missing ordinals in partition range**: If `partition > 0`, iterate through `[0, partition)` and create missing `ServingGroups` using `CurrentRevision`:
   - Use `CurrentRevision` from `ms.Status.CurrentRevision` if available
   - Retrieve template from `ControllerRevision` if it exists, otherwise use `ms.Spec.Template.Roles`
   - This ensures partition-protected replicas maintain their revision even when recreated

3. **Create new replicas beyond partition**: After filling partition gaps, create additional replicas starting from `maxOrdinal + 1` using the new revision:
   - Create `ControllerRevision` for the new revision
   - Update `status.UpdateRevision` to track the new revision being applied

4. **Fallback behavior**: If `partition` is not set or `partition=0`, use the original logic (create replicas from `maxOrdinal + 1`).

```go
if partition > 0 {
    // Fill missing ordinals in [0, partition) using CurrentRevision
    for ordinal := 0; ordinal < partition && ordinal < expectedCount; ordinal++ {
        if !existingOrdinals[ordinal] {
            revisionToUse := ms.Status.CurrentRevision
            // Create ServingGroup with CurrentRevision
            createServingGroup(ordinal, revisionToUse, rolesFromRevision)
        }
    }
}

// Create new ServingGroups beyond partition with newRevision
if toCreate > 0 {
    createControllerRevision(newRevision)
    for i := maxOrdinal + 1; i < maxOrdinal + 1 + toCreate; i++ {
        createServingGroup(i, newRevision, ms.Spec.Template.Roles)
    }
}
```

#### Rolling Update Logic with Partition Support

In `manageServingGroupRollingUpdate`, the logic is enhanced as follows:

1. **Detect partition value**: Read from `ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition`.

2. **Partition-aware deletion**: If `partition > 0`:
   - Iterate through `servingGroupList` in reverse order (highest ordinal first)
   - Skip replicas with `ordinal < partition` (protected)
   - Delete replicas with `ordinal >= partition` that are outdated
   - Stop when reaching protected replicas (`ordinal < partition`)

3. **Fallback behavior**: If `partition` is not set, use the original logic (update from highest ordinal downward).

```go
if partition > 0 {
    // Delete ServingGroups with ordinal >= partition
    for i := len(servingGroupList) - 1; i >= 0; i-- {
        _, ordinal := getOrdinal(servingGroupList[i].Name)
        if ordinal < partition {
            break // Skip partition-protected ServingGroups
        }
        if isOutdated(servingGroupList[i], revision) {
            deleteServingGroup(servingGroupList[i].Name)
            return
        }
    }
} else {
    // Original behavior: update from highest ordinal
    // ...
}
```

#### Partition Gap Filling Example

The following example demonstrates how partition gap filling works during scale-up operations.

**Scenario**: `ModelServing` with `partition=3`, `replicas=5`. Initially, R-0, R-1, R-2 exist. R-1 is deleted, then scale-up to `replicas=5` is triggered.

**Before modification (original behavior)**:
- Missing ordinals in `[0, partition)` are not filled
- New replicas are created at the end (maxOrdinal + 1)
- Partition semantics are broken

|        | R-0 | R-1 | R-2 | R-3 | R-4 | Note                                                                          |
|--------|-----|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅   | ✅   | ✅   | | | Initial state (partition=3) |
| Stage2 | ✅   | | ✅   | | | R-1 deleted (e.g., node failure) |
| Stage3 | ✅   | | ✅   | ⏳ | | Scale up to 5 replicas. R-3 created (maxOrdinal + 1) |
| Stage4 | ✅   | | ✅   | ✅   | ⏳ | R-4 created |
| Stage5 | ✅   | | ✅   | ✅   | ✅ | Scale-up complete, but R-1 gap remains |

**After modification (partition-aware)**:
- Missing ordinals in `[0, partition)` are filled first using `CurrentRevision`
- Then new replicas are created beyond partition using new revision
- Partition semantics are maintained

|        | R-0 | R-1 | R-2 | R-3 | R-4 | Note                                                                          |
|--------|-----|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅   | ✅   | ✅   | | | Initial state (partition=3) |
| Stage2 | ✅   | | ✅   | | | R-1 deleted (e.g., node failure) |
| Stage3 | ✅   | ⏳   | ✅   | | | Scale up to 5 replicas. First fill gap: R-1 recreated with CurrentRevision |
| Stage4 | ✅   | ✅   | ✅   | ⏳ | | After R-1 recreated. Now create R-3 with new revision |
| Stage5 | ✅   | ✅   | ✅   | ✅   | ⏳ | After R-3 created. Now create R-4 with new revision |
| Stage6 | ✅   | ✅   | ✅   | ✅   | ✅ | Scale-up complete. R-0, R-1, R-2 on CurrentRevision; R-3, R-4 on new revision |

**Legend**:
- ✅ Replica exists and running
- ⏳ Replica is being created
- (empty) Replica does not exist

#### Test Plan

<!--
**Note:** *Not required until targeted at a release.*

Consider the following in developing a test plan for this enhancement:
- Will there be e2e and integration tests, in addition to unit tests?
- How will it be tested in isolation vs with other components?

No need to outline all test cases, just the general strategy. Anything
that would count as tricky in the implementation, and anything particularly
challenging to test, should be called out.

-->


### Alternatives

<!--
What other approaches did you consider, and why did you rule them out? These do
not need to be as detailed as the proposal, but should include enough
information to express the idea and why it was not acceptable.
-->


<!--
Note: This is a simplified version of kubernetes enhancement proposal template.
https://github.com/kubernetes/enhancements/tree/3317d4cb548c396a430d1c1ac6625226018adf6a/keps/NNNN-kep-template
-->
