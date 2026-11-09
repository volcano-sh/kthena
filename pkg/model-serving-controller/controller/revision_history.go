/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type templateComparison int

const (
	templateUnknown templateComparison = iota
	templateEquivalent
	templateDifferent
)

// A history failure blocks only actions that depend on that snapshot.
type unresolvedRevisionError struct{ error }

func onlyRevisionErrors(err error) bool {
	if err == nil {
		return true
	}
	if _, ok := err.(*unresolvedRevisionError); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !onlyRevisionErrors(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyRevisionErrors(wrapped.Unwrap())
	}
	return false
}

type revisionSnapshot struct {
	workload *workloadv1alpha1.ModelServing
	roles    []workloadv1alpha1.Role
	err      error
}

type roleRevision struct {
	revision string
	roleName string
}

// revisionHistory resolves immutable ControllerRevision snapshots for one reconcile.
// It persists desired revisions and caches owned history for that reconcile.
// Missing or invalid history is cached only for this reconcile so the workqueue
// retry can observe repaired history without restarting the controller.
type revisionHistory struct {
	controller      *ModelServingController
	ms              *workloadv1alpha1.ModelServing
	snapshots       map[string]revisionSnapshot
	roleHashes      map[string]string
	roleComparisons map[roleRevision]templateComparison
	comparisonErrs  map[string]error
	collisionCount  *int32
	targetRevision  string
}

type revisionHistoryKey struct{}

func (h *revisionHistory) copyCollisionCount(ms *workloadv1alpha1.ModelServing) bool {
	if h.collisionCount == nil || *h.collisionCount == 0 ||
		(ms.Status.CollisionCount != nil && *ms.Status.CollisionCount >= *h.collisionCount) {
		return false
	}
	count := *h.collisionCount
	ms.Status.CollisionCount = &count
	return true
}

func (c *ModelServingController) withRevisionHistory(ctx context.Context, ms *workloadv1alpha1.ModelServing) context.Context {
	return context.WithValue(ctx, revisionHistoryKey{}, c.revisionHistory(ctx, ms))
}

func (c *ModelServingController) revisionHistory(ctx context.Context, ms *workloadv1alpha1.ModelServing) *revisionHistory {
	if history, ok := ctx.Value(revisionHistoryKey{}).(*revisionHistory); ok &&
		history.ms.UID == ms.UID && history.ms.Namespace == ms.Namespace && history.ms.Name == ms.Name &&
		history.ms.Generation == ms.Generation {
		return history
	}
	round := coordinatedRoundOf(ctx, ms)
	if round != nil && round.history != nil {
		return round.history
	}
	history := &revisionHistory{
		controller:      c,
		ms:              ms,
		snapshots:       make(map[string]revisionSnapshot),
		roleHashes:      make(map[string]string),
		roleComparisons: make(map[roleRevision]templateComparison),
		comparisonErrs:  make(map[string]error),
	}
	if round != nil {
		round.history = history
	}
	return history
}

func (h *revisionHistory) decode(cr *appsv1.ControllerRevision) revisionSnapshot {
	if cr == nil {
		return revisionSnapshot{err: fmt.Errorf("ControllerRevision is missing")}
	}
	if err := validateControllerRevisionOwner(h.ms, cr); err != nil {
		return revisionSnapshot{err: err}
	}
	workload, err := utils.ModelServingForControllerRevision(h.ms, cr)
	if err != nil {
		return revisionSnapshot{err: err}
	}
	roles := workload.Spec.Template.Roles
	if len(roles) == 0 {
		err = fmt.Errorf("ControllerRevision %s has no Role templates", cr.Name)
	}
	return revisionSnapshot{workload: workload, roles: roles, err: err}
}

func (h *revisionHistory) roles(ctx context.Context, revision string) ([]workloadv1alpha1.Role, error) {
	if snapshot, ok := h.snapshots[revision]; ok {
		return snapshot.roles, snapshot.err
	}

	snapshot := revisionSnapshot{err: fmt.Errorf("revision or Kubernetes client is missing")}
	if revision != "" && h.controller.kubeClientSet != nil {
		cr, err := h.controller.controllerRevisionInRound(ctx, h.ms, revision)
		if err != nil {
			snapshot.err = err
		} else {
			snapshot = h.decode(cr)
			if snapshot.err != nil {
				snapshot.err = fmt.Errorf("ControllerRevision %s: %w", revision, snapshot.err)
			}
		}
	}
	if snapshot.err != nil {
		snapshot.err = &unresolvedRevisionError{snapshot.err}
	}
	h.snapshots[revision] = snapshot
	if snapshot.err != nil {
		klog.Warningf("Cannot resolve revision %q for ModelServing %s/%s; template-driven creation and deletion are paused for affected replicas: %v",
			revision, h.ms.Namespace, h.ms.Name, snapshot.err)
		if h.controller.recorder != nil {
			h.controller.recorder.Eventf(h.ms, corev1.EventTypeWarning, "RevisionUnresolved",
				"Cannot resolve revision %q; template-driven creation and deletion are paused for affected replicas: %v", revision, snapshot.err)
		}
	}
	return snapshot.roles, snapshot.err
}

func (h *revisionHistory) workload(ctx context.Context, revision string) (*workloadv1alpha1.ModelServing, error) {
	if _, err := h.roles(ctx, revision); err != nil {
		return nil, err
	}
	return h.snapshots[revision].workload.DeepCopy(), nil
}

// appliedRoleTemplate resolves an existing rollout unit, independently of its
// eligibility for a future rollout. Current replica counts and controls are
// overlaid by the history decoder; only the applied template comes from history.
func (c *ModelServingController) appliedRoleTemplate(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName string, instance datastore.Role) (workloadv1alpha1.Role, string, string, error) {
	revision := instance.Revision
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		revision, _ = c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), groupName)
	}
	workload, err := c.revisionHistory(ctx, ms).workload(ctx, revision)
	if err != nil {
		return workloadv1alpha1.Role{}, revision, "", err
	}
	for _, role := range workload.Spec.Template.Roles {
		if role.Name == roleName {
			hash, err := utils.RoleRevisionHash(workload, roleName)
			return role, revision, hash, err
		}
	}
	return workloadv1alpha1.Role{}, revision, "", fmt.Errorf("Role %s is absent from applied revision %s", roleName, revision)
}

// workloadForPodGroup projects the applied layouts without changing the desired
// spec. A single Volcano subgroup policy has one size for all instances of a
// Role. During a mixed-layout rollout, retain the smallest applied template as
// a transitional scheduling floor. This is not a readiness or rollout budget:
// those still require each instance's complete revision-specific membership.
func (c *ModelServingController) workloadForPodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, group datastore.ServingGroup) (*workloadv1alpha1.ModelServing, error) {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return c.revisionHistory(ctx, ms).workload(ctx, group.Revision)
	}
	projected := ms.DeepCopy()
	for i, desired := range ms.Spec.Template.Roles {
		instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, desired.Name)
		if err != nil {
			c.revisionHistory(ctx, ms).recordComparisonError(group.Name+"/"+desired.Name+" PodGroup", err)
			return nil, err
		}
		effective := desired
		found := false
		for _, instance := range instances {
			if instance.Status == datastore.RoleDeleting {
				continue
			}
			applied, _, _, err := c.appliedRoleTemplate(ctx, ms, group.Name, desired.Name, instance)
			if err != nil {
				c.revisionHistory(ctx, ms).recordComparisonError(group.Name+"/"+instance.Name+" PodGroup", err)
				return nil, err
			}
			if !found || applied.WorkerReplicas < effective.WorkerReplicas {
				effective = applied
				found = true
			}
		}
		if !found && len(instances) == 0 && roleReplicas(desired) > 0 {
			partition, _, err := c.getPartition(rolePartition(ms, desired), roleReplicas(desired))
			if err != nil {
				c.revisionHistory(ctx, ms).recordComparisonError(group.Name+"/"+desired.Name+" PodGroup", err)
				return nil, err
			}
			if partition > 0 && ms.Status.CurrentRevision != "" {
				protected, _, _, err := c.appliedRoleTemplate(ctx, ms, group.Name, desired.Name, datastore.Role{Revision: ms.Status.CurrentRevision})
				if err != nil {
					return nil, err
				}
				// With no instances yet, use the same protected/unprotected
				// versions that scaleUpRoles will assign to the missing slots.
				if partition >= roleReplicas(desired) || protected.WorkerReplicas < effective.WorkerReplicas {
					effective = protected
				}
			}
		}
		// Counts and explicit gang minima remain current; missing members must
		// not reduce the worker count recorded in an instance's history.
		effective.Replicas = desired.Replicas
		projected.Spec.Template.Roles[i] = *effective.DeepCopy()
	}
	return projected, nil
}

// rolePodsComplete checks each required identity rather than counting arbitrary
// same-label Pods. Terminating members still occupy their names, but never
// supply readiness. A foreign or extra worker cannot stand in for a missing one.
func rolePodsComplete(ms *workloadv1alpha1.ModelServing, groupName, roleID string, role workloadv1alpha1.Role, revision string, pods []*corev1.Pod, requireReady bool) bool {
	members := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		if pod.Namespace == ms.Namespace && utils.IsOwnedByModelServingWithUID(pod, ms.UID) &&
			pod.Labels[workloadv1alpha1.GroupNameLabelKey] == groupName &&
			pod.Labels[workloadv1alpha1.RoleLabelKey] == role.Name &&
			pod.Labels[workloadv1alpha1.RoleIDKey] == roleID && utils.ObjectRevision(pod) == revision {
			members[pod.Name] = pod
		}
	}
	for ordinal := 0; ordinal <= int(role.WorkerReplicas); ordinal++ {
		pod := members[utils.GeneratePodName(groupName, roleID, ordinal)]
		if pod == nil || (requireReady && (pod.DeletionTimestamp != nil || !utils.IsPodRunningAndReady(pod) || inplace.Updating(pod))) {
			return false
		}
	}
	return true
}

// unitHasPods includes terminating members. Confirm absence against the API to
// avoid treating an informer observation gap after CREATE as whole-unit loss.
func (c *ModelServingController) unitHasPods(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleID string) (bool, error) {
	index, key := GroupNameKey, ms.Namespace+"/"+groupName
	selector := labels.Set{workloadv1alpha1.GroupNameLabelKey: groupName}
	if roleID != "" {
		roleName, _ := utils.GetParentNameAndOrdinal(roleID)
		index, key = RoleIDKey, ms.Namespace+"/"+groupName+"/"+roleName+"/"+roleID
		selector[workloadv1alpha1.RoleIDKey] = roleID
	}
	pods, err := c.getPodsByIndex(index, key)
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		if utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			return true, nil
		}
	}
	actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return false, err
	}
	for i := range actual.Items {
		if utils.IsOwnedByModelServingWithUID(&actual.Items[i], ms.UID) {
			return true, nil
		}
	}
	return false, nil
}

func (h *revisionHistory) roleHash(roleName string) (string, error) {
	if hash, ok := h.roleHashes[roleName]; ok {
		return hash, nil
	}
	hash, err := utils.RoleRevisionHash(h.ms, roleName)
	if err == nil {
		h.roleHashes[roleName] = hash
	}
	h.recordComparisonError("desired Role "+roleName, err)
	return hash, err
}

// desiredRevision uses the existing hash as a fast identity, then reuses an
// owned historical identity when its effective workload inputs are semantically
// equal. A genuinely different template is persisted before workload mutation.
func (h *revisionHistory) desiredRevision(ctx context.Context) (string, error) {
	if h.targetRevision != "" {
		return h.targetRevision, nil
	}
	data, err := utils.BuildRevisionData(h.ms)
	if err != nil {
		return "", err
	}
	cr, collisions, err := utils.RecordModelServingRevision(ctx, h.controller.kubeClientSet, h.ms, data)
	if err != nil {
		return "", fmt.Errorf("persist desired ControllerRevision: %w", err)
	}
	revision := cr.Labels[utils.ControllerRevisionRevisionLabelKey]
	if revision == "" {
		return "", fmt.Errorf("ControllerRevision %s has no revision identity", cr.Name)
	}
	if round := coordinatedRoundOf(ctx, h.ms); round != nil {
		round.snapshots[revision] = controllerRevisionObservation{revision: cr}
	}
	h.collisionCount = collisions
	h.snapshots[revision] = h.decode(cr)
	if h.snapshots[revision].err == nil {
		h.targetRevision = revision
	}
	return revision, h.snapshots[revision].err
}

func (h *revisionHistory) errors() error {
	keys := make([]string, 0, len(h.snapshots)+len(h.comparisonErrs))
	for revision, snapshot := range h.snapshots {
		if snapshot.err != nil {
			key := fmt.Sprintf("revision %q", revision)
			h.comparisonErrs[key] = snapshot.err
		}
	}
	for key := range h.comparisonErrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	joined := make([]error, 0, len(keys))
	for _, key := range keys {
		joined = append(joined, fmt.Errorf("resolve %s: %w", key, h.comparisonErrs[key]))
	}
	return errors.Join(joined...)
}

func (h *revisionHistory) recordComparisonError(key string, err error) {
	if err != nil {
		h.comparisonErrs[key] = err
	}
}

func (c *ModelServingController) compareServingGroupTemplate(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	group datastore.ServingGroup,
	targetRevision string,
) templateComparison {
	roleMode := ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate
	if (roleMode && c.store != nil) || group.Revision == targetRevision {
		// A ServingGroup revision alone cannot describe a partially updated
		// RoleRollingUpdate. Continue with the observed Role identities below.
	} else {
		// Validation requires at least one Role. Keep controller unit fixtures that
		// intentionally omit templates on the hash path; real ModelServings always
		// take the persisted semantic path below.
		if len(ms.Spec.Template.Roles) == 0 {
			return templateDifferent
		}
		history := c.revisionHistory(ctx, ms)
		workload, err := history.workload(ctx, group.Revision)
		if err != nil {
			return templateUnknown
		}
		equal, err := utils.EqualModelServingRevisions(group.Revision, targetRevision, workload, ms)
		if err != nil {
			history.recordComparisonError(group.Revision, err)
			return templateUnknown
		}
		if !equal {
			return templateDifferent
		}
	}

	if c.store == nil {
		return templateEquivalent
	}
	history := c.revisionHistory(ctx, ms)
	unknown := false
	for _, desired := range ms.Spec.Template.Roles {
		observedRoles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, desired.Name)
		if err != nil {
			history.recordComparisonError(
				fmt.Sprintf("ServingGroup %s Role %s", group.Name, desired.Name), err)
			return templateUnknown
		}
		for _, observed := range observedRoles {
			comparison := c.compareRoleTemplate(ctx, ms, group, desired.Name, observed)
			if roleMode && comparison == templateUnknown {
				// A known outdated Role can still progress within its own budget.
				// Preserve unknown status if no such Role exists; do not advance
				// the group identity merely because the known Roles are current.
				unknown = true
				continue
			}
			if comparison != templateEquivalent {
				return comparison
			}
		}
	}
	if roleMode {
		observedRoles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			history.recordComparisonError(fmt.Sprintf("ServingGroup %s observed Roles", group.Name), err)
			return templateUnknown
		}
		desiredNames := make(map[string]struct{}, len(ms.Spec.Template.Roles))
		for _, desired := range ms.Spec.Template.Roles {
			desiredNames[desired.Name] = struct{}{}
		}
		for name, instances := range observedRoles {
			if _, desired := desiredNames[name]; desired {
				continue
			}
			for _, observed := range instances {
				if observed.Status != datastore.RoleDeleting {
					// Admission freezes Role names, but legacy persisted state
					// must still reach the existing obsolete-Role cleanup path.
					return templateDifferent
				}
			}
		}
	}
	if unknown {
		return templateUnknown
	}
	return templateEquivalent
}

func (c *ModelServingController) compareRoleTemplate(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleName string,
	observed datastore.Role,
) templateComparison {
	var desired *workloadv1alpha1.Role
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			desired = &ms.Spec.Template.Roles[i]
			break
		}
	}
	if desired == nil {
		return templateDifferent
	}
	if ms.ResourceVersion != "" && isRoleRollingUpdate(ms) {
		live, pod, err := c.coordinatedLiveRole(ctx, ms, servingGroup.Name, roleName, observed)
		if err != nil || pod == nil || pod.DeletionTimestamp != nil {
			return templateUnknown
		}
		observed = live
	}
	history := c.revisionHistory(ctx, ms)
	expectedHash, err := history.roleHash(desired.Name)
	if err != nil {
		return templateUnknown
	}

	revision := observed.Revision
	if revision == "" {
		revision = servingGroup.Revision
	}
	key := roleRevision{revision: revision, roleName: desired.Name}
	if comparison, ok := history.roleComparisons[key]; ok {
		return comparison
	}
	workload, err := history.workload(ctx, revision)
	if err != nil {
		return templateUnknown
	}
	for _, historical := range workload.Spec.Template.Roles {
		if historical.Name != desired.Name {
			continue
		}
		comparison := templateDifferent
		historicalHash, err := utils.RoleRevisionHash(workload, desired.Name)
		if err != nil {
			history.recordComparisonError(revision, err)
			return templateUnknown
		}
		equal, err := utils.EqualRoleRevisions(historicalHash, expectedHash, workload, ms, desired.Name)
		if err != nil {
			history.recordComparisonError(revision, err)
			return templateUnknown
		}
		if equal {
			comparison = templateEquivalent
		}
		history.roleComparisons[key] = comparison
		return comparison
	}

	err = fmt.Errorf("Role %s is missing from ControllerRevision %s", desired.Name, revision)
	history.recordComparisonError(fmt.Sprintf("revision %q Role %s", revision, desired.Name), err)
	return templateUnknown
}

func (c *ModelServingController) roleTemplateHashForComparison(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleName string,
	role datastore.Role,
) (string, bool) {
	var desired *workloadv1alpha1.Role
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			desired = &ms.Spec.Template.Roles[i]
			break
		}
	}
	if desired == nil {
		return "", false
	}
	expectedHash, err := c.revisionHistory(ctx, ms).roleHash(desired.Name)
	if err != nil {
		return "", false
	}
	switch c.compareRoleTemplate(ctx, ms, servingGroup, desired.Name, role) {
	case templateEquivalent:
		return expectedHash, true
	case templateUnknown:
		return "", false
	default:
		revision := role.Revision
		if revision == "" {
			revision = servingGroup.Revision
		}
		workload, err := c.revisionHistory(ctx, ms).workload(ctx, revision)
		if err != nil {
			return "", false
		}
		for _, historical := range workload.Spec.Template.Roles {
			if historical.Name == roleName {
				hash, err := utils.RoleRevisionHash(workload, roleName)
				if hash == expectedHash {
					// The common comparator already established a real difference;
					// a hash collision must not turn it back into equality.
					return "", err == nil
				}
				return hash, err == nil
			}
		}
		return "", false
	}
}

// revisionTarget uses the persisted identity selected for this reconciliation.
// Standalone event helpers retain the computed identity until a sync resolves it.
func (c *ModelServingController) revisionTarget(ctx context.Context, ms *workloadv1alpha1.ModelServing) string {
	if history, ok := ctx.Value(revisionHistoryKey{}).(*revisionHistory); ok && history.ms.UID == ms.UID && history.ms.Generation == ms.Generation && history.targetRevision != "" {
		return history.targetRevision
	}
	return utils.ModelServingRevision(ms)
}
