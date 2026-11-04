# Volcano Scheduler

[Volcano](https://volcano.sh) is the default scheduler for Kthena `ModelServing` workloads. It provides gang scheduling, network topology-aware scheduling, and queue-based resource sharing for LLM inference.

For each `ServingGroup`, the ModelServing controller creates a Volcano [`PodGroup`](https://volcano.sh/docs/concepts/podgroup/) with the same name, so that Volcano schedules all pods of the group together.

## Prerequisites

- Install [Volcano](https://volcano.sh/en/docs/installation/) in your cluster. Role-level gang scheduling and role-level network topology require Volcano **1.14** or later.
- Install Kthena by following [Installation](../getting-started/installation.md).

Kthena detects Volcano at runtime, so you can install it before or after Kthena without restarting the controller.

**NOTE:** ModelServings whose pods were created before Volcano was installed are not gang scheduled until their pods are recreated. To avoid this, install Volcano before creating any ModelServing.

## Gang Scheduling

`spec.schedulerName` defaults to `volcano`, so gang scheduling is enabled for every ServingGroup without extra configuration:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: sample
spec:
  schedulerName: volcano
  replicas: 1
  template:
    roles:
      - name: prefill
        # ...
      - name: decode
        # ...
```

To require only a minimum number of replicas per role, for example in prefill/decode disaggregation, set `gangPolicy.minRoleReplicas`:

```yaml
spec:
  template:
    gangPolicy:
      minRoleReplicas:
        prefill: 2
        decode: 1
```

**NOTE:** If `minRoleReplicas` is not specified, Pods from all roles are scheduled together as a gang by default.

Set resource requests on all entry and worker containers. Kthena uses them to compute the PodGroup's `minResources`.

If you set `schedulerName` to any other scheduler, Kthena does not create PodGroups, and the pods are not gang scheduled. If Volcano is not installed, set `schedulerName: default-scheduler`; otherwise, the pods stay `Pending`.

For details, see [Gang Scheduling](../user-guide/gang-scheduling.md).

## Topology Aware Scheduling

Volcano can place pods that communicate frequently under the same network switch to reduce latency. Configure it with `networkTopology`:

```yaml
spec:
  template:
    networkTopology:
      groupPolicy:
        mode: hard
        highestTierAllowed: 2
      rolePolicy:
        mode: hard
        highestTierAllowed: 1
```

- `groupPolicy` applies to all pods in a ServingGroup.
- `rolePolicy` applies to the pods of each role replica.

The cluster topology must be described with Volcano `HyperNode` resources. For details, see [Network Topology](../user-guide/network-topology.md).

## Using Queues for Priority Scheduling

Volcano queues let you share and isolate cluster capacity between workloads. First, create a queue:

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: Queue
metadata:
  name: inference
spec:
  weight: 1
  reclaimable: false
```

Then assign the ModelServing to the queue with the `scheduling.volcano.sh/queue-name` annotation:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: sample
  annotations:
    scheduling.volcano.sh/queue-name: inference
spec:
  schedulerName: volcano
  # ...
```

The annotation must be on the ModelServing itself, not on the pod templates. Without it, Volcano places the PodGroups in the `default` queue.
