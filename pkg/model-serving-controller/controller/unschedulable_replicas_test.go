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

const unschedulableTestRevision = "test-revision"

var (
	unschedulableT1 = metav1.NewTime(time.Date(2026, 11, 20, 6, 40, 0, 0, time.UTC))
	unschedulableT2 = metav1.NewTime(time.Date(2026, 11, 20, 6, 42, 0, 0, time.UTC))
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
	return []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: unschedulableT1}}
}

// unschedulableTestModelServing has one Role with an entry and a worker Pod per ServingGroup.
func unschedulableTestModelServing(replicas int32) *workloadv1alpha1.ModelServing {
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "test-image:latest"}}}
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unschedulable", UID: "unschedulable-uid"},
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

func unschedulableTestPods(ms *workloadv1alpha1.ModelServing, ordinal int) (entry, worker *corev1.Pod) {
	role := ms.Spec.Template.Roles[0]
	group := utils.GenerateServingGroupName(ms.Name, ordinal)
	roleID := utils.GenerateRoleID(role.Name, 0)
	hash := utils.CalRoleTemplateHash(role)
	entry = utils.GenerateEntryPod(*role.DeepCopy(), ms, group, roleID, unschedulableTestRevision, hash)
	worker = utils.GenerateWorkerPod(*role.DeepCopy(), ms, group, roleID, 1, unschedulableTestRevision, hash)
	entry.Status.Phase, worker.Status.Phase = corev1.PodPending, corev1.PodPending
	return entry, worker
}

func newUnschedulableTestController(t *testing.T, ms *workloadv1alpha1.ModelServing) *ModelServingController {
	t.Helper()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(ms.DeepCopy()), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	t.Cleanup(c.workqueue.ShutDown)
	return c
}

func TestUpdateModelServingStatusUnschedulableReplicas(t *testing.T) {
	type group struct {
		status      datastore.ServingGroupStatus
		entry       []corev1.PodCondition
		worker      []corev1.PodCondition
		terminating bool // the worker Pod is being deleted
	}
	tests := []struct {
		name              string
		initial           workloadv1alpha1.ModelServingStatus
		groups            []group
		wantUnschedulable int32
		wantSince         *metav1.Time
		wantAvailable     int32
	}{
		{
			name: "one gang member that fits nowhere makes the group unschedulable",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: waitingForGang(unschedulableT1), worker: unschedulable(unschedulableT2)},
			},
			wantUnschedulable: 1,
			wantSince:         &unschedulableT2,
		},
		{
			name: "the earliest Pod decides since when a group is unschedulable",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: unschedulable(unschedulableT2), worker: unschedulable(unschedulableT1)},
			},
			wantUnschedulable: 1,
			wantSince:         &unschedulableT1,
		},
		{
			name: "the oldest unschedulable group decides the reported time",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: unschedulable(unschedulableT2), worker: unschedulable(unschedulableT2)},
				{status: datastore.ServingGroupCreating, entry: unschedulable(unschedulableT1), worker: unschedulable(unschedulableT1)},
			},
			wantUnschedulable: 2,
			wantSince:         &unschedulableT1,
		},
		{
			name: "zero LastTransitionTime does not override a real timestamp from another group",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: unschedulable(metav1.Time{}), worker: unschedulable(metav1.Time{})},
				{status: datastore.ServingGroupCreating, entry: unschedulable(unschedulableT2), worker: unschedulable(unschedulableT2)},
			},
			wantUnschedulable: 2,
			wantSince:         &unschedulableT2,
		},
		{
			name: "zero LastTransitionTime on all unschedulable Pods leaves UnschedulableSince nil",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: unschedulable(metav1.Time{}), worker: unschedulable(metav1.Time{})},
			},
			wantUnschedulable: 1,
			wantSince:         nil,
		},
		{
			name:    "the time moves to the next oldest group once the oldest is scheduled",
			initial: workloadv1alpha1.ModelServingStatus{UnschedulableReplicas: 2, UnschedulableSince: &unschedulableT1},
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: scheduled()},
				{status: datastore.ServingGroupCreating, entry: unschedulable(unschedulableT2), worker: unschedulable(unschedulableT2)},
			},
			wantUnschedulable: 1,
			wantSince:         &unschedulableT2,
		},
		{
			name: "a running group is available and a starting group is neither",
			groups: []group{
				{status: datastore.ServingGroupRunning, entry: scheduled(), worker: scheduled()},
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: scheduled()},
				{status: datastore.ServingGroupCreating, entry: unschedulable(unschedulableT1), worker: unschedulable(unschedulableT1)},
			},
			wantUnschedulable: 1,
			wantSince:         &unschedulableT1,
			wantAvailable:     1,
		},
		{
			name: "a group that is being deleted is not counted",
			groups: []group{
				{status: datastore.ServingGroupDeleting, entry: unschedulable(unschedulableT1), worker: unschedulable(unschedulableT1)},
			},
		},
		{
			name: "a terminating Pod is ignored",
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: unschedulable(unschedulableT1), terminating: true},
			},
		},
		{
			name: "a Pod held by a scheduling gate or hit by a scheduler error does not make the group unschedulable",
			groups: []group{
				{
					status: datastore.ServingGroupCreating,
					entry:  notScheduled(corev1.PodReasonSchedulingGated, unschedulableT1),
					worker: notScheduled(corev1.PodReasonSchedulerError, unschedulableT1),
				},
			},
		},
		{
			name:    "the fields are cleared once the Pods are scheduled",
			initial: workloadv1alpha1.ModelServingStatus{UnschedulableReplicas: 1, UnschedulableSince: &unschedulableT1},
			groups: []group{
				{status: datastore.ServingGroupCreating, entry: scheduled(), worker: scheduled()},
			},
		},
		{
			name:    "the fields are cleared when no group is left",
			initial: workloadv1alpha1.ModelServingStatus{UnschedulableReplicas: 1, UnschedulableSince: &unschedulableT1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := unschedulableTestModelServing(int32(max(1, len(tt.groups))))
			ms.Status = tt.initial
			c := newUnschedulableTestController(t, ms)
			key := utils.GetNamespaceName(ms)
			for ordinal, g := range tt.groups {
				c.store.AddServingGroup(key, ordinal, unschedulableTestRevision)
				require.NoError(t, c.store.UpdateServingGroupStatus(key, utils.GenerateServingGroupName(ms.Name, ordinal), g.status))
				entry, worker := unschedulableTestPods(ms, ordinal)
				entry.Status.Conditions, worker.Status.Conditions = g.entry, g.worker
				if g.terminating {
					worker.DeletionTimestamp = &unschedulableT2
				}
				require.NoError(t, c.podsInformer.GetIndexer().Add(entry))
				require.NoError(t, c.podsInformer.GetIndexer().Add(worker))
			}

			require.NoError(t, c.UpdateModelServingStatus(ms, unschedulableTestRevision))

			actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tt.wantUnschedulable, actual.Status.UnschedulableReplicas)
			require.True(t, tt.wantSince.Equal(actual.Status.UnschedulableSince), "want unschedulableSince %v, got %v", tt.wantSince, actual.Status.UnschedulableSince)
			require.Equal(t, tt.wantAvailable, actual.Status.AvailableReplicas)
			require.LessOrEqual(t, actual.Status.UnschedulableReplicas, actual.Status.Replicas-actual.Status.AvailableReplicas)
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
		add            bool
		old            []corev1.PodCondition
		new            []corev1.PodCondition
		oldTerminating bool
		newTerminating bool
		foreignOwner   bool
		wantEnqueue    bool
	}{
		{name: "a Pod becomes unschedulable", new: unschedulable(unschedulableT1), wantEnqueue: true},
		{name: "an unschedulable Pod is scheduled", old: unschedulable(unschedulableT1), new: scheduled(), wantEnqueue: true},
		{name: "a Pod is unschedulable since another time", old: unschedulable(unschedulableT1), new: unschedulable(unschedulableT2), wantEnqueue: true},
		{name: "an unschedulable Pod is added", add: true, new: unschedulable(unschedulableT1), wantEnqueue: true},
		{
			name:           "an unschedulable Pod starts terminating",
			old:            unschedulable(unschedulableT1),
			new:            unschedulable(unschedulableT1),
			newTerminating: true,
			wantEnqueue:    true,
		},
		{
			name:           "an already-terminating unschedulable Pod updates again",
			old:            unschedulable(unschedulableT1),
			new:            unschedulable(unschedulableT1),
			oldTerminating: true,
			newTerminating: true,
			wantEnqueue:    false,
		},
		{
			name:           "a scheduled Pod starts terminating",
			old:            scheduled(),
			new:            scheduled(),
			newTerminating: true,
			wantEnqueue:    false,
		},
		{
			name:           "a foreign-owned unschedulable Pod starts terminating",
			old:            unschedulable(unschedulableT1),
			new:            unschedulable(unschedulableT1),
			newTerminating: true,
			foreignOwner:   true,
			wantEnqueue:    false,
		},
		{
			name: "the scheduler only rewrites the message",
			old:  withMessage(unschedulable(unschedulableT1), "0/3 nodes are available"),
			new:  withMessage(unschedulable(unschedulableT1), "0/4 nodes are available"),
		},
		{name: "a gang member that fits keeps waiting", old: waitingForGang(unschedulableT1), new: waitingForGang(unschedulableT1)},
		{name: "a Pod without conditions is added", add: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := unschedulableTestModelServing(1)
			c := newUnschedulableTestController(t, ms)
			newPod, _ := unschedulableTestPods(ms, 0)
			if tt.foreignOwner {
				newPod.OwnerReferences[0].UID = "other-uid"
			}
			newPod.Status.Conditions = tt.new
			if tt.newTerminating {
				newPod.DeletionTimestamp = &unschedulableT2
			}
			require.NoError(t, c.podsInformer.GetIndexer().Add(newPod))

			var oldObj interface{}
			if !tt.add {
				oldPod := newPod.DeepCopy()
				oldPod.Status.Conditions = tt.old
				if tt.oldTerminating {
					oldPod.DeletionTimestamp = &unschedulableT2
				} else {
					oldPod.DeletionTimestamp = nil
				}
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
