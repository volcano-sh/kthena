# ModelServing Support for LeaderWorkerSet (LWS) API Proposal

## Abstract

This proposal aims to extend the capabilities of Kthena ModelServing to directly watch and handle LeaderWorkerSet (LWS) Custom Resources (CR). In this mode, users do not need to deploy the LWS native controller or any additional LWS services; only the LeaderWorkerSet CRD definition is required in the cluster. Once a user submits an LWS CR, the Model Serving Controller will automatically detect it and convert it into ModelServing internal resources. This allows reusing Kthena's existing orchestration, routing, and scaling capabilities to achieve seamless support for Leader/Worker style workloads.

## Motivation

Many multi-host inference stacks and ecosystem tools have standardized on using LeaderWorkerSet (LWS) as the API for workload description. To reduce migration costs for users and provide more flexible usage, ModelServing should be able to "compatible" with the LWS API. In this way, users can use the standard LWS CRD to define tasks, while underlying Pod management, lifecycle maintenance, and advanced features (such as service routing, auto-scaling) are unified and taken over by Kthena ModelServing.This benefits those lws users from migrating to kthena smoothly.

### Goals

- **LWS API Compatibility**: Allow users to directly submit LeaderWorkerSet CRs, with the Model Serving Controller responsible for taking over and implementing their semantics.
- **No Additional Components**: **No need** to deploy the LWS Controller or other additional services; only the CRD definition is required.
- **Reuse Kthena Capabilities**: By mapping LWS to ModelServing internal resources, automatically gain all Kthena features (e.g., ModelRouter, AutoScaler).
- **One-Way Synchronization**: Only support one-way conversion from LWS CR to ModelServing resources.

### Non-Goals

- Re-implementing logic completely identical to the LWS native controller within Model Serving (our goal is semantic mapping, not behavioral cloning).
- Supporting the reverse export of ModelServing resources as LWS CRs.

## LeaderWorkerSet Integration

### 1. Architecture Modifications

The architecture of the Model Serving Controller will undergo the following modifications to support the LWS API compatibility mode:

- **LWS Listener (Informer)**: Register a listener for `LeaderWorkerSet` CRs when the Controller starts.
- **CRD Dependency**: The LeaderWorkerSet CRD definition must exist in the environment, but **running the LWS Controller is not required**. User-submitted LWS CRs will not be processed by LWS native logic but will be taken over by the Model Serving Controller.
- **One-Way Conversion Engine**: Implement a one-way conversion logic from `LeaderWorkerSet` to `ModelServing` (or directly to the underlying Pod group).

**Workflow:**
1.  **User Action**: The user applies a `LeaderWorkerSet` YAML file in the cluster.
2.  **Event Capture**: The Model Serving Controller captures the Create/Update event of the LWS via the Informer.
3.  **Resource Conversion**: The Controller converts the Spec of the LWS CR into ModelServing's internal data structure (e.g., generating a Shadow ModelServing object, or directly building the ServingGroup/Role model in memory).
4.  **Resource Reconciliation**: The Controller reuses existing Reconcile logic to create and manage underlying Pods and Services.
5.  **Status Backfill**: The Controller aggregates the running status of Pods and backfills it into the Status field of the LWS CR, allowing users to view the status via `kubectl get lws`.

### 2. Feature Implementation Requirements

#### 2.1 LWS Deployment Instructions
- **Zero Additional Services**: Explicitly stated that no additional LWS Controller service needs to be deployed.
- **CRD Definition Only**: Only need to upload the CRD definition file in the environment (`kubectl apply -f lws-crd.yaml`).
- **No Business Conflict**: Since no LWS Controller is running, the user's LWS CR will not trigger any native business logic and will be completely interpreted and executed by the Model Serving Controller.

#### 2.2 Controller Listening Mechanism
- **Listening Source**: The Model Serving Controller is responsible for listening to CR change events of LWS.
- **One-Way Conversion**: Implement the conversion logic of `LWS CR -> Model Serving Resource`.
- **Exclude Reverse Mapping**: Explicitly **do not implement** or support reverse generation of LWS CRs from Model Serving resources. ModelServing is the underlying implementation engine, and LWS is one of the user interfaces.

### 3. Specific Implementation Steps

#### Step 1: Register LWS Informer
During the Controller initialization phase, check if the LeaderWorkerSet CRD exists in the cluster. If it exists, start the Informer for listening.

```go
// Pseudo-code example
if crdExists("leaderworkersets.leaderworkerset.x-k8s.io") {
    ctrl.NewControllerManagedBy(mgr).
        For(&lws.LeaderWorkerSet{}). // Listen to user-submitted LWS
        Complete(r)
}
```

#### Step 2: Implement Conversion Logic (Translation Layer)
Write an adapter to map LWS semantics to ModelServing.

- **Replicas Mapping**: `LWS.Spec.Replicas` -> `ModelServing.Spec.Replicas` (or ServingGroup replica count).
- **Template Mapping**:
    - `LWS.Spec.LeaderWorkerTemplate.LeaderTemplate` -> ModelServing Role (Entry/Leader).
    - `LWS.Spec.LeaderWorkerTemplate.WorkerTemplate` -> ModelServing Role (Worker).
    - `LWS.Spec.LeaderWorkerTemplate.Size` -> Used to calculate the number of Workers in each ServingGroup.

#### Step 3: Status Backfill (Status Sync)
Implement logic to write system running status back to LWS Status.
- Collect Ready status of underlying Pods.
- Calculate `ReadyReplicas`, `Conditions`.
- Update the Status field of the LWS CR.

### 4. Field Mapping Relationships and Conversion Rules

#### 4.1 Spec Mapping (LeaderWorkerSet -> ModelServing)

| LeaderWorkerSet Field (Source) | ModelServing Internal Semantics (Target) | Conversion Description |
|-------------------------------|--------------------------------|----------|
| `metadata.name` | `ModelServing Name` | Mapped to the corresponding ModelServing identifier |
| `spec.replicas` | `spec.replicas` | Defines the number of replicas for ServingGroup |
| `spec.leaderWorkerTemplate.leaderTemplate` | `spec.template.roles[Leader]` | Parsed as Leader role definition. If nil, use workerTemplate as Entry Pod. |
| `spec.leaderWorkerTemplate.workerTemplate` | `spec.template.roles[Worker]` | Parsed as Worker role definition |
| `spec.leaderWorkerTemplate.size` | `Worker Role Replicas` | Used to calculate replica count for Worker Role: `Replicas = Size - 1` (Assuming Leader is 1) |
| `spec.startupPolicy` | `Startup Policy` | Maps startup order policy (e.g., LeaderFirst) |

#### 4.2 Status Mapping (ModelServing -> LeaderWorkerSet)

| ModelServing Internal Status (Source) | LeaderWorkerSet Status (Target) | Conversion Description |
|--------------------------------|-------------------------------|----------|
| `ServingGroup Ready Count` | `status.readyReplicas` | Number of ready groups |
| `ServingGroup Total Count` | `status.replicas` | Number of currently existing groups |
| `Conditions` | `status.conditions` | Aggregated health status (Available, Progressing) |

### 5. Exception Handling Process

1.  **CRD Missing**:
    - If the LWS CRD is not installed in the cluster, the Controller will only log a message at startup and will not enable the LWS listening function, which does not affect existing ModelServing functions.

2.  **Illegal LWS Spec**:
    - If the LWS CR submitted by the user contains configurations not supported by ModelServing (e.g., unsupported advanced topology strategies), report an `InvalidSpec` error via Condition in Status and stop processing the CR.

3.  **Resource Conflict**:
    - Ensure that generated underlying resource names (Pod Name, Service Name) are deterministic and do not conflict with existing resources.
