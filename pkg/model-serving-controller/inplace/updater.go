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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// PodUpdater moves a Pod through the update phases with one guarded patch per step.
// On error, callers must re-read the Pod instead of retrying with it.
type PodUpdater struct {
	client kubernetes.Interface
	now    func() metav1.Time
}

func NewPodUpdater(client kubernetes.Interface) *PodUpdater {
	return &PodUpdater{client: client, now: metav1.Now}
}

// Mark starts an update of the Pod from sourceRevision to targetRevision and records its restart baseline.
// It is a no-op when the Pod is already updating to targetRevision.
func (u *PodUpdater) Mark(ctx context.Context, pod *corev1.Pod, ownerUID types.UID, unit, sourceRevision, targetRevision string) (*corev1.Pod, error) {
	if current := MatchingState(pod); current != nil && current.InProgress() {
		if current.TargetRevision == targetRevision {
			return pod, nil
		}
		return nil, fmt.Errorf("pod %s/%s is already updating to revision %s", pod.Namespace, pod.Name, current.TargetRevision)
	}
	if !utils.HasModelServingOwner(pod, ownerUID) {
		return nil, fmt.Errorf("pod %s/%s is not owned by ModelServing %s", pod.Namespace, pod.Name, ownerUID)
	}
	state := &State{
		Version:                StateVersion,
		OwnerUID:               ownerUID,
		PodUID:                 pod.UID,
		Unit:                   unit,
		SourceRevision:         sourceRevision,
		TargetRevision:         targetRevision,
		SourceRoleTemplateHash: utils.ObjectRoleTemplateHash(pod),
		Phase:                  PhaseDraining,
		StartedAt:              u.now(),
		Baseline:               BaselineFromStatus(pod),
	}
	return u.patch(ctx, pod, PatchOptions{State: state})
}

// PatchImages sets the target images and labels and moves the Pod to the Patched phase in one request.
// The recorded restart counts and image IDs identify the restart that applies the image.
func (u *PodUpdater) PatchImages(ctx context.Context, pod *corev1.Pod, images, labels map[string]string) (*corev1.Pod, error) {
	state := MatchingState(pod)
	if state == nil || !state.InProgress() {
		return nil, fmt.Errorf("pod %s/%s has no in-progress in-place update", pod.Namespace, pod.Name)
	}
	if state.Phase == PhasePatched {
		return pod, nil
	}
	state.Phase = PhasePatched
	now := u.now()
	state.PatchedAt = &now
	state.Patched = patchedContainers(pod, images)
	return u.patch(ctx, pod, PatchOptions{Images: images, Labels: labels, State: state})
}

// Complete finishes the update and replaces the baseline with the current restart counts.
func (u *PodUpdater) Complete(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	state := MatchingState(pod)
	if state == nil {
		return nil, fmt.Errorf("pod %s/%s has no in-place update state", pod.Namespace, pod.Name)
	}
	if !state.InProgress() {
		return pod, nil
	}
	state.Phase = PhaseDone
	state.Baseline = BaselineFromStatus(pod)
	state.Patched = nil
	return u.patch(ctx, pod, PatchOptions{State: state})
}

func (u *PodUpdater) patch(ctx context.Context, pod *corev1.Pod, opts PatchOptions) (*corev1.Pod, error) {
	data, err := BuildPatch(pod, opts)
	if err != nil {
		return nil, err
	}
	patched, err := u.client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.JSONPatchType, data, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("patch pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return patched, nil
}

func BaselineFromStatus(pod *corev1.Pod) map[string]ContainerBaseline {
	baseline := make(map[string]ContainerBaseline, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range statuses {
			baseline[statuses[i].Name] = ContainerBaseline{
				ContainerID:  statuses[i].ContainerID,
				RestartCount: statuses[i].RestartCount,
			}
		}
	}
	return baseline
}

func patchedContainers(pod *corev1.Pod, images map[string]string) map[string]PatchedContainer {
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		statuses[status.Name] = status
	}
	var patched map[string]PatchedContainer
	for _, container := range pod.Spec.Containers {
		image, ok := images[container.Name]
		if !ok || image == container.Image {
			continue
		}
		if patched == nil {
			patched = make(map[string]PatchedContainer)
		}
		status := statuses[container.Name]
		patched[container.Name] = PatchedContainer{Image: image, RestartCount: status.RestartCount, ImageID: status.ImageID}
	}
	return patched
}
