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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// State is stored in one Pod annotation. It is written in the same PATCH that
// changes the images, so a restarted controller can always tell an expected
// container restart from a crash, and can resume an unfinished update.
type State struct {
	// Revision is the ModelServing revision the Pod is being updated to.
	Revision string `json:"revision"`
	// StartedAt is used for the progress deadline.
	StartedAt metav1.Time `json:"startedAt"`
	// Patched holds, per container patched by the unfinished update, the target
	// image and the container that was running before the patch.
	Patched map[string]PatchedContainer `json:"patched,omitempty"`
	// Expected is the restart count each container may have because of in-place
	// updates. It accumulates over updates; anything above it is a crash.
	Expected map[string]int32 `json:"expected,omitempty"`
	// Done is set together with the revision label flip.
	Done bool `json:"done,omitempty"`
}

// PatchedContainer identifies the container instance an image patch replaces.
type PatchedContainer struct {
	Image string `json:"image"`
	// PrevContainerID and PrevImageID are empty when the container never started.
	PrevContainerID string `json:"prevContainerID,omitempty"`
	PrevImageID     string `json:"prevImageID,omitempty"`
}

// StateOf returns the Pod's in-place update state, or nil when there is none.
// Annotations written by the earlier state machine (versioned, with a phase) are
// migrated, so a controller upgrade keeps finished and unfinished updates intact.
func StateOf(pod *corev1.Pod) *State {
	raw, ok := pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey]
	if !ok {
		return nil
	}
	var probe struct {
		Version int    `json:"version"`
		Phase   string `json:"phase"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil
	}
	if probe.Version != 0 || probe.Phase != "" {
		return migrateLegacyState(raw)
	}
	state := &State{}
	if err := json.Unmarshal([]byte(raw), state); err != nil {
		return nil
	}
	return state
}

// legacyState is the annotation schema of the earlier in-place update implementation.
type legacyState struct {
	TargetRevision string      `json:"targetRevision"`
	Phase          string      `json:"phase"`
	StartedAt      metav1.Time `json:"startedAt"`
	// Baseline holds restart counts at patch time, or after completion once the phase is Done.
	Baseline map[string]struct {
		ContainerID  string `json:"containerID,omitempty"`
		RestartCount int32  `json:"restartCount"`
	} `json:"baseline,omitempty"`
	// Patched holds the containers the last patch changed; ImageID is the one before the patch.
	Patched map[string]struct {
		Image   string `json:"image"`
		ImageID string `json:"imageID,omitempty"`
	} `json:"patched,omitempty"`
}

func migrateLegacyState(raw string) *State {
	legacy := &legacyState{}
	if err := json.Unmarshal([]byte(raw), legacy); err != nil {
		return nil
	}
	state := &State{
		Revision:  legacy.TargetRevision,
		StartedAt: legacy.StartedAt,
		Done:      legacy.Phase == "Done",
		Expected:  map[string]int32{},
	}
	for name, baseline := range legacy.Baseline {
		state.Expected[name] = baseline.RestartCount
	}
	if state.Done {
		return state
	}
	state.Patched = map[string]PatchedContainer{}
	for name, patched := range legacy.Patched {
		// The restart of a patched container is expected on top of its baseline.
		state.Expected[name] = legacy.Baseline[name].RestartCount + 1
		state.Patched[name] = PatchedContainer{
			Image:           patched.Image,
			PrevContainerID: legacy.Baseline[name].ContainerID,
			PrevImageID:     patched.ImageID,
		}
	}
	return state
}

// InProgress reports whether an in-place update was started on the Pod and not yet finished.
func InProgress(pod *corev1.Pod) bool {
	return StateOf(pod).InProgress()
}

// Completed reports whether every patched container runs a new container instance
// with the target image and the Pod is Ready again.
func Completed(pod *corev1.Pod) bool {
	return StateOf(pod).Completed(pod)
}

// OnTarget is the package-level form of State.OnTarget.
func OnTarget(pod *corev1.Pod) bool {
	return StateOf(pod).OnTarget(pod)
}

// Updating reports whether the Pod is mid-update: started, but not yet Completed.
func Updating(pod *corev1.Pod) bool {
	return StateOf(pod).Updating(pod)
}

// UnexpectedRestart reports a restart not caused by an in-place update.
func UnexpectedRestart(pod *corev1.Pod) bool {
	return StateOf(pod).UnexpectedRestart(pod)
}

// The methods below take a State decoded once by StateOf, so a caller that needs
// several predicates for one Pod parses the annotation a single time. They are
// safe on a nil State, which stands for a Pod without in-place update state.

// InProgress is the method form of the package-level InProgress.
func (s *State) InProgress() bool {
	return s != nil && !s.Done
}

// Completed compares container IDs and images, instead of counting restarts. That
// rejects both the stale Ready condition kept until kubelet notices the patch and
// a crash of the old container, and it also covers a container that never started
// before (restart count stays 0).
func (s *State) Completed(pod *corev1.Pod) bool {
	// A crash on top of the update restart must go through recovery first.
	return s.OnTarget(pod) && !s.UnexpectedRestart(pod)
}

// OnTarget is Completed without the crash check: every patched container runs its target
// image in a fresh instance and the Pod is Ready. It lets recoveryPolicy None, which never
// restarts a crashed Pod, finish an update whose containers recovered by themselves.
func (s *State) OnTarget(pod *corev1.Pod) bool {
	if !s.InProgress() || !utils.IsPodRunningAndReady(pod) {
		return false
	}
	for name, patched := range s.Patched {
		// The live spec must still ask for the target image: a Pod edited meanwhile
		// could otherwise pass the runtime checks below while running another image.
		if !specRequests(pod, name, patched.Image) {
			return false
		}
		if status := containerStatus(pod, name); status == nil || !ranTarget(status, patched) {
			return false
		}
	}
	return true
}

// ranTarget reports whether the current container instance was started after the patch
// and runs the target image.
func ranTarget(status *corev1.ContainerStatus, patched PatchedContainer) bool {
	if status.ContainerID == "" || status.ContainerID == patched.PrevContainerID {
		return false
	}
	return sameImage(status.Image, patched.Image) || imageIDChanged(status.ImageID, patched.PrevImageID)
}

// Updating is the method form of the package-level Updating.
func (s *State) Updating(pod *corev1.Pod) bool {
	return s.InProgress() && !s.Completed(pod)
}

// UnexpectedRestart replaces a plain "container restarted" check. Restarts
// caused by an in-place update are expected and must not trigger recovery;
// any restart beyond them is a crash, also while an update is in progress.
func (s *State) UnexpectedRestart(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.RestartCount > 0 {
			return true
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		expected := int32(0)
		if s != nil {
			expected = s.Expected[status.Name]
		}
		if status.RestartCount > expected {
			return true
		}
		// The allowance is used up, but by a crash of the old container instead of the
		// update restart when the new instance does not run the target image.
		if patched, ok := s.patchedContainer(status.Name); ok && status.RestartCount == expected &&
			status.ContainerID != patched.PrevContainerID && status.ContainerID != "" && !ranTarget(&status, patched) {
			return true
		}
	}
	return false
}

func (s *State) patchedContainer(name string) (PatchedContainer, bool) {
	if !s.InProgress() {
		return PatchedContainer{}, false
	}
	patched, ok := s.Patched[name]
	return patched, ok
}

// TargetImages returns the images the Pod's containers should run in ms.
func TargetImages(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) map[string]string {
	role := findRole(ms, pod.Labels[workloadv1alpha1.RoleLabelKey])
	if role == nil {
		return nil
	}
	template := &role.EntryTemplate
	if pod.Labels[workloadv1alpha1.EntryLabelKey] != utils.Entry && role.WorkerTemplate != nil {
		template = role.WorkerTemplate
	}
	images := make(map[string]string, len(template.Spec.Containers))
	for _, container := range template.Spec.Containers {
		images[container.Name] = container.Image
	}
	return images
}

// ImagesToPatch selects, out of the target images, the containers that really have to
// change: those the update patched before, and those still running the image of the
// source template. A container whose live image matches neither was rewritten after
// creation (for example by a webhook). It is left alone when the template does not
// change it, and reported as drift otherwise, because overwriting it would be a guess.
func ImagesToPatch(pod *corev1.Pod, target, source map[string]string) (images map[string]string, drift []string) {
	prior := StateOf(pod)
	images = map[string]string{}
	for _, container := range pod.Spec.Containers {
		want, ok := target[container.Name]
		if !ok {
			continue
		}
		earlier, patchedBefore := prior.patchedContainer(container.Name)
		src, known := source[container.Name]
		if want == container.Image {
			// Already at the target without an update of ours: the runtime may still run the
			// old image and nothing would verify the switch, so it is not trusted.
			if known && src != want && !patchedBefore {
				drift = append(drift, container.Name)
			}
			continue
		}
		switch {
		// An earlier patch only proves ownership while the live image is still what it wrote.
		case (patchedBefore && container.Image == earlier.Image) || (known && container.Image == src):
			images[container.Name] = want
		case known && src == want:
		default:
			drift = append(drift, container.Name)
		}
	}
	sort.Strings(drift)
	return images, drift
}

func specRequests(pod *corev1.Pod, name, image string) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return container.Image == image
		}
	}
	return false
}

// MissingContainers returns the containers of the target template that the live Pod
// does not have, for example after admission renamed them. Such a Pod cannot be patched.
func MissingContainers(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) []string {
	var missing []string
	for name := range TargetImages(ms, pod) {
		found := false
		for _, container := range pod.Spec.Containers {
			if container.Name == name {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// sameImage compares image references after normalization, because the runtime
// reports "docker.io/library/nginx:latest" for a spec that says "nginx".
func sameImage(a, b string) bool {
	return normalizeImage(a) == normalizeImage(b)
}

// imageIDChanged is the fallback for runtimes that report a digest instead of the image
// name. Without a known previous ID (the container never started) it proves nothing.
func imageIDChanged(current, previous string) bool {
	return current != "" && previous != "" && current != previous
}

func normalizeImage(image string) string {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return image
	}
	if digested, ok := named.(reference.Digested); ok {
		// The digest identifies the image; a tag next to it is informational.
		return named.Name() + "@" + digested.Digest().String()
	}
	return reference.TagNameOnly(named).String()
}

// Start patches the images of pod and records the update state atomically.
// It is a no-op when the Pod is already being updated to revision. The
// resourceVersion precondition makes a concurrent double start conflict.
func Start(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, images map[string]string, revision string, now time.Time) (bool, error) {
	if state := StateOf(pod); state != nil && !state.Done && state.Revision == revision {
		return false, nil
	}
	state := State{Revision: revision, StartedAt: metav1.NewTime(now), Patched: map[string]PatchedContainer{}, Expected: map[string]int32{}}
	if prior := StateOf(pod); prior != nil {
		for name, count := range prior.Expected {
			state.Expected[name] = count
		}
		if !prior.Done {
			// Containers patched earlier must still be verified when the target is retargeted.
			for name, patched := range prior.Patched {
				state.Patched[name] = patched
			}
		}
	}
	containers := make([]map[string]string, 0, len(pod.Spec.Containers))
	for _, container := range pod.Spec.Containers {
		image, ok := images[container.Name]
		if !ok || image == container.Image {
			continue
		}
		patched := PatchedContainer{Image: image}
		if status := containerStatus(pod, container.Name); status != nil {
			patched.PrevContainerID, patched.PrevImageID = status.ContainerID, status.ImageID
		}
		state.Patched[container.Name] = patched
		// Restarts that happened before the patch stay behind the baseline; only the update restart is added.
		if status := containerStatus(pod, container.Name); status != nil && status.RestartCount > state.Expected[container.Name] {
			state.Expected[container.Name] = status.RestartCount
		}
		state.Expected[container.Name]++
		containers = append(containers, map[string]string{"name": container.Name, "image": image})
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	patch := map[string]interface{}{
		"metadata": preconditionedMetadata(pod, map[string]interface{}{
			"annotations": map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: string(raw)},
		}),
	}
	if len(containers) > 0 {
		patch["spec"] = map[string]interface{}{"containers": containers}
	}
	return true, applyPatch(ctx, client, pod, patch)
}

// Complete flips the revision label (and role template hash when roleHash is
// non-empty) and marks the state done. It must only run once every Pod of the
// update unit is Completed, so a unit never carries mixed revisions.
func Complete(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, revision, roleHash string) error {
	state := StateOf(pod)
	if state == nil {
		state = &State{Revision: revision, StartedAt: metav1.Now()}
	}
	state.Done = true
	// Drop allowances the update did not use (first start, coalesced patches),
	// otherwise a later real crash would hide behind them.
	for name := range state.Patched {
		if status := containerStatus(pod, name); status != nil && status.RestartCount < state.Expected[name] {
			state.Expected[name] = status.RestartCount
		}
	}
	state.Patched = nil
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	podLabels := map[string]string{workloadv1alpha1.RevisionLabelKey: revision}
	if roleHash != "" {
		podLabels[workloadv1alpha1.RoleTemplateHashLabelKey] = roleHash
	}
	return applyPatch(ctx, client, pod, map[string]interface{}{
		"metadata": preconditionedMetadata(pod, map[string]interface{}{
			"labels":      podLabels,
			"annotations": map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: string(raw)},
		}),
	})
}

// preconditionedMetadata pins the patch to the observed Pod instance. A conflict
// is retried with fresh state, and a Pod recreated under the same name is never
// marked done by a decision made about its predecessor.
func preconditionedMetadata(pod *corev1.Pod, metadata map[string]interface{}) map[string]interface{} {
	if pod.ResourceVersion != "" {
		metadata["resourceVersion"] = pod.ResourceVersion
	}
	if pod.UID != "" {
		metadata["uid"] = pod.UID
	}
	return metadata
}

func applyPatch(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, patch map[string]interface{}) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if _, err := client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, data, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}
