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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const unitTestTargetImage = "example.com/engine:v2"

func unitTestTemplate(mainImage, hash string) PodTemplate {
	return PodTemplate{
		Images:           map[string]string{"main": mainImage, "sidecar": "example.com/sidecar:v1"},
		RoleTemplateHash: hash,
	}
}

var (
	unitTestSource = unitTestTemplate("example.com/engine:v1", "hash-1")
	unitTestTarget = unitTestTemplate(unitTestTargetImage, "hash-2")
)

// newUnitTestPod returns a running and ready Pod on the source revision.
func newUnitTestPod(name string) *UnitPod {
	pod := newTestPod()
	pod.Name = name
	pod.UID = types.UID(name)
	pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-1"
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].ImagePullPolicy = corev1.PullIfNotPresent
		pod.Status.ContainerStatuses[i].Image = pod.Spec.Containers[i].Image
		pod.Status.ContainerStatuses[i].Ready = true
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}}
	return &UnitPod{Pod: pod, Target: unitTestTarget}
}

// setUnitTestPhase moves the Pod to phase as the PodUpdater would, without kubelet acting on a patch.
func setUnitTestPhase(t *testing.T, p *UnitPod, phase Phase) *UnitPod {
	t.Helper()
	pod := p.Pod
	state := newTestState(phase)
	state.PodUID = pod.UID
	state.Baseline = BaselineFromStatus(pod)
	if phase != PhaseDraining {
		state.PatchedAt = &metav1.Time{}
		main := pod.Status.ContainerStatuses[0]
		state.Patched = map[string]PatchedContainer{"main": {Image: unitTestTargetImage, RestartCount: main.RestartCount, ImageID: main.ImageID}}
		pod.Spec.Containers[0].Image = unitTestTargetImage
		pod.Labels[workloadv1alpha1.RevisionLabelKey] = "rev-2"
		pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-2"
	}
	if phase == PhaseDone {
		restartOnTarget(pod)
		state.Patched = nil
		state.Baseline = BaselineFromStatus(pod)
	}
	setState(t, pod, state)
	return p
}

// restartOnTarget does what kubelet does after the image patch: it restarts the main container on the target image.
func restartOnTarget(pod *corev1.Pod) {
	status := &pod.Status.ContainerStatuses[0]
	status.RestartCount++
	status.ContainerID = "containerd://main-2"
	status.Image = unitTestTargetImage
}

func podNames(pods []*UnitPod) []string {
	var names []string
	for _, p := range pods {
		names = append(names, p.Pod.Name)
	}
	return names
}

// TestPlanUnit derives the next step from the Pods alone, including after a controller crash between any two Pod patches.
func TestPlanUnit(t *testing.T) {
	source := func(name string) func(t *testing.T) *UnitPod {
		return func(*testing.T) *UnitPod { return newUnitTestPod(name) }
	}
	inPhase := func(name string, phase Phase) func(t *testing.T) *UnitPod {
		return func(t *testing.T) *UnitPod { return setUnitTestPhase(t, newUnitTestPod(name), phase) }
	}
	restarted := func(name string) func(t *testing.T) *UnitPod {
		return func(t *testing.T) *UnitPod {
			p := setUnitTestPhase(t, newUnitTestPod(name), PhasePatched)
			restartOnTarget(p.Pod)
			return p
		}
	}
	recreatedOnTarget := func(name string) func(t *testing.T) *UnitPod {
		return func(*testing.T) *UnitPod {
			p := newUnitTestPod(name)
			p.Pod.Spec.Containers[0].Image = unitTestTargetImage
			p.Pod.Status.ContainerStatuses[0].Image = unitTestTargetImage
			p.Pod.Labels[workloadv1alpha1.RevisionLabelKey] = "rev-2"
			p.Pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-2"
			return p
		}
	}

	tests := []struct {
		name      string
		pods      []func(t *testing.T) *UnitPod
		wantStep  Step
		wantPods  []string
		wantMatch string
	}{
		{name: "no pods", wantStep: StepNone},
		{name: "outdated unit is marked as a whole", pods: []func(*testing.T) *UnitPod{source("entry"), source("worker")}, wantStep: StepMark, wantPods: []string{"entry", "worker"}},
		{name: "crash after marking some pods marks the rest", pods: []func(*testing.T) *UnitPod{inPhase("entry", PhaseDraining), source("worker")}, wantStep: StepMark, wantPods: []string{"worker"}},
		{name: "marked unit is patched", pods: []func(*testing.T) *UnitPod{inPhase("entry", PhaseDraining), inPhase("worker", PhaseDraining)}, wantStep: StepPatch, wantPods: []string{"entry", "worker"}},
		{name: "crash after patching some pods patches the rest", pods: []func(*testing.T) *UnitPod{inPhase("entry", PhasePatched), inPhase("worker", PhaseDraining)}, wantStep: StepPatch, wantPods: []string{"worker"}},
		{name: "patched unit waits for kubelet", pods: []func(*testing.T) *UnitPod{inPhase("entry", PhasePatched), restarted("worker")}, wantStep: StepVerify, wantMatch: `container "main" of pod entry is not running image`},
		{name: "verified unit completes as a whole", pods: []func(*testing.T) *UnitPod{restarted("entry"), restarted("worker")}, wantStep: StepComplete, wantPods: []string{"entry", "worker"}},
		{name: "crash after completing some pods completes the rest", pods: []func(*testing.T) *UnitPod{inPhase("entry", PhaseDone), restarted("worker")}, wantStep: StepComplete, wantPods: []string{"worker"}},
		{name: "pod recreated on the target revision during the update needs no update", pods: []func(*testing.T) *UnitPod{recreatedOnTarget("entry"), restarted("worker")}, wantStep: StepComplete, wantPods: []string{"worker"}},
		{name: "completed unit", pods: []func(*testing.T) *UnitPod{inPhase("entry", PhaseDone), inPhase("worker", PhaseDone)}, wantStep: StepNone},
		{name: "unit recreated on the target revision", pods: []func(*testing.T) *UnitPod{recreatedOnTarget("entry")}, wantStep: StepNone},
		{
			name: "an unfinished update to another target is recreated",
			pods: []func(*testing.T) *UnitPod{func(t *testing.T) *UnitPod {
				p := setUnitTestPhase(t, newUnitTestPod("entry"), PhasePatched)
				state := mustParseState(t, p.Pod)
				state.TargetRevision = "rev-0"
				setState(t, p.Pod, state)
				return p
			}},
			wantStep:  StepRecreate,
			wantMatch: "pod entry is updating to revision rev-0",
		},
		{
			name: "a state written for another ModelServing is ignored",
			pods: []func(*testing.T) *UnitPod{func(t *testing.T) *UnitPod {
				p := setUnitTestPhase(t, newUnitTestPod("entry"), PhaseDraining)
				p.Pod.OwnerReferences = append(p.Pod.OwnerReferences, metav1.OwnerReference{
					APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind, UID: "other-uid",
				})
				state := mustParseState(t, p.Pod)
				state.OwnerUID = "other-uid"
				setState(t, p.Pod, state)
				return p
			}},
			wantStep: StepMark,
			wantPods: []string{"entry"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pods []*UnitPod
			for _, newPod := range tt.pods {
				pods = append(pods, newPod(t))
			}
			plan := PlanUnit(testOwnerUID, "rev-2", pods)
			assert.Equal(t, tt.wantStep, plan.Step, plan.Message)
			assert.Equal(t, tt.wantPods, podNames(plan.Pods))
			assert.Contains(t, plan.Message, tt.wantMatch)
		})
	}
}

func TestVerify(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *UnitPod)
		want   string
	}{
		{name: "restarted and ready on the target image"},
		{
			name:   "the image-change restart has not happened",
			mutate: func(p *UnitPod) { p.Pod.Status.ContainerStatuses[0].RestartCount-- },
			want:   `container "main" of pod entry has not restarted on image`,
		},
		{
			name:   "status reports another image",
			mutate: func(p *UnitPod) { p.Pod.Status.ContainerStatuses[0].Image = "example.com/engine:v1" },
			want:   "is not running image",
		},
		{
			name: "status reports a digest that the target image does not name",
			mutate: func(p *UnitPod) {
				p.Pod.Status.ContainerStatuses[0].Image = "example.com/engine:v2@sha256:" + sha
			},
			want: "is not running image",
		},
		{
			name:   "container is not ready",
			mutate: func(p *UnitPod) { p.Pod.Status.ContainerStatuses[1].Ready = false },
			want:   `container "sidecar" of pod entry is not ready`,
		},
		{
			name:   "containers ready condition is false",
			mutate: func(p *UnitPod) { p.Pod.Status.Conditions[0].Status = corev1.ConditionFalse },
			want:   "containers of pod entry are not ready",
		},
		{
			name:   "labels are not on the target revision",
			mutate: func(p *UnitPod) { p.Pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-1" },
			want:   "does not carry the labels of revision rev-2",
		},
		{
			name:   "spec image is not the target image",
			mutate: func(p *UnitPod) { p.Pod.Spec.Containers[1].Image = "example.com/sidecar:v9" },
			want:   `container "sidecar" of pod entry does not match revision rev-2`,
		},
		{
			name:   "terminating",
			mutate: func(p *UnitPod) { p.Pod.DeletionTimestamp = &metav1.Time{} },
			want:   "pod entry is terminating",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := setUnitTestPhase(t, newUnitTestPod("entry"), PhasePatched)
			restartOnTarget(p.Pod)
			if tt.mutate != nil {
				tt.mutate(p)
			}
			ok, message := Verify(p, MatchingState(p.Pod), "rev-2")
			assert.Equal(t, tt.want == "", ok)
			assert.Contains(t, message, tt.want)
		})
	}

	t.Run("the runtime reports the patched image under another name", func(t *testing.T) {
		p := newUnitTestPod("entry")
		p.Pod.Status.ContainerStatuses[0].ImageID = "sha256:v1"
		setUnitTestPhase(t, p, PhasePatched)
		restartOnTarget(p.Pod)
		status := &p.Pod.Status.ContainerStatuses[0]
		status.Image, status.ImageID = "mirror.example.com/engine:v2", "sha256:v2"
		ok, message := Verify(p, MatchingState(p.Pod), "rev-2")
		assert.True(t, ok, "a new image ID shows that the patched image runs: %s", message)

		status.Image, status.ImageID = "example.com/engine:v1", "sha256:v1"
		ok, _ = Verify(p, MatchingState(p.Pod), "rev-2")
		assert.False(t, ok, "an unchanged image ID under the old name may be a restart on the source image")
	})

	t.Run("a container created for the first time after the patch is verified", func(t *testing.T) {
		p := newUnitTestPod("entry")
		// The container never started on the source image, for example because its image could not be pulled.
		p.Pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "main"}
		setUnitTestPhase(t, p, PhasePatched)
		status := &p.Pod.Status.ContainerStatuses[0]
		status.ContainerID, status.Image, status.ImageID, status.Ready = "containerd://main-1", "example.com/engine:v1", "sha256:v1", true
		ok, _ := Verify(p, MatchingState(p.Pod), "rev-2")
		assert.False(t, ok, "kubelet may first start the container on the source image it was already pulling")

		status.Image, status.ImageID = unitTestTargetImage, "sha256:v2"
		ok, message := Verify(p, MatchingState(p.Pod), "rev-2")
		assert.True(t, ok, message)
	})
}

func TestCheckPatchable(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(pod *corev1.Pod)
		wantReason string
	}{
		{name: "source images"},
		{
			name:       "restart policy",
			mutate:     func(pod *corev1.Pod) { pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure },
			wantReason: ReasonIneligible,
		},
		{
			name: "container restart policy",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[1].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
			},
			wantReason: ReasonIneligible,
		},
		{
			name:   "pull policy mutated by admission",
			mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways },
		},
		{
			name: "extra container",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "debug", ImagePullPolicy: corev1.PullIfNotPresent})
			},
			wantReason: ReasonIneligible,
		},
		{
			name:       "renamed container",
			mutate:     func(pod *corev1.Pod) { pod.Spec.Containers[1].Name = "debug" },
			wantReason: ReasonIneligible,
		},
		{
			name:       "image set by someone else",
			mutate:     func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "example.com/engine:hotfix" },
			wantReason: ReasonUnexplainedImageDrift,
		},
		{
			name:       "target image set without the update",
			mutate:     func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = unitTestTargetImage },
			wantReason: ReasonUnexplainedImageDrift,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newUnitTestPod("entry")
			if tt.mutate != nil {
				tt.mutate(p.Pod)
			}
			fallback := CheckPatchable(p.Pod, unitTestSource, unitTestTarget)
			if tt.wantReason == "" {
				assert.Nil(t, fallback)
				return
			}
			require.NotNil(t, fallback)
			assert.Equal(t, tt.wantReason, fallback.Reason, fallback.Message)
		})
	}
}

func TestNewRevisionTemplates(t *testing.T) {
	ms := newEligibilityModelServing()
	ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "example.com/engine:latest"
	templates := NewRevisionTemplates(ms, "rev-2")
	hash := utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])

	entryPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		workloadv1alpha1.RoleLabelKey:  "decode",
		workloadv1alpha1.EntryLabelKey: utils.Entry,
	}}}
	entry, ok := templates.ForPod(entryPod)
	require.True(t, ok)
	assert.Equal(t, PodTemplate{
		Images:           map[string]string{"engine": "example.com/engine:v1"},
		RoleTemplateHash: hash,
	}, entry)
	assert.Equal(t, map[string]string{
		workloadv1alpha1.RevisionLabelKey:         "rev-2",
		workloadv1alpha1.RoleTemplateHashLabelKey: hash,
	}, entry.Labels("rev-2"))

	workerPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{workloadv1alpha1.RoleLabelKey: "decode"}}}
	worker, ok := templates.ForPod(workerPod)
	require.True(t, ok)
	assert.Equal(t, "example.com/engine:latest", worker.Images["engine"])

	_, ok = templates.ForPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{workloadv1alpha1.RoleLabelKey: "prefill"}}})
	assert.False(t, ok)
}

func TestUnitRevision(t *testing.T) {
	marked := func(phase Phase, source string) *corev1.Pod {
		pod := newTestPod()
		state := newTestState(phase)
		state.SourceRevision = source
		setState(t, pod, state)
		return pod
	}

	_, ok := UnitRevision([]*corev1.Pod{newTestPod(), marked(PhaseDone, "rev-0")}, testOwnerUID, "rev-2")
	assert.False(t, ok, "a unit without an unfinished update keeps its revision")

	unitRevision, ok := UnitRevision([]*corev1.Pod{marked(PhaseDraining, "rev-2"), marked(PhasePatched, "rev-1")}, testOwnerUID, "rev-2")
	assert.True(t, ok)
	assert.Equal(t, "rev-1", unitRevision, "a pod recreated on the target revision does not hide where the unit came from")

	unitRevision, ok = UnitRevision([]*corev1.Pod{marked(PhasePatched, "rev-1")}, testOwnerUID, "rev-3")
	assert.True(t, ok)
	assert.Equal(t, "rev-1", unitRevision, "a unit left behind by a newer revision stays on its source revision")

	unitRevision, ok = UnitRevision([]*corev1.Pod{marked(PhasePatched, "rev-1")}, testOwnerUID, "rev-1")
	assert.True(t, ok)
	assert.Equal(t, "rev-2", unitRevision, "after a revert to its source revision, the unit is still outdated")

	assert.True(t, InUpdate([]*corev1.Pod{newTestPod(), marked(PhasePatched, "rev-1")}, testOwnerUID))
	assert.False(t, InUpdate([]*corev1.Pod{marked(PhasePatched, "rev-1")}, "other-uid"))
}
