# Binpack scale down

Binpack scaling down is designed to maximize available node capacity, helping the cluster efficiently prepare for upcoming resource-intensive tasks.

## Overview

In Kthena, the Binpack Scale Down feature utilizes the `controller.kubernetes.io/pod-deletion-cost` annotation on each pod to intelligently manage capacity. Kthena evaluates the deletion cost assigned to each group or role based on the value of this annotation, ranks them in order from lowest to highest cost, and selectively removes the group or role with the lowest deletion cost. This targeted approach ensures that cluster resources are freed in a way that minimizes disruption, avoids unnecessary loss of high-priority workloads, and maintains optimal readiness for scaling large AI jobs.

## Preparation

### Prerequisites

- A running Kubernetes cluster with Kthena installed. You can use [Kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/docs/) to quickly set up a local cluster.
- Create a modelServing. The replicas of servingGroup and Role are large than one.

### Getting Started

After all pods managed by modelServing have been successfully deployed, use `kubectl get pod` to view the deployed pods.

```sh
NAMESPACE            NAME                                                  READY   STATUS    RESTARTS   AGE
default              sample-0-decode-0-0                                   1/1     Running   0          5m21s
default              sample-0-decode-1-0                                   1/1     Running   0          5m21s
default              sample-0-prefill-0-0                                  1/1     Running   0          5m21s
default              sample-0-prefill-1-0                                  1/1     Running   0          5m21s
default              sample-1-decode-0-0                                   1/1     Running   0          5m21s
default              sample-1-decode-1-0                                   1/1     Running   0          5m21s
default              sample-1-prefill-0-0                                  1/1     Running   0          5m21s
default              sample-1-prefill-1-0                                  1/1     Running   0          5m21s
```

Kthena's standard scale-down process sorts by pod ID, prioritizing deletion of pods with higher IDs. To differentiate from the original scale-down processing, we should use binpack to delete groups or roles with smaller serial numbers.

Because kthena's binpack functionality relies on `controller.kubernetes.io/pod-deletion-cost`. This annotation can be provided by any third-party component. To demonstrate kthena binpack's scale-down capability as simply as possible in this document, we manually apply this annotation to the pod using the `kubectl edit` command.

```sh
kubectl edit pod sample-1-decode-1-0
```

Because kthena calculates the deletionCost for `Roles` and `ServingGroups` by summing the deletionCost of their respective pods, adding annotations to `sample-1-decode-1-0` will cause `sample-1` to score higher than `sample-0`. At this point, when we change the `ServingGroup` replicas from 2 to 1, we will observe that `sample-0` is deleted.

```sh
kubectl get pod 

NAMESPACE            NAME                                                  READY   STATUS    RESTARTS   AGE
default              sample-1-decode-0-0                                   1/1     Running   0          23h
default              sample-1-decode-1-0                                   1/1     Running   0          23h
default              sample-1-prefill-0-0                                  1/1     Running   0          23h
default              sample-1-prefill-1-0                                  1/1     Running   0          23h
```

Similarly, when we change the replicas for the `decode` Role from 2 to 1, we will also see `decode-0` and being deleted.

```sh
kubectl get pod

NAMESPACE            NAME                                                  READY   STATUS    RESTARTS   AGE
default              sample-1-decode-1-0                                   1/1     Running   0          23h
default              sample-1-prefill-0-0                                  1/1     Running   0          23h
default              sample-1-prefill-1-0                                  1/1     Running   0          23h
```

### Scale up

After binpack scaling down, both the servingGroup Index and role Index will be changed. During scaling up, kthena will locate the largest Index and then incrementally create servingGroups and roles. This way, when performing subsequent deletions, if you opt for sequential deletion instead of binpack scale down, the newly created replicas will be deleted first, followed by the older replicas with longer running times.
