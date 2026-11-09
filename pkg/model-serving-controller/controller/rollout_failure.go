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
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	rolloutWorkloadFailure       = "WorkloadFailure"
	rolloutInfrastructureBlocked = "InfrastructureBlocked"
	rolloutRollbackNotApplicable = "RollbackNotApplicable"
	rolloutRollbackFailed        = "RollbackFailed"
	rolloutRolledBack            = "RolledBack"
	rolloutResumed               = "RolloutResumed"

	automaticRollbackEventReason = "AutomaticRollback"
	rollbackFieldManager         = "kthena-rollback"

	maxRolloutFailedMessageLength = 512
	maxRolloutEvidence            = 5
)

// workloadWaitingReasons are container waiting reasons caused by the Pod template itself.
var workloadWaitingReasons = sets.New(
	"ErrImagePull", "ImagePullBackOff", "InvalidImageName",
	"CreateContainerConfigError", "CreateContainerError", "RunContainerError",
	"CrashLoopBackOff",
)

// rolloutUnits counts the rollout units on the target revision.
type rolloutUnits struct {
	target int32
	ready  int32
}

// rolloutWaiting reports whether some target unit has not been Ready yet.
// A unit that was Ready and then failed is left to recovery, so a long-held canary never fails on one crash.
func rolloutWaiting(units rolloutUnits, progress *workloadv1alpha1.RolloutProgress) bool {
	return units.target > progress.MaxReadyUnits
}

type rolloutEvent struct {
	eventType string
	reason    string
	message   string
}

// rolloutEvaluation is the outcome of one status pass, applied only after the status write succeeds.
type rolloutEvaluation struct {
	changed      bool
	events       []rolloutEvent
	failureKey   rolloutFailureKey
	consumed     int32
	requeueAfter time.Duration
}

type rolloutFailureKey struct {
	uid      types.UID
	revision string
}

// rolloutFailureRecorder holds workload failures observed on Pods until a status write persists them.
type rolloutFailureRecorder struct {
	mu     sync.Mutex
	counts map[rolloutFailureKey]int32
}

func (r *rolloutFailureRecorder) add(key rolloutFailureKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[rolloutFailureKey]int32{}
	}
	r.counts[key]++
}

func (r *rolloutFailureRecorder) peek(key rolloutFailureKey) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[key]
}

func (r *rolloutFailureRecorder) consume(key rolloutFailureKey, n int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[key] <= n {
		delete(r.counts, key)
		return
	}
	r.counts[key] -= n
}

func rolloutFailurePolicy(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.RolloutFailurePolicy {
	if ms.Spec.RolloutStrategy == nil {
		return nil
	}
	return ms.Spec.RolloutStrategy.FailurePolicy
}

// rolloutHalted reports whether a failed rollout towards revision must not replace more outdated units.
func rolloutHalted(ms *workloadv1alpha1.ModelServing, revision string) bool {
	if rolloutFailurePolicy(ms) == nil {
		return false
	}
	progress := ms.Status.RolloutProgress
	if progress == nil || progress.Revision != revision {
		return false
	}
	return apiMeta.IsStatusConditionTrue(ms.Status.Conditions, string(workloadv1alpha1.ModelServingRolloutFailed))
}

func podDisrupted(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.DisruptionTarget && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// podRolloutRevision is the revision a Pod is moving to: an unfinished in-place update keeps its old label.
func podRolloutRevision(pod *corev1.Pod) string {
	if state := inplace.StateOf(pod); state.InProgress() {
		return state.Revision
	}
	return utils.ObjectRevision(pod)
}

// recordRolloutFailure counts a failed Pod against the tracked rollout when the Pod's own revision caused it.
func (c *ModelServingController) recordRolloutFailure(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) {
	if podDisrupted(pod) {
		return
	}
	c.recordRolloutRevisionFailure(ms, podRolloutRevision(pod))
}

func (c *ModelServingController) recordRolloutRevisionFailure(ms *workloadv1alpha1.ModelServing, revision string) {
	progress := ms.Status.RolloutProgress
	if rolloutFailurePolicy(ms) == nil || progress == nil || progress.Revision != revision {
		return
	}
	c.rolloutFailures.add(rolloutFailureKey{uid: ms.UID, revision: revision})
	c.enqueueModelServing(ms)
}

// classifyRolloutPod explains why a target Pod is not Ready.
// It returns a workload reason when the Pod template is at fault, and an infrastructure reason otherwise.
func classifyRolloutPod(pod *corev1.Pod) (workload, infrastructure string) {
	if podDisrupted(pod) {
		return "", "disrupted"
	}
	if pod.Status.Phase == corev1.PodFailed {
		return "failed", ""
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if status.State.Waiting != nil && workloadWaitingReasons.Has(status.State.Waiting.Reason) {
				return status.State.Waiting.Reason, ""
			}
		}
	}
	if inplace.UnexpectedRestart(pod) {
		return "restarted", ""
	}
	if pod.Spec.NodeName == "" {
		return "", "unscheduled"
	}
	if pod.Status.Phase == corev1.PodRunning && allContainersRunning(pod) {
		return "not ready", ""
	}
	return "", "starting"
}

func allContainersRunning(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) < len(pod.Spec.Containers) {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running == nil {
			return false
		}
	}
	return true
}

// collectRolloutEvidence classifies the target Pods that are not Ready.
func (c *ModelServingController) collectRolloutEvidence(ms *workloadv1alpha1.ModelServing, revision string) (workload, infrastructure []string) {
	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	pods, err := c.podsLister.Pods(ms.Namespace).List(selector)
	if err != nil {
		klog.Warningf("list Pods of ModelServing %s/%s for rollout evidence: %v", ms.Namespace, ms.Name, err)
		return nil, nil
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for _, pod := range pods {
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) || utils.IsPodTerminating(pod) ||
			podRolloutRevision(pod) != revision || utils.IsPodRunningAndReady(pod) {
			continue
		}
		workloadReason, infrastructureReason := classifyRolloutPod(pod)
		if workloadReason != "" {
			workload = append(workload, fmt.Sprintf("Pod %s: %s", pod.Name, workloadReason))
		} else {
			infrastructure = append(infrastructure, fmt.Sprintf("Pod %s: %s", pod.Name, infrastructureReason))
		}
	}
	return workload, infrastructure
}

func summarizeEvidence(evidence []string) string {
	if len(evidence) > maxRolloutEvidence {
		return strings.Join(evidence[:maxRolloutEvidence], "; ") + fmt.Sprintf("; and %d more", len(evidence)-maxRolloutEvidence)
	}
	return strings.Join(evidence, "; ")
}

// countRoleRolloutUnits counts Role instances on the target templates under RoleRollingUpdate.
func (c *ModelServingController) countRoleRolloutUnits(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup) rolloutUnits {
	var units rolloutUnits
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			continue
		}
		for roleName, instances := range roles {
			for _, instance := range instances {
				if instance == nil || instance.Status == datastore.RoleDeleting {
					continue
				}
				if c.compareRoleTemplate(ctx, ms, group, roleName, *instance) != templateEquivalent {
					continue
				}
				units.target++
				if instance.Status == datastore.RoleRunning {
					units.ready++
				}
			}
		}
	}
	return units
}

// setRolloutFailed updates the RolloutFailed condition and queues an event when it transitions.
func setRolloutFailed(ms *workloadv1alpha1.ModelServing, status metav1.ConditionStatus, reason, message string, eval *rolloutEvaluation) {
	if text := []rune(message); len(text) > maxRolloutFailedMessageLength {
		message = string(text[:maxRolloutFailedMessageLength-3]) + "..."
	}
	conditionType := string(workloadv1alpha1.ModelServingRolloutFailed)
	existing := apiMeta.FindStatusCondition(ms.Status.Conditions, conditionType)
	if existing != nil && existing.Status == status && existing.Reason == reason &&
		existing.Message == message && existing.ObservedGeneration == ms.Generation {
		return
	}
	transition := existing == nil || existing.Status != status || existing.Reason != reason
	apiMeta.SetStatusCondition(&ms.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ms.Generation,
	})
	eval.changed = true
	if transition {
		eventType := corev1.EventTypeNormal
		if status == metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		eval.events = append(eval.events, rolloutEvent{eventType: eventType, reason: reason, message: message})
	}
}

// resumeRollout lifts a reported failure, and with it a Halt.
func resumeRollout(ms *workloadv1alpha1.ModelServing, message string, eval *rolloutEvaluation) {
	if apiMeta.IsStatusConditionTrue(ms.Status.Conditions, string(workloadv1alpha1.ModelServingRolloutFailed)) {
		setRolloutFailed(ms, metav1.ConditionFalse, rolloutResumed, message, eval)
	}
}

// evaluateRollout tracks the rollout towards revision on the status copy and decides whether it failed.
// It changes only the copy; the caller persists it and then runs the returned events and requeue.
func (c *ModelServingController) evaluateRollout(ctx context.Context, latest, out *workloadv1alpha1.ModelServing,
	revision string, units rolloutUnits, complete bool) rolloutEvaluation {
	eval := rolloutEvaluation{failureKey: rolloutFailureKey{uid: latest.UID, revision: revision}}
	status := &out.Status
	policy := rolloutFailurePolicy(latest)
	if policy == nil {
		if status.RolloutProgress != nil {
			status.RolloutProgress = nil
			eval.changed = true
		}
		if apiMeta.RemoveStatusCondition(&status.Conditions, string(workloadv1alpha1.ModelServingRolloutFailed)) {
			eval.changed = true
		}
		return eval
	}

	c.settleRollback(ctx, latest, out, revision, complete, &eval)

	progress := status.RolloutProgress
	if progress != nil && progress.Revision != revision {
		progress = nil
		resumeRollout(out, fmt.Sprintf("The spec selects revision %s", revision), &eval)
	}
	if complete {
		if progress != nil {
			resumeRollout(out, fmt.Sprintf("Rollout to revision %s completed", revision), &eval)
		}
		progress = nil
	} else if progress == nil {
		// Only a template change starts tracking; scaling or recovery of the same revision is not a rollout.
		previousUpdate, previousCurrent := latest.Status.UpdateRevision, latest.Status.CurrentRevision
		if (previousUpdate != "" && previousUpdate != revision) || (previousCurrent != "" && previousCurrent != revision) {
			progress = &workloadv1alpha1.RolloutProgress{
				Revision:         revision,
				MaxTargetUnits:   units.target,
				MaxReadyUnits:    units.ready,
				LastProgressTime: metav1.Now(),
			}
		}
	}
	if progress == nil {
		if status.RolloutProgress != nil {
			status.RolloutProgress = nil
			eval.changed = true
		}
		return eval
	}
	if status.RolloutProgress != progress {
		status.RolloutProgress = progress
		eval.changed = true
	}

	// High-water marks make recreating a crashed unit no progress.
	if units.target > progress.MaxTargetUnits || units.ready > progress.MaxReadyUnits {
		progress.MaxTargetUnits = max(progress.MaxTargetUnits, units.target)
		progress.MaxReadyUnits = max(progress.MaxReadyUnits, units.ready)
		progress.LastProgressTime = metav1.Now()
		eval.changed = true
		resumeRollout(out, fmt.Sprintf("Rollout to revision %s made progress", revision), &eval)
	}
	if n := c.rolloutFailures.peek(eval.failureKey); n > 0 {
		progress.WorkloadFailures += n
		eval.consumed = n
		eval.changed = true
	}

	// A workload failure is final for this revision; an infrastructure failure is re-examined for new evidence.
	failed := apiMeta.FindStatusCondition(status.Conditions, string(workloadv1alpha1.ModelServingRolloutFailed))
	if failed != nil && failed.Status == metav1.ConditionTrue && failed.Reason != rolloutInfrastructureBlocked {
		return eval
	}
	if !rolloutWaiting(units, progress) {
		return eval
	}
	deadline, remaining := inPlaceDeadline(latest, progress.LastProgressTime.Time)
	if remaining > 0 {
		eval.requeueAfter = remaining
		return eval
	}

	workloadEvidence, infrastructureEvidence := c.collectRolloutEvidence(latest, revision)
	if progress.WorkloadFailures == 0 && len(workloadEvidence) == 0 {
		message := fmt.Sprintf("Revision %s made no progress for %s and no failure points to the revision", revision, deadline)
		if len(infrastructureEvidence) > 0 {
			message += ": " + summarizeEvidence(infrastructureEvidence)
		}
		setRolloutFailed(out, metav1.ConditionTrue, rolloutInfrastructureBlocked, message, &eval)
		return eval
	}

	failure := fmt.Sprintf("Revision %s made no progress for %s after %d failures", revision, deadline, progress.WorkloadFailures)
	if len(workloadEvidence) > 0 {
		failure += ": " + summarizeEvidence(workloadEvidence)
	}
	if policy.Action != workloadv1alpha1.RolloutFailureRollback {
		setRolloutFailed(out, metav1.ConditionTrue, rolloutWorkloadFailure, failure, &eval)
		return eval
	}
	if reason, blocker := c.rollbackBlocker(ctx, latest, out, revision); reason != "" {
		setRolloutFailed(out, metav1.ConditionTrue, reason, failure+"; not rolled back: "+blocker, &eval)
		return eval
	}

	// Record the intent first; the next sync writes the spec once the cache has observed it.
	target := latest.Status.CurrentRevision
	status.LastRollback = &workloadv1alpha1.AutomaticRollback{
		FromRevision: revision,
		ToRevision:   target,
		Phase:        workloadv1alpha1.RollbackPending,
		Reason:       failure,
		StartTime:    metav1.Now(),
	}
	setRolloutFailed(out, metav1.ConditionTrue, rolloutWorkloadFailure, failure+"; rolling back to revision "+target, &eval)
	return eval
}

// settleRollback advances a recorded rollback from what the current spec and rollout show.
func (c *ModelServingController) settleRollback(ctx context.Context, latest, out *workloadv1alpha1.ModelServing,
	revision string, complete bool, eval *rolloutEvaluation) {
	rollback := out.Status.LastRollback
	if rollback == nil {
		return
	}
	switch rollback.Phase {
	case workloadv1alpha1.RollbackPending:
		if revision == rollback.FromRevision {
			return
		}
		// The restored spec can resolve to a new identity for legacy history, so compare templates.
		if c.specMatchesRevision(ctx, latest, revision, rollback.ToRevision) {
			message := fmt.Sprintf("Rolled back from revision %s to revision %s", rollback.FromRevision, revision)
			rollback.Phase = workloadv1alpha1.RollbackApplied
			rollback.ToRevision = revision
			setRolloutFailed(out, metav1.ConditionFalse, rolloutRolledBack, message, eval)
		} else {
			rollback.Phase = workloadv1alpha1.RollbackAbandoned
			eval.events = append(eval.events, rolloutEvent{
				eventType: corev1.EventTypeNormal,
				reason:    automaticRollbackEventReason,
				message:   fmt.Sprintf("Rollback from revision %s abandoned because the spec changed", rollback.FromRevision),
			})
		}
		eval.changed = true
	case workloadv1alpha1.RollbackApplied:
		if complete && revision == rollback.ToRevision {
			rollback.Phase = workloadv1alpha1.RollbackCompleted
			eval.changed = true
		}
	}
	// A later completed rollout ends the once-per-revision protection.
	if complete && revision != rollback.ToRevision && rollback.Phase != workloadv1alpha1.RollbackPending {
		out.Status.LastRollback = nil
		eval.changed = true
	}
}

// specMatchesRevision compares the spec with a snapshot using the normalized inputs that define revision identity.
func (c *ModelServingController) specMatchesRevision(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision, target string) bool {
	snapshot, err := c.revisionHistory(ctx, ms).workload(ctx, target)
	if err != nil {
		return false
	}
	equal, err := utils.EqualModelServingRevisions(revision, target, ms, snapshot)
	return err == nil && equal
}

// rollbackBlocker returns the condition reason and explanation when the failed revision cannot be rolled back.
func (c *ModelServingController) rollbackBlocker(ctx context.Context, latest, out *workloadv1alpha1.ModelServing, revision string) (string, string) {
	if rollback := out.Status.LastRollback; rollback != nil {
		if rollback.Phase == workloadv1alpha1.RollbackApplied && rollback.ToRevision == revision {
			return rolloutRollbackFailed, fmt.Sprintf("the rollback from revision %s did not complete", rollback.FromRevision)
		}
		if rollback.FromRevision == revision && rollback.Phase != workloadv1alpha1.RollbackAbandoned {
			return rolloutRollbackNotApplicable, "the revision was already rolled back once"
		}
	}
	if latest.DeletionTimestamp != nil {
		return rolloutRollbackNotApplicable, "the ModelServing is being deleted"
	}
	target := latest.Status.CurrentRevision
	if target == "" || target == revision {
		return rolloutRollbackNotApplicable, "no completed revision to roll back to"
	}
	snapshot, err := c.revisionHistory(ctx, latest).workload(ctx, target)
	if err != nil {
		return rolloutRollbackNotApplicable, fmt.Sprintf("revision %s cannot be resolved: %v", target, err)
	}
	if utils.EffectiveSchedulerName(snapshot.Spec.SchedulerName) != utils.EffectiveSchedulerName(latest.Spec.SchedulerName) {
		return rolloutRollbackNotApplicable, fmt.Sprintf("revision %s uses a different scheduler", target)
	}
	if !samePlugins(snapshot, latest) {
		return rolloutRollbackNotApplicable, fmt.Sprintf("revision %s uses different plugins", target)
	}
	return "", ""
}

// samePlugins compares plugins the way the update webhook does, because it rejects any plugin change.
func samePlugins(a, b *workloadv1alpha1.ModelServing) bool {
	canonical := func(ms *workloadv1alpha1.ModelServing) ([]byte, error) {
		return utils.BuildRevisionData(&workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{Plugins: ms.Spec.Plugins},
		})
	}
	left, err := canonical(a)
	if err != nil {
		return false
	}
	right, err := canonical(b)
	return err == nil && bytes.Equal(left, right)
}

// restoreRoleTemplates returns ms with each Role's templates and worker count taken from the snapshot.
// Everything else, including replicas, rollout controls, plugins and metadata, is kept.
func restoreRoleTemplates(ms, snapshot *workloadv1alpha1.ModelServing) (*workloadv1alpha1.ModelServing, error) {
	historical := make(map[string]workloadv1alpha1.Role, len(snapshot.Spec.Template.Roles))
	for _, role := range snapshot.Spec.Template.Roles {
		historical[role.Name] = role
	}
	restored := ms.DeepCopy()
	for i := range restored.Spec.Template.Roles {
		role := &restored.Spec.Template.Roles[i]
		source, ok := historical[role.Name]
		if !ok {
			return nil, fmt.Errorf("revision snapshot has no Role %s", role.Name)
		}
		// Snapshots fill in the Pod schedulerName that the controller sets, so keep the user's value.
		entryScheduler := role.EntryTemplate.Spec.SchedulerName
		workerScheduler := ""
		if role.WorkerTemplate != nil {
			workerScheduler = role.WorkerTemplate.Spec.SchedulerName
		}
		role.EntryTemplate = *source.EntryTemplate.DeepCopy()
		role.EntryTemplate.Spec.SchedulerName = entryScheduler
		role.WorkerTemplate = source.WorkerTemplate.DeepCopy()
		if role.WorkerTemplate != nil {
			role.WorkerTemplate.Spec.SchedulerName = workerScheduler
		}
		role.WorkerReplicas = source.WorkerReplicas
	}
	return restored, nil
}

// applyPendingRollback writes the restored spec for a rollback whose intent is already in status.
// It reports true when it wrote the spec, so the caller stops this sync and waits for the update event.
func (c *ModelServingController) applyPendingRollback(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision string) (bool, error) {
	rollback := ms.Status.LastRollback
	if rolloutFailurePolicy(ms) == nil || rollback == nil || rollback.Phase != workloadv1alpha1.RollbackPending ||
		revision != rollback.FromRevision || ms.DeletionTimestamp != nil {
		return false, nil
	}
	snapshot, err := c.revisionHistory(ctx, ms).workload(ctx, rollback.ToRevision)
	if err != nil {
		return false, fmt.Errorf("resolve rollback revision %s: %w", rollback.ToRevision, err)
	}
	restored, err := restoreRoleTemplates(ms, snapshot)
	if err != nil {
		return false, err
	}
	// The object's resourceVersion makes a concurrent edit win; that edit then abandons the rollback.
	if _, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, restored, metav1.UpdateOptions{FieldManager: rollbackFieldManager}); err != nil {
		return false, fmt.Errorf("restore revision %s: %w", rollback.ToRevision, err)
	}
	klog.V(2).Infof("Rolled back ModelServing %s/%s from revision %s to revision %s", ms.Namespace, ms.Name, rollback.FromRevision, rollback.ToRevision)
	return true, nil
}

// finishRolloutEvaluation emits events and schedules the deadline check after a successful status pass.
func (c *ModelServingController) finishRolloutEvaluation(ms *workloadv1alpha1.ModelServing, eval rolloutEvaluation, persisted bool) {
	if persisted {
		if eval.consumed > 0 {
			c.rolloutFailures.consume(eval.failureKey, eval.consumed)
		}
		for _, event := range eval.events {
			c.emitRoleStatusEvent(ms, event.eventType, event.reason, event.message)
		}
	}
	if eval.requeueAfter > 0 {
		c.enqueueModelServingAfter(ms, eval.requeueAfter+time.Second)
	}
}
