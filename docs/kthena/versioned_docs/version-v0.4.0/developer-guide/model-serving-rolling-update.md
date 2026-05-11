# Rollout Strategy

Rolling updates represent a critical operational strategy for online services aiming to achieve zero downtime. In the context of LLM inference services, the implementation of rolling updates is important to reduce the risk of service unavailability.

Currently, `ModelServing` supports rolling upgrades at the `ServingGroup` level, enabling users to configure `Partitions` to control the rolling process.

- Partition: Indicates the ordinal at which the `ModelServing` should be partitioned for updates. During a rolling update, replicas with an ordinal greater than or equal to `Partition` will be updated. Replicas with an ordinal less than `Partition` will not be updated.

Here's a ModelServing configured with rollout strategy:

```yaml
spec:
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      partition: 0
```

In the following we'll show how rolling update processes for a `ModelServing` with four replicas. Three Replica status are simulated here:

- ✅ Replica has been updated
- ❎ Replica hasn't been updated
- ⏳ Replica is in rolling update

|        | R-0 | R-1 | R-2 | R-3 | Note                                                                          |
|--------|-----|-----|-----|-----|-------------------------------------------------------------------------------|
| Stage1 | ✅   | ✅   | ✅   | ✅   | Before rolling update                                                         |
| Stage2 | ❎   | ❎   | ❎   | ⏳   | Rolling update started, The replica with the highest ordinal (R-3) is updated |
| Stage3 | ❎   | ❎   | ⏳   | ✅   | R-3 is updated. The next replica (R-2) is now being updated                   |
| Stage4 | ❎   | ⏳   | ✅   | ✅   | R-2 is updated. The next replica (R-1) is now being updated                   |
| Stage5 | ⏳   | ✅   | ✅   | ✅   | R-1 is updated. The last replica (R-0) is now being updated                   |
| Stage6 | ✅   | ✅   | ✅   | ✅   | Update completed. All replicas are on the new version                         |

During a rolling upgrade, the controller deletes and rebuilds the replica with the highest sequence number among the replicas need to be updated. The next replica will not be updated until the new replica is running normally.
