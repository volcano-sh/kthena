/*
Copyright 2025 The Volcano Authors.

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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// A Role that reconciliation finds ready must report the transition the same
// way the Pod handler does, otherwise the Event stream depends on which path
// happened to observe it first.
func TestReconciliationEmitsRoleRunningEvent(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	events := record.NewFakeRecorder(10)
	c.recorder = events

	const (
		groupName = "cleanup-0"
		roleID    = "infer-0"
		revision  = "v1"
	)
	ms.Spec.Replicas = ptr.To[int32](1)
	role := workload.Role{Name: "infer", Replicas: ptr.To[int32](1), WorkerReplicas: 0}
	ms.Spec.Template.Roles = []workload.Role{role}
	c.store.AddRole(utils.GetNamespaceName(ms), groupName, role.Name, roleID, revision, "hash")
	_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, revision, ms.Spec.Template.Roles)
	require.NoError(t, err)

	labels := map[string]string{}
	for k, v := range svc.Labels {
		labels[k] = v
	}
	labels[workload.RevisionLabelKey] = revision
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            utils.GeneratePodName(groupName, roleID, 0),
			Namespace:       ms.Namespace,
			UID:             types.UID("entry"),
			Labels:          labels,
			OwnerReferences: svc.OwnerReferences,
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))

	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(ms), groupName, role.Name, roleID))
	require.NoError(t, c.manageRoleReplicasPerGroup(context.Background(), ms, groupName, role, 0, revision, true, nil))
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), groupName, role.Name, roleID),
		"a complete, Ready Role must reconcile to Running")

	announced := false
	for len(events.Events) > 0 {
		if strings.Contains(<-events.Events, "RoleRunning") {
			announced = true
		}
	}
	require.True(t, announced, "reconciling a Role to Running must emit a RoleRunning Event")
}

// Re-running a sync that changes nothing must stay quiet, so the Event stream
// is not one entry per resync.
func TestRepeatedRoleStatusUpdateEmitsOneEvent(t *testing.T) {
	c, _, ms, _ := auxiliaryFixture(t)
	events := record.NewFakeRecorder(10)
	c.recorder = events
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v1", "hash")

	for i := 0; i < 3; i++ {
		require.NoError(t, c.updateRoleStatus(ms, "cleanup-0", "infer", "infer-0", datastore.RoleRunning))
	}
	require.Len(t, events.Events, 1, "only the transition is an Event, not every sync that re-asserts it")
}
