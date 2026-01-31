---
title: Role Status Display in ModelServing.Status
authors:
- "@FAUST-BENCHOU" # Authors' GitHub accounts here.
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-01-23

---

## Role Status Display in ModelServing.Status via Kubernetes Events

### Summary

Currently, `ModelServing.Status` displays which `ServingGroups` are in scaling status through the `progressingGroups` field in Conditions. However, it lacks visibility into role-level status. Role status is stored in the datastore but not exposed to users, making it difficult to identify which roles have not been activated (e.g., still in Creating or Deleting state).

This proposal uses Kubernetes Events to expose role status information, enabling users to monitor the activation status of all roles across all ServingGroups through standard Kubernetes tooling (e.g., `kubectl describe`, `kubectl get events`).

### Motivation

#### Goals

- Expose role status information via Kubernetes Events for better observability
- Enable users to identify which roles are not yet activated (Creating/Deleting states)
- Avoid inefficient Status updates by using event-driven notifications

#### Non-Goals

### Proposal

#### User Stories

**Story 1: Monitor Role Activation Status**
As a user, I want to see which roles are still in Creating state so I can understand why my ModelServing is not fully ready.

**Story 2: Debug Role Scaling Issues**
As an operator, I want to identify roles stuck in Deleting state to troubleshoot scaling down issues.

#### Design Details

**No API Changes Required**

This proposal does not require any changes to the `ModelServingStatus` API. Instead, it leverages Kubernetes Events, which are a standard mechanism for exposing status information.

**Implementation Details**

1. **EventRecorder Setup**: Add `EventRecorder` to `ModelServingController`:

```go
type ModelServingController struct {
    // ... existing fields ...
    recorder record.EventRecorder
}

func NewModelServingController(...) (*ModelServingController, error) {
    // ... existing initialization ...
    
    // Create event broadcaster and recorder
    eventBroadcaster := record.NewBroadcaster()
    eventBroadcaster.StartStructuredLogging(0)
    eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
        Interface: kubeClientSet.CoreV1().Events(""),
    })
    recorder := eventBroadcaster.NewRecorder(
        scheme.Scheme,
        corev1.EventSource{Component: "modelserving-controller"},
    )
    
    c := &ModelServingController{
        // ... existing fields ...
        recorder: recorder,
    }
    // ...
}
```

2. **Event Emission Helper Function**: Create a helper function to emit role status events:

```go
// emitRoleStatusEvent emits a Kubernetes event when role status changes
func (c *ModelServingController) emitRoleStatusEvent(
    ms *workloadv1alpha1.ModelServing,
    servingGroupName, roleName, roleID string,
    status datastore.RoleStatus,
) {
    var eventType, reason, message string
    
    switch status {
    case datastore.RoleCreating:
        eventType = corev1.EventTypeWarning
        reason = "RoleCreating"
        message = fmt.Sprintf("Role %s/%s in ServingGroup %s is now Creating",
            roleName, roleID, servingGroupName)
    case datastore.RoleRunning:
        eventType = corev1.EventTypeNormal
        reason = "RoleRunning"
        message = fmt.Sprintf("Role %s/%s in ServingGroup %s is now Running",
            roleName, roleID, servingGroupName)
    // ...
    default:
        return
    }
    
    c.recorder.Event(ms, eventType, reason, message)
}
```

3. **Event Emission Points**: Emit events when role status changes:

**Role Creating** - When a new role is created:

```go
func (c *ModelServingController) scaleUpRoles(...) {
    // ... create pods ...
    roleID := utils.GenerateRoleID(targetRole.Name, newIndex)
    c.store.AddRole(utils.GetNamespaceName(ms), groupName, targetRole.Name, roleID, newRevision)
    // Emit event for role creation
    c.emitRoleStatusEvent(ms, groupName, targetRole.Name, roleID, datastore.RoleCreating)
}
```

**Role Running** - When a role becomes ready:

```go
func (c *ModelServingController) handleReadyPod(...) error {
    // ... add pod to store ...
    
    // Check if the role is ready
    roleReady, err := c.checkRoleReady(ms, servingGroupName, roleName, roleID)
    if roleReady {
        currentRoleStatus := c.store.GetRoleStatus(...)
        if currentRoleStatus != datastore.RoleRunning {
            c.store.UpdateRoleStatus(..., datastore.RoleRunning)
            // Emit event for role becoming ready
            c.emitRoleStatusEvent(ms, servingGroupName, roleName, roleID, datastore.RoleRunning)
        }
    }
    // ...
}
```

**Role Deleting** - When role deletion starts:

```go
func (c *ModelServingController) DeleteRole(...) {
    // ... check if already deleting ...
    err := c.store.UpdateRoleStatus(..., datastore.RoleDeleting)
    // Emit event for role deletion
    c.emitRoleStatusEvent(ms, groupName, roleName, roleID, datastore.RoleDeleting)
    // ... delete pods and services ...
}
```

**Role Failed** - When role transitions from Running back to Creating:

```go
func (c *ModelServingController) handleErrorPod(...) error {
    // ... handle error pod ...
    if roleStatus == datastore.RoleRunning {
        c.store.UpdateRoleStatus(..., datastore.RoleCreating)
        // Emit event for role failure
        c.emitRoleStatusEvent(ms, servingGroupName, roleName, roleID, datastore.RoleCreating)
    }
    // ...
}
```

**Key Design Decisions**

1. **Why emit events on status changes?**
   - Events are lightweight and efficient compared to Status updates
   - Only emit when actual state transitions occur (Creating → Running, Running → Deleting, etc.)
   - Avoids polling or periodic collection of all role statuses

```

#### Risks and Mitigations


### Test Plan


### Alternatives
