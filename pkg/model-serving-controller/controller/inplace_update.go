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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const defaultInPlaceProgressDeadline = 1800 * time.Second

var errInPlaceHistoryUnresolved = errors.New("in-place source history is unresolved")

// inPlaceDecision is the outcome of evaluating one update unit.
type inPlaceDecision int

const (
	// decisionInPlace means only container images differ and the unit can be patched.
	decisionInPlace inPlaceDecision = iota
	// decisionWait means the unit cannot be touched this round (e.g. a Pod is terminating).
	decisionWait
	// decisionRecreate means the unit must follow the regular delete-and-recreate path.
	decisionRecreate
)

// inPlaceRound memoizes what is identical for every unit of one reconcile: the
// ControllerRevision lookups, the "only images changed" verdicts, whether in-place
// updates are usable at all, and the target ControllerRevision. It lives in the
// context of a single manageRollingUpdate call and is used by one goroutine only.
type inPlaceRound struct {
	enabled *bool
	sources map[string]sourceRevision
	// imagesOnly is keyed by source revision, plus the Role name in Role mode.
	imagesOnly   map[string]bool
	targetStored bool
}

// sourceRevision is the outcome of reading the ControllerRevision a Pod was created from.
type sourceRevision struct {
	ms *workloadv1alpha1.ModelServing
}

type inPlaceRoundKey struct{}

func withInPlaceRound(ctx context.Context) context.Context {
	return context.WithValue(ctx, inPlaceRoundKey{}, &inPlaceRound{
		sources:    map[string]sourceRevision{},
		imagesOnly: map[string]bool{},
	})
}

// ensureRound returns ctx with a round, so everything below it shares one.
func ensureRound(ctx context.Context) context.Context {
	if _, ok := ctx.Value(inPlaceRoundKey{}).(*inPlaceRound); ok {
		return ctx
	}
	return withInPlaceRound(ctx)
}

// roundOf returns the round of ctx, or a throwaway one when ctx carries none.
func roundOf(ctx context.Context) *inPlaceRound {
	if round, ok := ctx.Value(inPlaceRoundKey{}).(*inPlaceRound); ok {
		return round
	}
	return &inPlaceRound{sources: map[string]sourceRevision{}, imagesOnly: map[string]bool{}}
}

// enabledFor is updatesInPlace, evaluated once per round.
func (r *inPlaceRound) enabledFor(c *ModelServingController, ms *workloadv1alpha1.ModelServing) bool {
	if r.enabled == nil {
		enabled := c.updatesInPlace(ms)
		r.enabled = &enabled
	}
	return *r.enabled
}

// updatesInPlace reports whether the rolling update of ms may use in-place updates.
// When it is false the controller behaves exactly as for the Recreate method.
func (c *ModelServingController) updatesInPlace(ms *workloadv1alpha1.ModelServing) bool {
	if inplace.UpdateMethodOf(ms) != workloadv1alpha1.InPlaceUpdateMethod {
		return false
	}
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		klog.Warningf("cannot build plugin chain of ModelServing %s/%s, fall back to recreate: %v", ms.Namespace, ms.Name, err)
		return false
	}
	if name, unsupported := chain.PluginWithoutInPlaceUpdate(); unsupported {
		klog.V(2).Infof("plugin %s of ModelServing %s/%s does not support in-place update, fall back to recreate", name, ms.Namespace, ms.Name)
		return false
	}
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate {
		// With no unavailable budget an in-place update could never start. Recreate uses surge instead.
		maxUnavailable, err := utils.GetMaxUnavailable(ms)
		return err == nil && maxUnavailable > 0
	}
	return true
}

// roleUpdatesInPlace is updatesInPlace for RoleRollingUpdate, where each Role owns its budget.
func (c *ModelServingController) roleUpdatesInPlace(ms *workloadv1alpha1.ModelServing, roleName string) bool {
	return c.updatesInPlace(ms) && roleBudgetAllowsInPlace(ms, roleName)
}

func roleBudgetAllowsInPlace(ms *workloadv1alpha1.ModelServing, roleName string) bool {
	for _, role := range ms.Spec.Template.Roles {
		if role.Name != roleName {
			continue
		}
		maxUnavailable, configured, err := utils.GetMaxUnavailableForRole(role)
		return err == nil && (!configured || maxUnavailable > 0)
	}
	return false
}

// updateServingGroup replaces deleting an outdated ServingGroup. It patches the
// images in place when that is possible and otherwise recreates the group.
func (c *ModelServingController) updateServingGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, sg datastore.ServingGroup, revision string) error {
	ctx = ensureRound(ctx)
	round := roundOf(ctx)
	if !round.enabledFor(c, ms) {
		return c.deleteServingGroup(ctx, ms, sg.Name)
	}
	pods, err := c.ownedPodsByIndex(ms, GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, sg.Name))
	if err != nil {
		return err
	}
	decision, reason, err := c.decideInPlace(ctx, ms, pods, revision, false, func(oldMS *workloadv1alpha1.ModelServing, _ *corev1.Pod) bool {
		same, err := inplace.OnlyImagesChanged(oldMS, ms)
		return err == nil && same
	})
	if err != nil {
		return err
	}
	switch decision {
	case decisionWait:
		return nil
	case decisionRecreate:
		c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "InPlaceUpdateFallback",
			fmt.Sprintf("ServingGroup %s is recreated instead of updated in place: %s", sg.Name, reason))
		return c.deleteServingGroup(ctx, ms, sg.Name)
	}
	return c.startInPlaceUpdate(ctx, ms, sg.Name, pods, revision)
}

// updateRole is updateServingGroup for a single Role replica.
func (c *ModelServingController) updateRole(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID, revision string) error {
	ctx = ensureRound(ctx)
	if !roundOf(ctx).enabledFor(c, ms) || !roleBudgetAllowsInPlace(ms, roleName) {
		c.DeleteRole(ctx, ms, groupName, roleName, roleID)
		return nil
	}
	pods, err := c.ownedPodsByIndex(ms, RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, roleName, roleID))
	if err != nil {
		return err
	}
	decision, reason, err := c.decideInPlace(ctx, ms, pods, revision, true, func(oldMS *workloadv1alpha1.ModelServing, pod *corev1.Pod) bool {
		return inplace.OnlyRoleImagesChanged(oldMS, ms, utils.GetRoleName(pod))
	})
	if err != nil {
		return err
	}
	switch decision {
	case decisionWait:
		return nil
	case decisionRecreate:
		c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "InPlaceUpdateFallback",
			fmt.Sprintf("Role %s/%s in ServingGroup %s is recreated instead of updated in place: %s", roleName, roleID, groupName, reason))
		c.DeleteRole(ctx, ms, groupName, roleName, roleID)
		return nil
	}
	return c.startInPlaceUpdate(ctx, ms, groupName, pods, revision)
}

// decideInPlace checks every Pod of an update unit. imagesOnly compares the
// template the Pod was created from (its labelled revision) with the desired one.
// perRole tells whether that verdict also depends on the Pod's Role. Source revisions
// and verdicts are memoized in the round, as all units of a rollout share them.
func (c *ModelServingController) decideInPlace(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	pods []*corev1.Pod,
	revision string,
	perRole bool,
	imagesOnly func(oldMS *workloadv1alpha1.ModelServing, pod *corev1.Pod) bool,
) (inPlaceDecision, string, error) {
	if len(pods) == 0 {
		return decisionRecreate, "no pods found", nil
	}
	if allPatchedFor(pods, revision) {
		// The unit was already patched for this revision and is only waiting for the
		// kubelet, so nothing is evaluated or read again.
		return decisionInPlace, "", nil
	}
	round := roundOf(ctx)
	for _, pod := range pods {
		if utils.IsPodTerminating(pod) {
			return decisionWait, "", nil
		}
		if reason := restartPolicyBlocksInPlace(pod); reason != "" {
			return decisionRecreate, reason, nil
		}
		podRevision := utils.ObjectRevision(pod)
		if podRevision == revision {
			continue
		}
		if missing := inplace.MissingContainers(ms, pod); len(missing) > 0 {
			return decisionRecreate, fmt.Sprintf("pod %s has no container %v of the target template", pod.Name, missing), nil
		}
		source, ok := round.sources[podRevision]
		if !ok {
			var err error
			source.ms, err = c.revisionHistory(ctx, ms).workload(ctx, podRevision)
			if err != nil {
				// Unresolved history must not turn into a recreate or adopt the
				// current plugin inputs for a legacy source revision.
				return decisionWait, "", err
			}
			round.sources[podRevision] = source
		}
		key := podRevision
		if perRole {
			key += "/" + utils.GetRoleName(pod)
		}
		same, ok := round.imagesOnly[key]
		if !ok {
			same = imagesOnly(source.ms, pod)
			round.imagesOnly[key] = same
		}
		if !same {
			return decisionRecreate, "changes other than container images", nil
		}
		if _, drift := inplace.ImagesToPatch(pod, inplace.TargetImages(ms, pod), inplace.TargetImages(source.ms, pod)); len(drift) > 0 {
			return decisionRecreate, fmt.Sprintf("pod %s runs images of containers %v that match neither the old nor the new template", pod.Name, drift), nil
		}
	}
	return decisionInPlace, "", nil
}

// allPatchedFor is true when every Pod has an unfinished in-place update to revision.
func allPatchedFor(pods []*corev1.Pod, revision string) bool {
	for _, pod := range pods {
		if state := inplace.StateOf(pod); !state.InProgress() || state.Revision != revision || utils.IsPodTerminating(pod) {
			return false
		}
	}
	return len(pods) > 0
}

// restartPolicyBlocksInPlace returns why the kubelet may not restart a container
// after its image changed, or "" when it always does.
func restartPolicyBlocksInPlace(pod *corev1.Pod) string {
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		return fmt.Sprintf("pod %s restartPolicy is not Always", pod.Name)
	}
	for _, container := range pod.Spec.Containers {
		if container.RestartPolicy != nil && *container.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			return fmt.Sprintf("container %s of pod %s has restartPolicy %s", container.Name, pod.Name, *container.RestartPolicy)
		}
		if len(container.RestartPolicyRules) > 0 {
			return fmt.Sprintf("container %s of pod %s has restartPolicyRules", container.Name, pod.Name)
		}
	}
	return ""
}

// ownedPodsByIndex lists Pods from an index and drops those owned by a different
// ModelServing incarnation, which must never be patched.
func (c *ModelServingController) ownedPodsByIndex(ms *workloadv1alpha1.ModelServing, index, key string) ([]*corev1.Pod, error) {
	pods, err := c.getPodsByIndex(index, key)
	if err != nil {
		return nil, err
	}
	owned := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			owned = append(owned, pod)
		}
	}
	return owned, nil
}

// startInPlaceUpdate patches every Pod of the unit and marks it unavailable at once,
// so the rollout budget accounts for the unit before the kubelet has restarted anything.
func (c *ModelServingController) startInPlaceUpdate(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, pods []*corev1.Pod, revision string) error {
	started := false
	for _, pod := range pods {
		state := inplace.StateOf(pod)
		if utils.ObjectRevision(pod) == revision && !state.InProgress() {
			continue
		}
		if state.InProgress() && state.Revision == revision {
			// Already patched by an earlier sync; only keep it accounted as unavailable.
			if err := c.markPodUnavailable(ms, groupName, pod); err != nil {
				return err
			}
			continue
		}
		if err := c.ensureTargetRevision(ctx, ms, revision); err != nil {
			return err
		}
		images := inplace.TargetImages(ms, pod)
		if source, ok := roundOf(ctx).sources[utils.ObjectRevision(pod)]; ok && source.ms != nil {
			images, _ = inplace.ImagesToPatch(pod, images, inplace.TargetImages(source.ms, pod))
		} else {
			images, _ = inplace.ImagesToPatch(pod, images, nil)
		}
		patched, err := inplace.Start(ctx, c.kubeClientSet, pod, images, revision, time.Now())
		if err != nil {
			return err
		}
		if patched {
			started = true
			klog.V(2).Infof("Pod %s/%s is updated in place to revision %s", pod.Namespace, pod.Name, revision)
		}
		if err := c.markPodUnavailable(ms, groupName, pod); err != nil {
			return err
		}
	}
	if started {
		c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "InPlaceUpdateStarted",
			fmt.Sprintf("Updating images in place in ServingGroup %s to revision %s", groupName, revision))
	}
	return nil
}

// ensureTargetRevision persists the target template so a later recovery never depends
// on the mutable spec. An existing revision is kept as is: role replicas are not part
// of the revision hash, so re-creating it mid-rollout would fail on a data mismatch.
func (c *ModelServingController) ensureTargetRevision(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision string) error {
	round := roundOf(ctx)
	if round.targetStored {
		return nil
	}
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil {
		return err
	}
	if cr == nil {
		data, err := utils.BuildRevisionData(ms)
		if err != nil {
			return err
		}
		cr, _, err = utils.RecordModelServingRevision(ctx, c.kubeClientSet, ms, data)
		if err != nil {
			return err
		}
		if cr.Labels[utils.ControllerRevisionRevisionLabelKey] != revision {
			return fmt.Errorf("in-place target revision %s disappeared; retry with the newly resolved identity", revision)
		}
	}
	target := c.revisionHistory(ctx, ms).decode(ctx, cr)
	if target.err != nil {
		return target.err
	}
	equal, err := utils.EqualModelServingRevisions(revision, revision, target.workload, ms)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("in-place target revision %s differs from the desired template", revision)
	}
	round.targetStored = true
	return nil
}

// inPlaceUnit is the set of Pods that move to the new revision together.
type inPlaceUnit struct {
	groupName string
	// roleName and roleID are empty for a ServingGroup unit.
	roleName string
	roleID   string
	pods     []*corev1.Pod
	// states[i] is the decoded in-place state of pods[i], nil when it has none.
	states []*inplace.State
}

// completeInPlaceUpdates finishes in-place updates. It is level-triggered: it only
// reads Pod state, so it is safe to run after a controller restart or repeatedly.
// It also runs when the update method was switched away from InPlace, so a unit
// left unfinished by an earlier in-place update still completes or hits its deadline.
func (c *ModelServingController) completeInPlaceUpdates(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision string) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		if errors.Is(err, datastore.ErrServingGroupNotFound) {
			return nil
		}
		return err
	}
	roleMode := ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate
	for _, sg := range groups {
		if sg.Status == datastore.ServingGroupDeleting {
			continue
		}
		pods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, sg.Name))
		if err != nil {
			return err
		}
		for _, unit := range splitInPlaceUnits(ms, sg.Name, pods, roleMode) {
			if err := c.reconcileInPlaceUnit(ctx, ms, unit, revision); err != nil {
				if errors.Is(err, errInPlaceHistoryUnresolved) {
					// Pause only this unit. Unknown history supplies no Ready
					// credit, but must not block independently safe units.
					c.revisionHistory(ctx, ms).recordComparisonError(unit.groupName+"/"+unit.roleID+" in-place update", err)
					continue
				}
				return err
			}
		}
	}
	return nil
}

func splitInPlaceUnits(ms *workloadv1alpha1.ModelServing, groupName string, pods []*corev1.Pod, roleMode bool) []*inPlaceUnit {
	byKey := map[string]*inPlaceUnit{}
	var units []*inPlaceUnit
	for _, pod := range pods {
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		key, roleName, roleID := groupName, "", ""
		if roleMode {
			roleName, roleID = utils.GetRoleName(pod), utils.GetRoleID(pod)
			key = roleName + "/" + roleID
		}
		unit, ok := byKey[key]
		if !ok {
			unit = &inPlaceUnit{groupName: groupName, roleName: roleName, roleID: roleID}
			byKey[key] = unit
			units = append(units, unit)
		}
		unit.pods = append(unit.pods, pod)
		unit.states = append(unit.states, inplace.StateOf(pod))
	}
	return units
}

func (c *ModelServingController) reconcileInPlaceUnit(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) error {
	active := false
	for _, state := range unit.states {
		active = active || state.InProgress()
	}
	if active {
		// Validate every member before any completion, retargeting or deadline
		// recovery. This also covers an unpatched member of a partial update.
		for _, pod := range unit.pods {
			if utils.IsPodTerminating(pod) {
				return nil
			}
			if _, err := c.revisionHistory(ctx, ms).workload(ctx, utils.ObjectRevision(pod)); err != nil {
				return errors.Join(errInPlaceHistoryUnresolved, err)
			}
		}
	}
	var earliest time.Time
	inProgress, retargeted := false, false
	for i, pod := range unit.pods {
		if utils.IsPodTerminating(pod) {
			return nil
		}
		state := unit.states[i]
		if state == nil || state.Done {
			continue
		}
		inProgress = true
		if state.Revision != revision {
			if utils.ObjectRevision(pod) == revision || c.roleRevertedTo(ctx, ms, unit, pod) {
				// The spec went back to the revision the Pod is labelled with, so the rollout
				// treats it as up to date although its images were already patched away.
				return c.recreateInPlaceUnit(ctx, ms, unit)
			}
			// The desired revision moved on; the regular rollout re-patches this unit.
			// It may never select it (for example a raised partition), so keep the
			// deadline running meanwhile, but never complete it towards the new revision.
			retargeted = true
		}
		if earliest.IsZero() || state.StartedAt.Time.Before(earliest) {
			earliest = state.StartedAt.Time
		}
	}
	if !inProgress {
		return c.repairInPlaceRevision(ctx, ms, unit, revision)
	}

	if !retargeted && hasUnpatchedOutdatedPod(unit, revision) {
		// A patch that keeps failing must not leave the unit half updated forever.
		if deadline, remaining := inPlaceDeadline(ms, earliest); remaining <= 0 && ms.Spec.RecoveryPolicy != workloadv1alpha1.NoneRestartPolicy {
			return c.handleInPlaceTimeout(ctx, ms, unit, revision, deadline)
		} else if remaining > 0 {
			c.enqueueModelServingAfter(ms, remaining)
		}
		// Resume independently of the rollout budget and partition, so a unit never stays half patched.
		return c.resumeInPlaceUnit(ctx, ms, unit, revision)
	}

	if retargeted {
		if unitSettled(ms, unit, revision) {
			return nil
		}
		return c.enforceInPlaceDeadline(ctx, ms, unit, revision, earliest)
	}

	if !unitCompleted(ms, unit, revision) {
		return c.enforceInPlaceDeadline(ctx, ms, unit, revision, earliest)
	}
	complete, err := c.inPlaceMembersPresent(ctx, ms, unit, revision)
	if err != nil {
		return err
	}
	if !complete {
		return c.enforceInPlaceDeadline(ctx, ms, unit, revision, earliest)
	}

	for i, pod := range unit.pods {
		if !unit.states[i].InProgress() {
			continue
		}
		hash, err := c.roleTemplateHashOf(ms, pod)
		if err != nil {
			return err
		}
		if err := inplace.Complete(ctx, c.kubeClientSet, pod, revision, hash); err != nil {
			return err
		}
	}
	if err := c.recordInPlaceRevision(ms, unit, revision); err != nil {
		return err
	}
	c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "InPlaceUpdateCompleted",
		fmt.Sprintf("In-place update of ServingGroup %s to revision %s completed", unit.groupName, revision))
	c.enqueueModelServing(ms)
	return nil
}

// inPlaceMembersPresent proves the whole unit is observed before advancing its
// identity. A successful CREATE may not yet be in the informer; completing just
// the visible entry would otherwise strand a late worker on the old revision.
func (c *ModelServingController) inPlaceMembersPresent(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) (bool, error) {
	expected := 0
	for _, desired := range ms.Spec.Template.Roles {
		if unit.roleName != "" && desired.Name != unit.roleName {
			continue
		}
		instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), unit.groupName, desired.Name)
		if err != nil {
			return false, err
		}
		if unit.roleName == "" && len(instances) != roleReplicas(desired) {
			return false, nil
		}
		found := false
		for _, instance := range instances {
			if unit.roleName != "" && instance.Name != unit.roleID {
				continue
			}
			if instance.Status == datastore.RoleDeleting {
				return false, nil
			}
			applied, source, _, err := c.appliedRoleTemplate(ctx, ms, unit.groupName, desired.Name, instance)
			if err != nil {
				return false, errors.Join(errInPlaceHistoryUnresolved, err)
			}
			members := make([]*corev1.Pod, 0, len(unit.pods))
			for _, pod := range unit.pods {
				if utils.IsPodTerminating(pod) {
					return false, nil
				}
				observed := utils.ObjectRevision(pod)
				if observed != source && observed != revision {
					continue
				}
				// A previous Complete may have patched some labels before its
				// response was lost. Both identities are valid during that retry.
				member := pod.DeepCopy()
				member.Labels[workloadv1alpha1.RevisionLabelKey] = source
				members = append(members, member)
			}
			if !rolePodsComplete(ms, unit.groupName, instance.Name, applied, source, members, false) {
				return false, nil
			}
			expected += 1 + int(applied.WorkerReplicas)
			found = true
		}
		if unit.roleName != "" && !found {
			return false, nil
		}
	}
	return expected > 0 && len(unit.pods) == expected, nil
}

// repairInPlaceRevision re-records the revision of a unit whose Pods were all flipped
// by an in-place update, in case the store missed it (for example the final Complete
// patch persisted but its response was lost, and the retry found nothing in progress).
func (c *ModelServingController) repairInPlaceRevision(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) error {
	flipped := false
	for i, pod := range unit.pods {
		if utils.ObjectRevision(pod) != revision {
			return nil
		}
		if state := unit.states[i]; state != nil && state.Done && state.Revision == revision {
			flipped = true
		}
	}
	if flipped {
		complete, err := c.inPlaceMembersPresent(ctx, ms, unit, revision)
		if err != nil || !complete {
			return err
		}
		return c.recordInPlaceRevision(ms, unit, revision)
	}
	return nil
}

// podFinished tells whether the update of a Pod is done. With recoveryPolicy None a crash is
// never recovered, so it must not keep a Pod that runs the target image from finishing.
func podFinished(ms *workloadv1alpha1.ModelServing, state *inplace.State, pod *corev1.Pod) bool {
	if ms.Spec.RecoveryPolicy == workloadv1alpha1.NoneRestartPolicy {
		return state.OnTarget(pod)
	}
	return state.Completed(pod)
}

// unitCompleted is true once every Pod has restarted with its new image and is Ready.
// Pods that were never part of the update must already carry the target revision.
func unitCompleted(ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) bool {
	for i, pod := range unit.pods {
		if unit.states[i].InProgress() {
			if !podFinished(ms, unit.states[i], pod) {
				return false
			}
			continue
		}
		if utils.ObjectRevision(pod) != revision {
			return false
		}
	}
	return true
}

// roleRevertedTo detects a revert at Role level: the spec went back to the template the Role
// was created from, while other Roles changed, so the overall revision differs from the Pod's.
// The rollout would skip the Role as up to date although its images were already patched away.
func (c *ModelServingController) roleRevertedTo(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, pod *corev1.Pod) bool {
	if unit.roleName == "" {
		return false
	}
	revision := utils.ObjectRevision(pod)
	history := c.revisionHistory(ctx, ms)
	previous, err := history.workload(ctx, revision)
	if err != nil {
		return false
	}
	equal, err := utils.EqualRoleRevisions(revision, "", previous, ms, unit.roleName)
	history.recordComparisonError(unit.groupName+"/"+unit.roleID+" in-place revert", err)
	return err == nil && equal
}

// hasUnpatchedOutdatedPod is true when some Pod of a unit, whose other Pods were patched,
// still has to be patched for revision (an earlier Start failed halfway).
func hasUnpatchedOutdatedPod(unit *inPlaceUnit, revision string) bool {
	for i, pod := range unit.pods {
		if !unit.states[i].InProgress() && utils.ObjectRevision(pod) != revision && !utils.IsPodTerminating(pod) {
			return true
		}
	}
	return false
}

// unitSettled is true when no member is left unpatched and every Pod that is mid-update has finished its own update.
func unitSettled(ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) bool {
	// A member that was never patched keeps the unit half updated, and the partition may keep
	// the rollout from selecting it again, so it must stay under the deadline.
	if hasUnpatchedOutdatedPod(unit, revision) {
		return false
	}
	for i, pod := range unit.pods {
		if unit.states[i].InProgress() && !podFinished(ms, unit.states[i], pod) {
			return false
		}
	}
	return true
}

// inPlaceDeadline returns the progress deadline and the time left since startedAt.
func inPlaceDeadline(ms *workloadv1alpha1.ModelServing, startedAt time.Time) (deadline, remaining time.Duration) {
	deadline = defaultInPlaceProgressDeadline
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.ProgressDeadlineSeconds != nil {
		deadline = time.Duration(*ms.Spec.RolloutStrategy.ProgressDeadlineSeconds) * time.Second
	}
	return deadline, time.Until(startedAt.Add(deadline))
}

func (c *ModelServingController) enforceInPlaceDeadline(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string, startedAt time.Time) error {
	deadline, remaining := inPlaceDeadline(ms, startedAt)
	if remaining > 0 {
		c.enqueueModelServingAfter(ms, remaining)
		return nil
	}
	return c.handleInPlaceTimeout(ctx, ms, unit, revision, deadline)
}

// handleInPlaceTimeout treats the Pods that did not finish in time as error Pods, so the
// recovery policy decides what happens: ServingGroupRecreate and RoleRecreate go through
// handleErrorPod (grace period, deletion, then recreation of the Role or ServingGroup),
// while None leaves the Pods alone like a plain Deployment would.
// Pods of the unit that were never patched are deleted directly: they are healthy, so the
// error path would keep them, and the unit cannot finish without them.
func (c *ModelServingController) handleInPlaceTimeout(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string, deadline time.Duration) error {
	c.emitRoleStatusEvent(ms, corev1.EventTypeWarning, "InPlaceUpdateTimeout",
		fmt.Sprintf("In-place update of ServingGroup %s exceeded progress deadline %s, applying recovery policy %s", unit.groupName, deadline, ms.Spec.RecoveryPolicy))
	if ms.Spec.RecoveryPolicy == workloadv1alpha1.NoneRestartPolicy {
		return nil
	}
	for i, pod := range unit.pods {
		if !unit.states[i].InProgress() {
			if utils.ObjectRevision(pod) != revision && !utils.IsPodTerminating(pod) {
				err := c.kubeClientSet.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, *metav1.NewPreconditionDeleteOptions(string(pod.UID)))
				if err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
			continue
		}
		if podFinished(ms, unit.states[i], pod) {
			continue
		}
		if err := c.handleErrorPod(ms, unit.groupName, pod); err != nil {
			return err
		}
	}
	return nil
}

func (c *ModelServingController) resumeInPlaceUnit(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) error {
	if unit.roleName == "" {
		return c.updateServingGroup(ctx, ms, datastore.ServingGroup{Name: unit.groupName}, revision)
	}
	return c.updateRole(ctx, ms, unit.groupName, unit.roleName, unit.roleID, revision)
}

func (c *ModelServingController) recreateInPlaceUnit(ctx context.Context, ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit) error {
	if unit.roleName == "" {
		return c.deleteServingGroup(ctx, ms, unit.groupName)
	}
	c.DeleteRole(ctx, ms, unit.groupName, unit.roleName, unit.roleID)
	return nil
}

func (c *ModelServingController) roleTemplateHashOf(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) (string, error) {
	return utils.RoleRevisionHash(ms, utils.GetRoleName(pod))
}

// recordInPlaceRevision brings the in-memory store in line with the flipped Pod labels.
func (c *ModelServingController) recordInPlaceRevision(ms *workloadv1alpha1.ModelServing, unit *inPlaceUnit, revision string) error {
	name := utils.GetNamespaceName(ms)
	seen := map[string]bool{}
	for _, pod := range unit.pods {
		roleName, roleID := utils.GetRoleName(pod), utils.GetRoleID(pod)
		key := roleName + "/" + roleID
		if seen[key] {
			continue
		}
		seen[key] = true
		hash, err := c.roleTemplateHashOf(ms, pod)
		if err != nil {
			return err
		}
		if err := c.store.UpdateRoleRevision(name, unit.groupName, roleName, roleID, revision, hash); err != nil {
			return err
		}
	}
	if unit.roleName == "" {
		if err := c.store.UpdateServingGroupRevision(name, unit.groupName, revision); err != nil {
			return err
		}
	}
	return nil
}
