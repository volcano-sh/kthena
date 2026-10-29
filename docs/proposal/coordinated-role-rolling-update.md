# Coordinated Role rolling updates

`RoleRollingUpdate` optionally coordinates selected Roles independently in each
ServingGroup. An omitted or empty `roles` list selects all Roles. Configuration
presence, selected names, `maxSkew`, and dependencies are immutable after creation;
reordering the declared sets/maps does not change the policy.

## Progress and partitions

For a changed Role, progress is the number of complete target-version Ready formal
instances divided by its latest desired replica count. Entry and every required
worker must be Ready. Partition-protected instances remain in this denominator;
a partition stop does not remove that Role from the baseline. Temporary surge
instances do not increase formal progress. Zero-capacity Roles provide neither a
progress denominator nor fictional Ready dependency capacity.

Ordinary cumulative target starts are limited to
`min(N, ceil((slowestReadyProgress + maxSkew) * N))`. Started but unready formal
instances and replacement reservations occupy the allowance without increasing
Ready progress. The integer calculation supports the full int32 replica domain.
Only one changed Role does not need a cross-Role progress limit.

Participating Roles must share a compatible nominal partition fraction: each Role
rounds up that fraction against its own replica count. Integer partitions specify
an interval of possible fractions. Validation checks that the intervals intersect;
it never rewrites individual partitions. Existing incompatible objects can receive
unrelated updates, but further changes to their templates, counts, or partitions
must restore compatibility. The controller blocks further stable replacements
while an incompatible rollout is present.

## Dependencies and availability

A dependency graph root waits for complete formal target-version Ready capacity
throughout its dependency closure before its first target start. Internal Roles
may start together. This is startup admission, not permanent serialization of
every edge. Once admitted, a root is not closed again solely because a dependency
later loses readiness. Old dependencies remain while an old caller still needs
them; a protected target instance is not an old caller merely because it is
protected. Missing capacity alone does not prove startup admission.

All Roles still obey local `maxUnavailable`, `maxSurge`, and absolute ordinal
partition protection. Cleanup allowance and healthy deletion allowance are
separate. Coordinated stable candidates form a descending ordinal prefix; a
blocked high candidate cannot be filtered away to expose a lower candidate.
This ordering also applies to Roles outside the selected progress set. Known
obsolete temporary surge capacity can be reclaimed separately under the same
availability ledger, without spending or returning stable start credit.
Terminating Pods still occupy capacity and reserve their deletion allowance.

## Full-rollout terminal rounding

A small replica count can make ordinary ceil rounding and old dependency
retention block each other at the final instance. A narrowly scoped terminal step
is allowed only with no partition retention, at most one formal old instance per
changed Role, all formal capacity observed and fully Ready, known history, no
in-flight replacement and no extra old temporary capacity. A caller that has no
remaining old caller of its own may start its final old instance while its old
dependency remains. All ordinary availability and dependency checks still apply.

For prefill/decode/fff = 8/4/10, dependencies prefill → decode → fff, and skew 10%,
the Ready target counts 7/3/9 can permit prefill's final step. The resulting 100%
versus 75% checkpoint exceeds configured skew by one discrete instance. This
exception is not used for a partition canary stop, unknown history, unavailable
targets, missing capacity or uncompleted replacements, and is recomputed each
reconcile rather than persisted.

## Expansion with Volcano Gang scheduling

During a simultaneous template update and replica increase, dependency admission
can delay a caller's added capacity. Its default Gang minimum stays at the
current owned PodGroup's published minimum while the dependency starts; after
admission it advances to the final requested count. For decode/prefill 4 → 6,
a legal sequence is minima 4/4 → 6/4 → 6/6. Explicit `minRoleReplicas` values are
preserved. Budgets and progress always use the latest desired count, not the
transient scheduling minimum. Ordinary failures do not lower a published minimum.
Each group progresses independently; restarts recompute from actual Pods,
revision history and the existing PodGroup without a private stage/member table.
