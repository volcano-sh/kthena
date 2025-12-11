---
title: Binpack Scale Down
authors:
- "@LiZhencheng9527" # Authors' GitHub accounts here.
reviewers:
- "@robot"
- TBD
approvers:
- "@robot"
- TBD

creation-date: 2025-11-06

---

## Binpack Scale Down

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

When scaling down a `ServingGroup` or `Role`, the binpack score determines which pods should be evicted.
This change will disrupt the existing logic that processes changes to `ServingGroup` and `Role` replicas in descending order by replica ID. This article will also explain how to minimize the impact on the original logic.

- Handling Binpack Scaling for the servingGroup.
- Handling Binpack Scaling for the Role.
- How should PodGroup's update logic adapt to binpacking?

### Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this proposal.  Describe why the change is important and the benefits to users.
-->

Binpack scaling down maximizes available node capacity to prepare for subsequent resource-intensive tasks. This approach addresses specific requirements in AI inference workloads.

#### Goals

<!--
List the specific goals of the proposal. What is it trying to achieve? How will we
know that this has succeeded?
-->

- Supports binpack scale down in specific scenarios without affecting other scaling capabilities.
- In non-binpack scenarios, capacity scaling operations will continue to follow the previous processing logic.
- The PodGroup update logic should adapt to binpacking.

#### Non-Goals

<!--
What is out of scope for this proposal? Listing non-goals helps to focus discussion
and make progress.
-->

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

##### Story 1

##### Story 2

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

Within modelServing, there are two granularity levels for scaling operations: ServingGroup and Role. Both methods ultimately evict a group of pods. Therefore, this proposal outlines how to calculate the Pod Eviction Cost for a group of pods.

In Kubernetes, the `PodDeletionCost` annotation specifies the cost associated with deleting a pod. We utilize Volcano's binpack plugin to update this annotation.

#### Cost Calculation for Deleting Roles and Service Groups

A role contains one entryPod and multiple workerPods. Each Pod annotates its deletion cost via `PodDeletionCost`. Thus, the cost of deleting the role can be obtained by simply summing these values. When `Role` scaling down, calculate the deletion cost for all `Roles` under the `ServingGroup`. Then sort them by score and select the `Roles` to be deleted.

Similar to scaling down at the `Role`, when scaling down a `ServingGroup`, the `PodDeletionCost` values of all Pods within the `ServingGroup` are summed. The `ServingGroup` to be deleted is then selected based on this sum.

```math
roleScore = \sum_{i=1}^{n} PodDeletionCost_{i}
```

```math
servingGroupScore = \sum_{i=1}^{m} roleScore_{i}
```

#### Pod Sequence Number Handling

The current `modelServing` pods operate similarly to `statefulSet`. During scaling, they are processed in ascending order by sequence number. However, in the binpack scale-down process, this processing logic is disrupted. However, during binpack scale-down operations, or when selecting `ServingGroups` or `Roles` for deletion based on scores, the target may not necessarily be the object with the highest serial number. This can disrupt the previously established processing logic.

To ensure maximum compatibility with existing logic, we have implemented this approach.

The logic behind scaling down is as described above. During the scaling up process, the ModelServing Controller will locates the largest index and then creates replicas incrementally.

For example:

|        | R-0 | R-1 | R-2 | R-3 | R-4 | Note                                                                          |
|--------|-----|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅   | ✅   | ✅   | | | Before Scaling update |
| Stage2 | ✅   | ⏳   | ✅   | | | Scaling down started, The replica with the lowest score(R-1) is deleting |
| Stage3 | ✅   | | ✅   | | | After Scaling down |
| Stage4 | ✅   | | ✅ | ⏳ | | Scale up 2 replicas. First create R-3. Then create R-4 |
| Stage5 | ✅   | | ✅ | ✅   | ⏳ | After Scaling up R-3 |
| Stage6 | ✅   | | ✅   | ✅   | ✅ | After Scaling up R-4 |

#### Impact of Binpacking on PodGroups

Kthena supports gang scheduling and network topology scheduling using Volcano's `PodGroup`. For the `PodGroup` to function properly, updates to the `PodGroup` must precede updates to the actual pods.

Previously, by performing scaling operations sequentially, it was possible to update `PodGroup` before pod updates within the cluster, ensuring consistency between the fields in `podGroup` and the actual resource status in the cluster.

However, after enabling binpack support, we cannot determine which `servingGroup` or `Role` will be deleted before the actual scaling down operation. This means that during scaling down, the `PodGroup` update must occur after the scaling process is completed. Fortunately, `PodGroup` does not affect scale-down operations. Therefore, we can implement special handling for scale-down scenarios.

**ServingGroup:**

Handling `ServingGroup` is relatively straightforward. Since `ServingGroup` and `PodGroup` maintain a one-to-one correspondence, once the `ServingGroup` scale-down is complete, you can simply delete the corresponding `PodGroup`. The normal `podGroup manager` processing occurs before the `ServingGroup` scale-down operation. During this podGroup manager processing, when the existing `ServingGroup` count exceeds the expected `ServingGroup` replicas, all existing podGroups are updated to ensure correct podGroup behavior if role scaling occur at the same time.

```go
// Get the exist ServingGroups
servingGroupList, err := m.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))

// Changes to the PodGroup will not affect Pods that have already been deployed.
// During binpack scale down, it is unknown which ServingGroup will be deleted.
// Therefore, return all podGroup names that exist.
// Detection of PodGroups is handled when ServingGroups are deleted.
podGroupNameListlength := max(expectedReplicas, len(servingGroupNameList))
nameList := make([]string, 0, podGroupNameListlength)
for _, group := range servingGroupNameList {
    _, index := utils.GetParentNameAndOrdinal(group.Name)
    if index > podGroupNameListlength-1 {
        nameList = append(nameList, group.Name)
        podGroupNameListlength = podGroupNameListlength - 1
    }
}

for i := 0; i < podGroupNameListlength; i++ {
    nameList = append(nameList, utils.GenerateServingGroupName(mi.GetName(), i))
}

for _, pgName := range nameList {
    // ... Process all podGroups in the nameList afterward ....
}
```

In an upgrade scenario, determine the gap between the existing number of `ServingGroups` and the expected number. Then generate the required `podGroup` names in ascending order of priority.

Since the final output will generate all `ServingGroup` names with an index less than the expected count, we only need to focus on existing `ServingGroups` where the index exceeds the expected count. First, we iterate through all existing `ServingGroups` to identify those with an index greater than the expected count, adding their names to the `nameList`. Then, we increment the expected count by one. Finally, we generate the required `ServingGroup` names.

**Role:**

Role replicas are represented by `MinTaskMember` within `PodGroup`. Since we cannot predict which `Role` will be deleted during binpack scaling down, we temporarily suspend PodGroup updates during scaling operations.

```go
// During scaling operations, podGroup does not affect scaling policies.
// Under the binpack scaling strategy, it is unknown which role replicas will be deleted.
// Therefore, no action is taken during scaling.
// PodGroup will updated after the role completes scaling down.
if len(roleList) > expectReplicas {
    continue
}
```

After each pod deletion completes, a `reconcile` will occurs. Therefore, once all pods requiring deletion within the cluster have been removed, the `len(roleList)` will match the `expectedReplicas`. Then update the MinTaskMember for the corresponding PodGroup. The logic for obtaining the RoleName is consistent with the previous logic for obtaining the ServingGroupName.

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