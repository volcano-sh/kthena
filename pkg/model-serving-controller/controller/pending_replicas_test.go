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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const pendingTestRevision = "test-revision"

var (
	pendingT1 = metav1.NewTime(time.Date(2026, 11, 20, 6, 40, 0, 0, time.UTC))
	pendingT2 = metav1.NewTime(time.Date(2026, 11, 20, 6, 42, 0, 0, time.UTC))
)

// notScheduled is the PodScheduled condition a scheduler sets on a Pod it has not placed.
func notScheduled(reason string, since metav1.Time) []corev1.PodCondition {
	return []corev1.PodCondition{{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionFalse,
		Reason:             reason,
		LastTransitionTime: since,
	}}
}

func unschedulable(since metav1.Time) []corev1.PodCondition {
	return notScheduled(corev1.PodReasonUnschedulable, since)
}

// waitingForGang is what Volcano reports for a gang member that fits on a node
// while the gang as a whole cannot be satisfied.
func waitingForGang(since metav1.Time) []corev1.PodCondition {
	return notScheduled("Schedulable", since)
}

func scheduled() []corev1.PodCondition {
	return []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: pendingT1}}
}

// pendingTestModelServing has one Role with an entry and a worker Pod per ServingGroup.
func pendingTestModelServing(replicas int32) *workloadv1alpha1.ModelServing {
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "test-image:latest"}}}
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pending", UID: "pending-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To(replicas),
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name:           "prefill",
				Replicas:       ptr.To[int32](1),
				EntryTemplate:  workloadv1alpha1.PodTemplateSpec{Spec: podSpec},
				WorkerReplicas: 1,
				WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{Spec: podSpec},
			}}},
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
		},
	}
}

func pendingTestPods(ms *workloadv1alpha1.ModelServing, ordinal int) (entry, worker *corev1.Pod) {
	role := ms.Spec.Template.Roles[0]
	group := utils.GenerateServingGroupName(ms.Name, ordinal)
	roleID := utils.GenerateRoleID(role.Name, 0)
	hash := utils.CalRoleTemplateHash(role)
	entry = utils.GenerateEntryPod(*role.DeepCopy(), ms, group, roleID, pendingTestRevision, hash)
	worker = utils.GenerateWorkerPod(*role.DeepCopy(), ms, group, roleID, 1, pendingTestRevision, hash)
	entry.Status.Phase, worker.Status.Phase = corev1.PodPending, corev1.PodPending
	return entry, worker
}

func newPendingTestController(t *testing.T, ms *workloadv1alpha1.ModelServing) *ModelServingController {
	t.Helper()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(ms.DeepCopy()), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	t.Cleanup(c.workqueue.ShutDown)
	return c
}

func TestUpdateModelServingStatusPendingReplicas(t *testing.T) {
	type group struct {
		status      datastore.ServingGroupStatus
		entry       []corev1.PodCondition
		worker      []corev1.PodCondition
		terminating bool // the worker Pod is being deleted
	}
	tests := []struct {
		name          string
		initial       workloadv1alpha1.ModelServingStatus
		groups        []group
		wantPending   int32
		wantSince     *metav1.Time
		wantAvailable int32
	}{
		{
			name: "one gang member that fits nowhere makes the group pending",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: waitingForGang(pendingT1), worker: unschedulable(pendingT2)},
			},
			wantPending: 1,
			wantSince:   &pendingT2,
		},
		{
			name: "the earliest Pod decides since when a group is pending",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: unschedulable(pendingT2), worker: unschedulable(pendingT1)},
			},
			wantPending: 1,
			wantSince:   &pendingT1,
		},
		{
			name: "the oldest pending group decides the reported time",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: unschedulable(pendingT2), worker: unschedulable(pendingT2)},
				{status: datastore.ServingGroupCreating, entry: unschedulable(pendingT1), worker: unschedulable(pendingT1)},
			},
			wantPending: 2,
			wantSince:   &pendingT1,
		},
		{
			name:    "the time moves to the next oldest group once the oldest is scheduled",
			initial: workloadv1alpha1.ModelServingStatus{PendingReplicas: 2, PendingSince: &pendingT1},
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: scheduled()},
				{status: datastore.ServingGroupCreating, entry: unschedulable(pendingT2), worker: unschedulable(pendingT2)},
			},
			wantPending: 1,
			wantSince:   &pendingT2,
		},
		{
			name: "a running group is available and a starting group is neither",
			groups: []group{
				{status: datastore.ServingGroupRunning, entry: scheduled(), worker: scheduled()},
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: scheduled()},
				{status: datastore.ServingGroupCreating, entry: unschedulable(pendingT1), worker: unschedulable(pendingT1)},
			},
			wantPending:   1,
			wantSince:     &pendingT1,
			wantAvailable: 1,
		},
		{
			name: "a group that is being deleted is not counted",
			groups: []group{
				{status: datastore.ServingGroupDeleting, entry: unschedulable(pendingT1), worker: unschedulable(pendingT1)},
			},
		},
		{
			name: "a terminating Pod is ignored",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: unschedulable(pendingT1), terminating: true},
			},
		},
		{
			name: "a Pod held by a scheduling gate or hit by a scheduler error does not make the group pending",
			groups: []group{
				{
					status: datastore.ServingGroupCreating,
					entry:  notScheduled(corev1.PodReasonSchedulingGated, pendingT1),
					worker: notScheduled(corev1.PodReasonSchedulerError, pendingT1),
				},
			},
		},
		{
			name:    "the fields are cleared once the Pods are scheduled",
			initial: workloadv1alpha1.ModelServingStatus{PendingReplicas: 1, PendingSince: &pendingT1},
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: scheduled()},
			},
		},
		{
			name:    "the fields are cleared when no group is left",
			initial: workloadv1alpha1.ModelServingStatus{PendingReplicas: 1, PendingSince: &pendingT1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := pendingTestModelServing(int32(max(1, len(tt.groups))))
			ms.Status = tt.initial
			c := newPendingTestController(t, ms)
			key := utils.GetNamespaceName(ms)
			for ordinal, g := range tt.groups {
				c.store.AddServingGroup(key, ordinal, pendingTestRevision)
				require.NoError(t, c.store.UpdateServingGroupStatus(key, utils.GenerateServingGroupName(ms.Name, ordinal), g.status))
				entry, worker := pendingTestPods(ms, ordinal)
				entry.Status.Conditions, worker.Status.Conditions = g.entry, g.worker
				if g.terminating {
					worker.DeletionTimestamp = &pendingT2
				}
				require.NoError(t, c.podsInformer.GetIndexer().Add(entry))
				require.NoError(t, c.podsInformer.GetIndexer().Add(worker))
			}

			require.NoError(t, c.UpdateModelServingStatus(ms, pendingTestRevision))

			actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tt.wantPending, actual.Status.PendingReplicas)
			require.True(t, tt.wantSince.Equal(actual.Status.PendingSince), "want pendingSince %v, got %v", tt.wantSince, actual.Status.PendingSince)
			require.Equal(t, tt.wantAvailable, actual.Status.AvailableReplicas)
			require.LessOrEqual(t, actual.Status.PendingReplicas, actual.Status.Replicas-actual.Status.AvailableReplicas)
		})
	}
}

func TestUpdatePodEnqueuesWhenUnschedulableStateChanges(t *testing.T) {
	withMessage := func(conditions []corev1.PodCondition, message string) []corev1.PodCondition {
		conditions[0].Message = message
		return conditions
	}
	tests := []struct {
		name string
		// add delivers the Pod as an add event, without an old object.
		add         bool
		old         []corev1.PodCondition
		new         []corev1.PodCondition
		wantEnqueue bool
	}{
		{name: "a Pod becomes unschedulable", new: unschedulable(pendingT1), wantEnqueue: true},
		{name: "an unschedulable Pod is scheduled", old: unschedulable(pendingT1), new: scheduled(), wantEnqueue: true},
		{name: "a Pod is unschedulable since another time", old: unschedulable(pendingT1), new: unschedulable(pendingT2), wantEnqueue: true},
		{name: "an unschedulable Pod is added", add: true, new: unschedulable(pendingT1), wantEnqueue: true},
		{
			name: "the scheduler only rewrites the message",
			old:  withMessage(unschedulable(pendingT1), "0/3 nodes are available"),
			new:  withMessage(unschedulable(pendingT1), "0/4 nodes are available"),
		},
		{name: "a gang member that fits keeps waiting", old: waitingForGang(pendingT1), new: waitingForGang(pendingT1)},
		{name: "a Pod without conditions is added", add: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := pendingTestModelServing(1)
			c := newPendingTestController(t, ms)
			newPod, _ := pendingTestPods(ms, 0)
			newPod.Status.Conditions = tt.new
			require.NoError(t, c.podsInformer.GetIndexer().Add(newPod))

			var oldObj interface{}
			if !tt.add {
				oldPod := newPod.DeepCopy()
				oldPod.Status.Conditions = tt.old
				oldObj = oldPod
			}
			c.updatePod(oldObj, newPod)

			if tt.wantEnqueue {
				assertQueuedKey(t, c.workqueue, namespacedKey(ms.Namespace, ms.Name))
			}
			assertQueueEmpty(t, c.workqueue)
		})
	}
}
