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

// This file contains unit tests for the manageRoleReplicas function, targeting the
// scaling behavior when the datastore contains roles in RoleDeleting status (pods still
// terminating). The tests verify that:
//
//   - Scale-up is triggered based on the count of active (non-Deleting) roles, not the
//     total role count, so that a pending pod deletion does not block creating the next
//     replica (e.g. Replicas changed 1→2 while the old pod is still Terminating).
//
//   - Scale-down is only triggered when the count of active roles exceeds expectedCount,
//     preventing unnecessary scaleDownRoles calls when Deleting entries temporarily
//     inflate len(roleList) beyond expectedCount.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// newRoleReplicasTestController creates a lightweight controller suitable for
// directly calling manageRoleReplicas without starting the informer loop.
func newRoleReplicasTestController(t *testing.T) (*ModelServingController, *kubefake.Clientset) {
	t.Helper()

	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()

	controller, err := NewModelServingController(
		kubeClient, kthenaClient, volcanoClient,
		apiextfake.NewSimpleClientset(),
	)
	require.NoError(t, err)

	// Inject a no-op PodGroupManager so CreatePodsByRole doesn't panic.
	controller.podGroupManager = &fakePodGroupManager{}

	return controller, kubeClient
}

// addEntryPodToIndexer adds an entry pod to the pods informer indexer so that
// the manageRoleReplicas pod-recreation loop skips the given role (pod already present).
//
// The pod is constructed with labels matching RoleIDIndexFunc's composite key format:
//
//	"<namespace>/<groupName>/<roleName>/<roleID>"
//
// Without this, the pod-recreation loop (which checks `len(pods) < expectedPods`) would
// see 0 pods for every Running role and trigger CreatePodsByRole for them, polluting
// the pod-action count and obscuring whether scale-up was actually triggered.
func addEntryPodToIndexer(t *testing.T, controller *ModelServingController, ns, groupName, roleName, roleID string, msUID types.UID) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      fmt.Sprintf("%s-%s-entry", groupName, roleID),
			Labels: map[string]string{
				workloadv1alpha1.GroupNameLabelKey: groupName,
				workloadv1alpha1.RoleLabelKey:      roleName,
				workloadv1alpha1.RoleIDKey:         roleID,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
					Kind:       workloadv1alpha1.ModelServingKind.Kind,
					UID:        msUID,
				},
			},
		},
	}
	if err := controller.podsInformer.GetIndexer().Add(pod); err != nil {
		t.Fatalf("addEntryPodToIndexer: failed to add pod %s to indexer: %v", pod.Name, err)
	}
}

// buildTestMSForRoleReplicas creates a minimal ModelServing with a single "infer"
// role that has Replicas=expectedRoleReplicas.
func buildTestMSForRoleReplicas(name, namespace string, expectedRoleReplicas int32) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "infer",
						Replicas: ptr.To[int32](expectedRoleReplicas),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:latest"},
								},
							},
						},
					},
				},
			},
		},
	}
}

// TestManageRoleReplicas_ScaleUpIgnoresRoleDeletingInStore is the primary regression
// test for the RoleDeleting count bug.
//
// This test asserts the DESIRED behavior (what should happen after the fix).
// It FAILS before the fix is applied and PASSES after the fix.
//
// Setup: store={infer-0(Running), infer-1(Deleting)}, expectedCount=2.
//
// DESIRED (after fix): activeCount=1 < expectedCount=2 → scaleUpRoles creates infer-2
//
//	→ store gains infer-2 entry → len(storeAfter) == 3
//
// BUG (current code): len(roleList)=2 == expectedCount=2 → silent no-op
//
//	→ store unchanged → len(storeAfter) == 2   ← this assertion fails → confirms bug
func TestManageRoleReplicas_ScaleUpIgnoresRoleDeletingInStore(t *testing.T) {
	controller, _ := newRoleReplicasTestController(t)

	const (
		msName   = "test-scale-up-blocked"
		ns       = "default"
		roleName = "infer"
		revision = "rev-1"
		groupOrd = 0
	)
	groupName := utils.GenerateServingGroupName(msName, groupOrd)
	ms := buildTestMSForRoleReplicas(msName, ns, 2)
	nsName := utils.GetNamespaceName(ms)

	// Seed store: infer-0 (Running) + infer-1 (Deleting, pod still terminating in cluster)
	controller.store.AddServingGroup(nsName, groupOrd, revision)
	controller.store.AddRole(nsName, groupName, roleName, utils.GenerateRoleID(roleName, 0), revision, "hash")
	require.NoError(t, controller.store.UpdateRoleStatus(nsName, groupName, roleName, utils.GenerateRoleID(roleName, 0), datastore.RoleRunning))
	controller.store.AddRole(nsName, groupName, roleName, utils.GenerateRoleID(roleName, 1), revision, "hash")
	require.NoError(t, controller.store.UpdateRoleStatus(nsName, groupName, roleName, utils.GenerateRoleID(roleName, 1), datastore.RoleDeleting))

	// Add infer-0 entry pod to indexer so the pod-recreation loop skips it.
	// Without this, the loop sees 0 pods for infer-0 and calls CreatePodsByRole for it,
	// making it appear as if scale-up was triggered even when it wasn't.
	addEntryPodToIndexer(t, controller, ns, groupName, roleName, utils.GenerateRoleID(roleName, 0), ms.UID)

	// Confirm starting state: 2 store entries (1 Running + 1 Deleting).
	roleListBefore, err := controller.store.GetRoleList(nsName, groupName, roleName)
	require.NoError(t, err)
	require.Len(t, roleListBefore, 2)

	// Call manageRoleReplicas with expectedCount=2.
	targetRole := ms.Spec.Template.Roles[0]
	controller.manageRoleReplicas(context.Background(), ms, groupName, targetRole, groupOrd, revision)

	// DESIRED (after fix): scale-up creates infer-2, store gains a 3rd entry.
	// BUG (before fix):
	//   Bug1: len(roleList)=2 == expectedCount=2 → scaleUpRoles not called at all.
	//   Bug2: even if called, toCreate = expectedCount - len(roleList) = 2-2 = 0.
	//   Result: store unchanged at 2 entries.
	roleListAfter, err := controller.store.GetRoleList(nsName, groupName, roleName)
	require.NoError(t, err)
	assert.Len(t, roleListAfter, 3,
		"scale-up should have added infer-2 to the store: activeCount=1 < expectedCount=2. "+
			"Bug: len(roleList)=%d includes RoleDeleting entry and equals expectedCount=2, "+
			"causing scaleUpRoles to be silently skipped",
		len(roleListBefore))

	// Verify the newly created role has the expected name.
	// scaleUpRoles derives the starting index from the current max index in roleList
	// (infer-1 has index 1), so the new role must be infer-2.
	expectedNewRoleID := utils.GenerateRoleID(roleName, 2)
	var newRole *datastore.Role
	for i := range roleListAfter {
		if roleListAfter[i].Name == expectedNewRoleID {
			newRole = &roleListAfter[i]
			break
		}
	}
	assert.NotNil(t, newRole,
		"expected new role %q to be present in the store after scale-up, got names: %v",
		expectedNewRoleID, func() []string {
			names := make([]string, len(roleListAfter))
			for i, r := range roleListAfter {
				names[i] = r.Name
			}
			return names
		}())
}

// TestManageRoleReplicas_ScaleDownNotTriggeredWhenActiveCountMatchesExpected verifies
// that scale-down is NOT triggered when the active (non-Deleting) role count equals
// expectedCount, even though len(roleList) > expectedCount due to Deleting entries.
//
// Setup: store={infer-0(Running), infer-1(Running), infer-2(Deleting)}, expectedCount=2.
// len(roleList)=3 > expectedCount=2, but activeRoleCount=2 == expectedCount=2.
//
// Asserts: both Running roles remain Running and the store entry count is unchanged,
// confirming that no unnecessary scale-down was triggered.
func TestManageRoleReplicas_ScaleDownNotTriggeredWhenActiveCountMatchesExpected(t *testing.T) {
	controller, _ := newRoleReplicasTestController(t)

	const (
		msName   = "test-no-scaledown-with-deleting"
		ns       = "default"
		roleName = "infer"
		revision = "rev-1"
		groupOrd = 0
	)
	groupName := utils.GenerateServingGroupName(msName, groupOrd)
	ms := buildTestMSForRoleReplicas(msName, ns, 2)
	nsName := utils.GetNamespaceName(ms)

	// Seed store: infer-0 + infer-1 (Running) + infer-2 (Deleting, pod still terminating).
	controller.store.AddServingGroup(nsName, groupOrd, revision)
	for _, idx := range []int{0, 1} {
		roleID := utils.GenerateRoleID(roleName, idx)
		controller.store.AddRole(nsName, groupName, roleName, roleID, revision, "hash")
		require.NoError(t, controller.store.UpdateRoleStatus(nsName, groupName, roleName, roleID, datastore.RoleRunning))
		addEntryPodToIndexer(t, controller, ns, groupName, roleName, roleID, ms.UID)
	}
	roleID2 := utils.GenerateRoleID(roleName, 2)
	controller.store.AddRole(nsName, groupName, roleName, roleID2, revision, "hash")
	require.NoError(t, controller.store.UpdateRoleStatus(nsName, groupName, roleName, roleID2, datastore.RoleDeleting))

	// Confirm starting state: 3 store entries (2 Running + 1 Deleting).
	roleListBefore, err := controller.store.GetRoleList(nsName, groupName, roleName)
	require.NoError(t, err)
	require.Len(t, roleListBefore, 3)

	targetRole := ms.Spec.Template.Roles[0]
	controller.manageRoleReplicas(context.Background(), ms, groupName, targetRole, groupOrd, revision)

	roleListAfter, err := controller.store.GetRoleList(nsName, groupName, roleName)
	require.NoError(t, err)

	// Store count must remain at 3: no scale-up (activeCount==expected) and no scale-down.
	assert.Len(t, roleListAfter, 3, "store count must not change when activeRoleCount==expectedCount")

	// Running roles must remain Running: scale-down must not have deleted infer-0 or infer-1.
	for _, r := range roleListAfter {
		if r.Name == utils.GenerateRoleID(roleName, 0) || r.Name == utils.GenerateRoleID(roleName, 1) {
			assert.NotEqual(t, datastore.RoleDeleting, r.Status,
				"Running role %s must not be marked Deleting by an unnecessary scale-down", r.Name)
		}
	}
}
