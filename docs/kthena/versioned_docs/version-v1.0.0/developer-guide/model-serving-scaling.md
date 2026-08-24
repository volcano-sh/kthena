# ModelServing Scaling

In cloud-native infrastructure projects, scaling plays a crucial role in resource optimization and cost control, enhancing service availability and quickly response, and simplifying operations management.

In modelServing, as has two layers of resource descriptions, `ServingGroup` and `Role`. Therefore, we also support the scale up and scale down of the `ServingGroup level` and `role level`.

## ServingGroup Scaling

When the `ModelServing.Replicas` is modified, it triggers the Scaling of the `ServingGroup` granularity.

When scaling is triggered, the status of the entire `ServingGroup` is set to `Creating` or `Deleting`, and then the `ServingGroup` creation or deletion process is performed.

After the `replicas` of `ServingGroups` meets expectations. Then update the status of the ServingGroup based on the status of all the pods in the ServingGroup.

Because `ServingGroups` are ordered in modelServing similar to statefulSet. Therefore, the expansion and contraction of the `ServingGroup level` are processed from the last ServingGroup. For example, when `replicas` increase from 2 to 4, first create `G-2`, then create `G-3`. When `replicas` reduces from 4 to 2, `G-3` is deleted first, followed by `G-2`.

### ServingGroup Scaling Process

In the following we'll show how scaling processes for a `ServingGroup` with four replicas. Three Replica status are simulated here:

- ✅ Replica has been processed and completed.
- ❎ Replica hasn't been processed.
- ⏳ Replica is in scaling
- (empty) Replica does not exist

**Scaling up:**

|        | G-0 | G-1 | G-2 | G-3 | Note                                                                          |
|--------|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅  | ✅   | ✅   | | Before Scaling up |
| Stage2 | ✅  | ✅   | ✅   | ⏳   | Scaling up started, The replica with the highest ordinal (G-3) is creating |
| Stage3 | ✅   | ✅   | ✅   | ✅   | After Scaling update |

**Scaling Down:**

|        | G-0 | G-1 | G-2 | G-3 | Note                                                                          |
|--------|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅   | ✅   | ✅   | ✅   | Before Scaling update |
| Stage2 | ✅   | ✅   | ✅   | ⏳   | Scaling down started, The replica with the highest ordinal (G-3) is deleting |
| Stage3 | ✅   | ✅   | ✅   | | After Scaling down |

## Role Scaling

With the rapid development of LLM inference technology, PD-disaggregates inference has gradually become a common architectural pattern. In this architecture, the `P instances` handle the model's Prefill stage, while the `D instances` handle the model's Decode stage.

PD-separated deployment can reduce system latency in LLM inference scenarios. However, in practical applications, the number of `P instances` and `D instances` may fluctuate due to business changes. To cope with such load fluctuations, it is especially important to dynamically adjust the number of P and D instances.

Dynamically adjusting the number of instances not only improves resource utilization, but also ensures that the system maintains good performance under high load. Therefore, in order to support flexible adjustment of PD instances, scaling needs to be supported at the role level.

When the `role.Replicas` is modified, it triggers the Scaling of the role granularity.

When scaling is triggered, the status of the entire `ServingGroup` is set to scaling, and then the pod creation or deletion process is performed.

After the replicas of pods meets expectations. Then update the status of the ServingGroup based on the status of all the pods in the `ServingGroup`.

And because the pods in the role are carrying sequential and labeled. All scaling is processed from the last pod.

## Role Scaling Process

Symbol meaning identical to [ServingGroup Scaling Process](#servinggroup-scaling-process)

|        | G-0 | G-1 | G-2 | G-3 | Note                                                                          |
|--------|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅   | ✅   | ✅   | ✅   | Before scaling up/down                                                         |
| Stage2 | ❎   | ❎   | ❎   | ⏳   | Scaling up/down started, The replica with the highest ordinal (G-3) is start. Creating/Deleting roles in the (G-3) |
| Stage3 | ❎   | ❎   | ⏳   | ✅   | G-3 is scaled. The roles in next replica (G-2) is now scaling                    |
| Stage4 | ❎   | ⏳   | ✅   | ✅   | G-2 is scaled. The roles in next replica (G-1) is now scaling                   |
| Stage5 | ⏳   | ✅   | ✅   | ✅   | G-1 is scaled. The roles in last replica (G-0) is now scaling                   |
| Stage6 | ✅   | ✅   | ✅   | ✅   | Scale completed.                         |
