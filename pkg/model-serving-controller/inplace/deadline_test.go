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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestProgressDeadline(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{}
	assert.Equal(t, 1800*time.Second, ProgressDeadline(ms), "without a rollout strategy")

	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{}
	assert.Equal(t, 1800*time.Second, ProgressDeadline(ms), "without a configured deadline")

	ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](60)
	assert.Equal(t, time.Minute, ProgressDeadline(ms))
}

func TestUnitStartedAt(t *testing.T) {
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	marked := func(phase Phase, startedAt time.Time) *corev1.Pod {
		pod := newTestPod()
		state := newTestState(phase)
		state.StartedAt = metav1.NewTime(startedAt)
		setState(t, pod, state)
		return pod
	}

	startedAt, ok := UnitStartedAt([]*corev1.Pod{
		marked(PhasePatched, early.Add(time.Minute)),
		marked(PhaseDraining, early),
		newTestPod(),
	}, testOwnerUID)
	assert.True(t, ok)
	assert.Equal(t, early, startedAt.UTC(), "the earliest start of an unfinished update")

	_, ok = UnitStartedAt([]*corev1.Pod{marked(PhaseDone, early), newTestPod()}, testOwnerUID)
	assert.False(t, ok, "finished updates have no start")

	_, ok = UnitStartedAt([]*corev1.Pod{marked(PhasePatched, early)}, "other-uid")
	assert.False(t, ok, "a state written for another ModelServing is ignored")
}

func TestUnverifiedPods(t *testing.T) {
	restarted := setUnitTestPhase(t, newUnitTestPod("restarted"), PhasePatched)
	restartOnTarget(restarted.Pod)
	recreated := newUnitTestPod("recreated")
	recreated.Pod.Spec.Containers[0].Image = unitTestTargetImage
	recreated.Pod.Labels[workloadv1alpha1.RevisionLabelKey] = "rev-2"
	recreated.Pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-2"

	pods := []*UnitPod{
		setUnitTestPhase(t, newUnitTestPod("draining"), PhaseDraining),
		setUnitTestPhase(t, newUnitTestPod("patched"), PhasePatched),
		restarted,
		setUnitTestPhase(t, newUnitTestPod("done"), PhaseDone),
		recreated,
	}
	assert.Equal(t, []string{"draining", "patched"}, podNames(UnverifiedPods(testOwnerUID, "rev-2", pods)),
		"only pods with an unfinished update that is not verified")
}
