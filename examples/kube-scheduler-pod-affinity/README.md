# Kube Scheduler Pod Affinity Examples

These examples show how to use native Kubernetes `podAffinity` with `ModelServing`
when `schedulerName` is `default-scheduler`.

They do not add or use a new Kthena topology API. They rely on the labels that
Kthena adds to generated pods:

- `modelserving.volcano.sh/name`
- `modelserving.volcano.sh/group-name`
- `modelserving.volcano.sh/role`
- `modelserving.volcano.sh/role-id`

The example containers use `ghcr.io/llm-d/llm-d-inference-sim:latest` as a
lightweight vLLM-compatible simulator image.

## ServingGroup Scope

File: `servinggroup-host-affinity.yaml`

Pods in the same generated ServingGroup prefer the same node.

```text
Topology key: kubernetes.io/hostname

ModelServing: servinggroup-affinity
|
+-- ServingGroup-0
|   +-- prefill: [entry, worker] -> node-a
|   +-- decode:  [entry, worker] -> node-a
|
+-- ServingGroup-1
    +-- prefill: [entry, worker] -> node-b
    +-- decode:  [entry, worker] -> node-b
```

## Role Scope

File: `role-host-affinity.yaml`

Pods in the same role instance and ServingGroup prefer the same node.

```text
Topology key: kubernetes.io/hostname

ModelServing: role-affinity
|
+-- ServingGroup-0
|   +-- prefill: [entry, worker] -> node-a
|   +-- decode:  [entry, worker] -> node-b
|
+-- ServingGroup-1
    +-- prefill: [entry, worker] -> node-c
    +-- decode:  [entry, worker] -> node-d
```

## ModelServing Scope

File: `modelserving-host-affinity.yaml`

All pods of the ModelServing prefer the same node.

```text
Topology key: kubernetes.io/hostname

ModelServing: modelserving-affinity
|
+-- ServingGroup-0
|   +-- prefill: [entry, worker] -> node-a
|   +-- decode:  [entry, worker] -> node-a
|
+-- ServingGroup-1
    +-- prefill: [entry, worker] -> node-a
    +-- decode:  [entry, worker] -> node-a
```

## Zone Scope

File: `servinggroup-zone-affinity.yaml`

Pods in the same ServingGroup prefer the same zone, but may run on different
nodes in that zone. This example also requires nodes to have the
`topology.kubernetes.io/zone` label so pods are not placed on unlabeled nodes.

```text
Topology key: topology.kubernetes.io/zone

ModelServing: servinggroup-zone-affinity
|
+-- ServingGroup-0
|   +-- prefill: [entry, worker] -> zone-a / node-a
|   +-- decode:  [entry, worker] -> zone-a / node-b
|
+-- ServingGroup-1
    +-- prefill: [entry, worker] -> zone-b / node-c
    +-- decode:  [entry, worker] -> zone-b / node-d
```
