---
title: ModelServing In-Place Rolling Update
authors:
- "@LiZhenCheng9527" # Authors' GitHub accounts here.
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-07-15

---

## ModelServing In-Place Rolling Update

### Summary

This proposal introduces an `InPlaceRollingUpdate` strategy for `ModelServing`. The strategy updates the images of existing entry and worker Pods by patching `spec.containers[*].image`. Kubernetes then restarts the affected containers in the existing Pods without deleting the Pods or scheduling replacements. Consequently, a successful update preserves each Pod's identity, assigned node, and network identity while avoiding the scheduling and startup costs of recreating a Role or an entire ServingGroup.

The strategy is intentionally limited to regular container image changes. A validating webhook rejects unsupported Pod template changes instead of silently falling back to recreation. The rollout operates on a Role instance: the entry Pod and all worker Pods sharing one Role ID are updated as a unit. Role-level `partition` and `maxUnavailable` settings control selection and concurrency.

A readiness-gate-based state machine, backed by a persistent update-state annotation, distinguishes container restarts initiated by an in-place update from ordinary failures. The controller sets a dedicated Pod readiness-gate condition to `False` before changing images, records the pre-update container status, and restores the condition to `True` only after the runtime has observed the target images. This prevents the existing recovery path from treating a planned restart as a reason to delete a Role or ServingGroup, removes the Pod from Service endpoints while its containers restart, and allows reconciliation to resume after a controller restart.

### Motivation

The existing `ServingGroupRollingUpdate` and `RoleRollingUpdate` strategies update workloads by deleting outdated resources and creating replacements. This is appropriate for arbitrary Pod template changes, but it is unnecessarily expensive when the only desired change is a container image.

Deleting a Pod can cause:

- a new scheduling decision and loss of the existing node allocation;
- changes to Pod UID and Pod IP;
- recreation of Services or other Role-scoped resources;
- repeated gang or topology-aware scheduling;
- model initialization, model loading, and cache warm-up costs; and reduced availability while a replacement Pod is being scheduled and initialized.

Kubernetes permits updates to regular container image fields on an existing Pod. Kubelet applies the new image by restarting the affected container while retaining the Pod object and node assignment. `ModelServing` should expose this behavior as an explicit update strategy with well-defined safety, availability, recovery, and status semantics.

The current controller also treats any Pod whose container restart count is greater than zero as an error. Without an explicit planned-update state, an image update could therefore trigger the existing `RecoveryPolicy` and delete the Role or ServingGroup. In-place updates require the controller to distinguish a planned container restart from an unrelated workload failure.

#### Goals

- Add `InPlaceRollingUpdate` as a `ModelServing` rollout strategy.
- Update existing Pods by patching regular entry and worker container images.
- Support Role-level `partition` and `maxUnavailable` controls.
- Reject Pod template changes that cannot be applied through an image-only in-place update.
- Use a dedicated Pod readiness gate as the primary indication that a Pod is currently updating.
- Persist rollout intent and pre-update container status on Pods so reconciliation can resume after a controller restart.
- Prevent planned container restarts from invoking Role or ServingGroup failure recovery.
- Preserve the existing behavior of `ServingGroupRollingUpdate` and `RoleRollingUpdate`.
- Report rollout progress and completion only after the new containers are running and the Pods are Ready.

#### Non-Goals

- Forcing a refresh when the image string in the `ModelServing` does not change, such as republishing the same mutable tag.
- Supporting `maxSurge`; an in-place rollout does not create additional Role instances.
- Automatically falling back to Pod, Role, or ServingGroup recreation when an in-place update cannot complete.

### Proposal

Users select the strategy through `spec.rolloutStrategy.type`. When a new image is submitted, the controller identifies outdated Role instances, applies Role-level partition and availability constraints, and patches all entry and worker Pods belonging to each selected Role instance.

The API server accepts the Pod image change and kubelet restarts the affected containers on the existing node. A normal image patch produces Pod update notifications; it does not delete and recreate the Pod. The controller monitors these updates until every Pod in the Role instance is running the target image and is Ready. It then commits the target revision and Role template hash and proceeds with the next eligible Role instance.

The validating webhook compares the old and new `ModelServing` on update. Changes to replica counts and rollout controls remain valid, but differences between old and new Pod templates must be limited to regular container image fields when `InPlaceRollingUpdate` is selected. Unsupported changes are rejected with a field-specific validation error.

#### User Stories (Optional)

##### Story 1: Upgrade a Large Model Without Rescheduling

A user runs a model whose Pods have expensive accelerator allocations and topology constraints. The user changes the inference server image and selects `InPlaceRollingUpdate`. Kthena patches the images of existing Pods in controlled batches. The containers restart, but the Pod UIDs, node assignments, and Pod IPs do not change. The user avoids another scheduling cycle and retains the placement selected for the workload.

##### Story 2: Recover From an Invalid Image

A user specifies an image that cannot be pulled. The affected Role remains in the updating state and the Pod is not deleted or rescheduled. Kthena reports a warning and does not start additional update batches beyond the availability budget. The user changes the `ModelServing` back to a valid image, and Kthena applies that image as a subsequent in-place update.

#### Notes/Constraints/Caveats (Optional)

- An in-place update preserves the Pod object and placement, not the running container process. Connections and in-memory state owned by the restarted container may be interrupted or lost.
- Image patching is supported only for regular containers in the initial implementation. Completed init containers are not reliably re-executed by changing their image.
- Users should use immutable tags or image digests. If the image string does not change, the `ModelServing` revision does not change and no rollout is triggered.
- Admission validation is the user-facing guardrail. The controller must repeat the image-only compatibility check before patching because admission may be disabled or bypassed.
- `maxUnavailable` counts Role instances, not individual Pods. An entry Pod and its workers are treated as one availability unit.
- Updating the Pod spec is not sufficient to declare success. The old container may remain Ready briefly after the API update, before kubelet processes the new spec.
- An external deletion, eviction, or node loss may still remove a Pod during an update. Such an event is an exceptional recovery path, not part of the normal in-place rollout.
- Pods created for `InPlaceRollingUpdate` contain the in-place readiness gate from creation time because `spec.readinessGates` cannot be added as part of an image-only update. Existing Pods without the gate require a compatibility path based on the persisted update-state annotation, or a one-time recreation before they receive full readiness-gate protection.

### Design Details

#### API

A new value is added to `RolloutStrategyType`:

```go
type RolloutStrategyType string

const (
    ServingGroupRollingUpdate RolloutStrategyType = "ServingGroupRollingUpdate"
    RoleRollingUpdate         RolloutStrategyType = "RoleRollingUpdate"
    InPlaceRollingUpdate      RolloutStrategyType = "InPlaceRollingUpdate"
)
```

`InPlaceRollingUpdate` uses the `RollingUpdateConfiguration` embedded in each Role:

- `partition` protects the first `Partition` Role instances.
- `maxUnavailable` limits the number of unavailable servingGroup
- `template.maxUnavailable` limits the number of unavailable role

The top-level `spec.rolloutStrategy.rollingUpdateConfiguration`, which configures ServingGroup-level rollout, is not valid with `InPlaceRollingUpdate`.

An example configuration is:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: llama
spec:
  replicas: 2
  rolloutStrategy:
    type: InPlaceRollingUpdate
  template:
    roles:
    - name: decode
      replicas: 4
      maxUnavailable: 1
      partition: 1
      entryTemplate:
        spec:
          containers:
          - name: server
            image: registry.example.com/server:v2
      workerReplicas: 1
      workerTemplate:
        spec:
          containers:
          - name: worker
            image: registry.example.com/worker:v2
```

No new user-facing Pod-level API is introduced. Internal annotations represent controller state and are not configuration inputs.

#### Admission Validation

The validating admission path must decode both `AdmissionRequest.Object` and `AdmissionRequest.OldObject` for update requests. Create requests continue to validate the resulting object without an old/new comparison.

When the effective strategy of the new object is `InPlaceRollingUpdate`, validation performs these checks:

1. Match old and new Roles by name.
2. Match entry and worker regular containers by stable container name and require compatible container sets.
3. Compare each old and new Role after normalizing regular container image fields.
4. Reject the request if any remaining Pod template field differs.
5. Reject image changes to init containers or ephemeral containers.
6. Validate Role-level `partition` and `maxUnavailable` with the existing integer-or-percentage rules.
7. Reject ServingGroup-level rolling update configuration for this strategy.

Replica counts and rollout control fields are evaluated independently and may change in the same request. Adding or removing a Role, changing Role structure, or changing any non-image Pod template field is not an in-place-compatible template update and is rejected.

If a user needs an unsupported Pod template change, the user must first choose `RoleRollingUpdate` or `ServingGroupRollingUpdate`, according to the desired recreation scope.

#### Controller Architecture

The existing `ModelServingController` remains the only owner of reconciliation. Rolling update execution is separated behind an internal rollout manager or strategy interface, but it is invoked from the existing reconciliation sequence and uses the same informer caches, work queue, datastore, clients, and status update path.

Conceptually, rollout dispatch becomes:

```text
ServingGroupRollingUpdate -> delete and recreate outdated ServingGroups
RoleRollingUpdate         -> delete and recreate outdated Role instances
InPlaceRollingUpdate      -> patch images of outdated Role instances
```

The in-place manager reuses the current revision and `ControllerRevision` mechanisms to recover both the current Role template and the target Role template. It also reuses Role template hashes to determine whether a Role instance is outdated.

Before each patch, the manager performs the image-only compatibility check again. This prevents an unsupported update from proceeding when webhook validation was unavailable.

#### Rollout Selection and Availability

For each ServingGroup and Role, the controller:

1. Resolves the Role-level partition.
2. Identifies Role instances whose stored Role template hash differs from the target hash.
3. Counts Role instances that are updating, deleting, or otherwise unavailable.
4. Resolves `maxUnavailable` and calculates the remaining update budget.
5. Selects eligible outdated Role instances using the existing rollout ordering, prioritizing unhealthy instances and then higher ordinals where applicable.
6. Starts no more Role updates than the remaining budget permits.

All Pods sharing the selected Role ID are included in that Role instance's update. The controller does not begin another batch until the current observations permit it under `maxUnavailable`.

#### Persistent Update State

The design uses two complementary persistent signals:

1. A dedicated `InPlaceUpdateReady` entry in `pod.spec.readinessGates`, with its corresponding Pod condition in status. `False` means the Pod is actively undergoing an in-place update; `True` means no in-place update is blocking readiness.
2. An in-place update-state annotation that identifies the target and records the pre-update runtime state needed to verify completion and account for the expected restart.

The readiness gate is injected when the Pod is created. The controller initializes its condition to `True` during normal startup. Before an image patch, the controller first updates the condition to `False` with a reason such as `StartInPlaceUpdate`. Kubernetes consequently reports the Pod as not Ready and removes it from Ready endpoints before the container restart begins.

The exact annotation key is an implementation detail, but its JSON value must contain at least:

- target `ModelServing` revision;
- target Role template hash;
- update start time; and
- for every changed regular container, its pre-update image ID, container ID, and restart count.

The datastore adds an updating Role state and an operation that atomically advances a Role's revision and Role template hash when the update completes. Pod status and annotations remain the durable source of truth; datastore state is a reconstructed cache.

The Role state transitions are:

```text
Running -> Updating -> Running
              |
              +-> remains Updating when the target image is not ready
```

The initial implementation does not transition automatically from `Updating` to deletion or recreation.

##### Determining Whether a Pod Is In-Place Updating

The controller uses the following ordered decision:

1. If the Pod contains the in-place readiness gate and its `InPlaceUpdateReady` condition is `False`, the Pod is in-place updating.
2. If the gate exists but its condition has not been created yet, the controller treats the Pod conservatively as not ready. It is considered an active in-place update only when a valid update-state annotation exists; otherwise the controller initializes the condition for normal startup.
3. If the gate condition is `True`, the Pod is no longer actively updating, even if the annotation is retained as a restart baseline.
4. For a legacy Pod without the readiness gate, a valid annotation whose target has not passed the completion check is used as a compatibility fallback. This path prevents accidental deletion but cannot provide readiness-gate traffic isolation.
5. A Role datastore state alone is never sufficient to classify an individual Pod. It is used to aggregate Role progress, while Pod status and annotation are authoritative.

This distinction is important: the annotation may remain after completion to explain the restart count, whereas `InPlaceUpdateReady=False` identifies the active update window. Conversely, relying only on the condition would lose the target and baseline information needed after a controller restart.

Malformed or target-inconsistent annotations are not treated as successful updates. The controller reports the error, keeps the Pod unavailable when the readiness condition is already `False`, and avoids both automatic completion and destructive recovery until reconciliation repairs or supersedes the state.

#### Patching Pods

For a selected Role instance, the controller generates or retrieves the target entry and worker templates and builds a mapping from container name to target image. For every existing Pod in the Role instance, it:

1. Reads the latest Pod from the API server when necessary for conflict handling.
2. Sets the `InPlaceUpdateReady` Pod condition to `False` before changing the Pod spec.
3. Records the target state and baseline image ID, container ID, and restart count in the update-state annotation.
4. Patches only the image fields of matching regular containers and the internal update annotation.
5. Retries status and spec conflicts independently using resource-version-aware client behavior.
6. Leaves revision and Role template hash labels at their observed values until completion.

The operation is idempotent. If one Pod has already been patched and another has not, the next reconciliation continues toward the same target without restarting the completed work unnecessarily.

#### Completion Detection

A changed container has completed the runtime update only when all of the following are true:

- its Pod spec contains the target image string;
- its current container status exists;
- its status image corresponds to the target image;
- its current state is Running; and
- kubelet has observably processed the change.

The preferred evidence that kubelet processed the change is that the current image ID differs from the recorded pre-update image ID, matching the fallback completion check used by RBG. If the target image string resolves to the same image ID, the controller may accept matching normalized spec and status image values; it must not accept an unchanged image ID while status still reports the old image. Container ID change is additional evidence but is not the primary criterion because an unrelated restart can also change it.

After all changed containers pass the runtime check, the controller sets `InPlaceUpdateReady=True`. Kubelet then recomputes the aggregate Pod Ready condition using `ContainersReady` and all readiness gates. A Pod update is complete only when both the custom condition and aggregate Pod Ready condition are `True`.

A Role instance completes only after all required entry and worker Pods satisfy the completion criteria. The controller then:

1. advances the Role revision and Role template hash in the datastore;
2. updates the Pods' revision and Role template hash labels;
3. retains the minimal completed-update restart baseline needed to interpret cumulative restart counts, replacing it on the next in-place update; and
4. returns the Role to `Running`.

For each container with a non-zero restart count, failure recovery compares the current status with the retained baseline:

- a changed container receives an allowance for the single kubelet-driven restart only when its image ID changed;
- a container that was not part of the image update receives no restart allowance;
- a restart count greater than the recorded baseline plus its allowance is an unexpected restart; and
- a restart count lower than the baseline indicates that the Pod was recreated and the baseline is stale.

This allows the controller to stop classifying the Pod as actively updating when the readiness condition becomes `True`, without immediately treating the historical planned restart as a new failure. Baselines are overwritten by the next update and removed when the corresponding Pod no longer exists.

The ServingGroup revision advances only when no unprotected Role instance remains outdated or updating. `ModelServing` `CurrentRevision`, `UpdateRevision`, replica counters, and conditions continue to follow the existing revision model, but they must not report an in-place update as complete merely because the Pod spec patch succeeded.

#### Pod Event Handling

A regular image patch updates the existing Pod and therefore enters the Pod update handler. It does not normally produce Pod delete or add events.

The update handler checks persistent in-place state before applying generic readiness or restart handling:

- If `InPlaceUpdateReady=False`, the handler identifies the Pod as actively updating, evaluates in-place progress, and re-enqueues the owning `ModelServing`.
- A restart associated with that target does not call the generic error handler and does not start the configured `RecoveryPolicy`.
- Once `InPlaceUpdateReady=True`, the Pod leaves the active update window. The retained restart baseline suppresses only the expected image-update restart; subsequent or unrelated restarts use the normal recovery path.

Delete and add handling covers exceptional cases:

- Deleting an updating Pod does not cause deletion of its Role or ServingGroup.
- The owning `ModelServing` is re-enqueued, and the missing Pod is recreated from the target Role template.
- The newly added Pod participates in the same target-state completion check.
- Pods without an active in-place target continue to follow the existing `RecoveryPolicy` behavior.

This distinction must use the persisted readiness condition, target identity, and per-container restart baseline, not merely `restartCount`, because restart count is cumulative.

#### Failure, Retry, and Rollback

Transient patch failures and conflicts are retried through normal reconciliation. Partial Role updates retain their target state and converge idempotently.

If a target image cannot be pulled or the new container cannot become Ready, the controller:

- keeps the existing Pod object;
- keeps the Role in the updating state;
- emits a warning event and reports update progress or failure through conditions;
- consumes the Role's availability budget; and
- does not automatically delete or reschedule the Pod.

A user-initiated rollback changes the image in the `ModelServing` to a previous valid image. The controller treats the latest accepted specification as the new desired target and reconciles any in-progress annotations to that target. Automatic rollback policy and progress deadlines can be added separately in a future proposal.

#### Observability

Kubernetes Events should identify when a Role instance starts, completes, or fails to make progress during an in-place update. Existing `ModelServing` conditions should indicate that an update remains in progress while any Role has an active target.

Debug cache output should expose the Role updating state and observed revision/hash, but must not expose secrets. Logs should include the `ModelServing`, ServingGroup, Role name, Role ID, target revision, and affected Pod without logging image pull credentials.

#### Test Plan

##### Admission Tests

- Accept create requests using `InPlaceRollingUpdate`.
- Accept changes to entry images, worker images, and multiple regular container images.
- Accept image changes combined with replica, partition, or `maxUnavailable` changes.
- Reject command, arguments, environment, resources, volumes, scheduling, metadata, and other Pod template changes.
- Reject init container and ephemeral container image changes.
- Reject container addition, removal, reordering, or renaming.
- Reject incompatible Role additions, removals, or structural changes.
- Validate missing or malformed old objects and admission payloads safely.

##### Controller and Datastore Unit Tests

- Verify generated patches modify only target images and internal annotations.
- Verify an entry Pod and all workers for one Role ID are patched as one rollout unit.
- Verify integer and percentage `partition` and `maxUnavailable` behavior.
- Verify already unavailable and updating Roles consume the availability budget.
- Verify patch conflicts and partially successful updates converge idempotently.
- Verify a controller restart reconstructs the updating state from the Pod readiness condition and update-state annotation.
- Verify Pods are created with the in-place readiness gate and its normal condition is initialized to `True`.
- Verify the condition is changed to `False` before the image patch and that the Pod is removed from Ready endpoints.
- Verify `isPodInPlaceUpdating` returns true for `InPlaceUpdateReady=False`, handles a missing condition conservatively, and uses annotation fallback only for legacy Pods without the gate.
- Verify a Pod that remains briefly `ContainersReady` after patching is not considered complete.
- Verify runtime completion requires updated image status or image ID before the custom condition returns to `True`, followed by aggregate Pod readiness.
- Verify the retained restart baseline permits exactly the expected restart of changed containers, does not excuse restarts of unchanged containers, and detects additional crashes after completion.
- Verify planned restarts do not invoke Role or ServingGroup recovery.
- Verify ordinary crashes still follow the configured `RecoveryPolicy`.
- Verify deleting an updating Pod does not cascade and that the missing Pod is recreated from the target template.
- Verify an invalid image blocks later batches without deleting the Pod.
- Verify changing the target image during an update converges to the latest accepted target.
- Verify existing `ServingGroupRollingUpdate` and `RoleRollingUpdate` tests remain unchanged and pass.

##### End-to-End Tests

An end-to-end test records Pod UID, Pod IP, node name, restart count, container ID, and image ID before changing the `ModelServing` image. It then verifies:

- Pod UID, Pod IP, and node name remain unchanged;
- container ID and image ID change;
- the in-place readiness-gate condition transitions from `True` to `False` and back to `True`;
- Pods return to Ready;
- no planned Pod deletion occurs;
- `partition` protects the expected Role instances;
- `maxUnavailable` limits concurrent Role updates;
- status revisions and conditions converge after completion; and
- an invalid image stalls without rescheduling, after which a valid image update recovers the rollout.

### Alternatives

#### Continue Using Delete-and-Recreate Rolling Updates

The existing strategies support arbitrary Pod template changes and remain the correct fallback for such updates. They do not meet this proposal's objective because deleting Pods discards placement and network identity and may incur substantial scheduling and initialization costs.

#### Add a Separate In-Place Update Controller

A separate controller could watch the same `ModelServing` and Pods. It would compete with the existing controller over rollout decisions, failure recovery, datastore state, and status updates. Coordinating ownership would require a new durable handoff protocol. Keeping one reconciliation owner and extracting an internal rollout manager provides code separation without conflicting writers.

#### Patch Images Without Persistent State

A controller could patch images and ignore restart events for a short period using an in-memory map. This is not safe across controller restarts, leadership changes, or delayed kubelet processing. Persistent Pod annotations provide resumable and inspectable intent.

#### Use Only an Annotation to Identify Active Updates

An annotation is necessary to persist target and baseline data, but using its presence as the active-update signal is ambiguous when historical restart baselines must remain after completion. Following the readiness-gate pattern used by RBG, this proposal uses `InPlaceUpdateReady=False` for the active window and the annotation for durable update details and post-update restart accounting. This also makes Kubernetes readiness and Service endpoint behavior reflect the update directly.

#### Automatically Fall Back to Recreation

Automatically deleting a Pod when an in-place update fails would violate the user's choice to preserve placement and could unexpectedly trigger expensive rescheduling. The initial design reports and blocks instead. Users can explicitly select a recreation-based strategy when that behavior is acceptable.

#### Use a Sidecar or External Lifecycle Manager

A sidecar or external agent could download artifacts or replace application processes, but it would require application-specific integration and would not provide a consistent Kubernetes container image update model. Patching the supported Pod image field uses native kubelet behavior and keeps orchestration in `ModelServing`.

#### Use Ephemeral Containers

Ephemeral containers are intended primarily for debugging and cannot replace the regular workload containers or provide the desired lifecycle and readiness semantics.

<!--
Note: This is a simplified version of kubernetes enhancement proposal template.
https://github.com/kubernetes/enhancements/tree/3317d4cb548c396a430d1c1ac6625226018adf6a/keps/NNNN-kep-template
-->
