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
)

type RestartCause int

const (
	NoRestart RestartCause = iota
	// InPlaceUpdateRestart is the expected restart that applies a patched image.
	InPlaceUpdateRestart
	// UnexpectedRestart follows the RecoveryPolicy.
	UnexpectedRestart
)

func (c RestartCause) String() string {
	switch c {
	case NoRestart:
		return "NoRestart"
	case InPlaceUpdateRestart:
		return "InPlaceUpdateRestart"
	case UnexpectedRestart:
		return "UnexpectedRestart"
	default:
		return "Unknown"
	}
}

// ClassifyRestart compares restart counts with the state's baseline, or with zero without a state.
// During an unfinished update, only the restart that applies the patched image is expected.
func ClassifyRestart(pod *corev1.Pod) RestartCause {
	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
		return NoRestart
	}

	if restarts, ok := UpdateRestarts(pod); ok {
		cause := NoRestart
		for _, r := range restarts {
			if r.Other > 0 {
				return UnexpectedRestart
			}
			if r.ImageChange > 0 {
				cause = InPlaceUpdateRestart
			}
		}
		return cause
	}

	var baseline map[string]ContainerBaseline
	if state := MatchingState(pod); state != nil {
		baseline = state.Baseline
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range statuses {
			if statuses[i].RestartCount > baseline[statuses[i].Name].RestartCount {
				return UnexpectedRestart
			}
		}
	}
	return NoRestart
}

type ContainerRestarts struct {
	Name    string
	Changed bool
	// ImageChange is 1 once kubelet restarts the container on the patched image.
	ImageChange     int32
	Other           int32
	LastTermination string
}

// UpdateRestarts splits the restarts of each container during an unfinished update into the image change and other restarts.
func UpdateRestarts(pod *corev1.Pod) ([]ContainerRestarts, bool) {
	state := MatchingState(pod)
	if state == nil || !state.InProgress() {
		return nil, false
	}

	var result []ContainerRestarts
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range statuses {
			status := &statuses[i]
			restarts := ContainerRestarts{
				Name:            status.Name,
				LastTermination: lastTermination(status),
			}
			baseline := state.Baseline[status.Name].RestartCount

			patched, changed := state.Patched[status.Name]
			if !changed {
				restarts.Other = max(0, status.RestartCount-baseline)
				result = append(result, restarts)
				continue
			}
			restarts.Changed = true
			restarts.Other = max(0, patched.RestartCount-baseline)
			sincePatch := status.RestartCount - patched.RestartCount
			if sincePatch > 0 && patched.ImageApplied(status) {
				restarts.ImageChange = 1
				restarts.Other += sincePatch - 1
			} else {
				restarts.Other += max(0, sincePatch)
			}
			result = append(result, restarts)
		}
	}
	return result, true
}

func lastTermination(status *corev1.ContainerStatus) string {
	terminated := status.LastTerminationState.Terminated
	if terminated == nil {
		return ""
	}
	if terminated.Reason == "" || terminated.Reason == "Error" {
		return fmt.Sprintf("Error (exit code %d)", terminated.ExitCode)
	}
	return terminated.Reason
}
