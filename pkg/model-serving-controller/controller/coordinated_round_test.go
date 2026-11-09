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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
)

func TestRoleReadinessSyncEmitsStatusEventsBeforeReadyCallback(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		mode    api.RolloutStrategyType
		refresh bool
	}{
		{name: "ServingGroup sync", mode: api.ServingGroupRollingUpdate},
		{name: "Role sync", mode: api.RoleRollingUpdate},
		{name: "Role availability refresh", mode: api.RoleRollingUpdate, refresh: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			ms := mergeReadyModelServing("role-status-events", 1, 1)
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: scenario.mode}
			ms.Spec.Template.Roles = ms.Spec.Template.Roles[:1]
			role := &ms.Spec.Template.Roles[0]
			role.WorkerReplicas = 1
			role.WorkerTemplate = role.EntryTemplate.DeepCopy()
			revision := utils.ModelServingRevision(ms)
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = revision, revision
			c := newRevisionTestController(t, ms)
			key, group, roleID := utils.GetNamespaceName(ms), ms.Name+"-0", role.Name+"-0"
			c.store.AddServingGroup(key, 0, revision)
			pods := addMergeReadyInstance(t, c, ms, group, *role, 0, revision, revision, utils.CalRoleTemplateHash(*role))
			require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, roleID, datastore.RoleCreating))
			recorder := record.NewFakeRecorder(8)
			c.recorder = recorder
			client := c.kubeClientSet.(*kubefake.Clientset)
			for _, ready := range []bool{false, true, false} {
				worker := pods[1].DeepCopy()
				worker.Status.Conditions[0].Status = corev1.ConditionFalse
				if ready {
					worker.Status.Conditions[0].Status = corev1.ConditionTrue
				}
				_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, worker, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(worker))
				previous := c.store.GetRoleStatus(key, group, role.Name, roleID)
				status := datastore.RoleCreating
				if ready {
					status = datastore.RoleRunning
				}
				for attempt := 0; attempt < 2; attempt++ {
					client.ClearActions()
					if scenario.refresh {
						require.NoError(t, c.refreshCoordinatedAvailabilityInRound(ctx, ms))
					} else {
						require.NoError(t, c.manageRoleReplicasPerGroup(ctx, ms, group, *role, 0, revision, true, nil))
					}
					require.Equal(t, status, c.store.GetRoleStatus(key, group, role.Name, roleID))
					// The sync observes health before the entry's Ready callback.
					require.NoError(t, c.handleReadyPod(ms, group, pods[0]))
					requireNoMergeReadyPodMutations(t, client.Actions())
					if attempt == 0 && previous != status {
						require.Len(t, recorder.Events, 1)
						require.Equal(t, fmt.Sprintf("Normal Role%s Role %s/%s in ServingGroup %s is now %s", status, role.Name, roleID, group, status), <-recorder.Events)
					} else {
						require.Empty(t, recorder.Events, "unchanged readiness must not emit another status event")
					}
				}
			}
		})
	}
}

func TestProtectedRoleHistorySurvivesReplacementGap(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		state          datastore.ServingGroupStatus
		complete       bool
		missingHistory bool
	}{
		{name: "creating group", state: datastore.ServingGroupCreating},
		{name: "cached running group", state: datastore.ServingGroupRunning},
		{name: "complete target", state: datastore.ServingGroupRunning, complete: true},
		{name: "missing history", state: datastore.ServingGroupCreating, missingHistory: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			ms := mergeReadyModelServing("protected-history-gap", 1, 3)
			ms.Spec.RolloutStrategy.RoleCoordination = nil
			ms.Spec.Template.Roles = ms.Spec.Template.Roles[:1]
			role := &ms.Spec.Template.Roles[0]
			role.Partition = &intstr.IntOrString{Type: intstr.Int, IntVal: 1}
			role.EntryTemplate.Spec.Containers[0].Image = "image:target"
			old := ms.DeepCopy()
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:previous"
			baseline, target := utils.ModelServingRevision(old), utils.ModelServingRevision(ms)
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = baseline, target
			c := newRevisionTestController(t, ms)
			require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, target), metav1.DeleteOptions{}))
			recordTestRevision(t, c.kubeClientSet, ms, target)
			if !scenario.missingHistory {
				recordTestRevision(t, c.kubeClientSet, old, baseline)
			}
			key, group := utils.GetNamespaceName(ms), ms.Name+"-0"
			c.store.AddServingGroup(key, 0, baseline)
			survivors := make(map[string]types.UID)
			first := 1
			if scenario.complete {
				first = 0
			}
			for ordinal := first; ordinal < 3; ordinal++ {
				pod := addMergeReadyInstance(t, c, ms, group, *role, ordinal, target, target, utils.CalRoleTemplateHash(*role))[0]
				survivors[pod.Name] = pod.UID
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, scenario.state))
			require.NoError(t, c.updateModelServingStatusInRound(ctx, ms, target, nil))
			observed, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			wantRevision := baseline
			if scenario.complete {
				wantRevision = target
			}
			require.Equal(t, wantRevision, observed.Status.CurrentRevision)
			require.Equal(t, target, observed.Status.UpdateRevision)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(observed))
			instances, err := c.store.GetRoleList(key, group, role.Name)
			require.NoError(t, err)
			err = c.scaleUpRoles(ctx, observed, group, *role, instances, 3, 0, target, true)
			if scenario.missingHistory {
				require.Error(t, err, "missing history must not be replaced by the target template")
			} else {
				require.NoError(t, err)
			}
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			wantCount := 3
			if scenario.missingHistory {
				wantCount = 2
			}
			require.Len(t, pods.Items, wantCount)
			for _, pod := range pods.Items {
				if uid, survived := survivors[pod.Name]; survived {
					require.Equal(t, uid, pod.UID)
					require.Equal(t, "image:target", pod.Spec.Containers[0].Image)
					require.Equal(t, target, utils.ObjectRevision(&pod))
				} else {
					require.Equal(t, utils.GeneratePodName(group, role.Name+"-0", 0), pod.Name)
					require.Equal(t, "image:previous", pod.Spec.Containers[0].Image)
					require.Equal(t, baseline, utils.ObjectRevision(&pod))
				}
			}
		})
	}
}

func TestServingGroupBudgetSharesOneObservation(t *testing.T) {
	ctx := context.Background()
	ms := mergeReadyModelServing("group-budget-round", 3, 1)
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &api.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1))}}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		role.WorkerReplicas = 1
		role.WorkerTemplate = role.EntryTemplate.DeepCopy()
	}
	revision, key := utils.ModelServingRevision(ms), utils.GetNamespaceName(ms)
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = revision, revision
	c := newRevisionTestController(t, ms)
	require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
	recordTestRevision(t, c.kubeClientSet, ms, revision)
	var worker *corev1.Pod
	for ordinal := 0; ordinal < 3; ordinal++ {
		group := utils.GenerateServingGroupName(ms.Name, ordinal)
		c.store.AddServingGroup(key, ordinal, revision)
		for _, role := range ms.Spec.Template.Roles {
			pods := addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role))
			worker = pods[1]
		}
		require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
	}
	client := c.kubeClientSet.(*kubefake.Clientset)
	for _, ready := range []bool{true, false} {
		if !ready {
			unready := worker.DeepCopy()
			unready.Status.Conditions[0].Status = corev1.ConditionFalse
			_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, unready, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
		groups, err := c.store.GetServingGroupByModelServing(key)
		require.NoError(t, err)
		client.ClearActions()
		round := withObservationRound(ctx, ms)
		observed, err := c.observeServingGroups(round, ms, groups)
		require.NoError(t, err)
		budget, err := c.servingGroupRolloutBudget(round, ms, observed, revision)
		require.NoError(t, err)
		expected := rolloutBudget{total: 1, healthy: 1}
		if !ready {
			expected = rolloutBudget{}
		}
		require.Equal(t, expected, budget, "the next budget must observe API readiness loss without an informer update")
		podLists, historyGets := 0, 0
		for _, action := range client.Actions() {
			if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
				podLists++
			}
			if action.GetVerb() == "get" && action.GetResource().Resource == "controllerrevisions" {
				historyGets++
			}
		}
		require.Equal(t, 1, podLists, "all groups, Roles and workers share one fresh Pod observation per budget")
		require.Equal(t, 1, historyGets, "one budget reuses the immutable applied history")
	}
}

func TestReadinessObservationsShareReadsAndRefreshBetweenCalls(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			ms := mergeReadyModelServing("readiness-facts", 1, 1)
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: mode}
			for i := range ms.Spec.Template.Roles {
				role := &ms.Spec.Template.Roles[i]
				role.WorkerReplicas = 1
				role.WorkerTemplate = role.EntryTemplate.DeepCopy()
			}
			revision, key, group := utils.ModelServingRevision(ms), utils.GetNamespaceName(ms), ms.Name+"-0"
			c := newRevisionTestController(t, ms)
			require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
			recordTestRevision(t, c.kubeClientSet, ms, revision)
			c.store.AddServingGroup(key, 0, revision)
			var worker *corev1.Pod
			for _, role := range ms.Spec.Template.Roles {
				worker = addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role))[1]
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			client := c.kubeClientSet.(*kubefake.Clientset)
			for _, observation := range []string{"Ready", "worker NotReady", "foreign replacement", "owned replacement"} {
				switch observation {
				case "worker NotReady":
					worker = worker.DeepCopy()
					worker.Status.Conditions[0].Status = corev1.ConditionFalse
					_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, worker, metav1.UpdateOptions{})
					require.NoError(t, err)
				case "foreign replacement":
					require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, worker.Name, metav1.DeleteOptions{}))
					worker = worker.DeepCopy()
					worker.UID += "-replacement"
					worker.OwnerReferences[0].UID = "another-owner"
					worker.Status.Conditions[0].Status = corev1.ConditionTrue
					_, err := client.CoreV1().Pods(ms.Namespace).Create(ctx, worker, metav1.CreateOptions{})
					require.NoError(t, err)
				case "owned replacement":
					worker.OwnerReferences[0].UID = ms.UID
					_, err := client.CoreV1().Pods(ms.Namespace).Update(ctx, worker, metav1.UpdateOptions{})
					require.NoError(t, err)
				}
				// The informer and store retain their original Ready worker UID.
				client.ClearActions()
				ready, err := c.checkServingGroupReadyWithContext(ctx, ms, group)
				require.NoError(t, err)
				require.Equal(t, observation == "Ready" || observation == "owned replacement", ready, observation)
				lists, gets := 0, 0
				for _, action := range client.Actions() {
					if action.Matches("list", "pods") {
						lists++
					}
					if action.Matches("get", "controllerrevisions") {
						gets++
					}
				}
				require.Equal(t, 1, lists, "each readiness call shares one new Pod snapshot")
				require.Equal(t, 1, gets, "Roles reuse their common immutable history")
			}
		})
	}
}

func TestReadyEventSharesOneObservation(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			ms := mergeReadyModelServing("ready-event-facts", 1, 1)
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: mode}
			for i := range ms.Spec.Template.Roles {
				role := &ms.Spec.Template.Roles[i]
				role.WorkerReplicas = 1
				role.WorkerTemplate = role.EntryTemplate.DeepCopy()
			}
			revision, key, group := utils.ModelServingRevision(ms), utils.GetNamespaceName(ms), ms.Name+"-0"
			c := newRevisionTestController(t, ms)
			require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
			recordTestRevision(t, c.kubeClientSet, ms, revision)
			c.store.AddServingGroup(key, 0, revision)
			client := c.kubeClientSet.(*kubefake.Clientset)
			var entry, worker *corev1.Pod
			for _, role := range ms.Spec.Template.Roles {
				pods := addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role))
				if entry == nil {
					entry, worker = pods[0], pods[1]
				}
			}
			worker = worker.DeepCopy()
			worker.Status.Conditions[0].Status = corev1.ConditionFalse
			_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, worker, metav1.UpdateOptions{})
			require.NoError(t, err)
			for _, complete := range []bool{false, true} {
				if complete {
					require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, worker.Name, metav1.DeleteOptions{}))
					worker = worker.DeepCopy()
					worker.UID += "-replacement"
					worker.Status.Conditions[0].Status = corev1.ConditionTrue
					_, err = client.CoreV1().Pods(ms.Namespace).Create(ctx, worker, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				client.ClearActions()
				require.NoError(t, c.handleReadyPod(ms, group, entry))
				lists, gets := 0, 0
				for _, action := range client.Actions() {
					if action.Matches("list", "pods") {
						lists++
					}
					if action.Matches("get", "controllerrevisions") {
						gets++
					}
				}
				require.Equal(t, 1, lists, "one Ready event shares its live observation across all readiness checks")
				require.Equal(t, 1, gets, "hook and readiness use the same immutable history")
			}
		})
	}
}

func TestRolePlanningObservationsRefreshBetweenCalls(t *testing.T) {
	ctx := context.Background()
	ms := mergeReadyModelServing("role-planning-facts", 1, 1)
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		role.WorkerReplicas = 1
		role.WorkerTemplate = role.EntryTemplate.DeepCopy()
	}
	revision, key, group := utils.ModelServingRevision(ms), utils.GetNamespaceName(ms), ms.Name+"-0"
	c := newRevisionTestController(t, ms)
	client := c.kubeClientSet.(*kubefake.Clientset)
	require.NoError(t, client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
	recordTestRevision(t, client, ms, revision)
	c.store.AddServingGroup(key, 0, revision)
	var entry, worker *corev1.Pod
	for _, role := range ms.Spec.Template.Roles {
		pods := addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role))
		if entry == nil {
			entry, worker = pods[0].DeepCopy(), pods[1].DeepCopy()
		}
	}
	for _, observation := range []string{"Ready", "worker NotReady", "foreign entry", "owned replacement"} {
		switch observation {
		case "worker NotReady":
			worker = worker.DeepCopy()
			worker.Status.Conditions[0].Status = corev1.ConditionFalse
			_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, worker, metav1.UpdateOptions{})
			require.NoError(t, err)
		case "foreign entry":
			worker = worker.DeepCopy()
			worker.Status.Conditions[0].Status = corev1.ConditionTrue
			_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, worker, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, entry.Name, metav1.DeleteOptions{}))
			entry = entry.DeepCopy()
			entry.UID += "-replacement"
			entry.OwnerReferences[0].UID = "another-owner"
			_, err = client.CoreV1().Pods(ms.Namespace).Create(ctx, entry, metav1.CreateOptions{})
			require.NoError(t, err)
		case "owned replacement":
			entry = entry.DeepCopy()
			entry.OwnerReferences[0].UID = ms.UID
			_, err := client.CoreV1().Pods(ms.Namespace).Update(ctx, entry, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
		// Keep the informer and datastore at their original Ready identities.
		for _, planning := range []string{"comparison", "candidates"} {
			client.ClearActions()
			sg := datastore.ServingGroup{Name: group, Revision: revision}
			if planning == "comparison" {
				want := templateEquivalent
				if observation == "foreign entry" {
					want = templateUnknown
				}
				require.Equal(t, want, c.compareServingGroupTemplate(ctx, ms, sg, revision), observation)
			} else {
				selected, outdated, err := c.rolesToDeleteForRoleRollingUpdateInRound(ctx, ms, sg, nil)
				require.NoError(t, err)
				require.Empty(t, selected, "current or unresolved identities must not be deleted")
				require.Equal(t, observation == "foreign entry", outdated, observation)
			}
			lists, gets := 0, 0
			for _, action := range client.Actions() {
				if action.GetResource().Resource == "pods" {
					require.Contains(t, []string{"get", "list"}, action.GetVerb(), "planning must not mutate Pods")
				}
				if action.Matches("list", "pods") {
					lists++
				}
				if action.Matches("get", "controllerrevisions") {
					gets++
				}
			}
			require.Equal(t, 1, lists, planning+": each call shares only its own Pod observation")
			require.Equal(t, 1, gets, planning+": common immutable history is read once")
		}
		actual, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, entry.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, entry.UID, actual.UID)
	}
}

func TestCoordinatedRoundReusesReadsAndRefreshesLiveFactsBetweenRounds(t *testing.T) {
	ms := mergeReadyModelServing("round-facts", 1, 1)
	c := newRevisionTestController(t, ms)
	key, revision, group := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms), ms.Name+"-0"
	c.store.AddServingGroup(key, 0, revision)
	role := ms.Spec.Template.Roles[0]
	// Measure the steady-state canonical path separately from one-time legacy pinning.
	require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(context.Background(), utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
	recordTestRevision(t, c.kubeClientSet, ms, revision)
	pod := addMergeReadyInstance(t, c, ms, group, role, 0, revision, revision, utils.CalRoleTemplateHash(role))[0]
	instances, err := c.store.GetRoleList(key, group, role.Name)
	require.NoError(t, err)
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()
	round := withCoordinatedRound(context.Background(), ms)
	for i := 0; i < 5; i++ {
		_, observed, err := c.coordinatedLiveRole(round, ms, group, role.Name, instances[0])
		require.NoError(t, err)
		require.Equal(t, pod.UID, observed.UID)
		ready, err := c.checkRoleReadyInRound(round, ms, group, role.Name, instances[0].Name)
		require.NoError(t, err)
		require.True(t, ready)
		roles, err := c.roleSpecsFromRevisionInRound(round, ms, revision)
		require.NoError(t, err)
		require.False(t, roleTemplateChanged(roles, role))
	}
	assertReads := func(podLists, revisionGets int) {
		t.Helper()
		actualLists, actualGets := 0, 0
		for _, action := range client.Actions() {
			if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
				actualLists++
			}
			if action.GetVerb() == "get" && action.GetResource().Resource == "controllerrevisions" {
				actualGets++
			}
		}
		require.Equal(t, podLists, actualLists)
		require.Equal(t, revisionGets, actualGets)
	}
	assertReads(1, 1)

	unready := pod.DeepCopy()
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err = client.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), unready, metav1.UpdateOptions{})
	require.NoError(t, err)
	ready, err := c.checkRoleReadyInRound(round, ms, group, role.Name, instances[0].Name)
	require.NoError(t, err)
	require.True(t, ready, "one planning round uses one consistent observation")
	assertReads(1, 1)

	nextRound := withCoordinatedRound(round, ms)
	ready, err = c.checkRoleReadyInRound(nextRound, ms, group, role.Name, instances[0].Name)
	require.NoError(t, err)
	require.False(t, ready, "a new round observes readiness loss without an informer update")
	assertReads(2, 2)

	require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}))
	replacement := pod.DeepCopy()
	replacement.UID = types.UID(string(pod.UID) + "-replacement")
	_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), replacement, metav1.CreateOptions{})
	require.NoError(t, err)
	_, previous, err := c.coordinatedLiveRole(nextRound, ms, group, role.Name, instances[0])
	require.NoError(t, err)
	require.Equal(t, pod.UID, previous.UID)
	_, latest, err := c.coordinatedLiveRole(withCoordinatedRound(nextRound, ms), ms, group, role.Name, instances[0])
	require.NoError(t, err)
	require.Equal(t, replacement.UID, latest.UID, "same-name replacement must not retain the previous round's UID")
	assertReads(3, 2)
}

func TestCoordinatedActionRechecksDependencyBudgetAndReplacementAfterPlanning(t *testing.T) {
	for _, method := range []api.UpdateMethod{api.RecreateUpdateMethod, api.InPlaceUpdateMethod} {
		for _, change := range []string{"none", "dependency-worker-unready", "sibling-worker-unready", "candidate-replaced-with-target"} {
			t.Run(string(method)+"/"+change, func(t *testing.T) {
				ms := mergeReadyModelServing("action-facts", 1, 2)
				ms.Spec.RolloutStrategy.UpdateMethod = method
				ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}
				for i := range ms.Spec.Template.Roles {
					role := &ms.Spec.Template.Roles[i]
					role.WorkerReplicas = 1
					role.EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
					role.EntryTemplate.Spec.Containers[0].Image = "test-image:target"
					role.WorkerTemplate = role.EntryTemplate.DeepCopy()
				}
				oldMS := ms.DeepCopy()
				for i := range oldMS.Spec.Template.Roles {
					oldMS.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "test-image:old"
					oldMS.Spec.Template.Roles[i].WorkerTemplate.Spec.Containers[0].Image = "test-image:old"
				}
				oldRevision, targetRevision := utils.ModelServingRevision(oldMS), utils.ModelServingRevision(ms)
				ms.Status.CurrentRevision, ms.Status.UpdateRevision = oldRevision, targetRevision
				c := newRevisionTestController(t, ms)
				_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, oldRevision, oldMS.Spec.Template.Roles)
				require.NoError(t, err)
				key, group := utils.GetNamespaceName(ms), ms.Name+"-0"
				c.store.AddServingGroup(key, 0, oldRevision)
				prefillOld, decodeOld := oldMS.Spec.Template.Roles[0], oldMS.Spec.Template.Roles[1]
				prefillTarget, decodeTarget := ms.Spec.Template.Roles[0], ms.Spec.Template.Roles[1]
				sibling := addMergeReadyInstance(t, c, ms, group, prefillOld, 0, oldRevision, oldRevision, utils.CalRoleTemplateHash(prefillOld))
				candidate := addMergeReadyInstance(t, c, ms, group, prefillOld, 1, oldRevision, oldRevision, utils.CalRoleTemplateHash(prefillOld))
				addMergeReadyInstance(t, c, ms, group, decodeOld, 0, oldRevision, oldRevision, utils.CalRoleTemplateHash(decodeOld))
				dependency := addMergeReadyInstance(t, c, ms, group, decodeTarget, 1, targetRevision, targetRevision, utils.CalRoleTemplateHash(decodeTarget))
				require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
				planning := withCoordinatedRound(context.Background(), ms)
				require.NoError(t, c.refreshCoordinatedAvailabilityInRound(planning, ms))
				policy, err := c.resolveRoleRolloutPolicy(planning, ms, targetRevision)
				require.NoError(t, err)
				groups, err := c.store.GetServingGroupByModelServing(key)
				require.NoError(t, err)
				require.Len(t, groups, 1)
				planned, _, err := c.rolesToDeleteForRoleRollingUpdateInRound(planning, ms, groups[0], policy.group(group))
				require.NoError(t, err)
				selected := roleToDelete{roleName: "prefill", roleID: "prefill-1"}
				require.Contains(t, planned, selected, "fixture must admit the candidate before the live facts change")
				require.Len(t, planned, 1, "the old decode must remain for its old prefill callers")

				client := c.kubeClientSet.(*kubefake.Clientset)
				switch change {
				case "dependency-worker-unready", "sibling-worker-unready":
					changed := dependency[1].DeepCopy()
					if change == "sibling-worker-unready" {
						changed = sibling[1].DeepCopy()
					}
					changed.Status.Conditions[0].Status = corev1.ConditionFalse
					_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), changed, metav1.UpdateOptions{})
					require.NoError(t, err)
				case "candidate-replaced-with-target":
					entry := utils.GenerateEntryPod(*prefillTarget.DeepCopy(), ms, group, "prefill-1", targetRevision, utils.CalRoleTemplateHash(prefillTarget))
					worker := utils.GenerateWorkerPod(*prefillTarget.DeepCopy(), ms, group, "prefill-1", 1, targetRevision, utils.CalRoleTemplateHash(prefillTarget))
					for i, replacement := range []*corev1.Pod{entry, worker} {
						replacement.UID = types.UID(string(candidate[i].UID) + "-target")
						replacement.Status = *candidate[i].Status.DeepCopy()
						require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(context.Background(), candidate[i].Name, metav1.DeleteOptions{}))
						_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), replacement, metav1.CreateOptions{})
						require.NoError(t, err)
					}
				}
				// Keep the planning context, informer and datastore unchanged. The
				// action boundary must obtain fresh API facts itself.
				client.ClearActions()
				_, err = c.deleteOutdatedRoles(planning, ms, groups, targetRevision, policy)
				require.NoError(t, err)
				if change == "none" {
					mutated := false
					for _, action := range client.Actions() {
						_, collectionDelete := action.(clienttesting.DeleteCollectionAction)
						if action.GetResource().Resource == "pods" && (action.GetVerb() == "patch" || collectionDelete) {
							mutated = true
						}
					}
					require.True(t, mutated, "positive control must dispatch an allowed rollout action")
					return
				}
				requireNoMergeReadyPodMutations(t, client.Actions())
				require.NotEqual(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, group, "prefill", "prefill-1"))
				for _, original := range candidate {
					actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), original.Name, metav1.GetOptions{})
					require.NoError(t, err)
					expectedUID := original.UID
					if change == "candidate-replaced-with-target" {
						expectedUID = types.UID(string(original.UID) + "-target")
						require.Equal(t, targetRevision, utils.ObjectRevision(actual))
						require.Equal(t, utils.CalRoleTemplateHash(prefillTarget), utils.ObjectRoleTemplateHash(actual))
					}
					require.Equal(t, expectedUID, actual.UID)
					require.Nil(t, actual.DeletionTimestamp)
				}
			})
		}
	}
}

func TestCoordinatedScaleDownRetainsHistorySafetyForTheSurvivingGroup(t *testing.T) {
	for _, damagedGroup := range []int{1, 0} {
		t.Run(fmt.Sprintf("damaged-group-%d", damagedGroup), func(t *testing.T) {
			ms := mergeReadyModelServing("scale-two-to-one", 1, 1)
			c := newRevisionTestController(t, ms)
			key, revision := utils.GetNamespaceName(ms), utils.ModelServingRevision(ms)
			for groupOrdinal := 0; groupOrdinal < 2; groupOrdinal++ {
				group := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
				c.store.AddServingGroup(key, groupOrdinal, revision)
				applied := revision
				if groupOrdinal == damagedGroup {
					applied = "missing-applied-history"
				}
				for _, role := range ms.Spec.Template.Roles {
					addMergeReadyInstance(t, c, ms, group, role, 0, applied, applied, utils.CalRoleTemplateHash(role))
				}
				require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			err := c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name)
			if damagedGroup == 1 {
				require.NoError(t, err, "discarded group's missing history cannot block explicit 2-to-1 scaling")
			} else {
				require.ErrorContains(t, err, "missing-applied-history", "retained group's history remains necessary for its safe reconciliation")
			}
			retained, discarded := utils.GenerateServingGroupName(ms.Name, 0), utils.GenerateServingGroupName(ms.Name, 1)
			require.NotEqual(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, retained))
			require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, discarded))
			deleted := false
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "pods" || action.GetVerb() == "list" || action.GetVerb() == "get" {
					continue
				}
				if action.GetVerb() == "patch" {
					assertDeletionScopePatch(t, action, utils.DeletionScopeServingGroup, "")
					require.Contains(t, action.(clienttesting.PatchAction).GetName(), discarded+"-")
					continue
				}
				deletion, ok := action.(clienttesting.DeleteAction)
				require.True(t, ok, "only the selected excess group may have a Pod mutation; got %s", action.GetVerb())
				require.Contains(t, deletion.GetName(), discarded+"-")
				require.NotNil(t, deletion.GetDeleteOptions().Preconditions)
				require.NotEmpty(t, *deletion.GetDeleteOptions().Preconditions.UID)
				deleted = true
			}
			require.True(t, deleted, "the same normal scale-down candidate must be removed in both history placements")
		})
	}
}

func TestManagedRoleObservationsRefreshForEachInstance(t *testing.T) {
	ctx := context.Background()
	ms := mergeReadyModelServing("managed-observations", 1, 2)
	ms.Spec.RolloutStrategy.RoleCoordination = nil
	ms.Spec.Template.Roles = ms.Spec.Template.Roles[:1]
	role := &ms.Spec.Template.Roles[0]
	role.Partition = ptr.To(intstr.FromInt(1))
	role.WorkerReplicas = 1
	role.WorkerTemplate = role.EntryTemplate.DeepCopy()
	oldMS := ms.DeepCopy()
	oldRole := &oldMS.Spec.Template.Roles[0]
	oldRole.EntryTemplate.Spec.Containers[0].Image = "old"
	oldRole.WorkerTemplate.Spec.Containers[0].Image = "old"
	revision, oldRevision := utils.ModelServingRevision(ms), utils.ModelServingRevision(oldMS)
	key, group := utils.GetNamespaceName(ms), ms.Name+"-0"
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = oldRevision, revision
	c := newRevisionTestController(t, ms)
	require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
	recordTestRevision(t, c.kubeClientSet, ms, revision)
	recordTestRevision(t, c.kubeClientSet, oldMS, oldRevision)
	c.store.AddServingGroup(key, 0, oldRevision)
	addMergeReadyInstance(t, c, ms, group, *oldRole, 0, oldRevision, oldRevision, utils.CalRoleTemplateHash(*oldRole))
	worker := addMergeReadyInstance(t, c, ms, group, *role, 1, revision, revision, utils.CalRoleTemplateHash(*role))[1].DeepCopy()
	client := c.kubeClientSet.(*kubefake.Clientset)
	injected := false
	client.PrependReactor("get", "controllerrevisions", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if injected || action.(clienttesting.GetAction).GetName() != utils.GenerateControllerRevisionName(ms.Name, oldRevision) {
			return false, nil, nil
		}
		// Planning skips protected instance 0. Its applied history is loaded
		// after its Pod observation, before the next instance is observed.
		injected = true
		worker.Status.Conditions[0].Status = corev1.ConditionFalse
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), worker, ms.Namespace); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	client.ClearActions()
	require.NoError(t, c.manageRoleReplicasPerGroup(ctx, ms, group, *role, 0, revision, true, nil))
	require.True(t, injected, "the protected instance must load its distinct applied history")
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, role.Name, role.Name+"-0"))
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, group, role.Name, role.Name+"-1"), "the second instance must observe the API-only Ready loss")
	requireNoMergeReadyPodMutations(t, client.Actions())

	worker.Status.Conditions[0].Status = corev1.ConditionTrue
	_, err := client.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, worker, metav1.UpdateOptions{})
	require.NoError(t, err)
	client.ClearActions()
	require.NoError(t, c.manageRoleReplicasPerGroup(ctx, ms, group, *role, 0, revision, true, nil))
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, role.Name, role.Name+"-1"), "the next invocation must observe API-only recovery")
	requireNoMergeReadyPodMutations(t, client.Actions())
}

func TestIndependentRolePlanningObservationsRefreshBetweenCalls(t *testing.T) {
	for _, entry := range []string{"groups", "role", "outdated"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			ms := mergeReadyModelServing("planning-observations", 3, 3)
			ms.Spec.RolloutStrategy.RoleCoordination = nil
			oldMS := ms.DeepCopy()
			oldMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
			revision, oldRevision, key := utils.ModelServingRevision(ms), utils.ModelServingRevision(oldMS), utils.GetNamespaceName(ms)
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = revision, revision
			c := newRevisionTestController(t, ms)
			require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, utils.GenerateControllerRevisionName(ms.Name, revision), metav1.DeleteOptions{}))
			recordTestRevision(t, c.kubeClientSet, ms, revision)
			recordTestRevision(t, c.kubeClientSet, oldMS, oldRevision)
			role, group := ms.Spec.Template.Roles[0], ms.Name+"-0"
			var first *corev1.Pod
			for ordinal := 0; ordinal < 3; ordinal++ {
				name := utils.GenerateServingGroupName(ms.Name, ordinal)
				c.store.AddServingGroup(key, ordinal, revision)
				for _, spec := range ms.Spec.Template.Roles {
					for instance := 0; instance < 3; instance++ {
						pod := addMergeReadyInstance(t, c, ms, name, spec, instance, revision, revision, utils.CalRoleTemplateHash(spec))[0]
						if name == group && spec.Name == role.Name && instance == 0 {
							first = pod.DeepCopy()
						}
					}
				}
			}
			groups, err := c.store.GetServingGroupByModelServing(key)
			require.NoError(t, err)
			instances, err := c.store.GetRoleList(key, group, role.Name)
			require.NoError(t, err)
			observe := func() bool {
				switch entry {
				case "groups":
					return c.hasUpdateableOutdatedServingGroup(ctx, ms, groups, revision, 0)
				case "role":
					return c.hasUpdateableOutdatedRoleInRound(ctx, ms, group, role, instances)
				default:
					outdated, _ := c.outdatedRolesInRound(ctx, ms, datastore.ServingGroup{Name: group, Revision: revision}, role, instances)
					return len(outdated) > 0
				}
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			for _, replaced := range []bool{false, true} {
				if replaced {
					require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, first.Name, metav1.DeleteOptions{}))
					first.UID += "-replacement"
					first.Labels[api.RevisionLabelKey] = oldRevision
					first.Labels[api.RoleTemplateHashLabelKey] = utils.CalRoleTemplateHash(oldMS.Spec.Template.Roles[0])
					first.Spec.Containers[0].Image = "old"
					_, err := client.CoreV1().Pods(ms.Namespace).Create(ctx, first, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				// Cache identity and revision stay unchanged between calls.
				client.ClearActions()
				require.Equal(t, replaced, observe(), "each invocation must read the new owned identity and applied revision")
				lists := 0
				for _, action := range client.Actions() {
					if action.Matches("list", "pods") {
						lists++
					}
				}
				require.Equal(t, 1, lists, "one read-only invocation shares its Pod observation")
				requireNoMergeReadyPodMutations(t, client.Actions())
			}
		})
	}
}
