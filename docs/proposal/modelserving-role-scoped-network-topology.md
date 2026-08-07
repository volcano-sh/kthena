---
title: Role-Scoped Network Topology Policies in ModelServing
authors:
- "@LiZhenCheng9527"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-07-06

---

## Role-Scoped Network Topology Policies in ModelServing

### Summary

This proposal introduces role-scoped network topology policies for ModelServing while preserving backward compatibility with the existing `spec.template.networkTopology.rolePolicy` API. Today, `rolePolicy` is copied to every generated `PodGroup.spec.subGroupPolicy` entry, so all roles in a ModelServing receive the same network topology constraint. This is too coarse for heterogeneous inference services where different roles have different scheduling requirements.

The proposed transition adds an optional role-level network topology policy to each ModelServing role. Existing manifests that only configure the ModelServing-level `rolePolicy` keep the legacy behavior. New manifests can configure topology policies under selected roles and leave other roles unconstrained. The legacy `rolePolicy` and the new role-level policy are mutually exclusive in one ServingGroup, which makes migration explicit and avoids ambiguous precedence.

### Motivation

PD-disaggregated inference services commonly contain multiple roles with different resource and scheduling requirements. For example, `prefill` and `decode` roles may require GPUs, RDMA resources, and network-topology-aware scheduling, while an `lb` role may be CPU-only and should run on general-purpose nodes.

The current ModelServing API only supports one `spec.template.networkTopology.rolePolicy` for all roles. When that policy is configured, the generated `PodGroup.spec.subGroupPolicy` for every role receives the same network topology constraint. As a result, roles that do not require network-topology-aware scheduling may be unnecessarily restricted to HyperNodes, may consume scarce RDMA-capable nodes, or may remain pending if general-purpose nodes are outside the required topology tier.

Using `groupPolicy` does not solve this issue because it applies to all pods in the ServingGroup. Splitting roles such as `lb` into a separate workload is also undesirable when those roles are logically and operationally part of the same ModelServing.

#### Goals

1. Allow each ModelServing role to define its own network topology scheduling policy.
2. Preserve backward compatibility with the existing `spec.template.networkTopology.rolePolicy` behavior.
3. Allow roles without a role-level policy and without a fallback `rolePolicy` to omit network topology constraints from their generated `SubGroupPolicy`.
4. Make the precedence between role-level and ModelServing-level network topology policies explicit and predictable.
5. Enable heterogeneous roles, such as `prefill`, `decode`, and `lb`, to remain in one ModelServing while applying topology constraints only where needed.

#### Non-Goals

1. This proposal does not remove `spec.template.networkTopology.rolePolicy` in the initial implementation.
2. This proposal does not change the semantics of `spec.template.networkTopology.groupPolicy`.

### Proposal

Add an optional network topology policy field to each ModelServing role. The controller will use this field when generating the corresponding `PodGroup.spec.subGroupPolicy[*].networkTopology`.

The policy selection rules are:

1. If a role defines a role-level network topology policy, use that role-level policy.
2. If no role defines a role-level policy and `spec.template.networkTopology.rolePolicy` is set, keep the legacy behavior and apply the ModelServing-level `rolePolicy` to every role.
3. `spec.template.networkTopology.rolePolicy` and `spec.template.roles[*].networkTopology` are mutually exclusive. Users should not configure both in the same ServingGroup.
4. If neither the role nor the ModelServing-level `rolePolicy` defines a policy, do not set `networkTopology` for that role's `SubGroupPolicy`.

This keeps existing manifests working as before because a ModelServing that only configures `spec.template.networkTopology.rolePolicy` will still apply that policy to every role. New manifests should omit the global `rolePolicy` and configure policies only on selected roles. The global `rolePolicy` is retained only as a compatibility field and is planned to be deprecated after role-level policies are available.

#### User Stories (Optional)

##### Story 1: Apply topology constraints only to inference roles

A user deploys a PD-disaggregated ModelServing with `prefill`, `decode`, and `lb` roles. In this deployment model, the user wants to deploy a router or load balancer per ServingGroup and manage it as the `lb` role in the same ModelServing as the inference roles. The `prefill` and `decode` roles require hard network-topology-aware scheduling with `highestTierAllowed: 1`. The `lb` role is CPU-only and does not require RDMA or topology-aware scheduling.

With role-level policies, the user configures network topology only for `prefill` and `decode`. The generated PodGroup contains topology constraints for those two roles, while the `lb` role's SubGroupPolicy has no `networkTopology` field and can be scheduled onto general-purpose nodes.

##### Story 2: Preserve existing global role policy behavior

An existing user already has ModelServing manifests that configure `spec.template.networkTopology.rolePolicy`. After upgrading the controller, the manifests continue to work without modification. Since the roles do not define role-level policies, each role still inherits the existing ModelServing-level `rolePolicy`.

##### Story 3: Use different policies for different roles

A user wants different roles to use different topology policies. Instead of mixing the legacy ModelServing-level `rolePolicy` with role-level policies, the user configures `networkTopology` under each role that needs a constraint. The controller uses each role's own policy and leaves roles without role-level policies unconstrained.

#### Notes/Constraints/Caveats (Optional)

The existing `spec.template.networkTopology.rolePolicy` becomes a compatibility field for existing manifests. It should not be removed as part of this proposal, but it is planned to be deprecated in a future release after role-level policies are available.

`spec.template.networkTopology.rolePolicy` and `spec.template.roles[*].networkTopology` are mutually exclusive. A ServingGroup should either use the legacy global `rolePolicy` style or the new role-level style, but not both. This avoids ambiguous configuration and makes migration explicit.

This proposal intentionally does not add an explicit opt-out field. If users need one role to avoid inheriting the global `rolePolicy`, they should omit the global `rolePolicy` and configure role-level policies on the roles that need topology constraints. An explicit opt-out can be considered separately if a future use case requires a global default policy with individual exceptions.

The role-level policy applies to the role's generated `SubGroupPolicy`, not directly to individual pods. This follows the current PodGroup integration model.

#### Risks and Mitigations

### Design Details

#### API Changes

Add a Kthena-owned `NetworkTopologySpec` adapter struct. New ModelServing APIs should use this struct instead of directly exposing the Volcano scheduler API type. This keeps the Kthena workload API stable if the Volcano API changes in an incompatible way.

```go
// NetworkTopologySpec defines the network topology scheduling policy exposed by Kthena.
// It is converted to the scheduler-specific representation by the controller.
type NetworkTopologySpec struct {
    // Mode defines whether the network topology constraint is required or preferred.
    // The value can only be “hard” or “soft.”
    // +kubebuilder:validation:Enum=hard;soft
    // +optional
    Mode string `json:"mode,omitempty"`

    // HighestTierAllowed defines the highest network topology tier allowed.
    // +optional
    HighestTierAllowed *int32 `json:"highestTierAllowed,omitempty"`
}
```

Add an optional `NetworkTopology` field to `Role`:

```go
type Role struct {
    // Existing fields omitted.

    // NetworkTopology defines the network topology scheduling requirement for this role.
    // When set, spec.template.networkTopology.rolePolicy must not be configured.
    // +optional
    NetworkTopology *NetworkTopologySpec `json:"networkTopology,omitempty"`
}
```

The existing `NetworkTopology` type should also use the Kthena adapter struct. The JSON shape remains compatible with existing manifests, while Kthena avoids exposing the Volcano Go type in its public workload API:

```go
type NetworkTopology struct {
    // GroupPolicy defines the network topology scheduling requirement of all instances within the ServingGroup.
    GroupPolicy *NetworkTopologySpec `json:"groupPolicy,omitempty"`

    // RolePolicy defines the default network topology scheduling requirement for roles.
    // Deprecated: use roles[*].networkTopology instead. This field is retained for backward compatibility only
    // and must not be configured together with any role-level networkTopology.
    RolePolicy *NetworkTopologySpec `json:"rolePolicy,omitempty"`
}
```

Add webhook validation to reject manifests that configure both the compatibility `rolePolicy` field and role-level network topology policies:

```go
  func validateNetworkTopologyPolicy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
    var allErrs field.ErrorList
    if ms == nil {
      return allErrs
    }

    templatePath := field.NewPath("spec").Child("template")
    rolePolicyConfigured := ms.Spec.Template.NetworkTopology != nil && ms.Spec.Template.NetworkTopology.RolePolicy != nil
    if !rolePolicyConfigured {
      return allErrs
    }

    for i, role := range ms.Spec.Template.Roles {
      if role.NetworkTopology == nil {
        continue
      }

      allErrs = append(allErrs, field.Forbidden(
        templatePath.Child("roles").Index(i).Child("networkTopology"),
        "spec.template.networkTopology.rolePolicy and spec.template.roles[*].networkTopology are mutually exclusive",
      ))
    }

    return allErrs
}
```

#### Example

Selected roles define their own topology policies, while `lb` receives no topology constraint:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: pd-disaggregated-sample
spec:
  schedulerName: volcano
  template:
    roles:
    - name: prefill
      replicas: 2
      workerReplicas: 1
      networkTopology:
        mode: hard
        highestTierAllowed: 1
      entryTemplate:
        spec:
          containers:
          - name: prefill
            image: example/prefill:latest
    - name: decode
      replicas: 2
      workerReplicas: 1
      networkTopology:
        mode: hard
        highestTierAllowed: 1
      entryTemplate:
        spec:
          containers:
          - name: decode
            image: example/decode:latest
    - name: lb
      replicas: 2
      workerReplicas: 0
      entryTemplate:
        spec:
          containers:
          - name: lb
            image: example/lb:latest
```

The generated `PodGroup.spec.subGroupPolicy` should contain `networkTopology` for `prefill` and `decode`, but not for `lb`:

```yaml
spec:
  subGroupPolicy:
  - labelSelector:
      matchLabels:
        modelserving.volcano.sh/name: sample
        modelserving.volcano.sh/role: prefill
    networkTopology:
      mode: hard
      highestTierAllowed: 1
  - labelSelector:
      matchLabels:
        modelserving.volcano.sh/name: sample
        modelserving.volcano.sh/role: decode
    networkTopology:
      mode: hard
      highestTierAllowed: 1
```

#### Controller Behavior

When building `PodGroup.spec.subGroupPolicy`, the controller should resolve the topology policy for each role independently:

```go
func resolveRoleNetworkTopology(ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role) *workloadv1alpha1.NetworkTopologySpec {
    if ms.Spec.Template.NetworkTopology == nil || ms.Spec.Template.NetworkTopology.RolePolicy == nil {
        return nil
    }
    // Legacy behavior: only apply rolePolicy when no role has a role-level policy configured.
    for _, r := range ms.Spec.Template.Roles {
        if r.NetworkTopology != nil {
            return nil
        }
    }
    return ms.Spec.Template.NetworkTopology.RolePolicy
}
```

Then `appendSubGroupPolicy` should assign the converted scheduler-specific topology policy to `SubGroupPolicySpec.NetworkTopology` only when the resolved Kthena policy is non-nil.

The controller should convert Kthena's `NetworkTopologySpec` to the scheduler-specific PodGroup representation at the boundary where it creates or updates the Volcano PodGroup. This conversion should be isolated in a small adapter function so future Volcano API changes do not force a ModelServing API change.

#### Compatibility and Migration

Existing manifests continue to work:

```yaml
spec:
  template:
    networkTopology:
      rolePolicy:
        mode: hard
        highestTierAllowed: 1
    roles:
    - name: prefill
    - name: decode
```

Both roles inherit the existing `rolePolicy` because no role-level policies are defined.

Users who need selective application should migrate by removing the global `rolePolicy` and adding `networkTopology` only to the roles that need it. During migration, users must not configure `spec.template.networkTopology.rolePolicy` and `spec.template.roles[*].networkTopology` at the same time.

#### Test Plan

1. Add API validation tests to verify that a ServingGroup is rejected when `spec.template.networkTopology.rolePolicy` and any `spec.template.roles[*].networkTopology` are configured together.
2. Add API validation tests to verify that `mode` only accepts `hard` and `soft`.
3. Add controller unit tests to verify that legacy manifests using only `spec.template.networkTopology.rolePolicy` keep the existing behavior.
4. Add controller unit tests to verify that role-level policies are applied only to the selected roles when the legacy `rolePolicy` is not set.
5. Add migration-oriented tests to verify that removing `rolePolicy` and adding role-level policies produces the expected `PodGroup.spec.subGroupPolicy` output.

### Addendum: Cross-Role Topology Grouping (`roleGroups`)

Per-role policies (above) let each role opt in or out of a topology constraint, but each
role's `SubGroupPolicy` entry is scheduled by Volcano as an independent unit. Configuring
the same `highestTierAllowed`/`highestTierName` value on two different roles (e.g.
`prefill` and `decode`) does **not** guarantee they land on the same HyperNode: Volcano's
`network-topology-aware` plugin resolves each `SubGroupPolicy` entry to its own
`SubJobInfo`, and computes its HyperNode candidates and `AllocatedHyperNode` completely
independently of any other entry (`pkg/scheduler/plugins/network-topology-aware/network_topology_aware.go`,
`AddHyperNodeGradientForSubJobFn`; `pkg/scheduler/api/sub_job_info.go`, `IsHardTopologyMode`).
Volcano's own docs acknowledge this directly: two partitions configured with the same
`highestTierAllowed` "might" be scheduled to different HyperNodes, with same-HyperNode
placement only encouraged, not guaranteed, by HyperNode-level bin packing scoring
(`docs/user-guide/how_to_use_network_topology_aware_scheduling.md`, section 2.5). This
was not caught in the original proposal and is the actual gap behind issue #1278's
"prefill and decode should share a HyperNode" requirement.

**How grouping is actually achieved.** A `SubJobInfo` is keyed by
`(job UID, SubGroupPolicy.Name, MatchLabelKeys values)`
(`pkg/scheduler/api/job_info.go`, `getOrCreateSubJob`; `pkg/scheduler/api/sub_job_info.go`,
`getSubJobID`). Any pods that match the same `SubGroupPolicy` entry's `LabelSelector` and
produce the same `MatchLabelKeys` values collapse into the same `SubJobInfo` — regardless
of which role they belong to. Kthena already stamps every pod in a ServingGroup instance,
across all roles, with an identical `GroupNameLabelKey` value
(`pkg/model-serving-controller/utils/utils.go`, `createBasePod`). So a single
`SubGroupPolicy` entry whose `LabelSelector` matches `role In [prefill, decode]` and whose
`MatchLabelKeys` is `[GroupNameLabelKey]` (instead of the per-role `[RoleIDKey]` used
elsewhere) causes every `prefill` and `decode` pod within one ServingGroup instance to
become one `SubJobInfo` — one shared `AllocatedHyperNode`, one jointly-enforced
`NetworkTopology` constraint. This uses Volcano's existing `SubGroupPolicy`/`MatchLabelKeys`
mechanism exactly as designed; no scheduler-side change is required.

**API addition.** `NetworkTopology.RoleGroups []RoleGroup`, where `RoleGroup` is
`{name string, roles []string (>=2), policy *NetworkTopologySpec}`. A role may belong to
at most one group and must not also configure `role.NetworkTopology`; `rolePolicy` and
`roleGroups` are mutually exclusive at the ServingGroup level — all enforced by the
webhook, following the same "explicit, no silent precedence" pattern already used for
`rolePolicy` vs. `role.NetworkTopology`.

**Explicit limitation — this is not exact pairwise correlation.** The grouping above
operates at ServingGroup-instance granularity: `GroupNameLabelKey` is constant for the
whole instance, so if a grouped role has more than one replica, *all* replicas of *all*
grouped roles in that instance are folded into the one shared subgroup together. This
does not pair a specific `prefill` replica with a specific `decode` replica (e.g. replica
0 with replica 0, leaving replica 1 free to land elsewhere from replica 0's pair) — for
the common 1-prefill/1-decode-per-instance case this is exactly the desired guarantee;
for xPyD topologies with multiple replicas per grouped role, it is a coarser "this
instance's whole prefill+decode pool together" guarantee. True pairwise correlation would
require Kthena to emit a new per-pair label shared identically across roles (e.g. a pair
index), which is not implemented here and is left as follow-up work if a concrete use
case needs it.

### Alternatives
