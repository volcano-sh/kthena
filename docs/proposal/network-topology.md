---
title: Support subGroup network topology
authors:
- "@LiZhencheng9527" # Authors' GitHub accounts here.
reviewers:
- "@robot"
- TBD
approvers:
- "@robot"
- TBD

creation-date: 2025-11-21

---

## Your short, descriptive title

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

This proposal enables Kthena to support a volcano network topology.

### Motivation

<!--
This section is for explicitly listing the motivation, goals, and non-goals of
this proposal.  Describe why the change is important and the benefits to users.
-->

In distributed AI inference, communication latency between nodes directly affects inference efficiency. By being aware of the network topology, frequently communicating tasks can be scheduled onto nodes that are closer in network distance, significantly reducing communication overhead. Since bandwidth varies across different network links, efficient task scheduling can avoid network congestion and fully utilize high-bandwidth links, thereby improving overall data transmission efficiency.

#### Goals

<!--
List the specific goals of the proposal. What is it trying to achieve? How will we
know that this has succeeded?
-->

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

At this stage, Volcano already supports task-level network topology.

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: network-topology-podgroup
spec:
  minMember: 6
  networkTopology:
    mode: hard 
    highestTierAllowed: 2
  subGroupPolicy: 
    - subGroupSize: 3
      minSubGroups: 2
      name: task
      matchPolicy:
        - labelKey: volcano.sh/task-subgroup-id
      networkTopology:
        mode: hard 
        highestTierAllowed: 1
```

A `subGroupPolicy` has been added to the `podGroup` to ensure task-level gang scheduling and network topology.

- `subGroupSize`: The number of pods in a subGroup.
- `minSubGroups`: The minimum replicas of subGroups.
- `matchPolicy`: The label key used to match pods.
- `networkTopology`: The network topology of a subGroup.

`ModelServing` provides a `GangPolicy`, which determines the minimum number of replicas expected for a role. Based on this configuration, we can create `podGroup`.

```yaml
# gangPolicy
gangPolicy:
  minRoleReplicas:
    prefill: 2    # Require at least 2 prefill role replicas
    decode: 1     # Require at least 1 decode role replica

# PodGroup
apiVersion: scheduling.volcano.sh/v1beta1
kind: PodGroup
metadata:
  name: network-topology-podgroup
spec:
  networkTopology:
    mode: hard 
    highestTierAllowed: 2
  minTaskMember:
    "prefill-0": 4    # 1 entry + 3 workers for prefill role replica 0
    "prefill-1": 4    # 1 entry + 3 workers for prefill role replica 1
    "decode-0": 3     # 1 entry + 2 workers for decode role replica 0
    # ... additional role replicas
  minResources:
  subGroupPolicy: 
    - name: network-topology
      matchPolicy:
        - labelKey: "modelserving.volcano.sh/role"
        - labelkey: "modelserving.volcano.sh/role-id"
      networkTopology:
        mode: hard 
        highestTierAllowed: 1
```

Using minTaskMember, specify the number of pods required for 2P instances and 1D instance. When creating a pod, ModelServing will add some labels to it. For example, `modelServing controller` creates a prefill-0 pod, this pod will add the following labels:

```yaml
modelserving.volcano.sh/role: prefill
modelserving.volcano.sh/role-id: prefill-0
```

So we can use the labels "modelserving.volcano.sh/role" and "modelserving.volcano.sh/role-id" to group the pods that need to be deployed.

Therefore, we still provide the `NetworkTopology` within the `ServingGroup` to ensure that the overall `NetworkTopology` of the `ServingGroup` aligns with the `NetworkTopology` of the `Roles`.

Therefore, we still provide the `NetworkTopologySpec` within the `ServingGroup`, where you can configure both the ServingGroup-level network topology and the role-level network topology.

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