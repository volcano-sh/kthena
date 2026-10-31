# Gang Scheduling

Gang Scheduling is a critical feature for Deep Learning workloads to enable all-or-nothing scheduling capability. Gang Scheduling avoids resource inefficiency and scheduling deadlock.

Kthena leverages Volcano to implement gang scheduler capabilities. Additionally, actively adapt to Volcano's multi-dimensional gang scheduling capabilities. Implement role-based gang scheduling.

## Overview

In kthena, when deploying models via modelServing, Volcano is employed as the default scheduler. The modelServing controller creates a PodGroup for each ServingGroup. Therefore, by default, users require no additional configuration; once the environment is prepared, they can benefit from gang scheduling for Group.

## Multi-Dimensional Gang Scheduling

Beyond the ServingGroup, Role-level gang scheduling constraints are also of paramount importance in PD-disaggregation scenarios. Whilst ensuring the overall number of pods is deployed simultaneously, it is also necessary to ensure that prefill pods and decode pods are deployed together. This prevents situations where only prefill or decode pods are deployed, thereby avoiding resource wastage.

Kthena uses the newly added `subGroupPolicy` capability in the Volcano PodGroup to enforce gang scheduling constraints on ServingGroup and Role. It should be noted that the volcano version must be greater than or equal to **1.14** to support the `subGroupPolicy` functionality.

```yaml
subGroupPolicy: 
- subGroupSize: 3
  minSubGroups: 2
  name: task
  matchPolicy:
    - labelKey: volcano.sh/task-subgroup-id
```

A `subGroupPolicy` has been added to the `podGroup.Spec` to ensure role-level gang scheduling

- `subGroupSize`: The number of pods in a subGroup.
- `minSubGroups`: The minimum replicas of subGroups.
- `matchPolicy`: The label key used to match pods.

Within modelServing, there is no need to concern oneself with all configurations within the `subGroupPolicy`. One need only configure the minimum number of replicas required for each Role via the `MinRoleReplicas` setting.

```go
type GangPolicy struct {
    // MinRoleReplicas defines the minimum number of replicas required for each role
    // in gang scheduling, pods in each role are strictly gang required.
    // This map allows users to specify different minimum replica requirements for different roles.
    // If this field is not set, all roles in the ServingGroup are considered gang required by default.
    // For example if you specify a 2P(prefill) 4D(decode) serving group and set the below gangPolicy:
    // ```yaml
    // gangPolicy:
    //   minRoleReplicas:
    //     prefill: 1
    //     decode: 1
    // ```
    // It will result in the following behavior:
    // At least one prefill and one decode must be scheduled before any of the pods in the serving group can run.
    // And pods within a role must be scheduled together.
    // +optional
    // +kubebuilder:validation:XValidation:rule="self == oldSelf", message="minRoleReplicas is immutable"
    MinRoleReplicas map[string]int32 `json:"minRoleReplicas,omitempty"`
}
```

## Dynamic Resource Allocation (DRA)

Kthena builds the PodGroup `minResources` from the container
`resources.requests` of each role. Pods that receive devices through
`resourceClaims` do not add a device entry to `resources.requests`, so a
ServingGroup that uses only DRA claims produces a PodGroup with an empty
`minResources` map.

Volcano's default scheduler configuration does not enable the `capacity`
plugin, and the default enqueue plugins evaluate scalar queue resources
only. Without the `capacity` plugin, a DRA-only gang can be admitted at
enqueue time and then partially bound when the cluster does not have
enough devices for every role.

To make the enqueue decision account for DRA devices, enable the
`capacity` plugin and declare a quota for the DeviceClass on the Queue
that the ModelServing uses. The Queue is selected with the
`scheduling.volcano.sh/queue-name` annotation, or `default` when the
annotation is not set. The `capacity` plugin replaces `proportion`, so do
not enable both.

```yaml
# ConfigMap volcano-system/volcano-scheduler-configmap
data:
  volcano-scheduler.conf: |
    actions: "enqueue, allocate, backfill, reclaim"
    tiers:
    - plugins:
      - name: priority
      - name: gang
      - name: conformance
    - plugins:
      - name: drf
      - name: predicates
      - name: capacity
        arguments:
          capacity.DynamicResourceAllocationEnable: true
      - name: nodeorder
```

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: Queue
metadata:
  name: default
spec:
  capability:
    "deviceclass/gpu.nvidia.com": "4"
```

With this configuration, the `capacity` plugin rejects the workload at
enqueue time when the requested DeviceClass count exceeds the Queue
`capability`, so the gang is not admitted.

The `capacity` plugin arguments are optional. When they are not set,
Volcano uses the Kubernetes `DynamicResourceAllocation` feature gate as
the default. The same Queue fields also enforce scalar quotas, so review
the existing `capability`, `deserved`, and `guarantee` values before
enabling it.

For the full quota model, including consumable capacity and shared
claims, see the Volcano
[DeviceClass Quota User Guide](https://github.com/volcano-sh/volcano/blob/master/docs/user-guide/how_to_use_dra_quota.md).

Partial binding with DRA claims is reported in
[#1887](https://github.com/volcano-sh/kthena/issues/1887).

## Preparation

### Prerequisites

- A running Kubernetes cluster with Kthena installed. You can use [Kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/docs/) to quickly set up a local cluster.
- Install Volcano which version is greater than or equal to **1.14**.
- Install Kthena which version is greater than or equal to **0.3.0**.

### Getting Started

1. Deploy model serving with the MinRoleReplicas configuration

```sh
kubectl apply -f examples/model-serving/gang-scheduling.yaml

NAMESPACE            NAME                                               READY   STATUS    RESTARTS   AGE
default              sample-0-decode-0-0                                1/1     Running   0          15s
default              sample-0-decode-0-1                                1/1     Running   0          15s
default              sample-0-decode-1-0                                1/1     Running   0          15s
default              sample-0-decode-1-1                                1/1     Running   0          15s
default              sample-0-prefill-0-0                               1/1     Running   0          15s
default              sample-0-prefill-0-1                               1/1     Running   0          15s
default              sample-0-prefill-1-0                               1/1     Running   0          15s
default              sample-0-prefill-1-1                               1/1     Running   0          15s
```

2. Check the status of PodGroup

```sh
kubectl get podgroup

NAME       STATUS    MINMEMBER   RUNNINGS   AGE
sample-0   Running   6           8          21s

kubectl get podgroup sample-0 -oyaml

## ... another configuration ...
spec:
  minMember: 6
  minResources:
    cpu: 600m
  queue: default
  subGroupPolicy:
  - labelSelector:
      matchLabels:
        modelserving.volcano.sh/name: sample
        modelserving.volcano.sh/role: prefill
    matchLabelKeys:
    - modelserving.volcano.sh/role-id
    minSubGroups: 2
    name: prefill
    subGroupSize: 2
  - labelSelector:
      matchLabels:
        modelserving.volcano.sh/name: sample
        modelserving.volcano.sh/role: decode
    matchLabelKeys:
    - modelserving.volcano.sh/role-id
    minSubGroups: 1
    name: decode
    subGroupSize: 2
## ... another configuration ...
```

When the `minSubGroups` value for the RoleName corresponding to `modelserving.volcano.sh/name` matches the value in `MinRoleReplicas`, this indicates that the configuration is correct.

**NOTE:** If you wish to try your own example, ensure the `resource` is set.

### Cleanup

```bash
kubectl delete modelserving sample
```
