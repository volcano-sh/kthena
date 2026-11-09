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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	rolloutRevA       = "rollout-rev-a"
	rolloutRevB       = "rollout-rev-b"
	rolloutRevC       = "rollout-rev-c"
	rolloutGoodImage  = "model:good"
	rolloutBadImage   = "model:bad"
	rolloutOtherImage = "model:other"
)

type rolloutFixture struct {
	t        *testing.T
	ctx      context.Context
	c        *ModelServingController
	kube     *kubefake.Clientset
	kthena   *kthenafake.Clientset
	recorder *record.FakeRecorder
	ms       *workloadv1alpha1.ModelServing
}

func rolloutModelServing(replicas int32, action workloadv1alpha1.RolloutFailureAction, image string) *workloadv1alpha1.ModelServing {
	ms := createStandardModelServing("rollout", replicas, 1)
	ms.UID = "rollout-uid"
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = image
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate}
	if action != "" {
		ms.Spec.RolloutStrategy.FailurePolicy = &workloadv1alpha1.RolloutFailurePolicy{Action: action}
	}
	return ms
}

func withImage(ms *workloadv1alpha1.ModelServing, image string) *workloadv1alpha1.ModelServing {
	out := ms.DeepCopy()
	out.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = image
	return out
}

// withGoodRevision returns ms with the Role templates recorded for revision A.
func withGoodRevision(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.ModelServing {
	out := withImage(ms, rolloutGoodImage)
	out.Spec.Template.Roles[0].WorkerReplicas = 2
	out.Spec.Template.Roles[0].WorkerTemplate = out.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	return out
}

// newRolloutFixture records revision A as the good image, B as the bad image and C as another image.
func newRolloutFixture(t *testing.T, ms *workloadv1alpha1.ModelServing) *rolloutFixture {
	t.Helper()
	ctx := context.Background()
	kube := kubefake.NewSimpleClientset()
	kthena := kthenafake.NewSimpleClientset(ms)
	c, err := NewModelServingController(kube, kthena, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	recorder := record.NewFakeRecorder(100)
	c.recorder = recorder
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))

	for revision, source := range map[string]*workloadv1alpha1.ModelServing{
		rolloutRevA: withGoodRevision(ms),
		rolloutRevB: withImage(ms, rolloutBadImage),
		rolloutRevC: withImage(ms, rolloutOtherImage),
	} {
		_, err := utils.CreateControllerRevision(ctx, kube, source, revision, source.Spec.Template.Roles)
		require.NoError(t, err)
	}
	return &rolloutFixture{t: t, ctx: ctx, c: c, kube: kube, kthena: kthena, recorder: recorder, ms: ms}
}

func (f *rolloutFixture) addGroup(ordinal int, revision string, status datastore.ServingGroupStatus) {
	f.t.Helper()
	key := utils.GetNamespaceName(f.ms)
	f.c.store.AddServingGroup(key, ordinal, revision)
	require.NoError(f.t, f.c.store.UpdateServingGroupStatus(key, utils.GenerateServingGroupName(f.ms.Name, ordinal), status))
}

func (f *rolloutFixture) addPod(name, revision string, mutate func(*corev1.Pod)) {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ms.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: f.ms.Name,
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.ms, workloadv1alpha1.ModelServingKind)},
		},
		Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if mutate != nil {
		mutate(pod)
	}
	require.NoError(f.t, f.c.podsInformer.GetIndexer().Add(pod))
}

func (f *rolloutFixture) updateStatus(revision string) workloadv1alpha1.ModelServingStatus {
	f.t.Helper()
	require.NoError(f.t, f.c.UpdateModelServingStatus(f.ms, revision))
	got, err := f.kthena.WorkloadV1alpha1().ModelServings(f.ms.Namespace).Get(f.ctx, f.ms.Name, metav1.GetOptions{})
	require.NoError(f.t, err)
	return got.Status
}

func waitingContainer(reason string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "main",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}}
	}
}

func rolloutFailedCondition(status workloadv1alpha1.ModelServingStatus) *metav1.Condition {
	return apiMeta.FindStatusCondition(status.Conditions, string(workloadv1alpha1.ModelServingRolloutFailed))
}

func assertRolloutFailed(t *testing.T, status workloadv1alpha1.ModelServingStatus, want metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := rolloutFailedCondition(status)
	require.NotNil(t, condition, "RolloutFailed condition")
	assert.Equal(t, want, condition.Status)
	assert.Equal(t, reason, condition.Reason, condition.Message)
}

func TestClassifyRolloutPod(t *testing.T) {
	tests := []struct {
		name               string
		mutate             func(*corev1.Pod)
		wantWorkload       string
		wantInfrastructure string
	}{
		{name: "image pull back-off", mutate: waitingContainer("ImagePullBackOff"), wantWorkload: "ImagePullBackOff"},
		{name: "invalid image name", mutate: waitingContainer("InvalidImageName"), wantWorkload: "InvalidImageName"},
		{name: "container config error", mutate: waitingContainer("CreateContainerConfigError"), wantWorkload: "CreateContainerConfigError"},
		{name: "crash loop back-off", mutate: waitingContainer("CrashLoopBackOff"), wantWorkload: "CrashLoopBackOff"},
		{
			name: "init container image error",
			mutate: func(pod *corev1.Pod) {
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  "init",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
				}}
			},
			wantWorkload: "ErrImagePull",
		},
		{
			name: "unexpected restart",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:         "main",
					RestartCount: 1,
					State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			},
			wantWorkload: "restarted",
		},
		{
			name:         "failed phase",
			mutate:       func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed },
			wantWorkload: "failed",
		},
		{
			name: "started but never ready",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  "main",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			},
			wantWorkload: "not ready",
		},
		{
			name:               "unscheduled",
			mutate:             func(pod *corev1.Pod) { pod.Spec.NodeName = "" },
			wantInfrastructure: "unscheduled",
		},
		{
			name: "evicted with failed phase",
			mutate: func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}}
			},
			wantInfrastructure: "disrupted",
		},
		{
			name: "still creating containers",
			mutate: func(pod *corev1.Pod) {
				waitingContainer("ContainerCreating")(pod)
			},
			wantInfrastructure: "starting",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "main"}}},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			}
			tt.mutate(pod)
			workload, infrastructure := classifyRolloutPod(pod)
			assert.Equal(t, tt.wantWorkload, workload)
			assert.Equal(t, tt.wantInfrastructure, infrastructure)
		})
	}
}

func TestEvaluateRollout(t *testing.T) {
	stale := metav1.NewTime(time.Now().Add(-time.Hour))
	recent := metav1.NewTime(time.Now().Add(-10 * time.Second))
	trackingB := func(lastProgress metav1.Time, failures int32) func(*workloadv1alpha1.ModelServingStatus) {
		return func(status *workloadv1alpha1.ModelServingStatus) {
			status.UpdateRevision = rolloutRevB
			status.RolloutProgress = &workloadv1alpha1.RolloutProgress{
				Revision:         rolloutRevB,
				MaxTargetUnits:   1,
				LastProgressTime: lastProgress,
				WorkloadFailures: failures,
			}
		}
	}
	type group struct {
		revision string
		status   datastore.ServingGroupStatus
	}
	type pod struct {
		name   string
		mutate func(*corev1.Pod)
	}

	tests := []struct {
		name     string
		action   workloadv1alpha1.RolloutFailureAction
		replicas int32
		image    string
		restored bool // the spec carries revision A's Role templates
		revision string
		status   func(*workloadv1alpha1.ModelServingStatus)
		groups   []group
		pods     []pod
		recorded int32
		want     func(*testing.T, workloadv1alpha1.ModelServingStatus)
	}{
		{
			name:   "no policy keeps status free of rollout tracking",
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assert.Nil(t, status.RolloutProgress)
				assert.Nil(t, rolloutFailedCondition(status))
			},
		},
		{
			name:   "a template change starts tracking",
			action: workloadv1alpha1.RolloutFailureHalt,
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				require.NotNil(t, status.RolloutProgress)
				assert.Equal(t, rolloutRevB, status.RolloutProgress.Revision)
				assert.Equal(t, int32(1), status.RolloutProgress.MaxTargetUnits)
				assert.Equal(t, int32(0), status.RolloutProgress.MaxReadyUnits)
				assert.Nil(t, rolloutFailedCondition(status))
			},
		},
		{
			name:   "steady state is not a rollout",
			action: workloadv1alpha1.RolloutFailureHalt,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				status.UpdateRevision, status.CurrentRevision = rolloutRevB, rolloutRevB
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assert.Nil(t, status.RolloutProgress)
			},
		},
		{
			name:   "deadline not reached",
			action: workloadv1alpha1.RolloutFailureHalt,
			status: trackingB(recent, 3),
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assert.Nil(t, rolloutFailedCondition(status))
			},
		},
		{
			name:   "recreating a crashed unit is not progress",
			action: workloadv1alpha1.RolloutFailureHalt,
			status: trackingB(recent, 0),
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				require.NotNil(t, status.RolloutProgress)
				assert.True(t, status.RolloutProgress.LastProgressTime.Equal(&recent))
			},
		},
		{
			name:     "a unit that was Ready and then crashed is left to recovery",
			action:   workloadv1alpha1.RolloutFailureRollback,
			replicas: 2,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 3)(status)
				status.RolloutProgress.MaxReadyUnits = 1
			},
			groups: []group{{rolloutRevA, datastore.ServingGroupRunning}, {rolloutRevB, datastore.ServingGroupCreating}},
			pods:   []pod{{"rollout-1-prefill-0-0", waitingContainer("CrashLoopBackOff")}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assert.Nil(t, rolloutFailedCondition(status))
				assert.Nil(t, status.LastRollback)
			},
		},
		{
			name:   "no evidence against the revision blames the environment",
			action: workloadv1alpha1.RolloutFailureRollback,
			status: trackingB(stale, 0),
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			pods:   []pod{{"rollout-0-prefill-0-0", func(p *corev1.Pod) { p.Spec.NodeName = "" }}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutInfrastructureBlocked)
				assert.Contains(t, rolloutFailedCondition(status).Message, "unscheduled")
				assert.Nil(t, status.LastRollback)
			},
		},
		{
			name:   "recorded failures with Halt",
			action: workloadv1alpha1.RolloutFailureHalt,
			status: trackingB(stale, 2),
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutWorkloadFailure)
				assert.Nil(t, status.LastRollback)
			},
		},
		{
			name:     "failures not yet persisted are folded into status",
			action:   workloadv1alpha1.RolloutFailureHalt,
			status:   trackingB(stale, 0),
			groups:   []group{{rolloutRevB, datastore.ServingGroupCreating}},
			recorded: 2,
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				require.NotNil(t, status.RolloutProgress)
				assert.Equal(t, int32(2), status.RolloutProgress.WorkloadFailures)
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutWorkloadFailure)
			},
		},
		{
			name:   "Pod evidence with Rollback records the intent",
			action: workloadv1alpha1.RolloutFailureRollback,
			status: trackingB(stale, 0),
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			pods:   []pod{{"rollout-0-prefill-0-0", waitingContainer("ImagePullBackOff")}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutWorkloadFailure)
				require.NotNil(t, status.LastRollback)
				assert.Equal(t, workloadv1alpha1.RollbackPending, status.LastRollback.Phase)
				assert.Equal(t, rolloutRevB, status.LastRollback.FromRevision)
				assert.Equal(t, rolloutRevA, status.LastRollback.ToRevision)
				assert.Contains(t, status.LastRollback.Reason, "ImagePullBackOff")
			},
		},
		{
			name:   "no completed revision to roll back to",
			action: workloadv1alpha1.RolloutFailureRollback,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 1)(status)
				status.CurrentRevision = rolloutRevB
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutRollbackNotApplicable)
				assert.Nil(t, status.LastRollback)
			},
		},
		{
			name:   "a revision is rolled back at most once",
			action: workloadv1alpha1.RolloutFailureRollback,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 1)(status)
				status.LastRollback = &workloadv1alpha1.AutomaticRollback{
					FromRevision: rolloutRevB, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackCompleted,
				}
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutRollbackNotApplicable)
				assert.Contains(t, rolloutFailedCondition(status).Message, "already rolled back once")
				assert.Equal(t, workloadv1alpha1.RollbackCompleted, status.LastRollback.Phase)
			},
		},
		{
			name:     "a stalled rollback is never rolled back again",
			action:   workloadv1alpha1.RolloutFailureRollback,
			revision: rolloutRevA,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				status.UpdateRevision = rolloutRevA
				status.RolloutProgress = &workloadv1alpha1.RolloutProgress{
					Revision: rolloutRevA, MaxTargetUnits: 1, LastProgressTime: stale, WorkloadFailures: 1,
				}
				status.LastRollback = &workloadv1alpha1.AutomaticRollback{
					FromRevision: rolloutRevB, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackApplied,
				}
			},
			groups: []group{{rolloutRevA, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionTrue, rolloutRollbackFailed)
			},
		},
		{
			name:     "progress lifts a halt",
			action:   workloadv1alpha1.RolloutFailureHalt,
			replicas: 2,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 1)(status)
				status.Conditions = []metav1.Condition{{
					Type: string(workloadv1alpha1.ModelServingRolloutFailed), Status: metav1.ConditionTrue,
					Reason: rolloutWorkloadFailure, LastTransitionTime: stale,
				}}
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupRunning}, {rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assertRolloutFailed(t, status, metav1.ConditionFalse, rolloutResumed)
				assert.Equal(t, int32(1), status.RolloutProgress.MaxReadyUnits)
			},
		},
		{
			name:   "a completed rollout ends tracking and the rollback guard",
			action: workloadv1alpha1.RolloutFailureRollback,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 0)(status)
				status.LastRollback = &workloadv1alpha1.AutomaticRollback{
					FromRevision: rolloutRevC, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackCompleted,
				}
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupRunning}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				assert.Nil(t, status.RolloutProgress)
				assert.Nil(t, status.LastRollback)
			},
		},
		{
			name:     "the restored spec marks the rollback applied",
			action:   workloadv1alpha1.RolloutFailureRollback,
			restored: true,
			revision: rolloutRevA,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 1)(status)
				status.Conditions = []metav1.Condition{{
					Type: string(workloadv1alpha1.ModelServingRolloutFailed), Status: metav1.ConditionTrue,
					Reason: rolloutWorkloadFailure, LastTransitionTime: stale,
				}}
				status.LastRollback = &workloadv1alpha1.AutomaticRollback{
					FromRevision: rolloutRevB, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackPending,
				}
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				require.NotNil(t, status.LastRollback)
				assert.Equal(t, workloadv1alpha1.RollbackApplied, status.LastRollback.Phase)
				assert.Equal(t, rolloutRevA, status.LastRollback.ToRevision)
				assertRolloutFailed(t, status, metav1.ConditionFalse, rolloutRolledBack)
				require.NotNil(t, status.RolloutProgress)
				assert.Equal(t, rolloutRevA, status.RolloutProgress.Revision)
			},
		},
		{
			name:     "a concurrent spec change abandons the rollback",
			action:   workloadv1alpha1.RolloutFailureRollback,
			image:    rolloutOtherImage,
			revision: rolloutRevC,
			status: func(status *workloadv1alpha1.ModelServingStatus) {
				trackingB(stale, 1)(status)
				status.LastRollback = &workloadv1alpha1.AutomaticRollback{
					FromRevision: rolloutRevB, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackPending,
				}
			},
			groups: []group{{rolloutRevB, datastore.ServingGroupCreating}},
			want: func(t *testing.T, status workloadv1alpha1.ModelServingStatus) {
				require.NotNil(t, status.LastRollback)
				assert.Equal(t, workloadv1alpha1.RollbackAbandoned, status.LastRollback.Phase)
				assert.Equal(t, rolloutRevC, status.RolloutProgress.Revision)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replicas, image, revision := tt.replicas, tt.image, tt.revision
			if replicas == 0 {
				replicas = 1
			}
			if image == "" {
				image = rolloutBadImage
			}
			if revision == "" {
				revision = rolloutRevB
			}
			ms := rolloutModelServing(replicas, tt.action, image)
			if tt.restored {
				ms = withGoodRevision(ms)
			}
			ms.Status.UpdateRevision, ms.Status.CurrentRevision = rolloutRevA, rolloutRevA
			if tt.status != nil {
				tt.status(&ms.Status)
			}
			f := newRolloutFixture(t, ms)
			for ordinal, g := range tt.groups {
				f.addGroup(ordinal, g.revision, g.status)
			}
			for _, p := range tt.pods {
				f.addPod(p.name, revision, p.mutate)
			}
			key := rolloutFailureKey{uid: ms.UID, revision: revision}
			for i := int32(0); i < tt.recorded; i++ {
				f.c.rolloutFailures.add(key)
			}

			tt.want(t, f.updateStatus(revision))
			assert.Zero(t, f.c.rolloutFailures.peek(key), "persisted failures are consumed")
		})
	}
}

func TestApplyPendingRollback(t *testing.T) {
	pending := &workloadv1alpha1.AutomaticRollback{
		FromRevision: rolloutRevB, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackPending,
	}
	tests := []struct {
		name        string
		action      workloadv1alpha1.RolloutFailureAction
		rollback    *workloadv1alpha1.AutomaticRollback
		revision    string
		wantApplied bool
	}{
		{name: "restores the last completed revision", action: workloadv1alpha1.RolloutFailureRollback, rollback: pending, revision: rolloutRevB, wantApplied: true},
		{name: "the spec already moved on", action: workloadv1alpha1.RolloutFailureRollback, rollback: pending, revision: rolloutRevC},
		{name: "the policy was removed", rollback: pending, revision: rolloutRevB},
		{
			name: "the rollback is already applied", action: workloadv1alpha1.RolloutFailureRollback, revision: rolloutRevB,
			rollback: &workloadv1alpha1.AutomaticRollback{FromRevision: rolloutRevB, ToRevision: rolloutRevA, Phase: workloadv1alpha1.RollbackApplied},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := rolloutModelServing(3, tt.action, rolloutBadImage)
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{
				MaxUnavailable: ptr.To(intstr.FromInt32(2)),
				Partition:      ptr.To(intstr.FromInt32(1)),
			}
			ms.Spec.RecoveryPolicy = workloadv1alpha1.ServingGroupRecreate
			ms.Status.LastRollback = tt.rollback.DeepCopy()
			f := newRolloutFixture(t, ms)
			before := ms.DeepCopy()

			applied, err := f.c.applyPendingRollback(f.ctx, ms, tt.revision)
			require.NoError(t, err)
			assert.Equal(t, tt.wantApplied, applied)

			got, err := f.kthena.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(f.ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			if !tt.wantApplied {
				assert.Equal(t, before.Spec, got.Spec)
				return
			}
			role := got.Spec.Template.Roles[0]
			assert.Equal(t, rolloutGoodImage, role.EntryTemplate.Spec.Containers[0].Image)
			assert.Equal(t, int32(2), role.WorkerReplicas)
			require.NotNil(t, role.WorkerTemplate)
			assert.Equal(t, rolloutGoodImage, role.WorkerTemplate.Spec.Containers[0].Image)
			assert.Empty(t, role.EntryTemplate.Spec.SchedulerName, "controller-normalized fields stay out of the spec")
			assert.Empty(t, role.WorkerTemplate.Spec.SchedulerName, "controller-normalized fields stay out of the spec")
			restoredMS := got.DeepCopy()
			assert.True(t, f.c.specMatchesRevision(f.ctx, restoredMS, "restored", rolloutRevA), "the restored spec resolves to revision A")

			// Everything outside the Role templates is kept.
			assert.Equal(t, before.Spec.Replicas, got.Spec.Replicas)
			assert.Equal(t, before.Spec.Template.Roles[0].Replicas, role.Replicas)
			assert.Equal(t, before.Spec.RolloutStrategy, got.Spec.RolloutStrategy)
			assert.Equal(t, before.Spec.RecoveryPolicy, got.Spec.RecoveryPolicy)
			assert.Equal(t, before.Spec.SchedulerName, got.Spec.SchedulerName)
			assert.Equal(t, before.Spec.Plugins, got.Spec.Plugins)
		})
	}
}

// TestSyncAppliesPendingRollback uses real revision identities to show that the restored spec resolves to the
// original revision, so the status pass can mark the rollback applied.
func TestSyncAppliesPendingRollback(t *testing.T) {
	ctx := context.Background()
	good := rolloutModelServing(1, workloadv1alpha1.RolloutFailureRollback, rolloutGoodImage)
	bad := withImage(good, rolloutBadImage)
	kube := kubefake.NewSimpleClientset()
	kthena := kthenafake.NewSimpleClientset(bad)
	c, err := NewModelServingController(kube, kthena, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)

	revA, err := c.revisionHistory(ctx, good).desiredRevision(ctx)
	require.NoError(t, err)
	revB, err := c.revisionHistory(ctx, bad).desiredRevision(ctx)
	require.NoError(t, err)
	require.NotEqual(t, revA, revB)

	bad.Status = workloadv1alpha1.ModelServingStatus{
		UpdateRevision:  revB,
		CurrentRevision: revA,
		LastRollback: &workloadv1alpha1.AutomaticRollback{
			FromRevision: revB, ToRevision: revA, Phase: workloadv1alpha1.RollbackPending,
		},
	}
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(bad))

	require.NoError(t, c.syncModelServing(ctx, namespacedKey(bad.Namespace, bad.Name)))
	got, err := kthena.WorkloadV1alpha1().ModelServings(bad.Namespace).Get(ctx, bad.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, rolloutGoodImage, got.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)

	restored, err := c.revisionHistory(ctx, got).desiredRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, revA, restored, "the restored spec reuses revision A's identity")
	assert.True(t, c.specMatchesRevision(ctx, got, restored, revA))
}

func TestManageRollingUpdateHaltsFailedRollout(t *testing.T) {
	for _, halted := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "halted"}[halted], func(t *testing.T) {
			ms := rolloutModelServing(3, workloadv1alpha1.RolloutFailureHalt, rolloutBadImage)
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{
				MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			}
			ms.Status.RolloutProgress = &workloadv1alpha1.RolloutProgress{Revision: rolloutRevB}
			if halted {
				ms.Status.Conditions = []metav1.Condition{{
					Type: string(workloadv1alpha1.ModelServingRolloutFailed), Status: metav1.ConditionTrue, Reason: rolloutWorkloadFailure,
				}}
			}
			f := newRolloutFixture(t, ms)
			for ordinal := 0; ordinal < 3; ordinal++ {
				f.addGroup(ordinal, rolloutRevA, datastore.ServingGroupRunning)
			}

			require.NoError(t, f.c.manageRollingUpdateWithPolicy(f.ctx, ms, rolloutRevB, &roleRolloutPolicy{}))
			groups, err := f.c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
			require.NoError(t, err)
			// A group without Pods leaves the store at once, so count the groups still serving revision A.
			replaced := 3
			for _, group := range groups {
				if group.Status != datastore.ServingGroupDeleting && group.Revision == rolloutRevA {
					replaced--
				}
			}
			if halted {
				assert.Zero(t, replaced, "a halted rollout replaces no outdated ServingGroup")
			} else {
				assert.Equal(t, 1, replaced, "maxUnavailable allows one replacement")
			}
		})
	}
}

func TestHandleErrorPodRecordsRolloutFailures(t *testing.T) {
	restarted := func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "main", RestartCount: 1,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
	}
	tests := []struct {
		name     string
		revision string
		mutate   func(*corev1.Pod)
		policy   bool
		want     int32
	}{
		{name: "crash on the tracked revision", revision: rolloutRevB, mutate: restarted, policy: true, want: 1},
		{name: "crash on an outdated revision", revision: rolloutRevA, mutate: restarted, policy: true},
		{name: "no failure policy", revision: rolloutRevB, mutate: restarted},
		{
			name: "eviction is not the revision's fault", revision: rolloutRevB, policy: true,
			mutate: func(pod *corev1.Pod) {
				restarted(pod)
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := workloadv1alpha1.RolloutFailureAction("")
			if tt.policy {
				action = workloadv1alpha1.RolloutFailureHalt
			}
			ms := rolloutModelServing(1, action, rolloutBadImage)
			ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
			ms.Status.RolloutProgress = &workloadv1alpha1.RolloutProgress{Revision: rolloutRevB}
			f := newRolloutFixture(t, ms)
			f.addGroup(0, tt.revision, datastore.ServingGroupRunning)
			f.addPod("rollout-0-prefill-0-0", tt.revision, tt.mutate)
			pod, err := f.c.podsLister.Pods(ms.Namespace).Get("rollout-0-prefill-0-0")
			require.NoError(t, err)

			require.NoError(t, f.c.handleErrorPod(ms, utils.GenerateServingGroupName(ms.Name, 0), pod))
			assert.Equal(t, tt.want, f.c.rolloutFailures.peek(rolloutFailureKey{uid: ms.UID, revision: rolloutRevB}))
		})
	}
}
