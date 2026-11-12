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
