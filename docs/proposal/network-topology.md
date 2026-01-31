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
      labelSelector:
        modelserving.volcano.sh/name: sample
        modelserving.volcano.sh/role: prefill
      matchLabelKeys:
        modelserving.volcano.sh/role-id
      networkTopology:
        mode: hard 
        highestTierAllowed: 1
```

A `subGroupPolicy` has been added to the `podGroup` to ensure task-level gang scheduling and network topology.

- `subGroupSize`: The number of pods in a subGroup.
- `minSubGroups`: The minimum replicas of subGroups.
- `matchLabelKeys`: The label key used to match pods.
- `networkTopology`: The network topology of a subGroup.

This YAML specifies that the PodGroup requires six pods to be deployed together, and all six pods must be scheduled on nodes with a maximum network topology distance of 2. Among these, the six pods are divided into two subGroups of three based on the value of the `volcano.sh/task-subgroup-id` label, and each group must be strictly scheduled on nodes with a network topology distance of 1.

**Note:** You can refer to [network topology](https://volcano.sh/en/docs/network_topology_aware_scheduling/) know more about network topology details.

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
      labelSelector:
        modelserving.volcano.sh/name: sample
        modelserving.volcano.sh/role: prefill
      matchLabelKeys:
        modelserving.volcano.sh/role-id
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

Therefore, we still provide the `NetworkTopologySpec` within the `ServingGroup`, where you can configure both the ServingGroup-level network topology and the role-level network topology.

```go
// ServingGroup is the smallest unit to complete the inference task
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.gangPolicy) || has(self.gangPolicy)", message="gangPolicy is required once set"
type ServingGroup struct {
    // RestartGracePeriodSeconds defines the grace time for the controller to rebuild the ServingGroup when an error occurs
    // Defaults to 0 (ServingGroup will be rebuilt immediately after an error)
    // +optional
    // +kubebuilder:default=0
    RestartGracePeriodSeconds *int64 `json:"restartGracePeriodSeconds,omitempty"`

    // GangPolicy defines the gang scheduler config.
    // +optional
    GangPolicy *GangPolicy `json:"gangPolicy,omitempty"`

    // NetworkTopology defines the network topology affinity scheduling policy for the roles of the group, it works only when the scheduler supports network topology feature (e.g. volcano).  
    // +optional
    NetworkTopology *NetworkTopology `json:"networkTopology,omitempty"`

    // +kubebuilder:validation:MaxItems=4
    // +kubebuilder:validation:MinItems=1
    // +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, y.name == x.name))", message="roles name must be unique"
    Roles []Role `json:"roles"`
}

// NetworkTopologySpec defines the network topology affinity scheduling policy for the roles and group, it works only when the scheduler supports network topology feature.
type NetworkTopology struct {
    // GroupPolicy defines the network topology of the ServingGroup.
    GroupPolicy *volcanoV1Beta1.NetworkTopologySpec `json:"groupPolicy,omitempty"`

    // RolePolicy defines the network topology of the role.
    RolePolicy *volcanoV1Beta1.NetworkTopologySpec `json:"rolePolicy,omitempty"`
}
```

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: sample
  namespace: default
spec:
  schedulerName: volcano
  replicas: 1  # servingGroup replicas
  template:
    gangPolicy:
      minRoleReplicas:
        prefill: 2
        decode: 1
    networkTopology:
      groupPolicy:
        mode: hard
        highestTierAllowed: 2
      rolePolicy:
        mode: hard
        highestTierAllowed: 1
    restartGracePeriodSeconds: 60
    roles:
      - name: prefill
        replicas: 2
        # ......
      - name: decode
        replicas: 1  # role replicas, for example, 1P1D
        # ......
``

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