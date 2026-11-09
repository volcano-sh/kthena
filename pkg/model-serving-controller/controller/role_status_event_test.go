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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// A shared informer writes a Pod into its cache before it calls the Pod update
// handler, so a sync that runs in between can change the role's status first.
// Whichever of them changes the status must send the Event for that change, once.
func TestRoleStatusEventSentOnceWhicheverPathChangesIt(t *testing.T) {
	changes := []struct {
		name      string
		updatePod func(*corev1.Pod)
		from, to  datastore.RoleStatus
		reason    string
		// requeued says whether the ModelServing is queued afterwards. Only the Pod
		// handler requeues: the sync and the refresh never requeue for a status they
		// change, because the rest of the sync already sees it.
		requeued func(handlerFirst, coordinated bool) bool
	}{
		// The handler requeues when it moves the role to Running itself.
		{name: "pod becomes ready", updatePod: setPodReady, from: datastore.RoleCreating, to: datastore.RoleRunning, reason: "RoleRunning",
			requeued: func(handlerFirst, _ bool) bool { return handlerFirst }},
		// handleErrorPod always requeues.
		{name: "pod fails", updatePod: setPodFailed, from: datastore.RoleRunning, to: datastore.RoleCreating, reason: "RoleCreating",
			requeued: func(bool, bool) bool { return true }},
		// The handler leaves the role's status alone, so only the sync changes it.
		// updatePod's default branch requeues only under role coordination.
		{name: "pod turns not ready", updatePod: setPodNotReady, from: datastore.RoleRunning, to: datastore.RoleCreating, reason: "RoleCreating",
			requeued: func(_, coordinated bool) bool { return coordinated }},
	}
	syncPaths := []struct {
		name        string
		coordinated bool
	}{
		{name: "sync"},
		{name: "coordinated availability refresh", coordinated: true},
	}
	for _, change := range changes {
		for _, syncPath := range syncPaths {
			for _, syncFirst := range []bool{false, true} {
				order := "pod handler before " + syncPath.name
				if syncFirst {
					order = syncPath.name + " before pod handler"
				}
				t.Run(change.name+"/"+order, func(t *testing.T) {
					ms := createStandardModelServing("role-status", 1, 1)
					if syncPath.coordinated {
						ms.ResourceVersion = "1"
						ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate, RoleCoordination: coordinationForTest("100%")}
					}
					// A second role without Pods keeps the ServingGroup from becoming Running,
					// so the ServingGroup's own requeue cannot hide a missing role requeue.
					decode := *ms.Spec.Template.Roles[0].DeepCopy()
					decode.Name = "decode"
					ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, decode)
					c := newRevisionTestController(t, ms)
					recorder := record.NewFakeRecorder(10)
					c.recorder = recorder

					key := utils.GetNamespaceName(ms)
					group := utils.GenerateServingGroupName(ms.Name, 0)
					revision := utils.ModelServingRevision(ms)
					for _, role := range ms.Spec.Template.Roles {
						c.store.AddServingGroupAndRole(key, group, revision, utils.CalRoleTemplateHash(role), role.Name, utils.GenerateRoleID(role.Name, 0))
					}
					prefill := ms.Spec.Template.Roles[0]
					prefillID := utils.GenerateRoleID(prefill.Name, 0)
					require.NoError(t, c.store.UpdateRoleStatus(key, group, prefill.Name, prefillID, change.from))
					pod := utils.GenerateEntryPod(prefill, ms, group, prefillID, revision, utils.CalRoleTemplateHash(prefill))
					change.updatePod(pod)
					// The coordinated refresh reads Pods from the API server, the sync from
					// the informer cache.
					_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.podsInformer.GetIndexer().Add(pod))

					runSync := func() {
						if syncPath.coordinated {
							require.NoError(t, c.refreshCoordinatedAvailability(ms))
							return
						}
						require.NoError(t, c.manageRoleReplicasPerGroup(context.Background(), ms, group, prefill, 0, revision, true, nil))
					}
					if syncFirst {
						runSync()
					}
					c.updatePod(nil, pod)
					if !syncFirst {
						runSync()
					}

					assert.Equal(t, change.to, c.store.GetRoleStatus(key, group, prefill.Name, prefillID))
					assert.Equal(t, 1, countRoleEvents(recorder, change.reason), "the role changed status once")
					if change.requeued(!syncFirst, syncPath.coordinated) {
						assertQueuedKey(t, c.workqueue, key.String())
					}
					assertQueueEmpty(t, c.workqueue)
				})
			}
		}
	}
}

// The sync also moves a Running role back to Creating when it cannot read the role's
// revision from the history. That change must send RoleCreating too.
func TestRoleCreatingEventSentWhenRevisionHistoryIsMissing(t *testing.T) {
	ms := createStandardModelServing("missing-history", 1, 1)
	c := newRevisionTestController(t, ms)
	recorder := record.NewFakeRecorder(10)
	c.recorder = recorder

	key := utils.GetNamespaceName(ms)
	group := utils.GenerateServingGroupName(ms.Name, 0)
	role := ms.Spec.Template.Roles[0]
	roleID := utils.GenerateRoleID(role.Name, 0)
	// No ControllerRevision is recorded for this revision.
	c.store.AddServingGroupAndRole(key, group, "unknown", utils.CalRoleTemplateHash(role), role.Name, roleID)
	require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, roleID, datastore.RoleRunning))

	err := c.manageRoleReplicasPerGroup(context.Background(), ms, group, role, 0, utils.ModelServingRevision(ms), true, nil)
	require.ErrorContains(t, err, "ControllerRevision is missing", "the sync returns the history error so it is retried")
	assert.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, group, role.Name, roleID))
	assert.Equal(t, 1, countRoleEvents(recorder, "RoleCreating"), "the role changed status once")
}

func setPodReady(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

func setPodFailed(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
}

func setPodNotReady(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
}

// countRoleEvents drains the recorder and counts the Normal Events with this reason.
func countRoleEvents(recorder *record.FakeRecorder, reason string) int {
	count := 0
	for len(recorder.Events) > 0 {
		if strings.HasPrefix(<-recorder.Events, corev1.EventTypeNormal+" "+reason+" ") {
			count++
		}
	}
	return count
}
