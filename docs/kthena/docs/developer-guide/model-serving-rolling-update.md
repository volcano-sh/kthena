# Rollout Strategy

Rolling updates represent a critical operational strategy for online services aiming to achieve zero downtime. In the context of LLM inference services, the implementation of rolling updates is important to reduce the risk of service unavailability.

Currently, `ModelServing` supports rolling upgrades at the `ServingGroup` level, enabling users to configure `Partitions` to control the rolling process.

- Partition: Indicates the ordinal at which the `ModelServing` should be partitioned for updates. During a rolling update, replicas with an ordinal greater than or equal to `Partition` will be updated. Replicas with an ordinal less than `Partition` will not be updated.

## What Triggers a Rolling Update

ModelServing automatically triggers a rolling update when you modify certain fields in `spec.template`. The controller uses a revision hash to detect template changes and initiate the rolling update process.

**Rolling updates are triggered by changes to:**

1. **Role configurations** - Container specs, environment variables, resource requests/limits, volumes, etc.
2. **Network topology policies** - `spec.template.networkTopology.groupPolicy` or `spec.template.networkTopology.rolePolicy`
3. **Gang scheduling policies** - `spec.template.gangPolicy` (primarily when adding gang scheduling for the first time)

**Scaling operations (NOT rolling updates):**

- **Role replicas** - Changing `spec.template.roles[*].replicas` is treated as a scaling operation, not a rolling update

### Example: Updating Network Topology

```yaml
# Initial deployment with tier-1 constraint
spec:
  template:
    networkTopology:
      groupPolicy:
        mode: hard
        highestTierAllowed: 1
    roles:
      - name: prefill
        replicas: 1

# Update to tier-2 - triggers rolling update
spec:
  template:
    networkTopology:
      groupPolicy:
        mode: hard
        highestTierAllowed: 2  # Changed!
    roles:
      - name: prefill
        replicas: 1
```

When you apply this change, the controller will:
1. Detect the template change via revision hash comparison
2. Create a new ControllerRevision to track the update
3. Perform a rolling update respecting the `rolloutStrategy` configuration
4. Reschedule pods according to the new topology constraints

For a complete example, see [network-topology-rolling-update.yaml](../assets/examples/model-serving/network-topology-rolling-update.yaml).

## Rollout Strategy Configuration

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
