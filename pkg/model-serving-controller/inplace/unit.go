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

package inplace

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	ReasonFallback = "InPlaceUpdateFallback"

	ReasonIneligible             = "InPlaceUpdateIneligible"
	ReasonRevisionHistoryMissing = "RevisionHistoryMissing"
	ReasonUnexplainedImageDrift  = "UnexplainedImageDrift"
	ReasonRetargetNotSupported   = "InPlaceRetargetNotSupported"
)

// Fallback is why a unit is recreated instead of updated in place.
type Fallback struct {
	Reason  string
	Message string
}

func (f *Fallback) String() string {
	return f.Reason + ": " + f.Message
}

func fallback(reason, format string, args ...interface{}) *Fallback {
	return &Fallback{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

type templateKey struct {
	role  string
	entry bool
}

// PodTemplate is what a Pod rendered from one template looks like at a revision.
type PodTemplate struct {
	Images           map[string]string
	RoleTemplateHash string
}

type RevisionTemplates struct {
	Revision  string
	templates map[templateKey]PodTemplate
}

// NewRevisionTemplates requires ms to be the ModelServing at revision.
func NewRevisionTemplates(ms *workloadv1alpha1.ModelServing, revision string) *RevisionTemplates {
	result := &RevisionTemplates{Revision: revision, templates: map[templateKey]PodTemplate{}}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		hash := utils.CalRoleTemplateHash(*role)
		result.templates[templateKey{role: role.Name, entry: true}] = newPodTemplate(&role.EntryTemplate, hash)
		if role.WorkerTemplate != nil {
			result.templates[templateKey{role: role.Name}] = newPodTemplate(role.WorkerTemplate, hash)
		}
	}
	return result
}

func newPodTemplate(template *workloadv1alpha1.PodTemplateSpec, hash string) PodTemplate {
	images := make(map[string]string, len(template.Spec.Containers))
	for _, container := range template.Spec.Containers {
		images[container.Name] = container.Image
	}
	return PodTemplate{Images: images, RoleTemplateHash: hash}
}

func (r *RevisionTemplates) ForPod(pod *corev1.Pod) (PodTemplate, bool) {
	key := templateKey{
		role:  utils.GetRoleName(pod),
		entry: pod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry,
	}
	template, ok := r.templates[key]
	return template, ok
}

func (t PodTemplate) Labels(revision string) map[string]string {
	return map[string]string{
		workloadv1alpha1.RevisionLabelKey:         revision,
		workloadv1alpha1.RoleTemplateHashLabelKey: t.RoleTemplateHash,
	}
}

// UnitPod is one Pod of a unit with the template it must match at the target revision.
type UnitPod struct {
	Pod    *corev1.Pod
	Target PodTemplate
}

func (p *UnitPod) HasTargetLabels(revision string) bool {
	return utils.ObjectRevision(p.Pod) == revision && utils.ObjectRoleTemplateHash(p.Pod) == p.Target.RoleTemplateHash
}

// CheckPatchable verifies that an image patch can move the live Pod from the source template to the target template.
// A live image that differs from the source image is drift, which a patch must not overwrite.
// imagePullPolicy is not compared, because admission may mutate it.
func CheckPatchable(pod *corev1.Pod, source, target PodTemplate) *Fallback {
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		return fallback(ReasonIneligible, "pod %s has restartPolicy %q, want %q", pod.Name, pod.Spec.RestartPolicy, corev1.RestartPolicyAlways)
	}
	if len(pod.Spec.Containers) != len(target.Images) {
		return fallback(ReasonIneligible, "pod %s has %d containers, the target revision has %d", pod.Name, len(pod.Spec.Containers), len(target.Images))
	}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if _, ok := target.Images[container.Name]; !ok {
			return fallback(ReasonIneligible, "pod %s has container %q, which the target revision does not have", pod.Name, container.Name)
		}
		if container.RestartPolicy != nil || len(container.RestartPolicyRules) > 0 {
			return fallback(ReasonIneligible, "container %q of pod %s sets its own restart policy", container.Name, pod.Name)
		}
		if sourceImage := source.Images[container.Name]; container.Image != sourceImage {
			return fallback(ReasonUnexplainedImageDrift, "container %q of pod %s runs image %q, but its source revision has %q",
				container.Name, pod.Name, container.Image, sourceImage)
		}
	}
	return nil
}

// Verify reports whether the Pod has finished its update to the target revision, or explains what it is waiting for.
// A changed container must also run the patched image after kubelet restarted it.
func Verify(p *UnitPod, state *State, revision string) (bool, string) {
	pod := p.Pod
	switch {
	case state == nil || state.Phase != PhasePatched || state.TargetRevision != revision:
		return false, fmt.Sprintf("pod %s is not patched to revision %s", pod.Name, revision)
	case pod.DeletionTimestamp != nil:
		return false, fmt.Sprintf("pod %s is terminating", pod.Name)
	case !p.HasTargetLabels(revision):
		return false, fmt.Sprintf("pod %s does not carry the labels of revision %s", pod.Name, revision)
	case pod.Spec.RestartPolicy != corev1.RestartPolicyAlways:
		return false, fmt.Sprintf("pod %s has restartPolicy %q", pod.Name, pod.Spec.RestartPolicy)
	}

	statuses := make(map[string]*corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for i := range pod.Status.ContainerStatuses {
		statuses[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
	}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		want := p.Target.Images[container.Name]
		if container.Image != want {
			return false, fmt.Sprintf("container %q of pod %s does not match revision %s", container.Name, pod.Name, revision)
		}
		status, ok := statuses[container.Name]
		if !ok {
			return false, fmt.Sprintf("container %q of pod %s has no status yet", container.Name, pod.Name)
		}
		if patched, changed := state.Patched[container.Name]; changed {
			if !patched.ImageApplied(status) {
				return false, fmt.Sprintf("container %q of pod %s is not running image %q yet", container.Name, pod.Name, want)
			}
			if !restartedSincePatch(status, patched, state.Baseline[container.Name]) {
				return false, fmt.Sprintf("container %q of pod %s has not restarted on image %q yet", container.Name, pod.Name, want)
			}
		}
		if !status.Ready {
			return false, fmt.Sprintf("container %q of pod %s is not ready", container.Name, pod.Name)
		}
	}
	if !podConditionTrue(pod, corev1.ContainersReady) {
		return false, fmt.Sprintf("containers of pod %s are not ready", pod.Name)
	}
	return true, ""
}

// restartedSincePatch reports whether kubelet has started the container again after the image patch.
// A container never created before the patch starts for the first time instead.
func restartedSincePatch(status *corev1.ContainerStatus, patched PatchedContainer, baseline ContainerBaseline) bool {
	if status.RestartCount > patched.RestartCount {
		return true
	}
	return baseline.ContainerID == "" && patched.RestartCount == 0 && status.ContainerID != ""
}

func podConditionTrue(pod *corev1.Pod, conditionType corev1.PodConditionType) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Step is the next action for a unit.
type Step string

const (
	// StepNone means every Pod carries the target revision and no update is unfinished.
	StepNone Step = "None"
	// StepMark records the update and the restart baseline on Pods without an unfinished update.
	StepMark Step = "Mark"
	// StepPatch patches the images of Pods that are marked but not patched.
	StepPatch Step = "Patch"
	// StepVerify waits for patched Pods to run and be ready on the target images.
	StepVerify Step = "Verify"
	// StepComplete finishes the update of every Pod once the whole unit is verified.
	StepComplete Step = "Complete"
	// StepRecreate means the unit is recreated instead.
	StepRecreate Step = "Recreate"
)

// Plan is the next step for a unit and the Pods it applies to.
type Plan struct {
	Step Step
	Pods []*UnitPod
	// Message explains why the unit is waiting or recreated.
	Message string
	Reason  string
}

// PlanUnit derives the next step of a unit from its Pods alone, so a restarted controller resumes from the same step.
// Every Pod is marked before any is patched, and no Pod completes until every Pod is verified.
// The traffic drain is not implemented yet, so marked Pods are patched right away.
func PlanUnit(ownerUID types.UID, revision string, pods []*UnitPod) Plan {
	var unmarked, draining, verified []*UnitPod
	inUpdate := false
	waiting := ""
	for _, p := range pods {
		state := ownedState(p.Pod, ownerUID)
		if state == nil || !state.InProgress() {
			// A Pod that completed its update, or was recreated on the target revision, needs no update.
			if !p.HasTargetLabels(revision) {
				unmarked = append(unmarked, p)
			}
			continue
		}
		inUpdate = true
		if state.TargetRevision != revision {
			return Plan{Step: StepRecreate, Reason: ReasonRetargetNotSupported,
				Message: fmt.Sprintf("pod %s is updating to revision %s, and changing the target of an unfinished in-place update is not supported yet", p.Pod.Name, state.TargetRevision)}
		}
		switch state.Phase {
		case PhaseDraining:
			draining = append(draining, p)
		case PhasePatched:
			if ok, message := Verify(p, state, revision); ok {
				verified = append(verified, p)
			} else if waiting == "" {
				waiting = message
			}
		}
	}

	switch {
	case !inUpdate && len(unmarked) == 0:
		return Plan{Step: StepNone}
	case len(unmarked) > 0:
		return Plan{Step: StepMark, Pods: unmarked}
	case len(draining) > 0:
		return Plan{Step: StepPatch, Pods: draining}
	case waiting != "":
		return Plan{Step: StepVerify, Message: waiting}
	default:
		return Plan{Step: StepComplete, Pods: verified}
	}
}

// ownedState returns the Pod's matching state only if it was written for the ModelServing with ownerUID.
func ownedState(pod *corev1.Pod, ownerUID types.UID) *State {
	state := MatchingState(pod)
	if state == nil || state.OwnerUID != ownerUID {
		return nil
	}
	return state
}

// InUpdate reports whether any Pod has an unfinished update written for the ModelServing with ownerUID.
func InUpdate(pods []*corev1.Pod, ownerUID types.UID) bool {
	for _, pod := range pods {
		if state := ownedState(pod, ownerUID); state != nil && state.InProgress() {
			return true
		}
	}
	return false
}

// UnitRoleTemplateHash returns a Role template hash that keeps a Role instance with an unfinished update outdated.
func UnitRoleTemplateHash(pods []*corev1.Pod, ownerUID types.UID, expectedHash string) (string, bool) {
	found := false
	for _, pod := range pods {
		state := ownedState(pod, ownerUID)
		if state == nil || !state.InProgress() {
			continue
		}
		found = true
		for _, hash := range []string{state.SourceRoleTemplateHash, utils.ObjectRoleTemplateHash(pod)} {
			if hash != "" && hash != expectedHash {
				return hash, true
			}
		}
	}
	return "", found
}

// UnitRevision returns the store revision of a unit with an unfinished update, which keeps the unit outdated.
// It is the source revision, or the target revision after a revert to the source revision.
func UnitRevision(pods []*corev1.Pod, ownerUID types.UID, revision string) (string, bool) {
	unitRevision, found := revision, false
	for _, pod := range pods {
		state := ownedState(pod, ownerUID)
		if state == nil || !state.InProgress() {
			continue
		}
		found = true
		if unitRevision != revision {
			continue
		}
		if state.SourceRevision != revision {
			unitRevision = state.SourceRevision
		} else {
			unitRevision = state.TargetRevision
		}
	}
	return unitRevision, found
}
