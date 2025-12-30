## ModelServing: Configurable Role Upgrade Order

### Summary

Add a “Role-ordered upgrade” capability to `ModelServing` rolling updates. When a ServingGroup contains multiple Roles (e.g., Prefill/Decode in PD-disaggregation), users can declare an ordered set of upgrade steps per ServingGroup. Each step is gated by the number of replicas that are both updated and Ready, ensuring the rollout proceeds in the intended sequence.

### Motivation

In PD-disaggregation, Prefill (P) and Decode (D) are different Roles. During an upgrade, if upgraded P cannot communicate with un-upgraded D (or vice versa), an arbitrary Role upgrade order can break a PD instance (ServingGroup) during rollout and cause disruption. We need a controlled, deterministic Role upgrade order with readiness gating at each step.

#### Goals

- Provide a deterministic Role upgrade sequence within each ServingGroup during rollout.
- Gate each step on the target Role’s “updated and Ready” replica count before progressing.
- Keep backward compatibility: if not configured, preserve the existing rollout behavior.

#### Non-Goals

- Redesign the existing ServingGroup-level rollout strategy type.
- Solve application-level compatibility in general (this proposal provides ordering + readiness gating only).

### Proposal

Extend `spec.rolloutStrategy.rollingUpdateConfiguration` with an optional `roleUpgrade` plan. When executing `ServingGroupRollingUpdate`, the controller upgrades each selected ServingGroup step-by-step in the order defined by `roleUpgrade.steps`.

Expected PD upgrade sequence (per PD instance / ServingGroup):

1. Upgrade 1 D replica (canary)
2. Wait for the upgraded D replica to be Ready
3. Upgrade 1 P replica (canary)
4. Wait for the upgraded P replica to be Ready
5. Upgrade remaining P replicas
6. Upgrade remaining D replicas

#### User Stories (Optional)

##### Story 1

As a platform administrator, I want to upgrade a PD-disaggregation deployment by first upgrading one Decode canary and waiting until it is Ready, then upgrading one Prefill canary and waiting until it is Ready, so that cross-version communication is verified and the risk of rollout disruption is reduced.

##### Story 2

As a model serving maintainer, I want a declarative upgrade plan that yields a reproducible and auditable upgrade order, rather than relying on implicit controller choices or a non-deterministic upgrade sequence.

#### Notes/Constraints/Caveats (Optional)

- The execution scope is per ServingGroup. Progress across different ServingGroups is still determined by the existing ServingGroupRollingUpdate strategy (e.g., partition).
- `updateTo` is a cumulative target for “updated-and-ready” replicas, not an incremental amount for the current step.
- If readiness cannot be achieved, the rollout will stall; clear status/events are needed for troubleshooting.

#### Risks and Mitigations

- Risk: A step never reaches Ready, causing the rollout to stall.
  - Mitigation: Expose the current step, target count, and satisfied count via conditions/events; do not advance to subsequent steps.
- Risk: Misconfiguration (unknown role, invalid target, decreasing `updateTo` for the same role) causes unexpected behavior.
  - Mitigation: Reject invalid configurations via webhook validation.
- Security review: This change only controls rollout ordering and does not introduce new permission surfaces or sensitive data handling; follow the existing controller change review process.
- UX/observability review: Surface step progress in status and events following existing CR status conventions.

### Design Details

#### Proposed API

Add the following to `spec.rolloutStrategy.rollingUpdateConfiguration`:

```yaml
spec:
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      partition: 0

      roleUpgrade:
        steps:
          - role: decode
            updateTo: 1
          - role: prefill
            updateTo: 1
          - role: prefill
            updateTo: 100%
          - role: decode
            updateTo: 100%
```

##### Semantics

- `roleUpgrade.steps[]`: an ordered list of steps applied per ServingGroup.
- `steps[].role`: must match a Role name in `spec.template.roles[].name`.
- `steps[].updateTo`: a cumulative target (absolute number or percentage) meaning:
  “how many replicas of this Role must be updated to the new revision and be Ready before moving to the next step”.

Examples:

- `updateTo: 1` means “at least 1 updated-and-ready replica”.
- `updateTo: 100%` means “all replicas updated-and-ready”.

If `roleUpgrade` is omitted, the controller uses the existing rollout behavior.

#### Controller behavior (high-level)

For each ServingGroup selected by the ServingGroupRollingUpdate strategy:

1. Compute `newRevision` from `spec.template.roles` (existing behavior).
2. If the ServingGroup is already fully updated, move to the next ServingGroup.
3. If `roleUpgrade.steps` is configured, run steps in order:
   - For the current step, convert `updateTo` (percentage/absolute) into the required updated-and-ready replica count for that Role.
   - If the Role has fewer updated-and-ready replicas than required, select one outdated replica for that Role and delete it.
   - The existing reconcile logic recreates it with the new revision; wait until it becomes Ready, then continue.
4. When all steps finish, consider the ServingGroup updated.

#### Validation rules

- Each `steps[].role` must exist in `spec.template.roles[].name`.
- `updateTo` must be > 0 and:
  - absolute: `<= role.replicas`
  - percentage: within `(0%, 100%]`
- A Role may appear multiple times, but `updateTo` must be non-decreasing for that Role across steps.

#### Compatibility

- Default behavior remains unchanged when `roleUpgrade` is not set.
- This feature works for both Pod-based roles and LWS-based roles, as it operates on Role replica upgrade boundaries.

### Alternatives

- Keep the current behavior: rely on a non-deterministic upgrade order and incidental readiness. This can break PD rollouts when cross-version incompatibilities exist.
- Provide only a global (cross-ServingGroup) Role upgrade order: simpler, but it does not guarantee a consistent per-PD-instance sequence and does not align with existing ServingGroupRollingUpdate semantics.
- Move compatibility checks entirely into the application: can improve correctness, but cannot replace controller-side ordering and gating, and increases application integration cost.

