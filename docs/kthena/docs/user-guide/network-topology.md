# Network Topology

In distributed AI inference, communication latency between nodes directly affects inference efficiency. By being aware of the network topology, frequently communicating tasks can be scheduled onto nodes that are closer in network distance, significantly reducing communication overhead. Since bandwidth varies across different network links, efficient task scheduling can avoid network congestion and fully utilize high-bandwidth links, thereby improving overall data transmission efficiency.

## Overview

kthena leverages Volcano to achieve network topology-aware scheduling. To shield the differences in data center network types, Volcano defines a new CRD HyperNode to represent the network topology, providing a standardized API interface. A HyperNode represents a network topology performance domain, typically mapped to a switch or tor. Multiple HyperNodes are connected hierarchically to form a tree structure. For example, the following diagram shows a network topology composed of multiple HyperNodes:

![Network Topology](images/networkTopology.svg)

In this structure, the communication efficiency between nodes depends on the HyperNode hierarchy span between them. For example:

- node0 and node1 belong to s0, achieving the highest communication efficiency.
- node1 and node2 need to cross two layers of HyperNodes (s0→s2→s1), resulting in lower communication efficiency.

Among the structures above, the `Tier` represents the hierarchy of the HyperNode. The lower the tier, the higher the communication efficiency between nodes within the HyperNode. For more details about HyperNode CRD, please refer to the [Volcano Network Topology Aware](https://volcano.sh/en/docs/network_topology_aware_scheduling/) documentation.

Volcano PodGroup can set the topology constraints of the job through the networkTopology field, supporting the following configurations:

- mode: Supports hard and soft modes.
  - hard: Hard constraint, tasks within the job must be deployed within the same HyperNode.
  - soft: Soft constraint, tasks are deployed within the same HyperNode as much as possible.
- highestTierAllowed: Used with hard mode, indicating the highest tier of HyperNode allowed for job deployment. This field is not required when mode is soft.

For example, the following configuration means the job can only be deployed within HyperNodes of tier 1 or lower, such as s0 and s1. Otherwise, the job will remain in the Pending state:

```yaml
spec:
  networkTopology:
    mode: hard
    highestTierAllowed: 1
```

kthena's model serving, provides fields to configure Network Topology constraints for Serving Groups and Roles. For example, the following configuration means the ServingGroup can be deployed within HyperNodes of tier 2 or lower, such as s2. The Role can be deployed within HyperNodes of tier 1 or lower, such as s0 and s1.

```yaml
spec:
  replicas: 1  # servingGroup replicas
  template:
    networkTopology:
      rolePolicy:
        mode: hard
        highestTierAllowed: 1
      groupPolicy:
        mode: hard
        highestTierAllowed: 2
```

### Prerequisites

- A running Kubernetes cluster with Kthena installed.
- Install [Volcano](https://volcano.sh/en/docs/installation/). If you need to experiment with role-based network topology aware scheduling, the Volcano component requires the `latest` image.

### Get Started

1. Create hyperNode resources

We need to create a HyperNode resource to represent the network topology of our local cluster. The cluster I used for the demonstration example is a three-node Kubernetes cluster created by KinD, with the three nodes named `kthena-control-plane`, `kthena-worker`, and `kthena-worker2`.

The `HyperNode` I created is as follows:

```yaml
apiVersion: topology.volcano.sh/v1alpha1
kind: HyperNode
metadata:
  name: s0
spec:
  tier: 1 # s0 is at tier1
  members:
  - type: Node
    selector:
      labelMatch:
        matchLabels:
          kubernetes.io/hostname: kthena-worker
------
apiVersion: topology.volcano.sh/v1alpha1
kind: HyperNode
metadata:
  name: s2
spec:
  tier: 2
  members:
  - type: HyperNode
    selector:
      exactMatch:
        name: "s0"
```

This creates a network topology structure of s2 → s0 → kthena-worker.

2. Create a modelServing instance:

```bash
kubectl apply -f examples/model-serving/network-topology.yaml

# View results
kubectl get podGroup

NAME       STATUS    MINMEMBER   RUNNINGS   AGE
sample-0   Running   4                      2s

kubectl get pod -oyaml | grep nodeName

nodeName: kthena-worker
nodeName: kthena-worker
nodeName: kthena-worker
nodeName: kthena-worker
```

As can be seen, Kthena creates a `PodGroup`, and then Volcano deploys all pods to the compliant `kthena-worker` based on the network topology policy configured within the PodGroup.

If the deployed model serving instance lacks a NetworkTopology policy, the pod will be deployed randomly in either `kthena-worker` or `kthena-worker2`.

```bash
# After deleting the Network Topology configuration
kubectl apply -f examples/model-serving/network-topology.yaml

# view results
kubectl get pod -oyaml | grep nodeName

nodeName: kthena-worker2
nodeName: kthena-worker2
nodeName: kthena-worker2
nodeName: kthena-worker
```

**NOTE:** When using Network Topology Aware Scheduling with your own configuration, ensure that `resources` are specified in `role.entryTemplate` and `role.workerTemplate`.

## Clean up

```bash
kubectl delete modelserving sample

kubectl delete hypernode s0 s2
```
