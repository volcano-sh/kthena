# ModelServing Scaling

In cloud-native infrastructure projects, scaling plays a crucial role in resource optimization and cost control, enhancing service availability and quickly response, and simplifying operations management.

In modelServing, as has two layers of resource descriptions, `ServingGroup` and `Role`. Therefore, we also support the scale up and scale down of the `ServingGroup level` and `role level`.

## ServingGroup Scaling

When the `ModelServing.Replicas` is modified, it triggers the Scaling of the `ServingGroup` granularity.

When scaling is triggered, the status of the entire `ServingGroup` is set to `Creating` or `Deleting`, and then the `ServingGroup` creation or deletion process is performed.

After the `replicas` of `ServingGroups` meets expectations. Then update the status of the ServingGroup based on the status of all the pods in the ServingGroup.

`ServingGroups` use deterministic ordinal-based identities similar to StatefulSet Pods. When scaling up, the controller creates only missing ordinals in `[0, replicas)`, in ascending order. For example, scaling from the contiguous set `G-0`, `G-1` to four replicas creates `G-2` and `G-3`. If the existing set is `G-0`, `G-2`, and `G-3`, scaling to four replicas recreates `G-1` instead of appending `G-4`.

A `ServingGroup` that is being deleted continues to reserve its ordinal in the controller datastore until all of its resources are gone. This prevents a replacement with the same deterministic name from being created too early. After deletion is observed and the datastore entry is removed, a later reconciliation can reuse that ordinal. Existing out-of-range ordinals from an older controller version are not proactively renumbered; the invariant applies to newly created `ServingGroups`.

Scale-down selection continues to consider readiness and deletion cost, with the ordinal used as a tie-breaker. It therefore does not always remove the highest ordinal first.

### ServingGroup Scaling Process

In the following we'll show how scaling processes for a `ServingGroup` with four replicas. Three Replica status are simulated here:

- ✅ Replica has been processed and completed.
- ❎ Replica hasn't been processed.
- ⏳ Replica is in scaling
- (empty) Replica does not exist

**Scaling up:**

| | G-0 | G-1 | G-2 | G-3 | Note |
| --- | --- | --- | --- | --- | --- |
| Stage1 | ✅ | ✅ | ✅ | | Before scaling up |
| Stage2 | ✅ | ✅ | ✅ | ⏳ | Scaling up started; the missing ordinal G-3 is being created |
| Stage3 | ✅ | ✅ | ✅ | ✅ | After scaling up |

**Scaling Down:**

| | G-0 | G-1 | G-2 | G-3 | Note |
| --- | --- | --- | --- | --- | --- |
| Stage1 | ✅ | ✅ | ✅ | ✅ | Before scaling down |
| Stage2 | ✅ | ✅ | ✅ | ⏳ | Scaling down started; G-3 is selected for deletion in this example |
| Stage3 | ✅ | ✅ | ✅ | | After scaling down |

## Role Scaling

With the rapid development of LLM inference technology, PD-disaggregates inference has gradually become a common architectural pattern. In this architecture, the `P instances` handle the model's Prefill stage, while the `D instances` handle the model's Decode stage.

PD-separated deployment can reduce system latency in LLM inference scenarios. However, in practical applications, the number of `P instances` and `D instances` may fluctuate due to business changes. To cope with such load fluctuations, it is especially important to dynamically adjust the number of P and D instances.

Dynamically adjusting the number of instances not only improves resource utilization, but also ensures that the system maintains good performance under high load. Therefore, in order to support flexible adjustment of PD instances, scaling needs to be supported at the role level.

When the `role.Replicas` is modified, it triggers the Scaling of the role granularity.

When scaling is triggered, the status of the entire `ServingGroup` is set to scaling, and then the pod creation or deletion process is performed.

After the replicas of pods meets expectations. Then update the status of the ServingGroup based on the status of all the pods in the `ServingGroup`.

Role instances also use deterministic ordinals. Within each `ServingGroup`, scale-up fills missing Role ordinals in `[0, role.replicas)` instead of appending after the maximum ordinal. A deleting Role reserves its ordinal until its Pods and Services are fully deleted, after which the ordinal can be reused. Scale-down uses readiness and deletion cost before the ordinal tie-breaker.

## Role Scaling Process

Symbol meaning identical to [ServingGroup Scaling Process](#servinggroup-scaling-process)

| | G-0 | G-1 | G-2 | G-3 | Note |
| --- | --- | --- | --- | --- | --- |
| Stage1 | ✅ | ✅ | ✅ | ✅ | Before scaling up/down |
| Stage2 | ⏳ | ❎ | ❎ | ❎ | Scaling has started for one ServingGroup |
| Stage3 | ✅ | ⏳ | ❎ | ❎ | The next ServingGroup is scaling |
| Stage4 | ✅ | ✅ | ⏳ | ❎ | Role replicas in the next ServingGroup are scaling |
| Stage5 | ✅ | ✅ | ✅ | ⏳ | Role replicas in the final ServingGroup are scaling |
| Stage6 | ✅ | ✅ | ✅ | ✅ | Scale completed |
