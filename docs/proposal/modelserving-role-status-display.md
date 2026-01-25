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

## Role Status Display in ModelServing.Status

### Summary

Currently, `ModelServing.Status` displays which `ServingGroups` are in scaling status through the `progressingGroups` field in Conditions. However, it lacks visibility into role-level status. Role status is stored in the datastore but not exposed to users, making it difficult to identify which roles have not been activated (e.g., still in Creating or Deleting state).

This proposal adds `RoleStatuses` field to `ModelServing.Status` to expose role status information, enabling users to monitor the activation status of all roles across all ServingGroups.

### Motivation

#### Goals

- Expose role status information in `ModelServing.Status` for better observability
- Enable users to identify which roles are not yet activated (Creating/Deleting states)
- Maintain consistency with existing ServingGroup status display pattern

#### Non-Goals

### Proposal

#### User Stories

**Story 1: Monitor Role Activation Status**
As a user, I want to see which roles are still in Creating state so I can understand why my ModelServing is not fully ready.

**Story 2: Debug Role Scaling Issues**
As an operator, I want to identify roles stuck in Deleting state to troubleshoot scaling down issues.

#### Design Details

**API Changes**

Add `RoleStatuses` field to `ModelServingStatus`:

```go
// ModelServingStatus defines the observed state of ModelServing
type ModelServingStatus struct {
    // ... existing fields ...
    
    // RoleStatuses track the status of roles across all ServingGroups.
    // This allows users to view which roles have not been activated.
    // +optional
    RoleStatuses []RoleStatusInfo `json:"roleStatuses,omitempty"`
}

// RoleStatusInfo represents the status of a specific role instance
type RoleStatusInfo struct {
    // ServingGroupName is the name of the ServingGroup this role belongs to
    ServingGroupName string `json:"servingGroupName"`
    // RoleName is the name of the role (e.g., "prefill", "decode")
    RoleName string `json:"roleName"`
    // RoleID is the unique identifier of the role instance (e.g., "prefill-0", "prefill-1")
    RoleID string `json:"roleID"`
    // Status is the current status of the role
    // Possible values: Creating, Running, Deleting, NotFound
    Status string `json:"status"`
}
```

**Implementation Details**

1. **Status Collection**: In `UpdateModelServingStatus`, collect role statuses from all ServingGroups:
   - Iterate through all ServingGroups (skip Deleting groups)
   - For each ServingGroup, iterate through all roles defined in ModelServing spec
   - Use `store.GetRoleList()` to retrieve role instances
   - Collect status information for each role instance

2. **Status Update**: Update `RoleStatuses` only when changes are detected:
   - Compare new role statuses with existing ones using a helper function
   - Update status only if there are differences to minimize API calls

3. **Field Identification**: Each role is uniquely identified by:
   - `ServingGroupName`: Identifies which ServingGroup the role belongs to
   - `RoleName`: Identifies the role type (e.g., "prefill", "decode")
   - `RoleID`: Identifies the specific role instance (e.g., "prefill-0", "prefill-1")
   - `Status`: Current state (Creating, Running, Deleting, NotFound)

**Key Design Decisions**

1. **Why these fields?**
   - The datastore stores roles as `map[string]map[string]*Role` (roleName -> roleID -> Role)
   - All role operations require `groupName`, `roleName`, and `roleID` to uniquely identify a role
   - This structure matches the existing datastore organization and API patterns

2. **Why collect all roles?**
   - Users need visibility into all roles, not just non-Running ones
   - Consistent with how ServingGroup status is displayed

3. **Why update only on changes?**
   - Minimizes unnecessary API updates

```

#### Risks and Mitigations


### Test Plan

**Unit Tests**: Controller unit tests cover role status collection and update logic:
- `TestUpdateModelServingStatusRoleStatuses`: Validates `status.roleStatuses` population across scenarios (Running, Creating, Deleting states; multiple ServingGroups; skipping Deleting groups; empty cases)
- `TestUpdateModelServingStatusRoleStatusesChangeDetection`: Validates roleStatuses update only when status changes

### Alternatives
