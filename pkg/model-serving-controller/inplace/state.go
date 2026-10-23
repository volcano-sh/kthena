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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const StateVersion = 1

type Phase string

const (
	PhaseDraining Phase = "Draining"
	PhasePatched  Phase = "Patched"
	PhaseDone     Phase = "Done"
)

type ContainerBaseline struct {
	ContainerID  string `json:"containerID,omitempty"`
	RestartCount int32  `json:"restartCount"`
}

type PatchedContainer struct {
	Image        string `json:"image"`
	RestartCount int32  `json:"restartCount"`
	ImageID      string `json:"imageID,omitempty"`
}

// ImageApplied reports whether the restarted container runs the patched image.
// It matches the image name, or an image ID that changed since the patch.
func (p PatchedContainer) ImageApplied(status *corev1.ContainerStatus) bool {
	if SameImage(status.Image, p.Image) {
		return true
	}
	return p.ImageID != "" && status.ImageID != "" && status.ImageID != p.ImageID
}

// State is stored in the InPlaceUpdateStateAnnotationKey annotation.
type State struct {
	Version                int          `json:"version"`
	OwnerUID               types.UID    `json:"ownerUID"`
	PodUID                 types.UID    `json:"podUID"`
	Unit                   string       `json:"unit"`
	SourceRevision         string       `json:"sourceRevision"`
	TargetRevision         string       `json:"targetRevision"`
	SourceRoleTemplateHash string       `json:"sourceRoleTemplateHash,omitempty"`
	PreviousTargets        []string     `json:"previousTargets,omitempty"`
	Phase                  Phase        `json:"phase"`
	StartedAt              metav1.Time  `json:"startedAt"`
	PatchedAt              *metav1.Time `json:"patchedAt,omitempty"`
	// Baseline holds restart counts at mark time, or after completion once Done.
	Baseline map[string]ContainerBaseline `json:"baseline,omitempty"`
	// Patched holds the containers whose image the last patch changed.
	Patched map[string]PatchedContainer `json:"patched,omitempty"`
}

func (s *State) InProgress() bool {
	return s.Phase != PhaseDone
}

func (s *State) Encode() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func ParseState(pod *corev1.Pod) (*State, error) {
	value, ok := pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey]
	if !ok {
		return nil, nil
	}
	state := &State{}
	if err := json.Unmarshal([]byte(value), state); err != nil {
		return nil, fmt.Errorf("decode in-place update state: %w", err)
	}
	if state.Version != StateVersion {
		return nil, fmt.Errorf("unsupported in-place update state version %d", state.Version)
	}
	switch state.Phase {
	case PhaseDraining, PhasePatched, PhaseDone:
	default:
		return nil, fmt.Errorf("unknown in-place update phase %q", state.Phase)
	}
	return state, nil
}

// MatchingState returns the state only if it was written for this Pod and its ModelServing.
// A stale or malformed state can never exempt a restart from recovery.
func MatchingState(pod *corev1.Pod) *State {
	state, err := ParseState(pod)
	if err != nil || state == nil {
		return nil
	}
	if state.PodUID == "" || state.PodUID != pod.UID {
		return nil
	}
	if state.OwnerUID == "" || !utils.HasModelServingOwner(pod, state.OwnerUID) {
		return nil
	}
	return state
}

func InUpdateWindow(pod *corev1.Pod) bool {
	state := MatchingState(pod)
	return state != nil && state.InProgress()
}
